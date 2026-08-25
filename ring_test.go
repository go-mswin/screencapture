// Copyright (c) the go-mswin/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

package screencapture

import (
	"sync"
	"testing"
	"time"
)

func frameN(n int) Frame {
	return Frame{Pix: make([]byte, 16), Width: 2, Height: 2, Stride: 8, Seq: uint64(n), At: time.Now()}
}

func TestRingRefusesToBeTooSmall(t *testing.T) {
	// Two slots cannot honour the borrow contract, so newRing raises rather
	// than accepting a ring that would hand out half-written frames.
	for _, n := range []int{-1, 0, 1, 2} {
		if got := newRing(n).n; got != MinQueueDepth {
			t.Fatalf("newRing(%d) has %d slots, want %d", n, got, MinQueueDepth)
		}
	}
	if got := newRing(8).n; got != 8 {
		t.Fatalf("newRing(8) has %d slots", got)
	}
}

func TestRingEmpty(t *testing.T) {
	r := newRing(3)
	if f, fresh := r.take(); fresh || f.Valid() {
		t.Fatalf("an empty ring handed out %+v (fresh=%v)", f, fresh)
	}
	if _, ok := r.peek(); ok {
		t.Fatal("an empty ring peeked a frame")
	}
	if r.supersededCount() != 0 {
		t.Fatal("an empty ring superseded something")
	}
}

func TestRingFreshness(t *testing.T) {
	r := newRing(3)
	i := r.next()
	r.publish(i, frameN(1))
	f, fresh := r.take()
	if !fresh || f.Seq != 1 {
		t.Fatalf("first take = seq %d, fresh %v", f.Seq, fresh)
	}
	// Taking again without a new publish is NOT fresh, and must hand back the
	// same frame rather than nothing: a compositor that redraws on a frame
	// that did not change still needs the pixels.
	f, fresh = r.take()
	if fresh || f.Seq != 1 {
		t.Fatalf("second take = seq %d, fresh %v", f.Seq, fresh)
	}
	j := r.next()
	if j == i {
		t.Fatalf("next() handed back the slot the consumer is borrowing (%d)", j)
	}
	r.publish(j, frameN(2))
	if f, fresh := r.take(); !fresh || f.Seq != 2 {
		t.Fatalf("after a publish, take = seq %d, fresh %v", f.Seq, fresh)
	}
}

func TestRingNeverHandsOutTheBorrowedSlot(t *testing.T) {
	// The invariant the whole contract rests on: while the consumer holds a
	// frame, the capture is never given that slot to write into.
	r := newRing(3)
	r.publish(r.next(), frameN(1))
	r.take() // borrow slot 0
	borrowed := r.lent
	for k := 0; k < 50; k++ {
		i := r.next()
		if i == borrowed {
			t.Fatalf("round %d: next() returned the borrowed slot %d", k, i)
		}
		r.publish(i, frameN(k+2))
	}
}

func TestRingSuperseded(t *testing.T) {
	r := newRing(3)
	for k := 0; k < 5; k++ {
		r.publish(r.next(), frameN(k))
	}
	// Four frames were replaced before anyone asked for one.
	if got := r.supersededCount(); got != 4 {
		t.Fatalf("superseded = %d, want 4", got)
	}
	if f, fresh := r.take(); !fresh || f.Seq != 4 {
		t.Fatalf("take after five publishes = seq %d, fresh %v", f.Seq, fresh)
	}
	// A frame that WAS delivered is not counted as superseded.
	r.publish(r.next(), frameN(9))
	if got := r.supersededCount(); got != 4 {
		t.Fatalf("superseded after a consumed frame = %d, want 4", got)
	}
}

func TestRingPeekDoesNotMoveTheBorrow(t *testing.T) {
	r := newRing(3)
	r.publish(r.next(), frameN(1))
	if f, ok := r.peek(); !ok || f.Seq != 1 {
		t.Fatalf("peek = seq %d, ok %v", f.Seq, ok)
	}
	// The peek must not have consumed the freshness.
	if _, fresh := r.take(); !fresh {
		t.Fatal("peek consumed the freshness flag")
	}
}

func TestRingPublishSeq(t *testing.T) {
	r := newRing(3)
	for want := uint64(1); want <= 4; want++ {
		if got := r.publishSeq(); got != want {
			t.Fatalf("publishSeq = %d, want %d", got, want)
		}
	}
}

func TestRingReset(t *testing.T) {
	r := newRing(3)
	r.publish(r.next(), frameN(1))
	r.take()
	r.reset()
	if f, fresh := r.take(); fresh || f.Valid() {
		t.Fatalf("a reset ring handed out %+v (fresh=%v)", f, fresh)
	}
	// After a reset every slot is writable again.
	if i := r.next(); i < 0 || i >= r.n {
		t.Fatalf("next() after reset = %d", i)
	}
}

func TestRingDegenerateFallbacks(t *testing.T) {
	// newRing can never build these, but the fallback exists so that a
	// hand-built ring degrades by losing a frame rather than by corrupting the
	// one being read. Both branches are exercised deliberately.
	full := &ring{n: 1, frames: make([]Frame, 1), ready: 0, lent: 0}
	if got := full.next(); got != 0 {
		t.Fatalf("a one-slot ring with a ready frame returned %d, want the ready slot 0", got)
	}
	lentOnly := &ring{n: 1, frames: make([]Frame, 1), ready: -1, lent: 0}
	if got := lentOnly.next(); got != 0 {
		t.Fatalf("a one-slot ring with nothing ready returned %d, want 0", got)
	}
}

// The ring is written by the capture goroutine and read by the consumer, so it
// is exercised under -race with both running flat out.
func TestRingConcurrent(t *testing.T) {
	r := newRing(4)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for k := 0; ; k++ {
			select {
			case <-stop:
				return
			default:
			}
			i := r.next()
			r.publish(i, frameN(k))
		}
	}()
	var last uint64
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) {
		f, fresh := r.take()
		if fresh {
			if f.Seq < last {
				t.Fatalf("sequence went backwards: %d after %d", f.Seq, last)
			}
			last = f.Seq
		}
		r.peek()
		r.supersededCount()
	}
	close(stop)
	wg.Wait()
	if last == 0 {
		t.Fatal("no frame was ever delivered")
	}
}
