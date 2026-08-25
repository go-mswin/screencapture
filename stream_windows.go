// Copyright (c) the go-mswin/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build windows

package screencapture

// The live stream: one goroutine capturing into a ring, a consumer borrowing
// the newest frame out of it.

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"
)

// backendCapture is what the two backends have in common. Everything above
// this line — pacing, the ring, statistics, the borrow contract — is written
// once against it.
type backendCapture interface {
	// Capture fills ring slot i and returns a view of it, the layout the OS
	// stated, how long the read-back took and how long it waited for the
	// desktop to change first. It reports [ErrNoFrame] when nothing changed.
	//
	// The two durations are separate because conflating them makes a
	// change-driven backend on an idle desktop look ruinously expensive when
	// it did almost no work at all.
	Capture(slot int, timeout time.Duration) (pix []byte, layout DIBLayout, readback, wait time.Duration, err error)
	// Size is the output size in pixels.
	Size() (int, int)
	// Path names the read-back route, for logs and the report.
	Path() string
	// Close releases everything. It is idempotent.
	Close()
}

// excluded remembers a window's display affinity so [Stream.Close] can put it
// back exactly as it was rather than assuming it was WDA_NONE.
type excluded struct {
	hwnd uintptr
	prev uint32
	had  bool
}

// Stream is a live capture. Create one with [CaptureDisplay] or
// [CaptureWindow] and close it when done; a Stream holds OS handles and a
// goroutine, and neither is reclaimed by the garbage collector.
type Stream struct {
	opt     Options
	backend Backend
	source  string
	note    string

	cap  backendCapture
	ring *ring

	// sig carries "a new frame arrived" to WaitFrame. It is buffered to one
	// and written non-blockingly, so a consumer that is not waiting never
	// slows the capture down.
	sig chan struct{}

	stop chan struct{}
	done chan struct{}

	mu       sync.Mutex
	stats    Stats
	err      error
	excluded []excluded

	closeOnce sync.Once
	closeErr  error
}

// Options returns the stream's resolved options: the defaults filled in and
// the source's native size substituted for a zero size.
func (s *Stream) Options() Options { return s.opt }

// Backend reports which capture route actually ran. With [BackendAuto] this is
// the only way to know whether duplication was available.
func (s *Stream) Backend() Backend { return s.backend }

// Source names what is being captured, for logs.
func (s *Stream) Source() string { return s.source }

// Path names the read-back route in use, e.g. "BitBlt(SRCCOPY|CAPTUREBLT) into
// a DIB section". It is what a performance report should quote alongside a
// millisecond figure, because the two GDI paths and the two duplication paths
// do not cost the same.
func (s *Stream) Path() string { return s.cap.Path() }

// Note explains a decision the stream made on the caller's behalf — most
// usefully why [BackendAuto] fell back to GDI. It is empty when there was
// nothing to explain.
func (s *Stream) Note() string { return s.note }

// Frame hands back a BORROWED view of the most recent captured frame and
// whether it is newer than the one the previous call returned.
//
// It allocates nothing and copies nothing: the bytes are the DIB section GDI
// blitted into, or the mapped D3D11 staging texture. They stay valid until the
// next call to Frame, [Stream.WaitFrame] or [Stream.Close].
//
// The boolean is the truth about whether anything changed, but only with
// [BackendDuplication]: GDI cannot tell, so it reports every poll as fresh.
func (s *Stream) Frame() (Frame, bool) { return s.ring.take() }

// WaitFrame blocks until a frame NEWER than the one the previous call returned
// is available, then returns it under the same borrow rules as [Stream.Frame].
//
// It reports [ErrNoFrame] when the context expires first, which on a still
// desktop under [BackendDuplication] is entirely normal and not a malfunction,
// and [ErrClosed] once the stream is closed. A capture error that stopped the
// stream is returned in preference to either.
func (s *Stream) WaitFrame(ctx context.Context) (Frame, error) {
	for {
		if f, fresh := s.ring.take(); fresh {
			return f, nil
		}
		if err := s.Err(); err != nil {
			return Frame{}, err
		}
		select {
		case <-s.sig:
			// A frame arrived; loop round and take it.
		case <-s.done:
			if f, fresh := s.ring.take(); fresh {
				return f, nil
			}
			if err := s.Err(); err != nil {
				return Frame{}, err
			}
			return Frame{}, ErrClosed
		case <-ctx.Done():
			return Frame{}, fmt.Errorf("%w: %w", ErrNoFrame, ctx.Err())
		}
	}
}

// Stats reports what the stream has seen since it started.
func (s *Stream) Stats() Stats {
	s.mu.Lock()
	st := s.stats
	s.mu.Unlock()
	st.Superseded = s.ring.supersededCount()
	return st
}

// Err reports the error that stopped the stream, or nil while it is running.
// A closed stream reports [ErrClosed] unless something worse happened first.
func (s *Stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// fail records the error that stopped the stream, keeping the first one.
func (s *Stream) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

// Close stops the capture goroutine, releases every OS handle and restores the
// display affinity of any window [Options.ExcludeWindows] changed. It is
// idempotent and always reports the same error.
func (s *Stream) Close() error {
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.done
		s.ring.reset()
		s.cap.Close()
		for _, e := range s.excluded {
			if e.had {
				setDisplayAffinity(e.hwnd, e.prev)
			} else {
				setDisplayAffinity(e.hwnd, wdaNone)
			}
		}
		s.excluded = nil
		s.mu.Lock()
		if s.err == nil {
			s.err = ErrClosed
		}
		s.mu.Unlock()
	})
	return s.closeErr
}

// signal wakes one WaitFrame without ever blocking the capture.
func (s *Stream) signal() {
	select {
	case s.sig <- struct{}{}:
	default:
	}
}

// run is the capture goroutine.
//
// It is pinned to one OS thread for the whole of its life. The Direct3D 11
// immediate context is not thread-safe, and a Go goroutine is free to move
// between OS threads at any function call; pinning removes the question
// entirely, for the price of one thread.
func (s *Stream) run() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(s.done)

	interval := frameInterval(s.opt.FPS)
	// The row-flip scratch buffer, allocated once. Nothing on the hot path
	// allocates, including the bottom-up path that never runs on a healthy
	// system.
	var scratch []byte

	for {
		select {
		case <-s.stop:
			return
		default:
		}
		started := time.Now()
		i := s.ring.next()
		pix, layout, cost, waited, err := s.cap.Capture(i, s.opt.Timeout)

		switch {
		case err == nil:
			// fall through
		case errors.Is(err, ErrNoFrame):
			s.mu.Lock()
			s.stats.Idle++
			s.mu.Unlock()
			if !s.pace(started, interval) {
				return
			}
			continue
		case errors.Is(err, ErrAccessLost):
			// Recoverable: the duplication was torn down by a mode change, a
			// secure-desktop transition or a session switch, and rebuilds
			// itself on the next attempt. The ring is emptied because the
			// staging textures the published frames pointed into are gone.
			s.ring.reset()
			s.mu.Lock()
			s.stats.AccessLost++
			s.mu.Unlock()
			if !s.sleep(50 * time.Millisecond) {
				return
			}
			continue
		default:
			s.fail(err)
			return
		}

		seq := s.ring.publishSeq()
		if cap(scratch) < layout.Stride {
			scratch = make([]byte, layout.Stride)
		}
		f, err := layout.Frame(pix, seq, time.Now(), scratch[:cap(scratch)])
		if err != nil {
			s.fail(err)
			return
		}
		s.ring.publish(i, f)
		s.mu.Lock()
		s.stats.Frames++
		if !s.stats.Last.IsZero() {
			s.stats.Interval = f.At.Sub(s.stats.Last)
		}
		s.stats.Last = f.At
		s.stats.Capture = cost
		s.stats.CaptureTotal += cost
		s.stats.Wait = waited
		s.stats.WaitTotal += waited
		s.mu.Unlock()
		s.signal()

		if !s.pace(started, interval) {
			return
		}
	}
}

// pace sleeps out whatever is left of the frame interval. It reports false
// when the stream was closed while it waited.
func (s *Stream) pace(started time.Time, interval time.Duration) bool {
	rest := interval - time.Since(started)
	if rest <= 0 {
		return true
	}
	return s.sleep(rest)
}

// sleep waits for d, or until the stream is closed. It reports false when it
// was the close that ended the wait.
func (s *Stream) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.stop:
		return false
	}
}

// CaptureDisplay starts capturing one display.
//
// The display is re-read from the system first, so a stale snapshot fails with
// [ErrNotFound] rather than capturing the wrong rectangle. With the default
// [BackendAuto] the stream tries Desktop Duplication and silently falls back to
// GDI when the request or the adapter rules it out; [Stream.Backend] and
// [Stream.Note] say which happened and why.
func CaptureDisplay(ctx context.Context, d Display, opt Options) (*Stream, error) {
	if err := opt.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	live, err := lookupDisplay(ctx, d)
	if err != nil {
		return nil, err
	}
	res, err := opt.resolve(live.PixelWidth, live.PixelHeight)
	if err != nil {
		return nil, err
	}
	want, err := pickBackend(opt.Backend, res, live.PixelWidth, live.PixelHeight, false, live.Duplicable())
	if err != nil {
		return nil, err
	}

	source := fmt.Sprintf("display %#x %s %dx%d", live.ID, live.DeviceName,
		live.PixelWidth, live.PixelHeight)
	var capture backendCapture
	var note string
	if want == BackendDuplication {
		dup, derr := newDuplicator(live, res.QueueDepth)
		if derr == nil {
			// Duplication reports the desktop's own size, which is the truth
			// about what will arrive; an option asking for anything else was
			// already routed to GDI by pickBackend.
			capture = dup
			want = BackendDuplication
		} else {
			if opt.Backend == BackendDuplication {
				return nil, derr
			}
			note = "fell back to GDI: " + derr.Error()
			want = BackendGDI
		}
	}
	if capture == nil {
		g, gerr := newGDIDisplayCapture(live, res, res.QueueDepth)
		if gerr != nil {
			return nil, gerr
		}
		capture = g
	}
	return newStream(res, want, source, note, capture, opt.ExcludeWindows)
}

// CaptureWindow starts capturing one window.
//
// Only [BackendGDI] can do this: Desktop Duplication captures whole outputs.
// Asking for [BackendDuplication] here is [ErrInvalidOption] rather than a
// silent substitution.
//
// The capture follows the window as it moves and as it is occluded, because
// PrintWindow renders through DWM rather than reading the screen. It does NOT
// follow a resize: the frame size is fixed when the stream starts, and a window
// that grows is captured with its excess clipped. Restart the stream to change
// size.
func CaptureWindow(ctx context.Context, w Window, opt Options) (*Stream, error) {
	if err := opt.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dpiOnce()
	live, err := liveWindow(w)
	if err != nil {
		return nil, err
	}
	res, err := opt.resolve(live.Bounds.W, live.Bounds.H)
	if err != nil {
		return nil, err
	}
	want, err := pickBackend(opt.Backend, res, live.Bounds.W, live.Bounds.H, true, false)
	if err != nil {
		return nil, err
	}
	g, err := newGDIWindowCapture(live, res, res.QueueDepth)
	if err != nil {
		return nil, err
	}
	source := fmt.Sprintf("window %#x %q %dx%d", live.ID, live.Title, live.Bounds.W, live.Bounds.H)
	note := ""
	if live.Minimized {
		note = "the window is minimised; PrintWindow renders it through DWM, " +
			"but a minimised window's contents may be stale"
	}
	return newStream(res, want, source, note, g, nil)
}

// newStream applies the exclusions, starts the capture goroutine and returns
// the stream. On any failure it undoes everything it did.
func newStream(opt Options, backend Backend, source, note string,
	capture backendCapture, exclude []uint64) (*Stream, error) {
	s := &Stream{
		opt:     opt,
		backend: backend,
		source:  source,
		note:    note,
		cap:     capture,
		ring:    newRing(opt.QueueDepth),
		sig:     make(chan struct{}, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	for _, id := range exclude {
		hwnd := uintptr(id)
		prev, had := displayAffinity(hwnd)
		if err := setDisplayAffinity(hwnd, wdaExcludeFromCapture); err != nil {
			// Undo the ones already applied: a half-applied exclusion list is
			// worse than none, because the consumer cannot tell which windows
			// are actually excluded.
			for _, e := range s.excluded {
				setDisplayAffinity(e.hwnd, e.prev)
			}
			capture.Close()
			return nil, err
		}
		s.excluded = append(s.excluded, excluded{hwnd: hwnd, prev: prev, had: had})
	}
	go s.run()
	return s, nil
}
