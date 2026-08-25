// Copyright (c) the go-mswin/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

package screencapture

import "sync"

// ring is the hand-off between the capture side, which writes whole frames,
// and the consumer, which borrows the newest one and holds the borrow until it
// asks again.
//
// The contract [Stream.Frame] promises — "these bytes stay valid until your
// next call" — is only true if the capture NEVER writes into the buffer the
// consumer is holding. That is the ring's whole job, and it needs three slots
// to do it: one the consumer has borrowed, one holding the newest complete
// frame, and one being written into. With two, a capture that finishes while
// the consumer still holds the previous frame has nowhere to go but on top of
// the frame it just published, and the consumer's next call hands back a
// half-written image. That is [MinQueueDepth].
//
// It is deliberately in an untagged file: it is pure logic, it is where the
// borrow contract actually lives, and it is therefore worth testing exhaustively
// on any machine rather than only on Windows.
type ring struct {
	mu sync.Mutex
	n  int
	// frames[i] describes slot i's most recently published contents.
	frames []Frame
	// ready is the slot holding the newest complete frame, -1 for none.
	ready int
	// lent is the slot the consumer is currently borrowing, -1 for none.
	lent int
	// delivered says the frame in ready has already been handed out, which is
	// what makes the freshness flag meaningful and what distinguishes a
	// superseded frame from a consumed one.
	delivered bool
	// cursor rotates the search for a free slot so the ring is used evenly
	// rather than ping-ponging between two slots and leaving the rest cold.
	cursor int
	// superseded counts frames replaced before anyone asked for them.
	superseded uint64
	// seq numbers published frames from 1.
	seq uint64
}

// newRing builds a ring of n slots. Fewer than [MinQueueDepth] cannot honour
// the borrow contract, so n is raised to it rather than accepted.
func newRing(n int) *ring {
	if n < MinQueueDepth {
		n = MinQueueDepth
	}
	return &ring{n: n, frames: make([]Frame, n), ready: -1, lent: -1}
}

// next picks a slot the capture may write into: any slot that is neither
// borrowed by the consumer nor holding the newest complete frame. With at
// least three slots and at most two excluded there is always one.
func (r *ring) next() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := 0; k < r.n; k++ {
		i := (r.cursor + k) % r.n
		if i != r.ready && i != r.lent {
			r.cursor = (i + 1) % r.n
			return i
		}
	}
	// Unreachable with n >= 3, and a deliberate choice rather than a panic if
	// it ever were: overwriting the newest undelivered frame loses a frame,
	// while overwriting the borrowed one corrupts what the consumer is
	// reading.
	if r.ready >= 0 {
		return r.ready
	}
	return 0
}

// publishSeq assigns the next sequence number. The capture side needs it
// BEFORE the frame is built, because the number is part of the frame.
func (r *ring) publishSeq() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	return r.seq
}

// publish makes slot i the newest complete frame. A frame that was ready and
// never taken is counted as superseded, which is how a consumer discovers it
// is slower than the capture.
func (r *ring) publish(i int, f Frame) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ready >= 0 && !r.delivered {
		r.superseded++
	}
	r.frames[i] = f
	r.ready = i
	r.delivered = false
}

// take hands back the newest complete frame and whether it is newer than the
// one the previous call returned. It allocates nothing: the Frame is a value
// and its Pix field aliases the capture buffer.
//
// Taking a frame moves the borrow: the slot previously lent becomes writable
// again, which is exactly why the returned bytes are only valid until the next
// call.
func (r *ring) take() (Frame, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ready < 0 {
		return Frame{}, false
	}
	if r.delivered && r.lent == r.ready {
		return r.frames[r.ready], false
	}
	r.lent = r.ready
	r.delivered = true
	return r.frames[r.ready], true
}

// peek reports the newest complete frame WITHOUT moving the borrow, for the
// statistics path which must not disturb the consumer's view.
func (r *ring) peek() (Frame, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ready < 0 {
		return Frame{}, false
	}
	return r.frames[r.ready], true
}

// supersededCount is how many frames were replaced before anyone took them.
func (r *ring) supersededCount() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.superseded
}

// reset forgets every published frame, so a stream that lost its buffers (a
// duplication torn down and rebuilt at a new size) cannot hand out a borrow
// into memory that no longer exists.
func (r *ring) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.frames {
		r.frames[i] = Frame{}
	}
	r.ready, r.lent, r.delivered = -1, -1, false
}
