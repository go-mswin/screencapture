// Copyright (c) the go-mswin/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build !windows

package screencapture

import "context"

// On every platform that is not Windows there is no GDI and no DXGI, so the
// entry points below report [ErrUnsupported] rather than failing to build. A
// consumer cross-compiles this package without having to think about it, and
// gets one clear error at run time instead of a link failure at build time.
//
// Everything portable — the option validation, the backend choice, the stride
// and bottom-up normalisation, the BGRA-to-RGBA conversion, the HRESULT
// mapping and the whole borrow ring — lives in screencapture.go, structs.go
// and ring.go and is fully exercised here, which is what lets the Linux and
// macOS lanes in CI cover it.

// Available reports false: GDI and DXGI exist only on Windows.
func Available() bool { return false }

// Authorized reports false. On Windows there is no permission gate at all and
// this reports [Available]; off Windows there is nothing to be authorized for.
func Authorized() bool { return false }

// RequestAuthorization reports false and prompts nothing.
func RequestAuthorization() bool { return false }

// Shareable reports [ErrUnsupported].
func Shareable(ctx context.Context) (*Content, error) { return nil, ErrUnsupported }

// CurrentProcessShareable reports [ErrUnsupported].
func CurrentProcessShareable(ctx context.Context) (*Content, error) { return nil, ErrUnsupported }

// Displays reports [ErrUnsupported].
func Displays(ctx context.Context) ([]Display, error) { return nil, ErrUnsupported }

// Windows reports [ErrUnsupported].
func Windows(ctx context.Context) ([]Window, error) { return nil, ErrUnsupported }

// CaptureDisplay reports [ErrUnsupported]. It still validates the options and
// still resolves the backend first, so a consumer's option bug — an
// out-of-range queue depth, or asking Desktop Duplication to draw the cursor —
// surfaces identically on every platform instead of only on the one machine
// that can run the capture.
func CaptureDisplay(ctx context.Context, d Display, opt Options) (*Stream, error) {
	if err := opt.Validate(); err != nil {
		return nil, err
	}
	res, err := opt.resolve(d.PixelWidth, d.PixelHeight)
	if err != nil {
		return nil, err
	}
	if _, err := pickBackend(opt.Backend, res, d.PixelWidth, d.PixelHeight, false, d.Duplicable()); err != nil {
		return nil, err
	}
	return nil, ErrUnsupported
}

// CaptureWindow reports [ErrUnsupported], after the same option validation and
// backend resolution as [CaptureDisplay].
func CaptureWindow(ctx context.Context, w Window, opt Options) (*Stream, error) {
	if err := opt.Validate(); err != nil {
		return nil, err
	}
	res, err := opt.resolve(w.Bounds.W, w.Bounds.H)
	if err != nil {
		return nil, err
	}
	if _, err := pickBackend(opt.Backend, res, w.Bounds.W, w.Bounds.H, true, false); err != nil {
		return nil, err
	}
	return nil, ErrUnsupported
}

// Stream is the non-Windows stand-in for a live capture. It can never be
// created here — [CaptureDisplay] and [CaptureWindow] always fail — but the
// type and its methods exist so consumer code compiles unchanged.
type Stream struct {
	opt     Options
	backend Backend
	source  string
	note    string
}

// Options returns the stream's resolved options.
func (s *Stream) Options() Options { return s.opt }

// Backend returns the capture route that would have run.
func (s *Stream) Backend() Backend { return s.backend }

// Source names what would be captured.
func (s *Stream) Source() string { return s.source }

// Path reports "" : there is no read-back route off Windows.
func (s *Stream) Path() string { return "" }

// Note reports the stream's explanatory note, always empty here.
func (s *Stream) Note() string { return s.note }

// Frame reports the zero frame and false.
func (s *Stream) Frame() (Frame, bool) { return Frame{}, false }

// WaitFrame reports [ErrUnsupported].
func (s *Stream) WaitFrame(ctx context.Context) (Frame, error) { return Frame{}, ErrUnsupported }

// Stats reports the zero statistics.
func (s *Stream) Stats() Stats { return Stats{} }

// Err reports nil.
func (s *Stream) Err() error { return nil }

// Close reports nil and is idempotent.
func (s *Stream) Close() error { return nil }
