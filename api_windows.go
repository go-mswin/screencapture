// Copyright (c) the go-mswin/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build windows

package screencapture

// The live Win32 glue. Everything reaches the OS through
// golang.org/x/sys/windows (no cgo), so this links with CGO_ENABLED=0.
//
// # Where the procedures come from
//
// The user32, gdi32 and kernel32 DLL HANDLES come from github.com/go-mswin/
// win32, the fleet's owned Win32 foundation, rather than being re-declared
// here: one NewLazySystemDLL per DLL per process is the point of that package.
// The individual procedures below are the capture-specific ones win32 does not
// wrap (it wraps the nine windowing procedures plus StretchDIBits), and they
// are bound off those shared handles. dxgi.dll, d3d11.dll, shcore.dll and
// dwmapi.dll are not in win32 at all — they are graphics and shell libraries,
// not the windowing core — so their handles are declared here.
//
// # Why there is no RtlMoveMemory here
//
// The fleet's usual pattern for reading OS memory from Go is to copy it with
// RtlMoveMemory, so that go vet's unsafeptr check never sees a uintptr being
// turned back into a pointer. This package cannot afford that: a copy of a 4K
// frame is several milliseconds out of a 16.6 ms budget, every frame.
//
// The way out is to never hold the address as a uintptr in the first place.
// CreateDIBSection, IDXGISurface::Map and ID3D11DeviceContext::Map all write
// their result THROUGH a pointer the caller supplies, so the destination is
// declared as a *byte or an unsafe.Pointer and the OS fills it in. No
// uintptr-to-pointer conversion ever happens in Go, unsafeptr has nothing to
// complain about, and unsafe.Slice then makes a borrowed view with no copy at
// all. See dibSection and the two Map wrappers.

import (
	"fmt"
	"sync"
	"syscall"
	"unsafe"

	"github.com/go-mswin/win32"
	"golang.org/x/sys/windows"
)

// Graphics and compositor DLLs this package needs and win32 does not carry.
// shcore is no longer among them: the per-monitor DPI query moved to win32
// with the rest of the display enumeration.
var (
	modDXGI   = windows.NewLazySystemDLL("dxgi.dll")
	modD3D11  = windows.NewLazySystemDLL("d3d11.dll")
	modDwmapi = windows.NewLazySystemDLL("dwmapi.dll")
)

// The procedures, bound off go-mswin/win32's shared DLL handles where the DLL
// is one it already owns.
var (
	procPrintWindow              = win32.User32.NewProc("PrintWindow")
	procGetCursorInfo            = win32.User32.NewProc("GetCursorInfo")
	procGetIconInfo              = win32.User32.NewProc("GetIconInfo")
	procDrawIconEx               = win32.User32.NewProc("DrawIconEx")
	procSetWindowDisplayAffinity = win32.User32.NewProc("SetWindowDisplayAffinity")
	procGetWindowDisplayAffinity = win32.User32.NewProc("GetWindowDisplayAffinity")
	procCreateDIBSection         = win32.Gdi32.NewProc("CreateDIBSection")
	procDwmGetWindowAttribute    = modDwmapi.NewProc("DwmGetWindowAttribute")
	procCreateDXGIFactory1       = modDXGI.NewProc("CreateDXGIFactory1")
	procD3D11CreateDevice        = modD3D11.NewProc("D3D11CreateDevice")
)

// Available reports whether this build can capture at all. On Windows it
// asks the OS rather than assuming: the two DLLs a GDI capture needs must
// load, which they do on every Windows since NT, and the answer is therefore
// only ever false on a stripped image.
func Available() bool { return win32.User32.Load() == nil && win32.Gdi32.Load() == nil }

// Authorized reports whether this process may capture. Windows has NO
// screen-recording permission gate — any process in an interactive window
// station may capture the desktop — so this is [Available]. It exists so that
// a consumer written against the macOS sibling compiles and behaves sensibly
// without a platform switch.
func Authorized() bool { return Available() }

// RequestAuthorization prompts nothing and reports [Authorized]. Windows has
// no grant to ask for; see [Authorized].
func RequestAuthorization() bool { return Authorized() }

// dpiOnce makes the process per-monitor-DPI-aware exactly once, and never
// fails loudly: an application that already declared its awareness in a
// manifest gets an error here, and that is the correct outcome — the manifest
// wins and the measurements are already real pixels.
//
// It matters because a DPI-unaware process is lied to by the OS: a 3840x2160
// monitor at 150% scaling is reported as 2560x1440 and every capture comes
// back at the virtualised size, silently blurry.
var dpiOnce = sync.OnceFunc(func() {
	win32.SetProcessDPIAwarenessContext(win32.DPIAwarenessPerMonitorV2)
})

// boolOf converts a Win32 BOOL return.
func boolOf(r uintptr) bool { return r != 0 }

// utf16ToString turns a NUL-terminated fixed-size UTF-16 field into a Go
// string, stopping at the first NUL. windows.UTF16ToString does the same for a
// slice, and is used through this wrapper so the [32]uint16 and [128]uint16
// array fields do not each need a slice expression at the call site.
func utf16ToString(b []uint16) string { return windows.UTF16ToString(b) }

// lastError is the calling thread's last Win32 error, wrapped for op. It is
// only meaningful immediately after a call that documented a failure, which is
// why every call site tests the return value FIRST.
func lastError(op string) error {
	e := windows.GetLastError()
	if e == nil {
		return fmt.Errorf("screencapture: %s failed with no error code", op)
	}
	return fmt.Errorf("screencapture: %s: %w", op, e)
}

// ---------------------------------------------------------------------------
// GDI device contexts and DIB sections
// ---------------------------------------------------------------------------

// hdc and hbitmap are go-mswin/win32's HDC and HBITMAP, aliased rather than
// redeclared so a handle can cross between this package and win32 without a
// conversion at every call.
type (
	hdc     = win32.HDC
	hbitmap = win32.HBITMAP
)

// screenDC returns the device context of the whole virtual screen. Its
// coordinate origin is the top-left of the PRIMARY monitor, so a monitor
// placed above or to the left of it is at negative coordinates and a BitBlt
// source offset for that monitor is negative too. That is correct and must not
// be clamped.
func screenDC() (hdc, error) { return win32.GetDC(0) }

// releaseScreenDC gives a screen DC back. A DC obtained from GetDC must be
// released with ReleaseDC, not DeleteDC; leaking them exhausts a small
// per-session pool and eventually every GDI call in the session fails.
func releaseScreenDC(h hdc) { win32.ReleaseDC(0, h) }

// memoryDC creates a memory device context compatible with ref.
func memoryDC(ref hdc) (hdc, error) { return win32.CreateCompatibleDC(ref) }

// deleteDC destroys a memory device context.
func deleteDC(h hdc) { win32.DeleteDC(h) }

// selectObject selects a GDI object into a device context and returns the
// previous one, which must be selected back before the DC is deleted.
func selectObject(h hdc, obj win32.HANDLE) win32.HANDLE { return win32.SelectObject(h, obj) }

// deleteObject destroys a GDI object.
func deleteObject(obj win32.HANDLE) { win32.DeleteObject(obj) }

// clearDC paints a device context black with PatBlt(BLACKNESS). PrintWindow
// composites rather than overwriting, so a buffer reused between frames keeps
// the previous frame's pixels wherever the window is transparent; clearing
// first is what stops a captured window from smearing.
func clearDC(h hdc, w, height int) {
	win32.PatBlt(h, 0, 0, int32(w), int32(height), win32.Blackness)
}

// dibSection is a top-down 32bpp BGRA DIB section together with a Go slice
// that ALIASES its pixels. Nothing is copied: bits is the address GDI itself
// blits into.
type dibSection struct {
	bmp    hbitmap
	pix    []byte
	width  int
	height int
	stride int
}

// newDIBSection creates a top-down 32bpp BI_RGB DIB section of the given size
// and returns it with its pixels exposed as a Go slice.
//
// CreateDIBSection is the one GDI call this package binds itself rather than
// taking from win32, because of what the fifth argument is: a `void**` the OS
// writes the pixel address into. The destination here is declared as a *byte,
// so the OS writes a Go-visible POINTER and unsafe.Slice can build the view
// directly. Had the address come back as a uintptr — the way it would from a
// wrapper returning a plain integer — turning it into a slice would be exactly
// the uintptr-to-pointer conversion go vet's unsafeptr check exists to catch,
// and the fleet's usual answer (copy it out with RtlMoveMemory) would put a
// full-frame memcpy on the hot path.
func newDIBSection(ref hdc, width, height int) (*dibSection, error) {
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("%w: DIB section %dx%d", ErrInvalidOption, width, height)
	}
	bi := newBitmapInfo(width, height)
	var bits *byte
	r, _, _ := procCreateDIBSection.Call(
		uintptr(ref),
		uintptr(unsafe.Pointer(&bi)),
		uintptr(dibRGBColors),
		uintptr(unsafe.Pointer(&bits)),
		0, 0,
	)
	if r == 0 || bits == nil {
		return nil, lastError("CreateDIBSection")
	}
	stride := AlignedStride(width, 32)
	return &dibSection{
		bmp:    hbitmap(r),
		pix:    unsafe.Slice(bits, stride*height),
		width:  width,
		height: height,
		stride: stride,
	}, nil
}

// layout describes the section in the OS's own convention, so the frame it
// produces goes through the same normalisation as anything else.
func (d *dibSection) layout() DIBLayout {
	// The section was created with a negative biHeight, so it is top-down and
	// the layout says so.
	return DIBLayout{Width: d.width, Height: -d.height, Stride: d.stride, BitCount: 32}
}

// free destroys the bitmap and drops the alias. Using the slice afterwards
// reads freed memory, so it is cleared rather than left dangling.
func (d *dibSection) free() {
	if d.bmp != 0 {
		deleteObject(win32.HANDLE(d.bmp))
		d.bmp = 0
	}
	d.pix = nil
}

// bitBlt copies a rectangle between device contexts.
func bitBlt(dst hdc, dx, dy, w, h int, src hdc, sx, sy int, rop uint32) error {
	return win32.BitBlt(dst, int32(dx), int32(dy), int32(w), int32(h),
		src, int32(sx), int32(sy), rop)
}

// stretchBlt copies a rectangle between device contexts, resampling.
func stretchBlt(dst hdc, dx, dy, dw, dh int, src hdc, sx, sy, sw, sh int, rop uint32) error {
	return win32.StretchBlt(dst, int32(dx), int32(dy), int32(dw), int32(dh),
		src, int32(sx), int32(sy), int32(sw), int32(sh), rop)
}

// setStretchMode sets the resampling mode of a device context. HALFTONE is the
// only one that averages rather than dropping pixels, which for a downscaled
// desktop is the difference between readable text and noise.
func setStretchMode(h hdc, mode int) { win32.SetStretchBltMode(h, int32(mode)) }

// printWindow renders a window into a device context through DWM, which is the
// only way to capture a window that is occluded or hardware composited.
func printWindow(hwnd uintptr, dst hdc, flags uint32) error {
	r, _, _ := procPrintWindow.Call(hwnd, uintptr(dst), uintptr(flags))
	if !boolOf(r) {
		return lastError("PrintWindow")
	}
	return nil
}

// drawCursor composites the mouse pointer into a device context whose origin
// is at (originX, originY) on the virtual screen, scaled by sx/sy. It reports
// whether anything was drawn: a hidden pointer, or a session with no cursor at
// all, is not an error.
func drawCursor(dst hdc, originX, originY int, sx, sy float64) bool {
	ci := cursorInfo{CbSize: uint32(unsafe.Sizeof(cursorInfo{}))}
	r, _, _ := procGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci)))
	if !boolOf(r) || ci.Flags&cursorShowing == 0 || ci.HCursor == 0 {
		return false
	}
	// The hotspot is where the cursor actually points; drawing at the raw
	// screen position puts the arrow's top-left corner under the pointer,
	// which is visibly a few pixels wrong on every frame.
	var ii iconInfo
	hotX, hotY := 0, 0
	if r, _, _ := procGetIconInfo.Call(ci.HCursor, uintptr(unsafe.Pointer(&ii))); boolOf(r) {
		hotX, hotY = int(ii.XHotspot), int(ii.YHotspot)
		// GetIconInfo hands over two bitmaps the caller owns.
		if ii.HbmMask != 0 {
			deleteObject(win32.HANDLE(ii.HbmMask))
		}
		if ii.HbmColor != 0 {
			deleteObject(win32.HANDLE(ii.HbmColor))
		}
	}
	x := int(float64(int(ci.PtScreenPos.X)-originX-hotX) * sx)
	y := int(float64(int(ci.PtScreenPos.Y)-originY-hotY) * sy)
	rr, _, _ := procDrawIconEx.Call(
		uintptr(dst), uintptr(int32(x)), uintptr(int32(y)), ci.HCursor,
		0, 0, 0, 0, uintptr(diNormal),
	)
	return boolOf(rr)
}

// setDisplayAffinity applies SetWindowDisplayAffinity to a window and reports
// the failure rather than swallowing it. The OS only permits this on a window
// the calling process owns; anything else comes back as access denied, and
// pretending it worked would let a consumer believe its overlay is excluded
// while it is being fed back into its own capture.
func setDisplayAffinity(hwnd uintptr, affinity uint32) error {
	r, _, _ := procSetWindowDisplayAffinity.Call(hwnd, uintptr(affinity))
	if !boolOf(r) {
		e := windows.GetLastError()
		if e == windows.ERROR_ACCESS_DENIED || e == windows.ERROR_INVALID_WINDOW_HANDLE {
			return fmt.Errorf("%w: SetWindowDisplayAffinity on window %#x: %v "+
				"(only a window this process OWNS can be excluded from capture)",
				ErrPermissionDenied, hwnd, e)
		}
		return lastError(fmt.Sprintf("SetWindowDisplayAffinity(%#x)", hwnd))
	}
	return nil
}

// displayAffinity reads a window's current display affinity, so it can be put
// back exactly as it was rather than reset to WDA_NONE.
func displayAffinity(hwnd uintptr) (uint32, bool) {
	var a uint32
	r, _, _ := procGetWindowDisplayAffinity.Call(hwnd, uintptr(unsafe.Pointer(&a)))
	return a, boolOf(r)
}

// ---------------------------------------------------------------------------
// COM, by hand
// ---------------------------------------------------------------------------

// The interface identities this package asks for.
var (
	iidIDXGIFactory1   = guid{0x770aae78, 0xf26f, 0x4dba, [8]byte{0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87}}
	iidIDXGIOutput1    = guid{0x00cddea8, 0x939b, 0x4b83, [8]byte{0xa3, 0x40, 0xa6, 0x85, 0x22, 0x66, 0x66, 0xcc}}
	iidID3D11Texture2D = guid{0x6f15aaf2, 0xd208, 0x4e89, [8]byte{0x9a, 0xb4, 0x48, 0x95, 0x35, 0xd3, 0x4f, 0x9c}}
)

// slot returns the nth entry of a COM object's vtable.
//
// A COM interface pointer points at a structure whose FIRST field is a pointer
// to an array of function pointers. Two dereferences reach the array, and no
// uintptr is involved at any step — which is what keeps go vet's unsafeptr
// check quiet on the hottest path in the package.
//
// Getting a slot number wrong is undetectable at the call site: the call lands
// in a different method of the same object and returns a plausible-looking
// HRESULT. Every number used here is written next to the interface it belongs
// to, in declaration order, for exactly that reason.
func slot(obj unsafe.Pointer, n int) uintptr {
	return (*(**[256]uintptr)(obj))[n]
}

// IUnknown slots, shared by every interface below.
const (
	slotQueryInterface = 0
	slotAddRef         = 1
	slotRelease        = 2
)

// release drops one reference. A nil pointer is ignored, so cleanup paths do
// not each need a guard.
func release(obj unsafe.Pointer) {
	if obj == nil {
		return
	}
	syscall.SyscallN(slot(obj, slotRelease), uintptr(obj))
}

// queryInterface asks an object for another interface.
func queryInterface(obj unsafe.Pointer, id *guid, op string) (unsafe.Pointer, error) {
	var out unsafe.Pointer
	r, _, _ := syscall.SyscallN(slot(obj, slotQueryInterface),
		uintptr(obj), uintptr(unsafe.Pointer(id)), uintptr(unsafe.Pointer(&out)))
	if err := hr(op, HRESULT(r)); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, hr(op, eNoInterface)
	}
	return out, nil
}
