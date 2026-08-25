// Copyright (c) the go-mswin/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build !windows

package screencapture

import (
	"context"
	"errors"
	"testing"
)

// Off Windows every entry point must answer ErrUnsupported rather than fail to
// build: a consumer cross-compiles this package without thinking about it.
func TestUnsupportedPlatform(t *testing.T) {
	ctx := context.Background()
	if Available() || Authorized() || RequestAuthorization() {
		t.Fatal("a non-Windows build claimed it can capture")
	}
	for name, fn := range map[string]func() error{
		"Shareable":               func() error { _, err := Shareable(ctx); return err },
		"CurrentProcessShareable": func() error { _, err := CurrentProcessShareable(ctx); return err },
		"Displays":                func() error { _, err := Displays(ctx); return err },
		"Windows":                 func() error { _, err := Windows(ctx); return err },
		"CaptureDisplay": func() error {
			_, err := CaptureDisplay(ctx, Display{PixelWidth: 100, PixelHeight: 100}, Options{})
			return err
		},
		"CaptureWindow": func() error {
			_, err := CaptureWindow(ctx, Window{Bounds: Rect{W: 10, H: 10}}, Options{})
			return err
		},
	} {
		if err := fn(); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s = %v, want ErrUnsupported", name, err)
		}
	}
}

// The option check must happen on EVERY platform, so a consumer's option bug
// surfaces on the developer's Mac rather than only on the one machine that can
// run the capture.
func TestOptionsAreCheckedEverywhere(t *testing.T) {
	ctx := context.Background()
	d := Display{PixelWidth: 1920, PixelHeight: 1080, AdapterIndex: 0, OutputIndex: 0}
	w := Window{Bounds: Rect{W: 800, H: 600}}

	if _, err := CaptureDisplay(ctx, d, Options{QueueDepth: 1}); !errors.Is(err, ErrInvalidOption) {
		t.Errorf("CaptureDisplay with a shallow queue = %v", err)
	}
	if _, err := CaptureWindow(ctx, w, Options{QueueDepth: 1}); !errors.Is(err, ErrInvalidOption) {
		t.Errorf("CaptureWindow with a shallow queue = %v", err)
	}
	// A display that reports no size cannot be resolved anywhere.
	if _, err := CaptureDisplay(ctx, Display{}, Options{}); !errors.Is(err, ErrInvalidOption) {
		t.Errorf("CaptureDisplay of a sizeless display = %v", err)
	}
	if _, err := CaptureWindow(ctx, Window{}, Options{}); !errors.Is(err, ErrInvalidOption) {
		t.Errorf("CaptureWindow of a sizeless window = %v", err)
	}
	// Backend=duplication at a size that is not the display's native one is
	// refused identically here and on Windows.
	if _, err := CaptureDisplay(ctx, d, Options{
		Backend: BackendDuplication, Width: 960, Height: 540,
	}); !errors.Is(err, ErrInvalidOption) {
		t.Errorf("CaptureDisplay(duplication, resized) = %v", err)
	}
	// Duplication of a window is impossible on every platform.
	if _, err := CaptureWindow(ctx, w, Options{Backend: BackendDuplication}); !errors.Is(err, ErrInvalidOption) {
		t.Errorf("CaptureWindow(duplication) = %v", err)
	}
}

// The stand-in Stream exists so consumer code compiles unchanged.
func TestStubStream(t *testing.T) {
	s := &Stream{opt: Options{Width: 4}, backend: BackendGDI, source: "nothing", note: ""}
	if s.Options().Width != 4 || s.Backend() != BackendGDI || s.Source() != "nothing" {
		t.Fatalf("stub accessors = %+v", s)
	}
	if s.Path() != "" || s.Note() != "" {
		t.Fatal("the stub claimed a read-back path")
	}
	if f, fresh := s.Frame(); fresh || f.Valid() {
		t.Fatal("the stub produced a frame")
	}
	if _, err := s.WaitFrame(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatal("WaitFrame did not report ErrUnsupported")
	}
	if (s.Stats() != Stats{}) {
		t.Fatal("the stub reported statistics")
	}
	if s.Err() != nil || s.Close() != nil || s.Close() != nil {
		t.Fatal("the stub's Err/Close must be nil and idempotent")
	}
}
