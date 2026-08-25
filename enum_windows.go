// Copyright (c) the go-mswin/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build windows

package screencapture

// Enumeration of what can be captured.
//
// Displays come from EnumDisplayMonitors, which is the authority on where a
// monitor sits on the virtual screen, and are then MATCHED to DXGI outputs by
// GDI device name (`\\.\DISPLAY1`). The match is what decides whether Desktop
// Duplication is even a candidate for a display, and doing it by device name
// rather than by index is what makes it correct on a machine with more than
// one adapter, where output 0 of adapter 1 is not monitor 1.
//
// Windows come from EnumWindows, filtered to the top-level, visible,
// non-tool-window ones a user would call windows, with DWM asked about
// cloaking so a background Universal Windows Platform app is reported as such
// rather than silently capturing blank.

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// enumMu serialises the monitor and window enumerations. Both hand a callback
// to the OS which appends to a slice, and the callback is a process-wide
// trampoline, so two concurrent enumerations would interleave into one slice.
var enumMu sync.Mutex

// Displays lists the capturable displays. It is a snapshot: monitors are
// attached and detached and get new HMONITOR values when they are, so re-read
// it rather than caching it.
func Displays(ctx context.Context) ([]Display, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dpiOnce()
	mons, err := enumMonitors()
	if err != nil {
		return nil, err
	}
	if len(mons) == 0 {
		return nil, ErrNoDisplay
	}
	// The DXGI match is best-effort: a machine where dxgi.dll will not load,
	// or an adapter that refuses enumeration, still has perfectly capturable
	// monitors through GDI. Such displays come back with AdapterIndex and
	// OutputIndex at -1, which Duplicable reports honestly.
	byName := dxgiOutputsByDeviceName()
	out := make([]Display, 0, len(mons))
	for _, m := range mons {
		d := m
		if o, ok := byName[d.DeviceName]; ok {
			d.AdapterIndex, d.OutputIndex, d.Rotation = o.adapter, o.output, o.rotation
		}
		out = append(out, d)
	}
	// A stable order: the primary display first, then left to right, then top
	// to bottom. A consumer compositing a panorama wants the same order on
	// every run.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Primary != out[j].Primary {
			return out[i].Primary
		}
		if out[i].Bounds.X != out[j].Bounds.X {
			return out[i].Bounds.X < out[j].Bounds.X
		}
		return out[i].Bounds.Y < out[j].Bounds.Y
	})
	return out, nil
}

// enumMonitors walks EnumDisplayMonitors and describes each monitor.
func enumMonitors() ([]Display, error) {
	enumMu.Lock()
	defer enumMu.Unlock()
	var found []Display
	cb := windows.NewCallback(func(hmon uintptr, _ uintptr, _ uintptr, _ uintptr) uintptr {
		if d, ok := describeMonitor(hmon); ok {
			found = append(found, d)
		}
		return 1 // keep enumerating
	})
	r, _, _ := procEnumDisplayMonitors.Call(0, 0, cb, 0)
	if !boolOf(r) {
		return nil, lastError("EnumDisplayMonitors")
	}
	return found, nil
}

// describeMonitor fills in everything GDI and the shell know about a monitor.
func describeMonitor(hmon uintptr) (Display, bool) {
	mi := monitorInfoEx{CbSize: uint32(unsafe.Sizeof(monitorInfoEx{}))}
	r, _, _ := procGetMonitorInfoW.Call(hmon, uintptr(unsafe.Pointer(&mi)))
	if !boolOf(r) {
		return Display{}, false
	}
	b := mi.RcMonitor.toRect()
	dpi := monitorDPI(hmon)
	d := Display{
		ID:           uint64(hmon),
		DeviceName:   utf16ToString(mi.SzDevice[:]),
		PixelWidth:   b.W,
		PixelHeight:  b.H,
		DPI:          dpi,
		Bounds:       b,
		Work:         mi.RcWork.toRect(),
		Primary:      mi.DwFlags&monitorinfoPrimary != 0,
		Rotation:     RotationUnspecified,
		AdapterIndex: -1,
		OutputIndex:  -1,
	}
	// The device-independent size is the pixel size divided by the DPI scale,
	// which is what an unaware application is shown. Rounding is to nearest so
	// a 1.5 scale on an odd pixel count does not lose a pixel.
	s := d.Scale()
	d.Width = int(float64(b.W)/s + 0.5)
	d.Height = int(float64(b.H)/s + 0.5)
	return d, true
}

// monitorDPI reads a monitor's effective DPI, falling back to 96 (no scaling)
// when shcore.dll is missing, which is the case before Windows 8.1.
func monitorDPI(hmon uintptr) int {
	if procGetDpiForMonitor.Find() != nil {
		return 96
	}
	var x, y uint32
	r, _, _ := procGetDpiForMonitor.Call(hmon, uintptr(mdtEffectiveDPI),
		uintptr(unsafe.Pointer(&x)), uintptr(unsafe.Pointer(&y)))
	if HRESULT(r).Failed() || x == 0 {
		return 96
	}
	return int(x)
}

// Windows lists the capturable top-level windows, most recently active first
// in z-order as EnumWindows reports them.
//
// The filter is deliberately the one a user would apply: visible, top-level,
// with a non-empty title, not a tool window. Everything else — the thousands
// of invisible message-only and shell windows a session carries — is noise
// nobody can usefully capture.
func Windows(ctx context.Context) ([]Window, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dpiOnce()
	enumMu.Lock()
	defer enumMu.Unlock()
	fg, _, _ := procGetForegroundWindow.Call()
	var found []Window
	cb := windows.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		if w, ok := describeWindow(hwnd, fg); ok {
			found = append(found, w)
		}
		return 1
	})
	r, _, _ := procEnumWindows.Call(cb, 0)
	// EnumWindows returns FALSE both when the callback stopped it and when it
	// genuinely failed. This callback never stops it, so a false here is real
	// — except that it also returns false with no error set on some builds
	// when the last callback returned normally, which is why an empty error is
	// only reported when nothing at all was found.
	if !boolOf(r) && len(found) == 0 {
		return nil, lastError("EnumWindows")
	}
	return found, nil
}

// describeWindow decides whether a window is worth listing and, if so,
// describes it.
func describeWindow(hwnd, foreground uintptr) (Window, bool) {
	const (
		gwlExStyle     = -20
		wsExToolWindow = 0x00000080
	)
	if vis, _, _ := procIsWindowVisible.Call(hwnd); !boolOf(vis) {
		return Window{}, false
	}
	if ex := windowLongPtr(hwnd, gwlExStyle); ex&wsExToolWindow != 0 {
		return Window{}, false
	}
	title := windowText(hwnd)
	if title == "" {
		return Window{}, false
	}
	var pid uint32
	procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	iconic, _, _ := procIsIconic.Call(hwnd)
	w := Window{
		ID:        uint64(hwnd),
		Title:     title,
		ClassName: windowClass(hwnd),
		PID:       int32(pid),
		Bounds:    windowBounds(hwnd),
		Active:    hwnd == foreground,
		Minimized: boolOf(iconic),
		Cloaked:   windowCloaked(hwnd),
	}
	w.OnScreen = !w.Minimized && !w.Cloaked && !w.Bounds.Empty()
	w.ExePath = processPath(pid)
	if w.ExePath != "" {
		w.AppName = filepath.Base(w.ExePath)
	}
	return w, true
}

// windowLongPtr reads one of a window's LONG_PTR fields.
//
// golang.org/x/sys/windows does not wrap this one and go-mswin/win32 does not
// yet either, so it is bound off win32's shared user32 handle rather than off a
// second LazyDLL. GetWindowLongPtrW exists only on 64-bit Windows, which is the
// only Windows this package targets.
func windowLongPtr(hwnd uintptr, index int) uintptr {
	r, _, _ := procGetWindowLongPtrW.Call(hwnd, uintptr(int32(index)))
	return r
}

// windowText reads a window's title.
func windowText(hwnd uintptr) string {
	n, _, _ := procGetWindowTextLengthW.Call(hwnd)
	if n == 0 {
		return ""
	}
	buf := make([]uint16, int(n)+1)
	got, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if got == 0 {
		return ""
	}
	return utf16ToString(buf[:got])
}

// windowClass reads a window's class name. 256 characters is the documented
// maximum a class name can be.
func windowClass(hwnd uintptr) string {
	var buf [256]uint16
	got, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if got == 0 {
		return ""
	}
	return utf16ToString(buf[:got])
}

// windowBounds returns what the user actually sees. GetWindowRect includes the
// invisible resize border DWM draws outside the frame — up to 8 pixels a side
// on a default theme — so capturing that rectangle gives a window with a
// transparent margin. DWMWA_EXTENDED_FRAME_BOUNDS is the corrected rectangle;
// GetWindowRect is only the fallback for a system without DWM.
func windowBounds(hwnd uintptr) Rect {
	var r rect
	if procDwmGetWindowAttribute.Find() == nil {
		res, _, _ := procDwmGetWindowAttribute.Call(hwnd, uintptr(dwmwaExtendedFrameBounds),
			uintptr(unsafe.Pointer(&r)), unsafe.Sizeof(r))
		if !HRESULT(res).Failed() && r.width() > 0 && r.height() > 0 {
			return r.toRect()
		}
	}
	res, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	if !boolOf(res) {
		return Rect{}
	}
	return r.toRect()
}

// windowCloaked reports whether DWM is hiding the window: a suspended
// Universal Windows Platform app, or a window belonging to another virtual
// desktop. Such a window is visible to EnumWindows and captures blank, so
// saying so is worth the extra call.
func windowCloaked(hwnd uintptr) bool {
	if procDwmGetWindowAttribute.Find() != nil {
		return false
	}
	var cloaked uint32
	res, _, _ := procDwmGetWindowAttribute.Call(hwnd, uintptr(dwmwaCloaked),
		uintptr(unsafe.Pointer(&cloaked)), unsafe.Sizeof(cloaked))
	return !HRESULT(res).Failed() && cloaked != 0
}

// processPath returns a process's full image path, or "" when the process
// cannot be opened — which is normal for anything running at a higher
// integrity level than this one, and is not worth an error.
func processPath(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return ""
	}
	return utf16ToString(buf[:n])
}

// Shareable is the whole snapshot: every display, every window, and the set of
// processes owning them. It is the Windows analogue of the macOS sibling's
// SCShareableContent, gathered from EnumDisplayMonitors and EnumWindows rather
// than from one system call.
func Shareable(ctx context.Context) (*Content, error) { return shareable(ctx, 0) }

// CurrentProcessShareable is [Shareable] restricted to this process's own
// windows. It exists for the same reason it does on macOS — it is the part of
// the snapshot that is guaranteed capturable — though on Windows nothing is
// gated behind a permission, so the restriction is a convenience rather than a
// way around a missing grant.
func CurrentProcessShareable(ctx context.Context) (*Content, error) {
	return shareable(ctx, int32(windows.GetCurrentProcessId()))
}

// shareable gathers the snapshot, optionally filtered to one process.
func shareable(ctx context.Context, onlyPID int32) (*Content, error) {
	ds, err := Displays(ctx)
	if err != nil {
		return nil, err
	}
	ws, err := Windows(ctx)
	if err != nil {
		return nil, err
	}
	if onlyPID != 0 {
		kept := ws[:0]
		for _, w := range ws {
			if w.PID == onlyPID {
				kept = append(kept, w)
			}
		}
		ws = kept
	}
	seen := map[int32]Application{}
	for _, w := range ws {
		if _, ok := seen[w.PID]; !ok {
			seen[w.PID] = Application{PID: w.PID, Name: w.AppName, ExePath: w.ExePath}
		}
	}
	apps := make([]Application, 0, len(seen))
	for _, a := range seen {
		apps = append(apps, a)
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].PID < apps[j].PID })
	return &Content{Displays: ds, Windows: ws, Applications: apps}, nil
}

// lookupDisplay re-reads a display so a capture works from what the system
// says NOW rather than from a stale snapshot the caller may have held for
// minutes. A monitor that has gone away fails here with a clear error instead
// of producing a capture of nothing.
func lookupDisplay(ctx context.Context, d Display) (Display, error) {
	ds, err := Displays(ctx)
	if err != nil {
		return Display{}, err
	}
	for _, cur := range ds {
		if cur.ID == d.ID || (d.DeviceName != "" && cur.DeviceName == d.DeviceName) {
			return cur, nil
		}
	}
	return Display{}, fmt.Errorf("%w: display %#x %q is no longer attached",
		ErrNotFound, d.ID, d.DeviceName)
}

// liveWindow re-reads a window and checks it still exists and still has
// pixels.
func liveWindow(w Window) (Window, error) {
	hwnd := uintptr(w.ID)
	if ok, _, _ := procIsWindow.Call(hwnd); !boolOf(ok) {
		return Window{}, fmt.Errorf("%w: window %#x no longer exists", ErrNotFound, w.ID)
	}
	fg, _, _ := procGetForegroundWindow.Call()
	cur, ok := describeWindow(hwnd, fg)
	if !ok {
		// The window exists but no longer passes the listing filter (it was
		// hidden, or lost its title). It can still be captured, so only the
		// geometry is refreshed.
		cur = w
		cur.Bounds = windowBounds(hwnd)
	}
	if cur.Bounds.Empty() {
		return Window{}, fmt.Errorf("%w: window %#x has no pixels (%s)",
			ErrNotFound, w.ID, cur.Bounds)
	}
	return cur, nil
}
