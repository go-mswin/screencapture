// Copyright (c) the go-mswin/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

// Package screencapture is a pure-Go, CGO-free capture of Windows displays and
// windows. It enumerates what a process may capture and streams a display (or
// a single window) as raw BGRA pixels.
//
// It is the Windows sibling of github.com/go-macos/screencapture and presents
// deliberately the same shape, so one consumer can drive both platforms through
// near-identical adapters. Where Windows genuinely differs the difference is
// named rather than papered over; see "Differences from the macOS sibling"
// below.
//
// # Two backends
//
// [BackendDuplication] is DXGI Desktop Duplication (IDXGIOutputDuplication).
// It is change-driven, it reads back through a D3D11 staging texture and it is
// what every serious capture tool on Windows uses. It captures a whole output
// only: no scaling, no cursor compositing, no single window.
//
// [BackendGDI] is BitBlt into a DIB section. It works everywhere — over RDP, on
// adapters that refuse duplication, on a session with no GPU — it can scale, it
// can draw the cursor and it can capture one window. It is poll-driven: it
// costs a full readback every frame whether or not anything moved.
//
// [BackendAuto] (the zero value) uses duplication when the request allows it
// and falls back to GDI otherwise, including when duplication is refused at
// run time. [Stream.Backend] reports which one actually ran.
//
// # The hot path
//
// The package is written for a compositor that redraws every frame and cannot
// afford a copy or an allocation per frame. [Stream.Frame] hands back a
// BORROWED view of the most recent captured frame — the DIB section GDI blitted
// into, or the mapped D3D11 staging texture, not a copy — together with a
// boolean saying whether it is newer than the one the previous call returned.
// In steady state a Frame call performs no allocation at all.
//
// The borrow is valid until the next call to [Stream.Frame], [Stream.WaitFrame]
// or [Stream.Close]. Copy out of it (see [Frame.CopyTight] or [Frame.NRGBA])
// if you need to keep it longer. The stream keeps a small ring of buffers
// ([Options.QueueDepth]) so the capture side never writes into the buffer the
// consumer is holding.
//
// # Stride
//
// A captured frame's rows are PADDED and the padding is not the same between
// the two backends: a DIB section is DWORD-aligned (which for 32bpp means
// Width*4 exactly), while a mapped D3D11 staging texture reports whatever
// RowPitch the driver chose, commonly the width rounded up to 256 bytes. Stride
// is CARRIED on every [Frame] and is never Width*4 by assumption. Always index
// with Stride, or use [Frame.Row].
//
// # Bottom-up DIBs are normalised here
//
// A Windows DIB is bottom-up by default: BITMAPINFOHEADER.biHeight is POSITIVE
// and row 0 is the BOTTOM row of the image. This package always asks for a
// top-down DIB (a negative biHeight) and, if a source ever hands back a
// bottom-up one anyway, flips it inside the package. A [Frame] handed to a
// consumer is ALWAYS top-down, row 0 at the top. See [DIBLayout].
//
// # Pixel format
//
// Frames are [FormatBGRA]: four bytes per pixel in blue, green, red, alpha
// order. That is what Windows produces natively at both ends —
// DXGI_FORMAT_B8G8R8A8_UNORM from duplication, a 32bpp BI_RGB DIB from GDI — so
// no conversion happens on the hot path. The alpha byte is carried through
// rather than forced to 255; a GDI screen capture leaves it at 0, which is why
// [Frame.NRGBA] has [Frame.NRGBAOpaque] next to it.
//
// # Permission
//
// Windows has no screen-recording permission gate: any process in an
// interactive session may capture the desktop. [Authorized] therefore reports
// true whenever [Available] does and [RequestAuthorization] prompts nothing.
// The failures that DO happen are different in kind: a service or a CI runner
// has no interactive desktop, and duplication is refused on some adapters —
// both surface as errors from the capture call, not as a missing grant.
//
// # Differences from the macOS sibling
//
//   - [Options.Backend] is new: macOS has exactly one capture route, Windows
//     has two with materially different properties.
//   - [Options.ExcludeWindows] holds HWNDs rather than CGWindowIDs and is
//     implemented with SetWindowDisplayAffinity, which the OS only permits on
//     windows the calling process owns. Excluding someone else's window is an
//     error rather than a silent no-op.
//   - [ErrAccessLost] is new. A duplication stream is torn down by the OS on a
//     mode change, a UAC secure-desktop transition or a session switch. The
//     stream recovers by itself; the sentinel exists so a consumer can tell a
//     recoverable interruption from a real failure.
//   - [Authorized] and [RequestAuthorization] are trivially true: see above.
//   - [Display.PixelWidth]/[Display.PixelHeight] are read with the process
//     marked per-monitor-DPI-aware, so they are real device pixels.
//   - GDI is not change-driven, so with [BackendGDI] the freshness flag from
//     [Stream.Frame] is true on every tick. Only [BackendDuplication] can tell
//     you that nothing moved.
package screencapture

import (
	"errors"
	"fmt"
	"image"
	"time"
)

// Sentinel errors. All are stable and may be matched with errors.Is.
var (
	// ErrUnsupported is reported on every non-Windows platform, and on a
	// Windows too old to carry the APIs this package needs (before Windows 8,
	// which is where Desktop Duplication and the per-monitor DPI calls
	// arrived).
	ErrUnsupported = errors.New("screencapture: unsupported on this platform (Windows 8 or later only)")

	// ErrPermissionDenied is reported when the OS refuses the capture for an
	// access reason: DXGI_ERROR_ACCESS_DENIED, E_ACCESSDENIED, or a
	// SetWindowDisplayAffinity on a window the process does not own. It is NOT
	// a user-facing grant like the macOS TCC prompt — Windows has no such
	// grant — so the remedy is a session or a privilege, not a settings pane.
	ErrPermissionDenied = errors.New("screencapture: access denied — " +
		"the desktop refused the capture; a process with no interactive desktop " +
		"(a service, a session-0 task, an unattended CI runner) cannot capture, " +
		"and a window owned by another process cannot be excluded")

	// ErrNoDisplay is reported when a capture was asked for and the system
	// listed no display at all.
	ErrNoDisplay = errors.New("screencapture: no capturable display")

	// ErrNotFound is reported when a display or window handle does not name
	// anything currently capturable.
	ErrNotFound = errors.New("screencapture: no such display or window")

	// ErrClosed is reported by every [Stream] method after [Stream.Close].
	ErrClosed = errors.New("screencapture: stream is closed")

	// ErrNoFrame is reported by [Stream.WaitFrame] when no frame arrived
	// before its context expired. With [BackendDuplication] it is NOT a
	// malfunction: a motionless desktop legitimately produces no frames.
	ErrNoFrame = errors.New("screencapture: no frame available")

	// ErrInvalidOption is reported by [Options.Validate] and wraps a
	// description of the offending field.
	ErrInvalidOption = errors.New("screencapture: invalid option")

	// ErrShortBuffer is reported by [Frame.CopyTight] when the destination is
	// too small to hold the frame.
	ErrShortBuffer = errors.New("screencapture: destination buffer too short")

	// ErrBackendUnavailable is reported when the backend the caller INSISTED
	// on cannot run here — most often DXGI Desktop Duplication refused by the
	// adapter (DXGI_ERROR_UNSUPPORTED), by a session with no interactive
	// desktop, or over a remote-desktop connection. With [BackendAuto] this is
	// never surfaced: the stream falls back to [BackendGDI] instead.
	ErrBackendUnavailable = errors.New("screencapture: the requested capture backend is unavailable here")

	// ErrAccessLost is reported when a live duplication stream was torn down
	// by the OS: a display mode change, a UAC secure-desktop transition, a
	// session switch, or the adapter being reset. It has no macOS counterpart.
	// The stream re-establishes duplication by itself, so a consumer normally
	// only ever sees it as a gap in frames; it is exposed so that a consumer
	// which wants to know CAN.
	ErrAccessLost = errors.New("screencapture: desktop duplication access lost (mode change, secure desktop or session switch)")
)

// PixelFormat names the layout of a captured frame.
type PixelFormat uint32

// FormatBGRA is 32-bit BGRA. It is the only format this package streams: it is
// what a compositor wants, it is what Windows produces natively at both ends
// (DXGI_FORMAT_B8G8R8A8_UNORM and a 32bpp BI_RGB DIB), and it is packed rather
// than planar so a frame row is one contiguous run of bytes.
//
// The value is the four-character code 'BGRA', matching the macOS sibling's
// CoreVideo OSType so a cross-platform consumer can compare the two directly.
const FormatBGRA PixelFormat = 0x42475241 // 'BGRA'

// String renders the format as its four-character code, e.g. "BGRA".
func (f PixelFormat) String() string {
	b := [4]byte{byte(f >> 24), byte(f >> 16), byte(f >> 8), byte(f)}
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return fmt.Sprintf("PixelFormat(%#08x)", uint32(f))
		}
	}
	return string(b[:])
}

// BytesPerPixel is the size of one pixel in this format.
func (f PixelFormat) BytesPerPixel() int {
	if f == FormatBGRA {
		return 4
	}
	return 0
}

// Backend names a capture route.
type Backend uint8

// The capture routes. See the package documentation for what separates them.
const (
	// BackendAuto uses [BackendDuplication] when the request allows it and
	// falls back to [BackendGDI] otherwise. It is the zero value.
	BackendAuto Backend = iota
	// BackendDuplication is DXGI Desktop Duplication: change-driven, whole
	// outputs only, no scaling and no cursor.
	BackendDuplication
	// BackendGDI is BitBlt into a DIB section: poll-driven, works everywhere,
	// scales, draws the cursor, captures a single window.
	BackendGDI
)

// String renders the backend for logs.
func (b Backend) String() string {
	switch b {
	case BackendAuto:
		return "auto"
	case BackendDuplication:
		return "duplication"
	case BackendGDI:
		return "gdi"
	}
	return fmt.Sprintf("Backend(%d)", uint8(b))
}

// Rect is a rectangle in the virtual-screen coordinate space, in device
// PIXELS. It mirrors the Win32 RECT reported by GetMonitorInfo and
// GetWindowRect with the process marked per-monitor-DPI-aware, and is stated
// as an origin plus a size so it reads like the macOS sibling's CGRect.
//
// The virtual screen's origin is the top-left of the PRIMARY display, so a
// display placed to the left of or above the primary one has NEGATIVE X or Y.
// That is normal and must not be clamped.
type Rect struct {
	X, Y, W, H int
}

// String renders the rectangle as "(x,y)+(w×h)".
func (r Rect) String() string { return fmt.Sprintf("(%d,%d)+(%d×%d)", r.X, r.Y, r.W, r.H) }

// Empty reports whether the rectangle encloses no area.
func (r Rect) Empty() bool { return r.W <= 0 || r.H <= 0 }

// Rotation is how a display's image is rotated relative to the panel, matching
// DXGI_MODE_ROTATION.
type Rotation uint32

// The four DXGI_MODE_ROTATION values plus the "driver did not say" one.
const (
	RotationUnspecified Rotation = 0
	RotationIdentity    Rotation = 1
	Rotation90          Rotation = 2
	Rotation180         Rotation = 3
	Rotation270         Rotation = 4
)

// String renders the rotation in degrees.
func (r Rotation) String() string {
	switch r {
	case RotationUnspecified:
		return "unspecified"
	case RotationIdentity:
		return "0°"
	case Rotation90:
		return "90°"
	case Rotation180:
		return "180°"
	case Rotation270:
		return "270°"
	}
	return fmt.Sprintf("Rotation(%d)", uint32(r))
}

// Display is a capturable display.
//
// PixelWidth and PixelHeight are the monitor's real device pixels, read with
// the process marked per-monitor-DPI-aware. Width and Height are the same
// measurement in DEVICE-INDEPENDENT PIXELS at the monitor's DPI, which is the
// closest Windows equivalent of the macOS sibling's "points": they are what an
// unaware application believes the monitor to be. Hand PixelWidth/PixelHeight
// to [Options] for a capture with no resampling.
type Display struct {
	// ID is a stable-per-session identifier. It is the HMONITOR value; do not
	// persist it, monitors get new handles when the layout changes.
	ID uint64
	// DeviceName is the display's GDI device name, e.g. `\\.\DISPLAY1`. It is
	// what matches a monitor to a DXGI output.
	DeviceName string
	// Width and Height are the size in device-independent pixels.
	Width, Height int
	// PixelWidth and PixelHeight are the size in real device pixels.
	PixelWidth, PixelHeight int
	// DPI is the monitor's effective dots per inch; 96 means no scaling.
	DPI int
	// Bounds is the monitor's position and size on the virtual screen, in
	// device pixels. It can have negative X or Y.
	Bounds Rect
	// Work is Bounds minus the taskbar and any other appbar.
	Work Rect
	// Primary reports whether this is the display holding the origin.
	Primary bool
	// Rotation is how the image sits on the panel.
	Rotation Rotation
	// AdapterIndex and OutputIndex locate the display in the DXGI enumeration
	// (IDXGIFactory1::EnumAdapters1 then IDXGIAdapter::EnumOutputs). They are
	// -1 when the display was not matched to a DXGI output, which is what
	// happens on an adapter with no duplication support; such a display can
	// still be captured with [BackendGDI].
	AdapterIndex, OutputIndex int
}

// Scale is the display's DPI scale factor (device pixels per
// device-independent pixel), 1 when the display reports no usable DPI. It is
// the analogue of the macOS sibling's backing scale factor.
func (d Display) Scale() float64 {
	if d.DPI <= 0 {
		return 1
	}
	return float64(d.DPI) / 96.0
}

// Duplicable reports whether this display was matched to a DXGI output and can
// therefore be tried with [BackendDuplication]. A false here is final; a true
// is only a candidate, because the adapter may still refuse.
func (d Display) Duplicable() bool { return d.AdapterIndex >= 0 && d.OutputIndex >= 0 }

// String renders the display for logs.
func (d Display) String() string {
	return fmt.Sprintf("display %#x %s %dx%d px (%dx%d dip @ %d dpi) at %s",
		d.ID, d.DeviceName, d.PixelWidth, d.PixelHeight, d.Width, d.Height, d.DPI, d.Bounds)
}

// Window is a capturable top-level window.
type Window struct {
	// ID is the HWND. Like an HMONITOR it is only valid while the window
	// lives.
	ID uint64
	// Title is the window text.
	Title string
	// ClassName is the window class, which survives a title change and is
	// often the better way to find a window again.
	ClassName string
	// AppName is the base name of the owning process's executable, e.g.
	// "notepad.exe". It is empty when the process could not be opened, which
	// happens for anything running at a higher integrity level.
	AppName string
	// ExePath is the owning process's full image path, empty for the same
	// reason as AppName.
	ExePath string
	// PID is the owning process.
	PID int32
	// Bounds is the window's extended frame bounds — what the user sees, with
	// the invisible DWM resize border already removed — in device pixels on
	// the virtual screen.
	Bounds Rect
	// OnScreen reports whether the window is visible and not minimised.
	OnScreen bool
	// Active reports whether this is the foreground window.
	Active bool
	// Minimized reports whether the window is iconic. A minimised window has
	// no pixels to capture.
	Minimized bool
	// Cloaked reports whether DWM is hiding the window — a background
	// Universal Windows Platform app, or a window on another virtual desktop.
	// Cloaked windows are listed but generally capture as blank.
	Cloaked bool
}

// String renders the window for logs.
func (w Window) String() string {
	return fmt.Sprintf("window %#x %q [%s/%s] at %s", w.ID, w.Title, w.AppName, w.ClassName, w.Bounds)
}

// Application is a process owning capturable windows.
type Application struct {
	PID     int32
	Name    string
	ExePath string
}

// Content is a snapshot of what the calling process may capture. It is a
// snapshot: windows open and close, so re-read it rather than caching it.
type Content struct {
	Displays     []Display
	Windows      []Window
	Applications []Application
}

// Display returns the display with the given HMONITOR.
func (c *Content) Display(id uint64) (Display, error) {
	for _, d := range c.Displays {
		if d.ID == id {
			return d, nil
		}
	}
	return Display{}, fmt.Errorf("%w: display %#x", ErrNotFound, id)
}

// DisplayByName returns the display with the given GDI device name, e.g.
// `\\.\DISPLAY1`.
func (c *Content) DisplayByName(name string) (Display, error) {
	for _, d := range c.Displays {
		if d.DeviceName == name {
			return d, nil
		}
	}
	return Display{}, fmt.Errorf("%w: display %q", ErrNotFound, name)
}

// MainDisplay returns the primary display, or the first one if none is flagged
// as primary.
func (c *Content) MainDisplay() (Display, error) {
	if len(c.Displays) == 0 {
		return Display{}, ErrNoDisplay
	}
	for _, d := range c.Displays {
		if d.Primary {
			return d, nil
		}
	}
	return c.Displays[0], nil
}

// Window returns the window with the given HWND.
func (c *Content) Window(id uint64) (Window, error) {
	for _, w := range c.Windows {
		if w.ID == id {
			return w, nil
		}
	}
	return Window{}, fmt.Errorf("%w: window %#x", ErrNotFound, id)
}

// WindowsByTitle returns every window whose title is exactly title.
func (c *Content) WindowsByTitle(title string) []Window {
	var out []Window
	for _, w := range c.Windows {
		if w.Title == title {
			out = append(out, w)
		}
	}
	return out
}

// WindowsByClass returns every window of the given class.
func (c *Content) WindowsByClass(class string) []Window {
	var out []Window
	for _, w := range c.Windows {
		if w.ClassName == class {
			out = append(out, w)
		}
	}
	return out
}

// WindowsOfPID returns every window owned by the given process.
func (c *Content) WindowsOfPID(pid int32) []Window {
	var out []Window
	for _, w := range c.Windows {
		if w.PID == pid {
			out = append(out, w)
		}
	}
	return out
}

// Options configures a capture stream.
//
// The zero Options is usable: it captures the source at its native pixel size,
// at up to 60 frames per second, without the cursor, choosing the backend
// automatically.
type Options struct {
	// Width and Height are the requested frame size in device PIXELS. Zero
	// means "the source's native pixel size". A size that is not the native
	// one forces [BackendGDI], because Desktop Duplication cannot resample.
	Width, Height int

	// FPS is the CEILING on the frame rate, not a guarantee. With
	// [BackendDuplication] frames only arrive when the desktop changes, so the
	// real rate can be far lower; with [BackendGDI] it is the polling rate and
	// is met unless a readback takes longer than the interval. Zero means
	// [DefaultFPS].
	FPS float64

	// ShowsCursor draws the mouse pointer into the captured frames. Desktop
	// Duplication delivers the cursor as separate shape and position metadata
	// rather than compositing it, so this forces [BackendGDI], which composites
	// it with DrawIconEx.
	ShowsCursor bool

	// QueueDepth is how many capture buffers the stream keeps in a ring. Zero
	// means [DefaultQueueDepth]. It must leave room for the two frames this
	// package holds on the consumer's behalf (the one lent out and the one
	// waiting), so values below [MinQueueDepth] are rejected, and it is capped
	// at [MaxQueueDepth] because on Windows each slot is a full-size DIB
	// section or D3D11 staging texture — at 4K that is 33 MB apiece.
	QueueDepth int

	// ExcludeWindows lists HWNDs to keep out of a DISPLAY capture — for
	// example your own overlay, so capturing the screen it sits on does not
	// feed it back into itself.
	//
	// It is implemented with SetWindowDisplayAffinity(WDA_EXCLUDEFROMCAPTURE),
	// which the OS only allows on windows the CALLING PROCESS owns; asking to
	// exclude somebody else's window fails with [ErrPermissionDenied] rather
	// than being silently ignored. The affinity is restored on [Stream.Close].
	// Ignored for a window capture.
	ExcludeWindows []uint64

	// ScalesToFit letterboxes the source into Width×Height instead of
	// cropping it when the aspect ratios differ. Only meaningful when a size
	// is requested, and therefore only with [BackendGDI].
	ScalesToFit bool

	// Backend selects the capture route. The zero value [BackendAuto] picks
	// duplication when the rest of these options allow it and falls back to
	// GDI when they do not or when the adapter refuses. Naming a backend
	// explicitly turns a refusal into [ErrBackendUnavailable] and an
	// incompatible option into [ErrInvalidOption], which is what you want in a
	// test.
	Backend Backend

	// Timeout bounds one AcquireNextFrame call in the duplication backend. It
	// is not a deadline on the stream: when it expires the backend simply
	// tries again, which is how "nothing changed" is expressed. Zero means
	// [DefaultTimeout]. Ignored by [BackendGDI].
	Timeout time.Duration
}

// Defaults applied to the zero value of the corresponding [Options] field.
const (
	// DefaultFPS is the frame-rate ceiling used when Options.FPS is zero.
	DefaultFPS = 60.0
	// DefaultQueueDepth is the ring size used when Options.QueueDepth is zero.
	DefaultQueueDepth = 3
	// MinQueueDepth is the smallest ring this package accepts: one buffer lent
	// to the consumer, one holding the newest complete frame, one being
	// written.
	MinQueueDepth = 3
	// MaxQueueDepth is the largest ring this package accepts. Each slot is a
	// full-size surface, so a deep ring costs real memory for no benefit.
	MaxQueueDepth = 16
	// MaxDimension is the largest frame edge accepted, a sanity bound well
	// above any real display; it exists so a mistaken value fails loudly
	// instead of asking the compositor for a terabyte.
	MaxDimension = 32768
	// DefaultTimeout is the AcquireNextFrame timeout used when Options.Timeout
	// is zero. It is short enough that a Close is honoured promptly and long
	// enough that a still desktop does not spin.
	DefaultTimeout = 100 * time.Millisecond
	// MaxTimeout is the largest AcquireNextFrame timeout accepted. Beyond this
	// a Close would appear to hang.
	MaxTimeout = 10 * time.Second
)

// Validate reports whether the options are self-consistent, wrapping
// [ErrInvalidOption]. It does not consult the system, so it behaves the same on
// every platform and a consumer's option bug surfaces identically everywhere.
func (o Options) Validate() error {
	if o.Width < 0 || o.Height < 0 {
		return fmt.Errorf("%w: negative size %dx%d", ErrInvalidOption, o.Width, o.Height)
	}
	if (o.Width == 0) != (o.Height == 0) {
		return fmt.Errorf("%w: Width and Height must both be set or both be zero, got %dx%d",
			ErrInvalidOption, o.Width, o.Height)
	}
	if o.Width > MaxDimension || o.Height > MaxDimension {
		return fmt.Errorf("%w: size %dx%d exceeds the %d-pixel limit",
			ErrInvalidOption, o.Width, o.Height, MaxDimension)
	}
	if o.FPS < 0 {
		return fmt.Errorf("%w: negative FPS %g", ErrInvalidOption, o.FPS)
	}
	if o.FPS > 0 && o.FPS < 0.01 {
		return fmt.Errorf("%w: FPS %g is below the 0.01 minimum", ErrInvalidOption, o.FPS)
	}
	if o.QueueDepth < 0 {
		return fmt.Errorf("%w: negative QueueDepth %d", ErrInvalidOption, o.QueueDepth)
	}
	if o.QueueDepth > 0 && o.QueueDepth < MinQueueDepth {
		return fmt.Errorf("%w: QueueDepth %d is below the minimum of %d",
			ErrInvalidOption, o.QueueDepth, MinQueueDepth)
	}
	if o.QueueDepth > MaxQueueDepth {
		return fmt.Errorf("%w: QueueDepth %d exceeds the maximum of %d",
			ErrInvalidOption, o.QueueDepth, MaxQueueDepth)
	}
	if o.Timeout < 0 {
		return fmt.Errorf("%w: negative Timeout %s", ErrInvalidOption, o.Timeout)
	}
	if o.Timeout > MaxTimeout {
		return fmt.Errorf("%w: Timeout %s exceeds the maximum of %s",
			ErrInvalidOption, o.Timeout, MaxTimeout)
	}
	switch o.Backend {
	case BackendAuto, BackendDuplication, BackendGDI:
	default:
		return fmt.Errorf("%w: unknown Backend %d", ErrInvalidOption, uint8(o.Backend))
	}
	if o.Backend == BackendDuplication {
		if o.ShowsCursor {
			return fmt.Errorf("%w: Backend=duplication cannot composite the cursor; "+
				"use BackendAuto to fall back to GDI, or drop ShowsCursor", ErrInvalidOption)
		}
		if o.ScalesToFit {
			return fmt.Errorf("%w: Backend=duplication cannot resample, so ScalesToFit is meaningless; "+
				"use BackendAuto to fall back to GDI", ErrInvalidOption)
		}
	}
	return nil
}

// resolve fills the zero fields from the defaults and from the source's native
// pixel size, and returns the options actually used. It validates first, so a
// resolved Options is always usable.
func (o Options) resolve(nativeW, nativeH int) (Options, error) {
	if err := o.Validate(); err != nil {
		return Options{}, err
	}
	r := o
	if r.Width == 0 {
		if nativeW <= 0 || nativeH <= 0 {
			return Options{}, fmt.Errorf("%w: no size given and the source reports %dx%d",
				ErrInvalidOption, nativeW, nativeH)
		}
		r.Width, r.Height = nativeW, nativeH
	}
	if r.FPS == 0 {
		r.FPS = DefaultFPS
	}
	if r.QueueDepth == 0 {
		r.QueueDepth = DefaultQueueDepth
	}
	if r.Timeout == 0 {
		r.Timeout = DefaultTimeout
	}
	return r, nil
}

// frameInterval converts a frame-rate ceiling to the polling period the GDI
// backend ticks at. A non-positive fps yields the interval for [DefaultFPS]
// rather than a division by zero, and the result is never below a
// microsecond.
func frameInterval(fps float64) time.Duration {
	if fps <= 0 {
		fps = DefaultFPS
	}
	d := time.Duration(float64(time.Second)/fps + 0.5)
	if d < time.Microsecond {
		d = time.Microsecond
	}
	return d
}

// pickBackend decides which backend a resolved set of options and a source
// actually get. want is the caller's [Options.Backend]; nativeW/nativeH are the
// source's native pixel size; isWindow says the source is a window rather than
// a display; duplicable says the source is a display that was matched to a DXGI
// output.
//
// It returns the backend to try and, for an explicit request that cannot be
// honoured, an error. [BackendAuto] never errors here: it degrades.
func pickBackend(want Backend, opt Options, nativeW, nativeH int, isWindow, duplicable bool) (Backend, error) {
	// Every reason duplication cannot serve THIS request, in the order a
	// reader would ask them.
	var why string
	switch {
	case isWindow:
		why = "Desktop Duplication captures whole outputs, not single windows"
	case !duplicable:
		why = "this display was not matched to a DXGI output"
	case opt.ShowsCursor:
		why = "Desktop Duplication does not composite the cursor"
	case opt.ScalesToFit:
		why = "Desktop Duplication cannot resample"
	case opt.Width != nativeW || opt.Height != nativeH:
		why = fmt.Sprintf("Desktop Duplication cannot resample, and %dx%d is not the source's native %dx%d",
			opt.Width, opt.Height, nativeW, nativeH)
	}
	switch want {
	case BackendGDI:
		return BackendGDI, nil
	case BackendDuplication:
		if why != "" {
			return BackendAuto, fmt.Errorf("%w: %s", ErrInvalidOption, why)
		}
		return BackendDuplication, nil
	default: // BackendAuto
		if why != "" {
			return BackendGDI, nil
		}
		return BackendDuplication, nil
	}
}

// DIBLayout describes a raw bitmap exactly as Windows states it, in the
// BITMAPINFOHEADER convention, and is the one place bottom-up images are dealt
// with.
//
// A Windows DIB is bottom-up by DEFAULT. A POSITIVE Height means row 0 of the
// buffer is the BOTTOM row of the image; a NEGATIVE Height means row 0 is the
// top row, which is what this package always asks for and what a consumer
// always receives. Leaking a bottom-up buffer to a compositor produces a
// vertically mirrored screen, and leaking an assumed Width*4 stride produces a
// sheared one — both are why this type exists rather than a pair of ints.
type DIBLayout struct {
	// Width is the image width in pixels. It is always positive; a negative
	// width has no meaning in a BITMAPINFOHEADER and is rejected.
	Width int
	// Height is the BITMAPINFOHEADER biHeight: positive for a bottom-up
	// image, negative for a top-down one.
	Height int
	// Stride is the number of BYTES per row. Zero means "the DWORD-aligned
	// minimum for this width and depth", which is what GDI uses when nobody
	// says otherwise; any other value is used as given, which is what a mapped
	// D3D11 staging texture's RowPitch needs.
	Stride int
	// BitCount is the bit depth. Only 32 is supported; anything else is
	// rejected rather than silently mis-indexed.
	BitCount int
}

// AlignedStride is the DWORD-aligned minimum number of bytes per row for a
// width and bit depth, which is what GDI lays a DIB out with when no stride is
// stated: ((width*bits + 31) / 32) * 4. For 32bpp it is exactly width*4, which
// is precisely why an assumption that "stride is width*4" survives long enough
// on Windows to fail somewhere else — a mapped D3D11 surface does not obey it.
func AlignedStride(width, bitCount int) int {
	if width <= 0 || bitCount <= 0 {
		return 0
	}
	return ((width*bitCount + 31) / 32) * 4
}

// Normalize turns a raw layout into the top-down geometry this package hands
// out: an absolute height, an effective stride, and a flag saying whether the
// source rows are in bottom-up order and must therefore be flipped.
//
// It validates as it goes, so a driver that reports nonsense fails here with a
// message rather than a hundred lines further on with an index panic.
func (l DIBLayout) Normalize() (width, height, stride int, bottomUp bool, err error) {
	if l.BitCount != 32 {
		return 0, 0, 0, false, fmt.Errorf("%w: bit depth %d is not supported, only 32bpp BGRA is",
			ErrInvalidOption, l.BitCount)
	}
	if l.Width <= 0 {
		return 0, 0, 0, false, fmt.Errorf("%w: non-positive DIB width %d", ErrInvalidOption, l.Width)
	}
	if l.Height == 0 {
		return 0, 0, 0, false, fmt.Errorf("%w: zero DIB height", ErrInvalidOption)
	}
	if l.Width > MaxDimension || l.Height > MaxDimension || l.Height < -MaxDimension {
		return 0, 0, 0, false, fmt.Errorf("%w: DIB %dx%d exceeds the %d-pixel limit",
			ErrInvalidOption, l.Width, l.Height, MaxDimension)
	}
	width = l.Width
	height = l.Height
	bottomUp = height > 0
	if !bottomUp {
		height = -height
	}
	stride = l.Stride
	if stride == 0 {
		stride = AlignedStride(width, l.BitCount)
	}
	if stride < width*4 {
		return 0, 0, 0, false, fmt.Errorf("%w: stride %d is narrower than %d bytes of %d-pixel row",
			ErrInvalidOption, stride, width*4, width)
	}
	if stride%4 != 0 {
		return 0, 0, 0, false, fmt.Errorf("%w: stride %d is not a multiple of 4", ErrInvalidOption, stride)
	}
	return width, height, stride, bottomUp, nil
}

// Size is the number of bytes a buffer must hold for this layout.
func (l DIBLayout) Size() (int, error) {
	_, h, stride, _, err := l.Normalize()
	if err != nil {
		return 0, err
	}
	return h * stride, nil
}

// Frame builds a top-down [Frame] over pix, flipping the rows IN PLACE first
// if the layout is bottom-up. It allocates nothing: the returned Frame's Pix
// aliases pix.
//
// scratch, when it is at least Stride bytes long, is used as the row-swap
// buffer so that even the flip is allocation-free; a shorter scratch (nil
// included) makes the flip allocate one row. The flip itself costs one pass
// over the image, which is why the package asks GDI for a top-down DIB in the
// first place and this path stays cold.
func (l DIBLayout) Frame(pix []byte, seq uint64, at time.Time, scratch []byte) (Frame, error) {
	w, h, stride, bottomUp, err := l.Normalize()
	if err != nil {
		return Frame{}, err
	}
	need := h * stride
	if len(pix) < need {
		return Frame{}, fmt.Errorf("%w: a %dx%d frame at stride %d needs %d bytes, got %d",
			ErrShortBuffer, w, h, stride, need, len(pix))
	}
	pix = pix[:need]
	if bottomUp {
		flipRows(pix, h, stride, scratch)
	}
	return Frame{Pix: pix, Width: w, Height: h, Stride: stride, Seq: seq, At: at}, nil
}

// flipRows reverses the row order of a stride-major image in place. pix must
// hold exactly height*stride bytes. scratch is used as the swap buffer when it
// is long enough, so the whole operation can be allocation-free.
func flipRows(pix []byte, height, stride int, scratch []byte) {
	if height < 2 || stride <= 0 {
		return
	}
	if len(scratch) < stride {
		scratch = make([]byte, stride)
	}
	tmp := scratch[:stride]
	for top, bot := 0, height-1; top < bot; top, bot = top+1, bot-1 {
		a := pix[top*stride : top*stride+stride]
		b := pix[bot*stride : bot*stride+stride]
		copy(tmp, a)
		copy(a, b)
		copy(b, tmp)
	}
}

// Frame is a BORROWED view of one captured frame.
//
// Pix aliases memory owned by the capture: the DIB section GDI blitted into, or
// the mapped D3D11 staging texture. It stays valid only until the next
// [Stream.Frame], [Stream.WaitFrame] or [Stream.Close] on the stream that
// produced it. Do not retain it; copy with [Frame.CopyTight] or [Frame.NRGBA]
// if you need it to outlive the borrow.
//
// The rows are always top-down: row 0 is the top of the image. Bottom-up DIBs
// are normalised inside the package, never handed out.
type Frame struct {
	// Pix is the frame's bytes in [FormatBGRA], Stride bytes per row, Height
	// rows. len(Pix) == Stride*Height.
	Pix []byte
	// Width and Height are the frame's size in pixels.
	Width, Height int
	// Stride is the number of BYTES per row. It is NOT necessarily Width*4:
	// a mapped D3D11 staging texture commonly pads rows to 256 bytes.
	Stride int
	// Seq counts frames since the stream started; it is 0 before the first
	// frame and strictly increases afterwards.
	Seq uint64
	// At is when the capture completed.
	At time.Time
}

// Valid reports whether the frame holds pixels.
func (f Frame) Valid() bool {
	return f.Width > 0 && f.Height > 0 && f.Stride >= f.Width*4 && len(f.Pix) >= f.Stride*f.Height
}

// TightLen is the number of bytes the frame occupies with no row padding,
// Width*4*Height.
func (f Frame) TightLen() int { return f.Width * 4 * f.Height }

// Row returns row y of the frame, Width*4 bytes with the padding trimmed off.
// Row 0 is the TOP row. It does not allocate. It returns nil for an
// out-of-range y or an invalid frame.
func (f Frame) Row(y int) []byte {
	if !f.Valid() || y < 0 || y >= f.Height {
		return nil
	}
	off := y * f.Stride
	return f.Pix[off : off+f.Width*4 : off+f.Width*4]
}

// CopyTight copies the frame into dst with the row padding removed, so dst
// holds Width*4*Height bytes of contiguous BGRA. It reports how many bytes it
// wrote, or [ErrShortBuffer] if dst is too small. It allocates nothing.
func (f Frame) CopyTight(dst []byte) (int, error) {
	if !f.Valid() {
		return 0, ErrNoFrame
	}
	n := f.TightLen()
	if len(dst) < n {
		return 0, fmt.Errorf("%w: need %d bytes, got %d", ErrShortBuffer, n, len(dst))
	}
	rowLen := f.Width * 4
	// The fast path: an unpadded frame is one contiguous run. A DIB section is
	// always in this case at 32bpp; a mapped staging texture usually is not.
	if f.Stride == rowLen {
		return copy(dst, f.Pix[:n]), nil
	}
	for y := 0; y < f.Height; y++ {
		src := y * f.Stride
		copy(dst[y*rowLen:(y+1)*rowLen], f.Pix[src:src+rowLen])
	}
	return n, nil
}

// NRGBA copies the frame into a freshly allocated image.NRGBA, swapping BGRA
// to RGBA as it goes and carrying the alpha byte through unchanged. It is the
// convenience path for saving a frame to disk; it allocates, so it does not
// belong in a per-frame loop.
//
// A GDI screen capture leaves alpha at ZERO, so an image saved this way is
// fully transparent. Use [Frame.NRGBAOpaque] for anything you intend to look
// at; use this one when the alpha channel carries real information, which for
// a window capture with a transparent corner it does.
func (f Frame) NRGBA() (*image.NRGBA, error) { return f.nrgba(false) }

// NRGBAOpaque is [Frame.NRGBA] with every alpha byte forced to 255. It is what
// you want for a screenshot: GDI's BitBlt does not fill the alpha channel, so
// the honest conversion produces an invisible PNG.
func (f Frame) NRGBAOpaque() (*image.NRGBA, error) { return f.nrgba(true) }

func (f Frame) nrgba(opaque bool) (*image.NRGBA, error) {
	if !f.Valid() {
		return nil, ErrNoFrame
	}
	img := image.NewNRGBA(image.Rect(0, 0, f.Width, f.Height))
	for y := 0; y < f.Height; y++ {
		src := f.Pix[y*f.Stride:]
		dst := img.Pix[y*img.Stride:]
		for x := 0; x < f.Width; x++ {
			dst[x*4+0] = src[x*4+2] // B -> R
			dst[x*4+1] = src[x*4+1] // G
			dst[x*4+2] = src[x*4+0] // R -> B
			if opaque {
				dst[x*4+3] = 0xff
			} else {
				dst[x*4+3] = src[x*4+3]
			}
		}
	}
	return img, nil
}

// Uniform reports whether every pixel of the frame is identical, and the pixel
// if so. It is the cheap check that a capture actually captured something: a
// stream that silently hands back an untouched buffer is uniformly zero, and
// that is the classic silent failure this package's own tests assert against.
func (f Frame) Uniform() (bgra [4]byte, uniform bool) {
	if !f.Valid() {
		return bgra, false
	}
	first := f.Row(0)
	copy(bgra[:], first[:4])
	for y := 0; y < f.Height; y++ {
		row := f.Row(y)
		for x := 0; x < len(row); x += 4 {
			if row[x] != bgra[0] || row[x+1] != bgra[1] ||
				row[x+2] != bgra[2] || row[x+3] != bgra[3] {
				return bgra, false
			}
		}
	}
	return bgra, true
}

// Differs reports how many PIXELS of two frames are not identical, and whether
// the two frames are comparable at all. Two frames of different sizes are not
// comparable and report false. It ignores stride padding, so two frames of the
// same size captured through different backends compare correctly.
//
// It exists for the verification protocol: proving a capture is live means
// proving the content CHANGES between frames, and a count is a far better
// answer than a boolean when it does not.
func (f Frame) Differs(g Frame) (pixels int, comparable bool) {
	if !f.Valid() || !g.Valid() || f.Width != g.Width || f.Height != g.Height {
		return 0, false
	}
	for y := 0; y < f.Height; y++ {
		a, b := f.Row(y), g.Row(y)
		for x := 0; x < len(a); x += 4 {
			if a[x] != b[x] || a[x+1] != b[x+1] || a[x+2] != b[x+2] || a[x+3] != b[x+3] {
				pixels++
			}
		}
	}
	return pixels, true
}

// Stats reports what a stream has seen since it started.
type Stats struct {
	// Frames is the number of frames actually delivered with pixels.
	Frames uint64
	// Idle is the number of capture attempts that produced no new frame. With
	// [BackendDuplication] that is AcquireNextFrame timing out, which is how
	// the OS says "nothing changed"; with [BackendGDI] it stays 0, because a
	// poll always produces a frame.
	Idle uint64
	// Superseded is the number of delivered frames that were replaced by a
	// newer one before the consumer ever asked for them. A large value next to
	// Frames means the consumer is slower than the capture.
	Superseded uint64
	// AccessLost counts how many times a live duplication stream was torn down
	// by the OS and re-established. It stays 0 with [BackendGDI].
	AccessLost uint64
	// Last is when the most recent frame with pixels was captured.
	Last time.Time
	// Interval is the gap between the two most recent frames with pixels.
	Interval time.Duration
	// Capture is how long the most recent READ-BACK took: for GDI the BitBlt
	// into the DIB section, for duplication the CopyResource plus Map (or the
	// MapDesktopSurface copy). It is the number that decides whether a display
	// fits a frame budget.
	//
	// It deliberately EXCLUDES the time spent waiting for the desktop to
	// change, which for a change-driven backend is most of the wall clock and
	// none of the cost. Conflating the two makes an idle desktop look
	// expensive; see [Stats.Wait].
	Capture time.Duration
	// CaptureTotal is the sum of every Capture, so a consumer can compute a
	// mean over a run rather than reading one sample.
	CaptureTotal time.Duration
	// Wait is how long the most recent frame spent waiting for the desktop to
	// change before its read-back began. It is 0 for [BackendGDI], which does
	// not wait, and is the bulk of the interval for [BackendDuplication] on a
	// quiet desktop.
	Wait time.Duration
	// WaitTotal is the sum of every Wait.
	WaitTotal time.Duration
}

// FPS is the instantaneous rate implied by [Stats.Interval], 0 when fewer than
// two frames have arrived.
func (s Stats) FPS() float64 {
	if s.Interval <= 0 {
		return 0
	}
	return float64(time.Second) / float64(s.Interval)
}

// MeanCapture is the average read-back time over the whole run, 0 before the
// first frame.
func (s Stats) MeanCapture() time.Duration {
	if s.Frames == 0 {
		return 0
	}
	return time.Duration(uint64(s.CaptureTotal) / s.Frames)
}

// MeanWait is the average time a frame spent waiting for the desktop to
// change, 0 before the first frame and 0 throughout for [BackendGDI].
func (s Stats) MeanWait() time.Duration {
	if s.Frames == 0 {
		return 0
	}
	return time.Duration(uint64(s.WaitTotal) / s.Frames)
}

// HRESULT is a Windows COM status code. It is carried as an unsigned value
// because that is how every reference states it (0x887A0004, not -2005270524).
type HRESULT uint32

// Failed reports whether the code has the severity bit set.
func (h HRESULT) Failed() bool { return h&0x80000000 != 0 }

// Name is the documented constant for this code, or "" for one this package
// does not know.
func (h HRESULT) Name() string { return hresultNames[h] }

// String renders the code as its constant name and hex value.
func (h HRESULT) String() string {
	if n := h.Name(); n != "" {
		return fmt.Sprintf("%s (%#08x)", n, uint32(h))
	}
	return fmt.Sprintf("%#08x", uint32(h))
}

// The HRESULTs this package can actually surface, named so a failure reads as
// a name rather than as a bare hex number.
const (
	sOK                          HRESULT = 0x00000000
	sFalse                       HRESULT = 0x00000001
	eAccessDenied                HRESULT = 0x80070005
	eOutOfMemory                 HRESULT = 0x8007000E
	eInvalidArg                  HRESULT = 0x80070057
	eNoInterface                 HRESULT = 0x80004002
	eNotImpl                     HRESULT = 0x80004001
	eFail                        HRESULT = 0x80004005
	dxgiErrorNotFound            HRESULT = 0x887A0002
	dxgiErrorMoreData            HRESULT = 0x887A0003
	dxgiErrorUnsupported         HRESULT = 0x887A0004
	dxgiErrorDeviceRemoved       HRESULT = 0x887A0005
	dxgiErrorDeviceHung          HRESULT = 0x887A0006
	dxgiErrorDeviceReset         HRESULT = 0x887A0007
	dxgiErrorInvalidCall         HRESULT = 0x887A0001
	dxgiErrorAccessLost          HRESULT = 0x887A0026
	dxgiErrorWaitTimeout         HRESULT = 0x887A0027
	dxgiErrorSessionDisconnected HRESULT = 0x887A0028
	dxgiErrorRestrictToOutput    HRESULT = 0x887A0029
	dxgiErrorNotCurrentlyAvail   HRESULT = 0x887A0022
	dxgiErrorAccessDenied        HRESULT = 0x887A002B
	dxgiErrorNameAlreadyExists   HRESULT = 0x887A002C
	dxgiErrorSDKComponentMissing HRESULT = 0x887A002D
)

// hresultNames maps every HRESULT this package recognises to the constant a
// reader would look up.
var hresultNames = map[HRESULT]string{
	sOK:                          "S_OK",
	sFalse:                       "S_FALSE",
	eAccessDenied:                "E_ACCESSDENIED",
	eOutOfMemory:                 "E_OUTOFMEMORY",
	eInvalidArg:                  "E_INVALIDARG",
	eNoInterface:                 "E_NOINTERFACE",
	eNotImpl:                     "E_NOTIMPL",
	eFail:                        "E_FAIL",
	dxgiErrorInvalidCall:         "DXGI_ERROR_INVALID_CALL",
	dxgiErrorNotFound:            "DXGI_ERROR_NOT_FOUND",
	dxgiErrorMoreData:            "DXGI_ERROR_MORE_DATA",
	dxgiErrorUnsupported:         "DXGI_ERROR_UNSUPPORTED",
	dxgiErrorDeviceRemoved:       "DXGI_ERROR_DEVICE_REMOVED",
	dxgiErrorDeviceHung:          "DXGI_ERROR_DEVICE_HUNG",
	dxgiErrorDeviceReset:         "DXGI_ERROR_DEVICE_RESET",
	dxgiErrorNotCurrentlyAvail:   "DXGI_ERROR_NOT_CURRENTLY_AVAILABLE",
	dxgiErrorAccessLost:          "DXGI_ERROR_ACCESS_LOST",
	dxgiErrorWaitTimeout:         "DXGI_ERROR_WAIT_TIMEOUT",
	dxgiErrorSessionDisconnected: "DXGI_ERROR_SESSION_DISCONNECTED",
	dxgiErrorRestrictToOutput:    "DXGI_ERROR_RESTRICT_TO_OUTPUT_STALE",
	dxgiErrorAccessDenied:        "DXGI_ERROR_ACCESS_DENIED",
	dxgiErrorNameAlreadyExists:   "DXGI_ERROR_NAME_ALREADY_EXISTS",
	dxgiErrorSDKComponentMissing: "DXGI_ERROR_SDK_COMPONENT_MISSING",
}

// COMError is a failure reported by DXGI, Direct3D or COM itself, carrying the
// HRESULT. Codes this package recognises unwrap to a sentinel — notably
// DXGI_ERROR_UNSUPPORTED unwraps to [ErrBackendUnavailable] and
// DXGI_ERROR_ACCESS_LOST to [ErrAccessLost] — so errors.Is works without anyone
// having to know the numbers.
type COMError struct {
	// HR is the status code.
	HR HRESULT
	// Op names the operation that failed, e.g. "IDXGIOutput1::DuplicateOutput".
	Op string
	// Detail is extra context, e.g. which adapter, and may be empty.
	Detail string
}

// Error renders the operation, the constant name for the code and the hex
// value, splicing in the remedy for the codes that have one.
func (e *COMError) Error() string {
	op := e.Op
	if op == "" {
		op = "COM"
	}
	s := fmt.Sprintf("screencapture: %s: %s", op, e.HR)
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	switch e.HR {
	case dxgiErrorUnsupported:
		s += "; Desktop Duplication is refused by this adapter or session " +
			"(a remote-desktop connection, a headless or session-0 process, " +
			"and some virtual display drivers all refuse it) — BackendGDI works there"
	case dxgiErrorNotCurrentlyAvail:
		s += "; another process already holds the duplication for this output, " +
			"and Windows allows only a limited number at once"
	case dxgiErrorAccessLost, dxgiErrorSessionDisconnected:
		s += "; this is recoverable — the stream re-establishes duplication by itself"
	}
	return s
}

// Unwrap maps the codes with a sentinel to that sentinel.
func (e *COMError) Unwrap() error {
	switch e.HR {
	case eAccessDenied, dxgiErrorAccessDenied:
		return ErrPermissionDenied
	case dxgiErrorUnsupported, dxgiErrorNotCurrentlyAvail, eNotImpl:
		return ErrBackendUnavailable
	case dxgiErrorAccessLost, dxgiErrorSessionDisconnected,
		dxgiErrorDeviceRemoved, dxgiErrorDeviceReset, dxgiErrorDeviceHung:
		return ErrAccessLost
	case dxgiErrorWaitTimeout:
		return ErrNoFrame
	case dxgiErrorNotFound:
		return ErrNotFound
	}
	return nil
}

// hr builds a [COMError] for op, or nil when the code did not fail. Passing a
// success code returns nil so a caller can write `if err := hr(op, code); err
// != nil` on every call site without a separate test.
func hr(op string, code HRESULT, detail ...string) error {
	if !code.Failed() {
		return nil
	}
	e := &COMError{HR: code, Op: op}
	if len(detail) > 0 {
		e.Detail = detail[0]
	}
	return e
}
