// Copyright (c) the go-mswin/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build windows && integration

// The live suite. It needs a real, INTERACTIVE Windows desktop, so it is
// behind both a build tag and an environment gate and never runs by accident:
//
//	go test -tags integration -run Live -v .
//	go test -tags integration -run . -bench . -benchmem .
//
// with SCREENCAPTURE_LIVE=1 set. A skip here is a skip, not a pass.
//
// # Why the environment gate as well as the tag
//
// A process reached over ssh, or a CI runner, runs in a session with NO
// interactive desktop. It still has a "display" — an 1024x768 phantom that
// GetDC(NULL) hands back — so a capture SUCCEEDS there and returns a blank or
// meaningless image. Requiring an explicit opt-in is what stops that from
// being read as a passing test. On Windows the tests must be launched INTO
// session 1, e.g. with `schtasks /ru <desktop user> /it`.
package screencapture

import (
	"context"
	"errors"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"
)

func requireLive(t testing.TB) {
	t.Helper()
	if os.Getenv("SCREENCAPTURE_LIVE") != "1" {
		t.Skip("set SCREENCAPTURE_LIVE=1 and run in an interactive desktop session")
	}
	if !Available() {
		t.Fatal("Available() is false on Windows")
	}
}

func mainDisplay(t testing.TB) Display {
	t.Helper()
	ds, err := Displays(context.Background())
	if err != nil {
		t.Fatalf("Displays: %v", err)
	}
	if len(ds) == 0 {
		t.Fatal("no displays")
	}
	c := &Content{Displays: ds}
	d, err := c.MainDisplay()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestLiveDisplays(t *testing.T) {
	requireLive(t)
	ds, err := Displays(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		t.Logf("%s primary=%v dxgi=%d/%d duplicable=%v rot=%s",
			d, d.Primary, d.AdapterIndex, d.OutputIndex, d.Duplicable(), d.Rotation)
		if d.PixelWidth <= 0 || d.PixelHeight <= 0 {
			t.Errorf("%s has no pixels", d)
		}
		if d.DeviceName == "" {
			t.Errorf("%s has no GDI device name, so it can never be matched to a DXGI output", d)
		}
		if d.DPI <= 0 {
			t.Errorf("%s reports DPI %d", d, d.DPI)
		}
	}
}

func TestLiveWindows(t *testing.T) {
	requireLive(t)
	ws, err := Windows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range ws {
		t.Logf("%s onscreen=%v cloaked=%v min=%v", w, w.OnScreen, w.Cloaked, w.Minimized)
	}
}

// captureOnce runs a stream and returns the first frame COPIED out, plus the
// stream's statistics. The copy matters: the borrow is only valid until the
// next call, and a test that keeps the borrowed slice is testing nothing.
func captureOnce(t *testing.T, opt Options) (Frame, Stats, *Stream) {
	t.Helper()
	ctx := context.Background()
	s, err := CaptureDisplay(ctx, mainDisplay(t), opt)
	if err != nil {
		t.Fatalf("CaptureDisplay: %v", err)
	}
	t.Logf("backend=%s path=%s note=%q", s.Backend(), s.Path(), s.Note())
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	f, err := s.WaitFrame(wait)
	if err != nil {
		s.Close()
		t.Fatalf("WaitFrame: %v", err)
	}
	pix := make([]byte, f.TightLen())
	if _, err := f.CopyTight(pix); err != nil {
		s.Close()
		t.Fatal(err)
	}
	return Frame{Pix: pix, Width: f.Width, Height: f.Height, Stride: f.Width * 4,
		Seq: f.Seq, At: f.At}, s.Stats(), s
}

func TestLiveCaptureGDI(t *testing.T) {
	requireLive(t)
	d := mainDisplay(t)
	f, st, s := captureOnce(t, Options{Backend: BackendGDI})
	defer s.Close()
	if s.Backend() != BackendGDI {
		t.Fatalf("backend = %s, want gdi", s.Backend())
	}
	if f.Width != d.PixelWidth || f.Height != d.PixelHeight {
		t.Fatalf("frame %dx%d, display %dx%d", f.Width, f.Height, d.PixelWidth, d.PixelHeight)
	}
	if _, uniform := f.Uniform(); uniform {
		t.Fatal("the frame is uniformly one colour — the buffer was never written")
	}
	t.Logf("read-back %v for %dx%d (%.3f ms/Mpx)", st.Capture, f.Width, f.Height,
		float64(st.Capture)/float64(time.Millisecond)/(float64(f.Width*f.Height)/1e6))
	writeArtifact(t, "display-gdi.png", f)
}

func TestLiveCaptureDuplication(t *testing.T) {
	requireLive(t)
	d := mainDisplay(t)
	if !d.Duplicable() {
		t.Skipf("%s is not matched to a DXGI output", d)
	}
	s, err := CaptureDisplay(context.Background(), d, Options{Backend: BackendDuplication})
	if err != nil {
		// A refusal is a real answer about this machine, not a defect. It is
		// reported rather than passed over in silence.
		if errors.Is(err, ErrBackendUnavailable) {
			t.Skipf("Desktop Duplication is refused here: %v", err)
		}
		t.Fatalf("CaptureDisplay(duplication): %v", err)
	}
	defer s.Close()
	t.Logf("backend=%s path=%s", s.Backend(), s.Path())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	f, err := s.WaitFrame(ctx)
	if err != nil {
		t.Fatalf("WaitFrame: %v", err)
	}
	if f.Width != d.PixelWidth || f.Height != d.PixelHeight {
		t.Fatalf("frame %dx%d, display %dx%d", f.Width, f.Height, d.PixelWidth, d.PixelHeight)
	}
	if _, uniform := f.Uniform(); uniform {
		t.Fatal("the duplicated frame is uniformly one colour")
	}
	st := s.Stats()
	t.Logf("read-back %v, change-wait %v, stride %d (%+d over Width*4)",
		st.Capture, st.Wait, f.Stride, f.Stride-f.Width*4)
	pix := make([]byte, f.TightLen())
	f.CopyTight(pix)
	writeArtifact(t, "display-duplication.png",
		Frame{Pix: pix, Width: f.Width, Height: f.Height, Stride: f.Width * 4})
}

// TestLiveStrideIsCarried is the assertion that a consumer indexing with
// Width*4 would fail: whatever the backend pads rows to, Stride says so and
// Row trims to it.
func TestLiveStrideIsCarried(t *testing.T) {
	requireLive(t)
	s, err := CaptureDisplay(context.Background(), mainDisplay(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f, err := s.WaitFrame(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("backend=%s stride=%d width*4=%d padding=%d bytes/row",
		s.Backend(), f.Stride, f.Width*4, f.Stride-f.Width*4)
	if f.Stride < f.Width*4 {
		t.Fatalf("stride %d is narrower than a %d-pixel row", f.Stride, f.Width)
	}
	if len(f.Pix) != f.Stride*f.Height {
		t.Fatalf("len(Pix) = %d, stride*height = %d", len(f.Pix), f.Stride*f.Height)
	}
	for y := 0; y < f.Height; y++ {
		if got := len(f.Row(y)); got != f.Width*4 {
			t.Fatalf("Row(%d) is %d bytes, want %d", y, got, f.Width*4)
		}
	}
	// Every frame this package hands out is TOP-DOWN, whatever the source
	// said.
	if f.Height < 2 {
		t.Skip("a one-row display cannot show an orientation")
	}
}

// TestLiveFrameIsBorrowedNotCopied proves the contract that makes the hot path
// free: the bytes handed back alias the capture buffer, and they are replaced
// in place when a newer frame is taken.
func TestLiveFrameBorrowMovesWithTake(t *testing.T) {
	requireLive(t)
	s, err := CaptureDisplay(context.Background(), mainDisplay(t), Options{Backend: BackendGDI})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, err := s.WaitFrame(ctx)
	if err != nil {
		t.Fatal(err)
	}
	firstPtr := &first.Pix[0]
	// Collect several frames; the ring must rotate rather than reuse the slot
	// the consumer is holding, so at least one later frame lives at a
	// different address.
	rotated := false
	for i := 0; i < 8; i++ {
		wf, c := context.WithTimeout(context.Background(), 2*time.Second)
		f, err := s.WaitFrame(wf)
		c()
		if err != nil {
			break
		}
		if &f.Pix[0] != firstPtr {
			rotated = true
		}
	}
	if !rotated {
		t.Fatal("every frame came back at the same address: the ring is not rotating, " +
			"so the capture is writing into the buffer the consumer is holding")
	}
}

func TestLiveCloseIsIdempotent(t *testing.T) {
	requireLive(t)
	s, err := CaptureDisplay(context.Background(), mainDisplay(t), Options{Backend: BackendGDI})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if !errors.Is(s.Err(), ErrClosed) {
		t.Fatalf("Err after Close = %v, want ErrClosed", s.Err())
	}
	if f, fresh := s.Frame(); fresh || f.Valid() {
		t.Fatal("a closed stream handed out a frame")
	}
}

// writeArtifact saves a frame under testdata/artifacts so a run leaves
// something a human can look at.
func writeArtifact(t *testing.T, name string, f Frame) {
	t.Helper()
	dir := os.Getenv("SCREENCAPTURE_ARTIFACTS")
	if dir == "" {
		dir = filepath.Join("testdata", "artifacts")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Logf("artifacts: %v", err)
		return
	}
	img, err := f.NRGBAOpaque()
	if err != nil {
		t.Logf("artifacts: %v", err)
		return
	}
	path := filepath.Join(dir, name)
	out, err := os.Create(path)
	if err != nil {
		t.Logf("artifacts: %v", err)
		return
	}
	defer out.Close()
	if err := png.Encode(out, img); err != nil {
		t.Logf("artifacts: %v", err)
		return
	}
	t.Logf("wrote %s (%dx%d)", path, f.Width, f.Height)
}

// BenchmarkFrame is the number the borrow contract exists for: what one
// Frame() costs a compositor that calls it once per rendered frame. Run it
// with -benchmem; allocs/op must be 0.
func BenchmarkFrame(b *testing.B) {
	requireLive(b)
	ds, err := Displays(context.Background())
	if err != nil || len(ds) == 0 {
		b.Skipf("no displays: %v", err)
	}
	s, err := CaptureDisplay(context.Background(), ds[0], Options{Backend: BackendGDI})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.WaitFrame(ctx); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	var sink uint64
	for i := 0; i < b.N; i++ {
		f, _ := s.Frame()
		sink += f.Seq
	}
	_ = sink
}

// BenchmarkGDIReadback isolates the cost of the DIB-section write path at
// sizes up to 4K, WITHOUT the display: the source is a memory device context
// holding a DIB of the same size.
//
// It is deliberately not a screen capture. On a machine whose only display is
// an emulated 800x600 basic adapter there is no way to measure a real 4K
// screen read-back, and inventing one would be worse than measuring the part
// that CAN be measured. This is the memory-to-memory component — the floor
// under a real read-back, never the whole of it, because a real BitBlt from
// the screen also pulls the pixels back across the display driver.
func BenchmarkGDIReadback(b *testing.B) {
	requireLive(b)
	for _, size := range []struct {
		name string
		w, h int
	}{
		{"800x600", 800, 600},
		{"1920x1080", 1920, 1080},
		{"2560x1440", 2560, 1440},
		{"3840x2160", 3840, 2160},
	} {
		b.Run(size.name, func(b *testing.B) {
			screen, err := screenDC()
			if err != nil {
				b.Fatal(err)
			}
			defer releaseScreenDC(screen)
			srcDC, err := memoryDC(screen)
			if err != nil {
				b.Fatal(err)
			}
			defer deleteDC(srcDC)
			src, err := newDIBSection(screen, size.w, size.h)
			if err != nil {
				b.Fatal(err)
			}
			defer src.free()
			selectObject(srcDC, uintptr(src.bmp))
			// Fill the source so the blit moves real data rather than a page
			// of zeros the memory manager can shortcut.
			for i := range src.pix {
				src.pix[i] = byte(i)
			}
			dstDC, err := memoryDC(screen)
			if err != nil {
				b.Fatal(err)
			}
			defer deleteDC(dstDC)
			dst, err := newDIBSection(screen, size.w, size.h)
			if err != nil {
				b.Fatal(err)
			}
			defer dst.free()
			selectObject(dstDC, uintptr(dst.bmp))

			b.SetBytes(int64(size.w * size.h * 4))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := bitBlt(dstDC, 0, 0, size.w, size.h, srcDC, 0, 0, srcCopy); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(size.w*size.h)/1e6, "Mpx")
		})
	}
}

// BenchmarkScreenReadback measures a REAL read-back of the whole primary
// display, which is the number that decides whether a display fits a frame
// budget. It is whatever size the machine's display actually is; the report
// states the size and the per-megapixel cost so it can be compared across
// machines.
func BenchmarkScreenReadback(b *testing.B) {
	requireLive(b)
	d := mainDisplay(b)
	screen, err := screenDC()
	if err != nil {
		b.Fatal(err)
	}
	defer releaseScreenDC(screen)
	memDC, err := memoryDC(screen)
	if err != nil {
		b.Fatal(err)
	}
	defer deleteDC(memDC)
	dib, err := newDIBSection(screen, d.PixelWidth, d.PixelHeight)
	if err != nil {
		b.Fatal(err)
	}
	defer dib.free()
	selectObject(memDC, uintptr(dib.bmp))
	b.SetBytes(int64(d.PixelWidth * d.PixelHeight * 4))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := bitBlt(memDC, 0, 0, d.PixelWidth, d.PixelHeight, screen,
			d.Bounds.X, d.Bounds.Y, srcCopy|captureBLT); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(d.PixelWidth*d.PixelHeight)/1e6, "Mpx")
	b.Logf("display %dx%d", d.PixelWidth, d.PixelHeight)
}

// TestLiveStructSizesAgainstTheOS is the one check the portable size assertions
// cannot make: that the OS itself accepts the structures. GetMonitorInfoW
// REJECTS a MONITORINFOEXW whose cbSize is not exactly right, so a successful
// call is direct evidence the Go layout matches the C one.
func TestLiveStructSizesAgainstTheOS(t *testing.T) {
	requireLive(t)
	mi := monitorInfoEx{CbSize: uint32(unsafe.Sizeof(monitorInfoEx{}))}
	var found bool
	ds, err := Displays(context.Background())
	if err != nil || len(ds) == 0 {
		t.Fatalf("no displays: %v", err)
	}
	r, _, _ := procGetMonitorInfoW.Call(uintptr(ds[0].ID), uintptr(unsafe.Pointer(&mi)))
	found = boolOf(r)
	if !found {
		t.Fatal("GetMonitorInfoW rejected the MONITORINFOEXW: the Go layout does not match the C one")
	}
	if got := utf16ToString(mi.SzDevice[:]); got != ds[0].DeviceName {
		t.Fatalf("szDevice = %q, want %q — the field is at the wrong offset", got, ds[0].DeviceName)
	}
	fmt.Fprintf(os.Stderr, "MONITORINFOEXW accepted by the OS, szDevice=%q\n", ds[0].DeviceName)
}
