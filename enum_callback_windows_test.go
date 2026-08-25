// Copyright (c) the go-mswin/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build windows

package screencapture

import (
	"context"
	"testing"
)

// The regression that made this package's migration onto go-mswin/win32 a bug
// fix rather than a tidy-up.
//
// Displays() and Windows() each used to build a windows.NewCallback INSIDE
// themselves. The Go runtime allocates those out of a pool capped at 2000 for
// the whole process (runtime.cb_max) and never frees one; going past the cap
// is not a recoverable panic but runtime.throw("too many callback functions"),
// which kills the process outright. Shareable() spends TWO of the 2000 per
// call, so it died at about 1000.
//
// It was measured, not reasoned about: the released v0.1.1, asked Windows() in
// a loop on the Win11 ARM64 VM, printed "survived 1750 calls" and then died
// with exactly that fatal error, exit code 2.
//
// win32 keeps ONE trampoline per enumeration for the whole process, so there
// is no longer any number of calls that does this. 1500 Shareable() calls is
// 3000 under the old accounting, half again past the cap.
//
// With the defect present this test does not FAIL — the test binary dies
// partway through, with no error to inspect and no stack anything can catch.
// Surviving is the proof. It needs no interactive session, which is why it is
// not behind the integration tag: it runs on every Windows CI lane, on both
// architectures.
func TestEnumerationSurvivesPastTheCallbackCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: the callback-ceiling walk is the slow kind of cheap")
	}
	const calls = 1500 // 2 enumerations each = 3000, vs a cap of 2000
	ctx := context.Background()
	for i := range calls {
		if _, err := Shareable(ctx); err != nil {
			t.Fatalf("Shareable call %d: %v", i, err)
		}
	}
	t.Logf("CALLBACK_CEILING_OK %d Shareable() calls = %d enumerations, cap is 2000",
		calls, calls*2)
}
