// Copyright (c) the go-mswin/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build windows

package main

// The animator: a real, visible window that repaints itself in a different
// colour many times a second.
//
// It exists so the verification protocol can assert that captured frames
// CHANGE, without asking anybody to move a mouse and without trusting that
// something else on the desktop happens to be animating. A capture that hands
// back a static buffer looks perfectly healthy on every other check; this is
// the one that catches it.
//
// The window itself is built entirely on github.com/go-mswin/win32 — the
// fleet's owned Win32 foundation. Nothing here binds a procedure of its own:
// the window creation, the message pump, the show/topmost calls, the device
// context and the BGRA blit all come from there.

import (
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/go-mswin/win32"
)

var (
	procShowWindow    = win32.User32.NewProc("ShowWindow")
	procUpdateWindow  = win32.User32.NewProc("UpdateWindow")
	procGetDC         = win32.User32.NewProc("GetDC")
	procReleaseDC     = win32.User32.NewProc("ReleaseDC")
	procSetWindowPos  = win32.User32.NewProc("SetWindowPos")
	procSetForeground = win32.User32.NewProc("SetForegroundWindow")
)

// animator is a window being repainted in a loop.
type animator struct {
	hwnd win32.HWND
	w, h int

	stopOnce sync.Once
	quit     chan struct{}
	done     chan struct{}
	ready    chan error
}

// animClass is registered once per process; registering the same class name
// twice fails, and the failure would look like "the window could not be
// created".
var (
	animClassOnce sync.Once
	animClassErr  error
	animProc      uintptr
)

// startAnimator opens the window and starts repainting it.
//
// The window is created and pumped on ONE pinned OS thread, because a window
// belongs to the thread that created it and its messages are delivered to that
// thread's queue. The painting happens on a DIFFERENT goroutine through a
// device context of its own, which Win32 permits and which keeps the message
// pump responsive while the paint loop runs.
func startAnimator(w, h int) (*animator, error) {
	if w <= 0 || h <= 0 {
		w, h = 640, 480
	}
	a := &animator{
		w: w, h: h,
		quit:  make(chan struct{}),
		done:  make(chan struct{}),
		ready: make(chan error, 1),
	}
	go a.windowThread()
	if err := <-a.ready; err != nil {
		return nil, err
	}
	go a.paintLoop()
	return a, nil
}

// windowThread creates the window and runs its message pump until stop.
func (a *animator) windowThread() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(a.done)

	animClassOnce.Do(func() {
		animProc = win32.NewCallback(func(h win32.HWND, msg uint32, wp win32.WPARAM, lp win32.LPARAM) win32.LRESULT {
			if msg == win32.WMDestroy {
				win32.PostQuitMessage(0)
				return 0
			}
			return win32.DefWindowProc(h, msg, wp, lp)
		})
		name, err := win32.UTF16PtrFromString("wsccheckAnimator")
		if err != nil {
			animClassErr = err
			return
		}
		inst := win32.GetModuleHandle(nil)
		wc := win32.WndClassExW{
			CbSize:        uint32(unsafe.Sizeof(win32.WndClassExW{})),
			Style:         win32.CSHRedraw | win32.CSVRedraw,
			LpfnWndProc:   animProc,
			HInstance:     inst,
			HCursor:       win32.LoadCursor(0, win32.IDCArrow),
			LpszClassName: name,
		}
		if _, err := win32.RegisterClassEx(&wc); err != nil {
			animClassErr = err
		}
	})
	if animClassErr != nil {
		a.ready <- animClassErr
		return
	}

	class, _ := win32.UTF16PtrFromString("wsccheckAnimator")
	title, _ := win32.UTF16PtrFromString("wsccheck animator")
	// WS_EX_TOPMOST, because the thing being proved is that captured frames
	// CHANGE, and a window sitting behind whatever else owns the desktop
	// proves nothing. On a machine parked on a full-screen setup page — which
	// is exactly what a fresh VM is — a non-topmost animator is simply not on
	// screen and the assertion fails for a reason that has nothing to do with
	// the capture.
	hwnd, err := win32.CreateWindowEx(win32.WSExTopmost, class, title, win32.WSOverlappedWindow,
		40, 40, int32(a.w), int32(a.h), 0, 0, win32.GetModuleHandle(nil), nil)
	if err != nil {
		a.ready <- fmt.Errorf("CreateWindowEx: %w", err)
		return
	}
	a.hwnd = hwnd
	win32.ShowWindow(hwnd, win32.SWShow)
	win32.UpdateWindow(hwnd)
	// HWND_TOPMOST again after the window is mapped: a window created topmost
	// can still be pushed down by whatever had the foreground.
	win32.SetWindowPos(hwnd, win32.HWNDTopmost, 0, 0, 0, 0,
		win32.SWPNoSize|win32.SWPNoMove|win32.SWPShowWindow)
	win32.SetForegroundWindow(hwnd)
	a.ready <- nil

	// A goroutine posts WM_QUIT when the animator is stopped, so the pump ends
	// without needing a timer.
	go func() {
		<-a.quit
		win32.PostMessage(hwnd, win32.WMClose, 0, 0)
		win32.DestroyWindow(hwnd)
		win32.PostMessage(hwnd, win32.WMQuit, 0, 0)
	}()
	win32.Pump()
}

// paintLoop repaints the window in a colour that changes every frame, plus a
// block that travels across it. A changing FLAT colour alone would be
// indistinguishable from a capture that is reading the wrong buffer and
// happening to see something else change; a block at a known position makes
// the captured image legible in the PNG artefact too.
func (a *animator) paintLoop() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	rgba := make([]byte, a.w*a.h*4)
	bgra := make([]byte, a.w*a.h*4)
	tick := time.NewTicker(16 * time.Millisecond)
	defer tick.Stop()
	frame := 0
	for {
		select {
		case <-a.quit:
			return
		case <-tick.C:
		}
		frame++
		// The background cycles through a saturated hue so consecutive frames
		// differ in every pixel.
		br, bg, bb := byte(frame*7), byte(frame*13), byte(255-frame*5)
		for i := 0; i < len(rgba); i += 4 {
			rgba[i+0], rgba[i+1], rgba[i+2], rgba[i+3] = br, bg, bb, 255
		}
		// A white block sweeping left to right.
		bw := a.w / 8
		bh := a.h / 6
		bx := (frame * 9) % (a.w - bw)
		for y := a.h/2 - bh/2; y < a.h/2+bh/2; y++ {
			for x := bx; x < bx+bw; x++ {
				o := (y*a.w + x) * 4
				rgba[o+0], rgba[o+1], rgba[o+2], rgba[o+3] = 255, 255, 255, 255
			}
		}
		win32.PackBGRA(bgra, rgba)
		dc, err := win32.GetDC(a.hwnd)
		if err != nil {
			continue
		}
		win32.StretchDIBitsBGRA(dc, 0, 0, int32(a.w), int32(a.h),
			int32(a.w), int32(a.h), bgra)
		win32.ReleaseDC(a.hwnd, dc)
	}
}

// stop closes the window and ends both loops. It is idempotent.
func (a *animator) stop() {
	a.stopOnce.Do(func() {
		close(a.quit)
		select {
		case <-a.done:
		case <-time.After(2 * time.Second):
		}
	})
}
