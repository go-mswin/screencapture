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
	"unsafe"

	"github.com/go-mswin/win32"
	"golang.org/x/sys/windows"
)

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

// enumMonitors walks the monitors and describes each one.
//
// The walk itself is go-mswin/win32's, and the reason is not tidiness. This
// package used to build a windows.NewCallback here, INSIDE the function, so
// every Displays() allocated a trampoline out of a pool the runtime caps at
// 2000 for the whole process — after which runtime.throw kills it outright.
// win32 keeps ONE trampoline per enumeration, and the serialisation the
// process-wide trampoline requires, so neither is this package's problem any
// more.
func enumMonitors() ([]Display, error) {
	var found []Display
	err := win32.EnumDisplayMonitors(func(mon win32.HMONITOR, _ win32.HDC, _ win32.Rect) bool {
		if d, ok := describeMonitor(mon); ok {
			found = append(found, d)
		}
		return true // keep enumerating
	})
	if err != nil {
		return nil, fmt.Errorf("screencapture: %w", err)
	}
	return found, nil
}

// describeMonitor fills in everything GDI and the shell know about a monitor.
func describeMonitor(mon win32.HMONITOR) (Display, bool) {
	mi, err := win32.GetMonitorInfo(mon)
	if err != nil {
		return Display{}, false
	}
	b := toRect(mi.RcMonitor)
	dpi := monitorDPI(mon)
	d := Display{
		ID:           uint64(mon),
		DeviceName:   mi.Device(),
		PixelWidth:   b.W,
		PixelHeight:  b.H,
		DPI:          dpi,
		Bounds:       b,
		Work:         toRect(mi.RcWork),
		Primary:      mi.Primary(),
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
// when shcore.dll is missing, which is the case before Windows 8.1, or when
// the monitor no longer answers.
func monitorDPI(mon win32.HMONITOR) int {
	x, _, err := win32.GetDpiForMonitor(mon, win32.MDTEffectiveDPI)
	if err != nil {
		return win32.DefaultDPI
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
	fg := win32.GetForegroundWindow()
	var found []Window
	// As with the monitors: the walk and its single process-wide trampoline
	// are go-mswin/win32's. win32.EnumWindows already distinguishes a callback
	// that stopped the walk from a genuine failure, and this callback never
	// stops it — so an error here is real, and an empty result with no error
	// is a session that genuinely has nothing to show.
	if err := win32.EnumWindows(func(hwnd win32.HWND) bool {
		if w, ok := describeWindow(hwnd, fg); ok {
			found = append(found, w)
		}
		return true
	}); err != nil && len(found) == 0 {
		return nil, fmt.Errorf("screencapture: %w", err)
	}
	return found, nil
}

// describeWindow decides whether a window is worth listing and, if so,
// describes it.
func describeWindow(hwnd, foreground win32.HWND) (Window, bool) {
	if !win32.IsWindowVisible(hwnd) {
		return Window{}, false
	}
	if ex := win32.GetWindowLongPtr(hwnd, win32.GWLExStyle); ex&win32.WSExToolWindow != 0 {
		return Window{}, false
	}
	title := win32.GetWindowText(hwnd)
	if title == "" {
		return Window{}, false
	}
	pid, _ := win32.GetWindowThreadProcessID(hwnd)
	w := Window{
		ID:        uint64(hwnd),
		Title:     title,
		ClassName: win32.GetClassName(hwnd),
		PID:       int32(pid),
		Bounds:    windowBounds(hwnd),
		Active:    hwnd == foreground,
		Minimized: win32.IsIconic(hwnd),
		Cloaked:   windowCloaked(hwnd),
	}
	w.OnScreen = !w.Minimized && !w.Cloaked && !w.Bounds.Empty()
	w.ExePath = processPath(pid)
	if w.ExePath != "" {
		w.AppName = filepath.Base(w.ExePath)
	}
	return w, true
}

// windowBounds returns what the user actually sees. GetWindowRect includes the
// invisible resize border DWM draws outside the frame — up to 8 pixels a side
// on a default theme — so capturing that rectangle gives a window with a
// transparent margin. DWMWA_EXTENDED_FRAME_BOUNDS is the corrected rectangle;
// GetWindowRect is only the fallback for a system without DWM.
func windowBounds(hwnd win32.HWND) Rect {
	var r rect
	if procDwmGetWindowAttribute.Find() == nil {
		res, _, _ := procDwmGetWindowAttribute.Call(uintptr(hwnd), uintptr(dwmwaExtendedFrameBounds),
			uintptr(unsafe.Pointer(&r)), unsafe.Sizeof(r))
		if !HRESULT(res).Failed() && r.Width() > 0 && r.Height() > 0 {
			return toRect(r)
		}
	}
	wr, err := win32.GetWindowRect(hwnd)
	if err != nil {
		return Rect{}
	}
	return toRect(wr)
}

// windowCloaked reports whether DWM is hiding the window: a suspended
// Universal Windows Platform app, or a window belonging to another virtual
// desktop. Such a window is visible to EnumWindows and captures blank, so
// saying so is worth the extra call.
func windowCloaked(hwnd win32.HWND) bool {
	if procDwmGetWindowAttribute.Find() != nil {
		return false
	}
	var cloaked uint32
	res, _, _ := procDwmGetWindowAttribute.Call(uintptr(hwnd), uintptr(dwmwaCloaked),
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
	hwnd := win32.HWND(w.ID)
	if !win32.IsWindow(hwnd) {
		return Window{}, fmt.Errorf("%w: window %#x no longer exists", ErrNotFound, w.ID)
	}
	cur, ok := describeWindow(hwnd, win32.GetForegroundWindow())
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
