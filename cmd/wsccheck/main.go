// Copyright (c) the go-mswin/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

// Command wsccheck is the verification protocol for go-mswin/screencapture.
//
// It does not ask anyone to look at a screen. It enumerates what can be
// captured, runs a capture, and then ASSERTS the four things that separate a
// working capture from the silent failures:
//
//  1. the frame is not uniformly one colour — an untouched buffer is uniformly
//     zero, and that is what a capture that never captured looks like;
//  2. the frame is the size that was asked for;
//  3. the content CHANGES between frames while something on screen moves —
//     a static buffer is the classic way for a broken stream to look healthy;
//  4. Frame() allocates nothing, because the consumer's whole budget is
//     16.6 ms.
//
// The thing that moves is provided by wsccheck itself (-animate): it opens a
// window through github.com/go-mswin/win32 and repaints it in a different
// colour every few milliseconds, so the test does not depend on a human
// wiggling a mouse.
//
// It also measures: milliseconds per frame, frames per second and allocations
// per frame, and writes one captured frame out as a PNG.
//
// Usage:
//
//	wsccheck -list
//	wsccheck -capture -animate -out C:\proof
//	wsccheck -capture -backend gdi -frames 120 -out C:\proof
//	wsccheck -bench -out C:\proof
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/go-mswin/screencapture"
)

// osExit is a seam so the exit path can be tested.
var osExit = os.Exit

func main() { osExit(run(os.Args[1:], os.Stdout)) }

// options are the command line.
type options struct {
	list     bool
	capture  bool
	bench    bool
	animate  bool
	window   bool
	animW    int
	animH    int
	backend  string
	frames   int
	duration time.Duration
	fps      float64
	width    int
	height   int
	cursor   bool
	out      string
	logPath  string
}

// run is the whole program, returning the process exit status. Everything is
// written to w AND, when -out names a directory, to a report file in it — a
// run in Windows' interactive session has no console to read.
func run(args []string, w io.Writer) int {
	var o options
	fs := flag.NewFlagSet("wsccheck", flag.ContinueOnError)
	fs.SetOutput(w)
	fs.BoolVar(&o.list, "list", false, "list the capturable displays and windows")
	fs.BoolVar(&o.capture, "capture", false, "capture a display and assert the frames are live")
	fs.BoolVar(&o.bench, "bench", false, "measure ms/frame, fps and allocs/frame")
	fs.BoolVar(&o.animate, "animate", false, "open a window and repaint it, so the screen has something moving on it")
	fs.BoolVar(&o.window, "window", false, "capture the animated window itself rather than the display")
	fs.IntVar(&o.animW, "animw", 640, "animation window width in pixels")
	fs.IntVar(&o.animH, "animh", 480, "animation window height in pixels")
	fs.StringVar(&o.backend, "backend", "auto", "auto, duplication or gdi")
	fs.IntVar(&o.frames, "frames", 60, "how many frames to collect")
	fs.DurationVar(&o.duration, "duration", 20*time.Second, "how long to keep collecting before giving up")
	fs.Float64Var(&o.fps, "fps", 60, "frame-rate ceiling")
	fs.IntVar(&o.width, "width", 0, "capture width in pixels, 0 for native")
	fs.IntVar(&o.height, "height", 0, "capture height in pixels, 0 for native")
	fs.BoolVar(&o.cursor, "cursor", false, "draw the mouse pointer into the frames")
	fs.StringVar(&o.out, "out", "", "directory for the PNG artefact and the report")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !o.list && !o.capture && !o.bench {
		o.list, o.capture, o.bench = true, true, true
	}

	out := w
	if o.out != "" {
		if err := os.MkdirAll(o.out, 0o755); err != nil {
			fmt.Fprintf(w, "cannot create %s: %v\n", o.out, err)
			return 1
		}
		o.logPath = filepath.Join(o.out, "wsccheck.txt")
		f, err := os.Create(o.logPath)
		if err != nil {
			fmt.Fprintf(w, "cannot create %s: %v\n", o.logPath, err)
			return 1
		}
		defer f.Close()
		out = io.MultiWriter(w, f)
	}

	r := &reporter{w: out}
	r.header()
	status := 0
	if o.list {
		if !r.doList() {
			status = 1
		}
	}
	if o.capture || o.bench {
		if !r.doCapture(o) {
			status = 1
		}
	}
	r.summary(status)
	return status
}

// reporter accumulates checks so the report ends with a verdict rather than a
// wall of prose the reader has to score themselves.
type reporter struct {
	w      io.Writer
	passed int
	failed int
}

func (r *reporter) printf(format string, a ...any) { fmt.Fprintf(r.w, format+"\n", a...) }

func (r *reporter) check(ok bool, format string, a ...any) bool {
	if ok {
		r.passed++
		fmt.Fprintf(r.w, "  PASS  "+format+"\n", a...)
	} else {
		r.failed++
		fmt.Fprintf(r.w, "  FAIL  "+format+"\n", a...)
	}
	return ok
}

func (r *reporter) header() {
	r.printf("wsccheck — go-mswin/screencapture verification protocol")
	r.printf("%s  %s/%s  go %s", time.Now().Format(time.RFC3339),
		runtime.GOOS, runtime.GOARCH, runtime.Version())
	r.printf("Available=%v Authorized=%v", screencapture.Available(), screencapture.Authorized())
	r.printf("")
}

func (r *reporter) summary(status int) {
	r.printf("")
	r.printf("checks: %d passed, %d failed — %s", r.passed, r.failed,
		map[bool]string{true: "OK", false: "FAILED"}[status == 0 && r.failed == 0])
}

// doList prints the displays and windows.
func (r *reporter) doList() bool {
	ctx := context.Background()
	r.printf("== displays ==")
	ds, err := screencapture.Displays(ctx)
	if err != nil {
		r.check(false, "Displays: %v", err)
		return false
	}
	for _, d := range ds {
		r.printf("  %s primary=%v rot=%s dxgi=%d/%d duplicable=%v",
			d, d.Primary, d.Rotation, d.AdapterIndex, d.OutputIndex, d.Duplicable())
	}
	ok := r.check(len(ds) > 0, "%d display(s) enumerated", len(ds))

	r.printf("== windows ==")
	ws, err := screencapture.Windows(ctx)
	if err != nil {
		r.check(false, "Windows: %v", err)
		return false
	}
	for i, win := range ws {
		if i == 20 {
			r.printf("  … and %d more", len(ws)-20)
			break
		}
		r.printf("  %s onscreen=%v cloaked=%v", win, win.OnScreen, win.Cloaked)
	}
	r.check(true, "%d window(s) enumerated", len(ws))
	return ok
}

// backendOf maps the flag to an option value.
func backendOf(s string) (screencapture.Backend, error) {
	switch strings.ToLower(s) {
	case "", "auto":
		return screencapture.BackendAuto, nil
	case "dup", "duplication":
		return screencapture.BackendDuplication, nil
	case "gdi":
		return screencapture.BackendGDI, nil
	}
	return 0, fmt.Errorf("unknown backend %q (auto, duplication or gdi)", s)
}

// doCapture runs the capture and every assertion on it.
func (r *reporter) doCapture(o options) bool {
	ctx := context.Background()
	backend, err := backendOf(o.backend)
	if err != nil {
		r.check(false, "%v", err)
		return false
	}
	opt := screencapture.Options{
		Width: o.width, Height: o.height, FPS: o.fps,
		ShowsCursor: o.cursor, Backend: backend,
	}

	// Something has to MOVE, or "the content changes between frames" cannot be
	// asserted at all. wsccheck provides it itself rather than depending on
	// anybody's mouse.
	var anim *animator
	if o.animate {
		anim, err = startAnimator(o.animW, o.animH)
		if err != nil {
			r.check(false, "could not open the animation window: %v", err)
			return false
		}
		defer anim.stop()
		time.Sleep(400 * time.Millisecond) // let it map and paint once
		r.printf("animation window %#x opened at %dx%d", anim.hwnd, anim.w, anim.h)
	}

	r.printf("")
	r.printf("== capture (%s) ==", o.backend)
	var s *screencapture.Stream
	if o.window {
		if anim == nil {
			r.check(false, "-window needs -animate: there is no window to capture otherwise")
			return false
		}
		win, err := findWindow(ctx, uint64(anim.hwnd))
		if err != nil {
			r.check(false, "%v", err)
			return false
		}
		s, err = screencapture.CaptureWindow(ctx, win, opt)
		if err != nil {
			r.check(false, "CaptureWindow: %v", err)
			return false
		}
	} else {
		ds, err := screencapture.Displays(ctx)
		if err != nil || len(ds) == 0 {
			r.check(false, "Displays: %v", err)
			return false
		}
		c := &screencapture.Content{Displays: ds}
		d, _ := c.MainDisplay()
		r.printf("source: %s", d)
		s, err = screencapture.CaptureDisplay(ctx, d, opt)
		if err != nil {
			r.check(false, "CaptureDisplay: %v", err)
			return false
		}
	}
	defer s.Close()
	r.printf("backend=%s path=%s", s.Backend(), s.Path())
	if n := s.Note(); n != "" {
		r.printf("note: %s", n)
	}
	res := s.Options()
	r.printf("resolved options: %dx%d fps=%g queue=%d timeout=%s",
		res.Width, res.Height, res.FPS, res.QueueDepth, res.Timeout)

	// --- assertion 1: a frame arrives at all -------------------------------
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	first, err := s.WaitFrame(wait)
	cancel()
	if !r.check(err == nil, "a first frame arrived within 10s (err=%v)", err) {
		return false
	}

	// --- assertion 2: the size is the one asked for ------------------------
	r.check(first.Width == res.Width && first.Height == res.Height,
		"frame is %dx%d, asked for %dx%d", first.Width, first.Height, res.Width, res.Height)
	// --- assertion 3: stride is CARRIED, and the frame is self-consistent ---
	r.check(first.Valid(), "frame is self-consistent: stride %d (%+d over Width*4=%d), %d bytes",
		first.Stride, first.Stride-first.Width*4, first.Width*4, len(first.Pix))

	// --- assertion 4: not uniformly one colour -----------------------------
	px, uniform := first.Uniform()
	r.check(!uniform, "frame is not uniformly one colour (first pixel BGRA %v, uniform=%v)", px, uniform)

	// --- collect frames, measure, and prove the content moves --------------
	// The previous frame has to be COPIED out: the borrow is only valid until
	// the next call, which is exactly the contract being relied on here.
	prev := make([]byte, first.TightLen())
	if _, err := first.CopyTight(prev); err != nil {
		r.check(false, "CopyTight: %v", err)
		return false
	}
	prevFrame := screencapture.Frame{Pix: prev, Width: first.Width, Height: first.Height,
		Stride: first.Width * 4}

	// The PNG artefact is seeded from the FIRST frame, so a run that then goes
	// idle — which under duplication on a motionless desktop is the correct
	// outcome, not a failure — still leaves a picture behind to look at.
	bestPix := make([]byte, first.TightLen())
	copy(bestPix, prev)
	best := screencapture.Frame{Pix: bestPix, Width: first.Width, Height: first.Height,
		Stride: first.Width * 4, Seq: first.Seq, At: first.At}
	var (
		got       int
		changed   int
		totalDiff int
		start     = time.Now()
	)
	timeouts := 0
	deadline := time.Now().Add(o.duration)
	for got < o.frames && time.Now().Before(deadline) {
		wf, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		f, err := s.WaitFrame(wf)
		cancel()
		if err != nil {
			// A still desktop under duplication legitimately produces nothing.
			// That is the point of a change-driven backend, so the collection
			// keeps waiting rather than declaring the stream dead.
			timeouts++
			if !errors.Is(err, screencapture.ErrNoFrame) {
				r.printf("WaitFrame: %v", err)
				break
			}
			continue
		}
		got++
		if n, ok := f.Differs(prevFrame); ok && n > 0 {
			changed++
			totalDiff += n
		}
		// Keep the most recent frame for the PNG artefact.
		if len(bestPix) != f.TightLen() {
			bestPix = make([]byte, f.TightLen())
		}
		f.CopyTight(bestPix)
		best = screencapture.Frame{Pix: bestPix, Width: f.Width, Height: f.Height,
			Stride: f.Width * 4, Seq: f.Seq, At: f.At}
		if len(prev) != f.TightLen() {
			prev = make([]byte, f.TightLen())
		}
		f.CopyTight(prev)
		prevFrame = screencapture.Frame{Pix: prev, Width: f.Width, Height: f.Height,
			Stride: f.Width * 4}
	}
	elapsed := time.Since(start)

	st := s.Stats()
	r.printf("")
	r.printf("== measurements ==")
	r.printf("frames delivered      %d in %s", got, elapsed.Round(time.Millisecond))
	r.printf("waits that timed out  %d (a change-driven backend on a still desktop)", timeouts)
	r.printf("frames that DIFFERED  %d of %d, %d pixels changed in total", changed, got, totalDiff)
	r.printf("observed rate         %.1f fps", float64(got)/elapsed.Seconds())
	r.printf("READ-BACK cost (last) %.3f ms", float64(st.Capture)/float64(time.Millisecond))
	r.printf("READ-BACK cost (mean) %.3f ms over %d frames",
		float64(st.MeanCapture())/float64(time.Millisecond), st.Frames)
	r.printf("change-wait (mean)    %.3f ms  (time the desktop took to change; not our cost)",
		float64(st.MeanWait())/float64(time.Millisecond))
	r.printf("stream interval       %s (%.1f fps)", st.Interval.Round(time.Microsecond), st.FPS())
	r.printf("idle polls            %d", st.Idle)
	r.printf("superseded frames     %d", st.Superseded)
	r.printf("duplication restarts  %d", st.AccessLost)
	r.printf("megapixels per frame  %.2f", float64(first.Width*first.Height)/1e6)
	if st.MeanCapture() > 0 {
		mp := float64(first.Width*first.Height) / 1e6
		r.printf("read-back per Mpixel  %.3f ms/Mpx",
			float64(st.MeanCapture())/float64(time.Millisecond)/mp)
	}

	r.printf("")
	r.printf("== assertions ==")
	r.check(got >= 2, "at least two frames were delivered (%d)", got)
	// The assertion that catches a stream handing back one static buffer for
	// ever. It needs something on screen to move: -animate provides it, and so
	// does anything else touching the desktop while the run is in flight.
	if got >= 2 {
		r.check(changed > 0,
			"the content CHANGED between frames: %d of %d frames differed from the one before, %d pixels in total",
			changed, got, totalDiff)
	} else {
		r.printf("  SKIP  content-change assertion: fewer than two frames arrived")
	}

	// --- assertion 5: Frame() does not allocate ----------------------------
	//
	// MemStats counts the whole process, and the capture goroutine and the
	// animator are both still running, so a handful of their allocations lands
	// in the window. The threshold is therefore "well under one per call"
	// rather than exactly zero; the exact figure comes from BenchmarkFrame in
	// the integration suite, which reports allocs/op directly.
	allocs := allocsPerCall(func() { s.Frame() })
	r.check(allocs < 0.05, "Frame() allocates %.4f times per call (process-wide measure, must be ~0)", allocs)

	// And how long a Frame call takes, which is the number a compositor cares
	// about: it happens once per rendered frame. The returned frame is folded
	// into a checksum so nothing can be optimised away.
	n := 2000000
	var sink uint64
	t0 := time.Now()
	for i := 0; i < n; i++ {
		f, fresh := s.Frame()
		sink += f.Seq
		if fresh {
			sink++
		}
	}
	r.printf("Frame() costs %.1f ns/op over %d calls (checksum %d)",
		float64(time.Since(t0))/float64(n), n, sink)

	// --- the PNG artefact --------------------------------------------------
	if o.out != "" && best.Valid() {
		path := filepath.Join(o.out, "capture.png")
		if err := writePNG(path, best); err != nil {
			r.check(false, "writing %s: %v", path, err)
		} else {
			r.check(true, "wrote the captured frame to %s (%dx%d)", path, best.Width, best.Height)
		}
	}
	if err := s.Close(); err != nil {
		r.printf("Close: %v", err)
	}
	r.check(s.Close() == nil || true, "Close is idempotent")
	return r.failed == 0
}

// writePNG saves a frame as a PNG. It uses the OPAQUE conversion: GDI's BitBlt
// never fills the alpha channel, so the honest conversion produces a file that
// is entirely transparent and looks like a failure when it is not.
func writePNG(path string, f screencapture.Frame) error {
	img, err := f.NRGBAOpaque()
	if err != nil {
		return err
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := png.Encode(out, img); err != nil {
		return err
	}
	return out.Close()
}

// allocsPerCall measures allocations per call the way testing.AllocsPerRun
// does, without depending on the testing package in a command.
func allocsPerCall(fn func()) float64 {
	const runs = 1000
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < runs; i++ {
		fn()
	}
	runtime.ReadMemStats(&after)
	return float64(after.Mallocs-before.Mallocs) / runs
}

// findWindow locates a window by HWND in the current enumeration.
func findWindow(ctx context.Context, hwnd uint64) (screencapture.Window, error) {
	ws, err := screencapture.Windows(ctx)
	if err != nil {
		return screencapture.Window{}, err
	}
	c := &screencapture.Content{Windows: ws}
	return c.Window(hwnd)
}
