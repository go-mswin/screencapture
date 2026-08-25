// Copyright (c) the go-mswin/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-mswin/screencapture"
)

func TestBackendOf(t *testing.T) {
	for in, want := range map[string]screencapture.Backend{
		"":            screencapture.BackendAuto,
		"auto":        screencapture.BackendAuto,
		"AUTO":        screencapture.BackendAuto,
		"dup":         screencapture.BackendDuplication,
		"duplication": screencapture.BackendDuplication,
		"gdi":         screencapture.BackendGDI,
		"GDI":         screencapture.BackendGDI,
	} {
		got, err := backendOf(in)
		if err != nil || got != want {
			t.Errorf("backendOf(%q) = %s, %v", in, got, err)
		}
	}
	if _, err := backendOf("vulkan"); err == nil {
		t.Fatal("an unknown backend was accepted")
	}
}

func TestRunRejectsBadFlags(t *testing.T) {
	var buf bytes.Buffer
	if got := run([]string{"-nonsense"}, &buf); got != 2 {
		t.Fatalf("exit = %d, want 2", got)
	}
	if !strings.Contains(buf.String(), "nonsense") {
		t.Fatalf("the flag error was not reported: %q", buf.String())
	}
}

func TestRunListWritesAReport(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	// The status depends on the platform: off Windows every entry point
	// reports ErrUnsupported and the run fails, which is the correct answer.
	// What is asserted here is that the report is produced either way, because
	// a run in Windows' interactive session has no console and the file is the
	// only thing anybody sees.
	run([]string{"-list", "-out", dir}, &buf)
	body, err := os.ReadFile(filepath.Join(dir, "wsccheck.txt"))
	if err != nil {
		t.Fatalf("no report written: %v", err)
	}
	for _, want := range []string{"wsccheck", "Available=", "checks:"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the report does not contain %q:\n%s", want, body)
		}
	}
	if !strings.Contains(buf.String(), "wsccheck") {
		t.Error("nothing was written to the console as well as the file")
	}
}

func TestRunRefusesAnUnwritableOutDir(t *testing.T) {
	var buf bytes.Buffer
	// A path whose parent is a FILE cannot be created as a directory on any
	// platform.
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run([]string{"-list", "-out", filepath.Join(f, "sub")}, &buf); got != 1 {
		t.Fatalf("exit = %d, want 1", got)
	}
}

func TestReporterCounts(t *testing.T) {
	var buf bytes.Buffer
	r := &reporter{w: &buf}
	r.check(true, "yes")
	r.check(false, "no %d", 7)
	if r.passed != 1 || r.failed != 1 {
		t.Fatalf("passed=%d failed=%d", r.passed, r.failed)
	}
	r.summary(1)
	s := buf.String()
	if !strings.Contains(s, "PASS  yes") || !strings.Contains(s, "FAIL  no 7") {
		t.Fatalf("report = %q", s)
	}
	if !strings.Contains(s, "FAILED") {
		t.Fatalf("a failing run did not say so: %q", s)
	}
}

// allocSink is package-level so the allocation below cannot stay on the stack.
var allocSink []byte

func TestAllocsPerCall(t *testing.T) {
	if got := allocsPerCall(func() {}); got != 0 {
		t.Fatalf("an empty function allocates %v times", got)
	}
	// The allocation has to ESCAPE, or the compiler stack-allocates it and the
	// measurement — correctly — reports zero.
	if got := allocsPerCall(func() { allocSink = make([]byte, 4096) }); got < 0.9 {
		t.Fatalf("a function that allocates measured %v", got)
	}
}

func TestWritePNGRejectsAnInvalidFrame(t *testing.T) {
	if err := writePNG(filepath.Join(t.TempDir(), "x.png"), screencapture.Frame{}); err == nil {
		t.Fatal("an invalid frame was written")
	}
}

func TestWritePNG(t *testing.T) {
	f := screencapture.Frame{Pix: make([]byte, 4*2*2), Width: 2, Height: 2, Stride: 8}
	path := filepath.Join(t.TempDir(), "x.png")
	if err := writePNG(path, f); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Size() == 0 {
		t.Fatalf("stat = %v, %v", st, err)
	}
}
