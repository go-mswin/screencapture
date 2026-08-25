// Copyright (c) the go-mswin/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build windows

package screencapture

// The GDI backend: BitBlt (or StretchBlt, or PrintWindow) into a DIB section.
//
// It is the simple one and the universal one. It works over a remote-desktop
// connection, on an adapter that refuses Desktop Duplication, and on a session
// with no GPU worth the name. It scales, it can draw the cursor, and it is the
// only route to a single window. What it cannot do is tell you that nothing
// changed: every tick costs a full read-back of the source whether or not a
// pixel moved, which is the number that decides whether a large display fits a
// frame budget.
//
// # Where the copy is NOT
//
// The DIB section's pixels are aliased by a Go slice, not copied into one (see
// newDIBSection). BitBlt writes straight into the memory the consumer later
// borrows. There is exactly one traversal of the pixels per frame, inside GDI,
// and none in Go.
//
// # Why there is a ring of them
//
// A single DIB section would be written by the next BitBlt while the consumer
// is still reading the previous frame out of it, which shows up as tearing
// under load and nowhere else. Each ring slot therefore owns its own DIB
// section and its own memory device context.

import (
	"fmt"
	"time"

	"github.com/go-mswin/win32"
)

// gdiSource is what a GDI capture reads from.
type gdiSource struct {
	// hwnd is the window to capture, or 0 for a display.
	hwnd uintptr
	// srcX, srcY, srcW, srcH is the rectangle to read, in the source device
	// context's own coordinates: virtual-screen coordinates for a display
	// (which can be negative), window-relative for a window.
	srcX, srcY, srcW, srcH int
}

// gdiCapture is a GDI capture with one DIB section per ring slot.
type gdiCapture struct {
	src gdiSource
	// dstW, dstH is the requested output size, which differs from the source
	// size when the caller asked for scaling.
	dstW, dstH int
	scale      bool
	fit        bool
	cursor     bool

	slots []gdiSlot
	// screen is the source device context for a display capture, held for the
	// stream's lifetime because GetDC/ReleaseDC per frame is a measurable cost
	// and a per-session handle pool that can be exhausted.
	screen hdc
	closed bool
}

// gdiSlot is one ring slot: a memory device context with a DIB section
// selected into it.
type gdiSlot struct {
	dc   hdc
	dib  *dibSection
	prev win32.HANDLE
}

// newGDIDisplayCapture prepares a GDI capture of one display's rectangle.
func newGDIDisplayCapture(d Display, opt Options, slots int) (*gdiCapture, error) {
	c := &gdiCapture{
		src: gdiSource{
			srcX: d.Bounds.X, srcY: d.Bounds.Y,
			srcW: d.Bounds.W, srcH: d.Bounds.H,
		},
		dstW: opt.Width, dstH: opt.Height,
		cursor: opt.ShowsCursor,
		fit:    opt.ScalesToFit,
	}
	return c.init(slots)
}

// newGDIWindowCapture prepares a GDI capture of one window.
//
// The source rectangle is the window's own client-plus-frame area starting at
// (0,0): PrintWindow renders into the destination context at the origin, and
// BitBlt from a window device context is likewise window-relative. Using the
// window's screen coordinates here would read the wrong part of the desktop
// the moment the window moved.
func newGDIWindowCapture(w Window, opt Options, slots int) (*gdiCapture, error) {
	c := &gdiCapture{
		src: gdiSource{
			hwnd: uintptr(w.ID),
			srcX: 0, srcY: 0,
			srcW: w.Bounds.W, srcH: w.Bounds.H,
		},
		dstW: opt.Width, dstH: opt.Height,
		cursor: opt.ShowsCursor,
		fit:    opt.ScalesToFit,
	}
	return c.init(slots)
}

// init allocates the screen device context and the ring of DIB sections.
func (c *gdiCapture) init(slots int) (*gdiCapture, error) {
	if c.src.srcW <= 0 || c.src.srcH <= 0 {
		return nil, fmt.Errorf("%w: source rectangle %dx%d has no pixels",
			ErrInvalidOption, c.src.srcW, c.src.srcH)
	}
	if c.dstW <= 0 || c.dstH <= 0 {
		c.dstW, c.dstH = c.src.srcW, c.src.srcH
	}
	c.scale = c.dstW != c.src.srcW || c.dstH != c.src.srcH
	screen, err := screenDC()
	if err != nil {
		return nil, err
	}
	c.screen = screen
	c.slots = make([]gdiSlot, slots)
	for i := range c.slots {
		dc, err := memoryDC(c.screen)
		if err != nil {
			c.Close()
			return nil, err
		}
		dib, err := newDIBSection(c.screen, c.dstW, c.dstH)
		if err != nil {
			deleteDC(dc)
			c.Close()
			return nil, err
		}
		prev := selectObject(dc, win32.HANDLE(dib.bmp))
		// HALFTONE averages instead of dropping pixels. On a downscaled
		// desktop it is the difference between readable text and noise, and it
		// is per-device-context state, so it is set once here rather than per
		// frame.
		if c.scale {
			setStretchMode(dc, halftone)
		}
		c.slots[i] = gdiSlot{dc: dc, dib: dib, prev: prev}
	}
	return c, nil
}

// Size is the capture's output size in pixels.
func (c *gdiCapture) Size() (int, int) { return c.dstW, c.dstH }

// Path names the read-back route in use, for the report.
func (c *gdiCapture) Path() string {
	switch {
	case c.src.hwnd != 0:
		return "PrintWindow(PW_RENDERFULLCONTENT) into a DIB section"
	case c.scale:
		return "StretchBlt(HALFTONE) into a DIB section"
	}
	return "BitBlt(SRCCOPY|CAPTUREBLT) into a DIB section"
}

// Capture reads one frame into ring slot i.
//
// The returned slice ALIASES the slot's DIB section: BitBlt wrote into exactly
// those bytes. It stays valid until the same slot is captured into again,
// which the ring guarantees does not happen while a consumer holds it.
func (c *gdiCapture) Capture(i int, _ time.Duration) ([]byte, DIBLayout, time.Duration, time.Duration, error) {
	if c.closed {
		return nil, DIBLayout{}, 0, 0, ErrClosed
	}
	// GDI never waits: it reads the source whether or not anything changed, so
	// the whole cost is read-back and the wait is always zero.
	started := time.Now()
	s := c.slots[i]
	if c.src.hwnd != 0 {
		if err := c.captureWindow(s); err != nil {
			return nil, DIBLayout{}, time.Since(started), 0, err
		}
	} else if err := c.captureScreen(s); err != nil {
		return nil, DIBLayout{}, time.Since(started), 0, err
	}
	if c.cursor {
		// The cursor is composited in the DESTINATION's coordinate space, so
		// its screen position is offset by the source origin and scaled by the
		// same factor the image was. A window capture uses the window's screen
		// origin, which is read fresh because the window may have moved.
		ox, oy := c.src.srcX, c.src.srcY
		if c.src.hwnd != 0 {
			// gdiSource keeps a raw uintptr because the capture-specific
			// procedures it feeds (printWindow, the display-affinity pair)
			// are bound here and take one. windowBounds is win32's typed
			// world, so the conversion happens at the seam rather than the
			// whole hot path being retyped.
			b := windowBounds(win32.HWND(c.src.hwnd))
			ox, oy = b.X, b.Y
		}
		drawCursor(s.dc, ox, oy,
			float64(c.dstW)/float64(c.src.srcW), float64(c.dstH)/float64(c.src.srcH))
	}
	return s.dib.pix, s.dib.layout(), time.Since(started), 0, nil
}

// captureScreen blits a rectangle of the virtual screen.
func (c *gdiCapture) captureScreen(s gdiSlot) error {
	// CAPTUREBLT includes layered windows — anything with transparency, which
	// on a modern desktop is most notification and menu surfaces. Without it
	// they simply are not in the capture.
	const rop = srcCopy | captureBLT
	if c.scale {
		return c.stretchInto(s, c.screen, rop)
	}
	return bitBlt(s.dc, 0, 0, c.dstW, c.dstH, c.screen, c.src.srcX, c.src.srcY, rop)
}

// captureWindow renders a window into the slot.
//
// PrintWindow with PW_RENDERFULLCONTENT goes through DWM, which is the only
// way to get the pixels of a window that is occluded, minimised to another
// virtual desktop, or hardware composited. It composites rather than
// overwriting, so the slot is cleared first: otherwise the previous frame
// shows through wherever the window is transparent, and a rounded window
// corner keeps a ghost of the frame before it.
//
// When the caller asked for a size other than the window's own, PrintWindow
// still renders at the window's size, so it goes to a scratch device context
// and is stretched from there.
func (c *gdiCapture) captureWindow(s gdiSlot) error {
	if !c.scale {
		clearDC(s.dc, c.dstW, c.dstH)
		return printWindow(c.src.hwnd, s.dc, pwRenderFullContent)
	}
	scratchDC, err := memoryDC(c.screen)
	if err != nil {
		return err
	}
	defer deleteDC(scratchDC)
	scratch, err := newDIBSection(c.screen, c.src.srcW, c.src.srcH)
	if err != nil {
		return err
	}
	defer scratch.free()
	prev := selectObject(scratchDC, win32.HANDLE(scratch.bmp))
	defer selectObject(scratchDC, prev)
	clearDC(scratchDC, c.src.srcW, c.src.srcH)
	if err := printWindow(c.src.hwnd, scratchDC, pwRenderFullContent); err != nil {
		return err
	}
	return c.stretchInto(s, scratchDC, srcCopy)
}

// stretchInto resamples the source rectangle into the slot, letterboxing when
// ScalesToFit was asked for and the aspect ratios differ.
func (c *gdiCapture) stretchInto(s gdiSlot, src hdc, rop uint32) error {
	dx, dy, dw, dh := 0, 0, c.dstW, c.dstH
	if c.fit {
		// Letterbox: the largest rectangle of the source's aspect ratio that
		// fits, centred, with the rest left as the cleared background.
		sw, sh := c.src.srcW, c.src.srcH
		if c.dstW*sh > c.dstH*sw {
			dw = c.dstH * sw / sh
		} else {
			dh = c.dstW * sh / sw
		}
		dx, dy = (c.dstW-dw)/2, (c.dstH-dh)/2
		clearDC(s.dc, c.dstW, c.dstH)
	}
	return stretchBlt(s.dc, dx, dy, dw, dh, src,
		c.src.srcX, c.src.srcY, c.src.srcW, c.src.srcH, rop)
}

// Close destroys every device context and DIB section. It is idempotent.
func (c *gdiCapture) Close() {
	if c.closed {
		return
	}
	c.closed = true
	for i := range c.slots {
		s := &c.slots[i]
		if s.dc != 0 {
			// The original bitmap must go back into the device context before
			// either is destroyed; deleting a bitmap that is still selected
			// leaks it.
			if s.prev != 0 {
				selectObject(s.dc, s.prev)
			}
			deleteDC(s.dc)
			s.dc = 0
		}
		if s.dib != nil {
			s.dib.free()
			s.dib = nil
		}
	}
	if c.screen != 0 {
		releaseScreenDC(c.screen)
		c.screen = 0
	}
}
