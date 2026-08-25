// Copyright (c) the go-mswin/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

package screencapture

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestPixelFormatString(t *testing.T) {
	if got := FormatBGRA.String(); got != "BGRA" {
		t.Fatalf("FormatBGRA = %q, want BGRA", got)
	}
	// A code with a non-printable byte must not be rendered as garbage text.
	if got := PixelFormat(0x00010203).String(); got != "PixelFormat(0x00010203)" {
		t.Fatalf("unprintable format = %q", got)
	}
	if got := FormatBGRA.BytesPerPixel(); got != 4 {
		t.Fatalf("BytesPerPixel = %d, want 4", got)
	}
	if got := PixelFormat(0).BytesPerPixel(); got != 0 {
		t.Fatalf("unknown BytesPerPixel = %d, want 0", got)
	}
}

func TestBackendString(t *testing.T) {
	for b, want := range map[Backend]string{
		BackendAuto:        "auto",
		BackendDuplication: "duplication",
		BackendGDI:         "gdi",
		Backend(9):         "Backend(9)",
	} {
		if got := b.String(); got != want {
			t.Errorf("Backend(%d) = %q, want %q", uint8(b), got, want)
		}
	}
}

func TestRotationString(t *testing.T) {
	for r, want := range map[Rotation]string{
		RotationUnspecified: "unspecified",
		RotationIdentity:    "0°",
		Rotation90:          "90°",
		Rotation180:         "180°",
		Rotation270:         "270°",
		Rotation(77):        "Rotation(77)",
	} {
		if got := r.String(); got != want {
			t.Errorf("Rotation(%d) = %q, want %q", uint32(r), got, want)
		}
	}
}

func TestRect(t *testing.T) {
	// A monitor to the LEFT of the primary one has a negative origin, and that
	// must survive being printed and tested for emptiness.
	r := Rect{X: -1920, Y: -120, W: 1920, H: 1080}
	if got := r.String(); got != "(-1920,-120)+(1920×1080)" {
		t.Fatalf("Rect.String = %q", got)
	}
	if r.Empty() {
		t.Fatal("a 1920x1080 rectangle reported empty")
	}
	for _, e := range []Rect{{}, {W: 10}, {H: 10}, {W: -1, H: 5}} {
		if !e.Empty() {
			t.Errorf("%s reported non-empty", e)
		}
	}
}

func TestDisplayScaleAndStrings(t *testing.T) {
	d := Display{
		ID: 0x42, DeviceName: `\\.\DISPLAY1`,
		Width: 2560, Height: 1440, PixelWidth: 3840, PixelHeight: 2160,
		DPI: 144, Bounds: Rect{W: 3840, H: 2160}, Primary: true,
		AdapterIndex: 0, OutputIndex: 1,
	}
	if got := d.Scale(); got != 1.5 {
		t.Fatalf("Scale = %v, want 1.5", got)
	}
	if !d.Duplicable() {
		t.Fatal("a display matched to adapter 0 output 1 is not duplicable")
	}
	if (Display{DPI: 0}).Scale() != 1 {
		t.Fatal("a display with no DPI must scale 1")
	}
	if (Display{AdapterIndex: -1, OutputIndex: -1}).Duplicable() {
		t.Fatal("an unmatched display reported duplicable")
	}
	if !strings.Contains(d.String(), `\\.\DISPLAY1`) {
		t.Fatalf("Display.String lost the device name: %s", d)
	}
	w := Window{ID: 0x99, Title: "Untitled", ClassName: "Notepad", AppName: "notepad.exe"}
	if !strings.Contains(w.String(), "Notepad") {
		t.Fatalf("Window.String lost the class: %s", w)
	}
}

func testContent() *Content {
	return &Content{
		Displays: []Display{
			{ID: 1, DeviceName: `\\.\DISPLAY1`, PixelWidth: 1920, PixelHeight: 1080},
			{ID: 2, DeviceName: `\\.\DISPLAY2`, PixelWidth: 2560, PixelHeight: 1440, Primary: true},
		},
		Windows: []Window{
			{ID: 10, Title: "a", ClassName: "C1", PID: 100},
			{ID: 11, Title: "b", ClassName: "C2", PID: 100},
			{ID: 12, Title: "a", ClassName: "C1", PID: 200},
		},
	}
}

func TestContentLookups(t *testing.T) {
	c := testContent()
	if d, err := c.Display(2); err != nil || d.PixelWidth != 2560 {
		t.Fatalf("Display(2) = %v, %v", d, err)
	}
	if _, err := c.Display(99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Display(99) error = %v, want ErrNotFound", err)
	}
	if d, err := c.DisplayByName(`\\.\DISPLAY1`); err != nil || d.ID != 1 {
		t.Fatalf("DisplayByName = %v, %v", d, err)
	}
	if _, err := c.DisplayByName("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DisplayByName error = %v", err)
	}
	if d, err := c.MainDisplay(); err != nil || d.ID != 2 {
		t.Fatalf("MainDisplay = %v, %v", d, err)
	}
	if w, err := c.Window(11); err != nil || w.Title != "b" {
		t.Fatalf("Window(11) = %v, %v", w, err)
	}
	if _, err := c.Window(0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Window(0) error = %v", err)
	}
	if got := c.WindowsByTitle("a"); len(got) != 2 {
		t.Fatalf("WindowsByTitle(a) = %d windows, want 2", len(got))
	}
	if got := c.WindowsByClass("C2"); len(got) != 1 || got[0].ID != 11 {
		t.Fatalf("WindowsByClass(C2) = %v", got)
	}
	if got := c.WindowsOfPID(100); len(got) != 2 {
		t.Fatalf("WindowsOfPID(100) = %d windows, want 2", len(got))
	}
	if got := c.WindowsOfPID(999); got != nil {
		t.Fatalf("WindowsOfPID(999) = %v, want nil", got)
	}
}

func TestContentMainDisplayFallbacks(t *testing.T) {
	empty := &Content{}
	if _, err := empty.MainDisplay(); !errors.Is(err, ErrNoDisplay) {
		t.Fatalf("MainDisplay on an empty content = %v, want ErrNoDisplay", err)
	}
	// No display flagged primary: the first one stands in rather than failing.
	none := &Content{Displays: []Display{{ID: 7}, {ID: 8}}}
	if d, err := none.MainDisplay(); err != nil || d.ID != 7 {
		t.Fatalf("MainDisplay with no primary = %v, %v", d, err)
	}
}

func TestOptionsValidate(t *testing.T) {
	if err := (Options{}).Validate(); err != nil {
		t.Fatalf("the zero Options must be valid, got %v", err)
	}
	full := Options{Width: 100, Height: 50, FPS: 30, QueueDepth: 4,
		Timeout: time.Second, Backend: BackendGDI, ShowsCursor: true, ScalesToFit: true}
	if err := full.Validate(); err != nil {
		t.Fatalf("a fully specified Options must be valid, got %v", err)
	}
	bad := []struct {
		name string
		opt  Options
		want string
	}{
		{"negative width", Options{Width: -1, Height: 1}, "negative size"},
		{"negative height", Options{Width: 1, Height: -1}, "negative size"},
		{"width only", Options{Width: 100}, "both be zero"},
		{"height only", Options{Height: 100}, "both be zero"},
		{"too wide", Options{Width: MaxDimension + 1, Height: 1}, "exceeds the"},
		{"too tall", Options{Width: 1, Height: MaxDimension + 1}, "exceeds the"},
		{"negative fps", Options{FPS: -1}, "negative FPS"},
		{"tiny fps", Options{FPS: 0.001}, "below the 0.01 minimum"},
		{"negative depth", Options{QueueDepth: -1}, "negative QueueDepth"},
		{"shallow depth", Options{QueueDepth: 2}, "below the minimum"},
		{"deep depth", Options{QueueDepth: MaxQueueDepth + 1}, "exceeds the maximum"},
		{"negative timeout", Options{Timeout: -time.Second}, "negative Timeout"},
		{"huge timeout", Options{Timeout: MaxTimeout + 1}, "exceeds the maximum"},
		{"unknown backend", Options{Backend: Backend(4)}, "unknown Backend"},
		{"duplication + cursor", Options{Backend: BackendDuplication, ShowsCursor: true}, "cannot composite the cursor"},
		{"duplication + fit", Options{Backend: BackendDuplication, ScalesToFit: true}, "cannot resample"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opt.Validate()
			if !errors.Is(err, ErrInvalidOption) {
				t.Fatalf("error = %v, want ErrInvalidOption", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestOptionsResolve(t *testing.T) {
	got, err := (Options{}).resolve(1920, 1080)
	if err != nil {
		t.Fatal(err)
	}
	if got.Width != 1920 || got.Height != 1080 {
		t.Fatalf("resolved size = %dx%d, want the native 1920x1080", got.Width, got.Height)
	}
	if got.FPS != DefaultFPS || got.QueueDepth != DefaultQueueDepth || got.Timeout != DefaultTimeout {
		t.Fatalf("defaults not applied: %+v", got)
	}
	// An explicit size is not overwritten by the native one.
	got, err = (Options{Width: 640, Height: 360, FPS: 24, QueueDepth: 5, Timeout: time.Second}).
		resolve(1920, 1080)
	if err != nil {
		t.Fatal(err)
	}
	if got.Width != 640 || got.FPS != 24 || got.QueueDepth != 5 || got.Timeout != time.Second {
		t.Fatalf("explicit values overwritten: %+v", got)
	}
	// An invalid Options fails before anything is filled in.
	if _, err := (Options{QueueDepth: 1}).resolve(100, 100); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("resolve of an invalid Options = %v", err)
	}
	// A source that reports no size and a caller that gave none is the one
	// case that cannot be resolved at all.
	if _, err := (Options{}).resolve(0, 0); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("resolve with no size anywhere = %v", err)
	}
	if _, err := (Options{}).resolve(100, 0); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("resolve with a zero native height = %v", err)
	}
}

func TestFrameInterval(t *testing.T) {
	if got := frameInterval(60); got != 16666667*time.Nanosecond {
		t.Fatalf("frameInterval(60) = %v", got)
	}
	if got := frameInterval(0); got != frameInterval(DefaultFPS) {
		t.Fatalf("frameInterval(0) = %v, want the DefaultFPS interval", got)
	}
	if got := frameInterval(-5); got != frameInterval(DefaultFPS) {
		t.Fatalf("frameInterval(-5) = %v", got)
	}
	// A rate faster than a microsecond clamps rather than rounding to zero and
	// spinning.
	if got := frameInterval(1e12); got != time.Microsecond {
		t.Fatalf("frameInterval(1e12) = %v, want 1µs", got)
	}
}

func TestPickBackend(t *testing.T) {
	native := Options{Width: 1920, Height: 1080}
	cases := []struct {
		name       string
		want       Backend
		opt        Options
		isWindow   bool
		duplicable bool
		expect     Backend
		expectErr  string
	}{
		{name: "auto picks duplication", want: BackendAuto, opt: native, duplicable: true, expect: BackendDuplication},
		{name: "auto falls back for a window", want: BackendAuto, opt: native, isWindow: true, expect: BackendGDI},
		{name: "auto falls back when unmatched", want: BackendAuto, opt: native, expect: BackendGDI},
		{name: "auto falls back for the cursor", want: BackendAuto, duplicable: true,
			opt: Options{Width: 1920, Height: 1080, ShowsCursor: true}, expect: BackendGDI},
		{name: "auto falls back for letterboxing", want: BackendAuto, duplicable: true,
			opt: Options{Width: 1920, Height: 1080, ScalesToFit: true}, expect: BackendGDI},
		{name: "auto falls back for a resize", want: BackendAuto, duplicable: true,
			opt: Options{Width: 960, Height: 540}, expect: BackendGDI},
		{name: "gdi is always honoured", want: BackendGDI, opt: native, duplicable: true, expect: BackendGDI},
		{name: "gdi honoured for a window", want: BackendGDI, opt: native, isWindow: true, expect: BackendGDI},
		{name: "explicit duplication on a window", want: BackendDuplication, opt: native, isWindow: true,
			expectErr: "whole outputs"},
		{name: "explicit duplication unmatched", want: BackendDuplication, opt: native,
			expectErr: "not matched to a DXGI output"},
		{name: "explicit duplication with a cursor", want: BackendDuplication, duplicable: true,
			opt: Options{Width: 1920, Height: 1080, ShowsCursor: true}, expectErr: "does not composite the cursor"},
		{name: "explicit duplication letterboxed", want: BackendDuplication, duplicable: true,
			opt: Options{Width: 1920, Height: 1080, ScalesToFit: true}, expectErr: "cannot resample"},
		{name: "explicit duplication resized", want: BackendDuplication, duplicable: true,
			opt: Options{Width: 960, Height: 540}, expectErr: "not the source's native"},
		{name: "explicit duplication at native size", want: BackendDuplication, opt: native,
			duplicable: true, expect: BackendDuplication},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pickBackend(tc.want, tc.opt, 1920, 1080, tc.isWindow, tc.duplicable)
			if tc.expectErr != "" {
				if !errors.Is(err, ErrInvalidOption) {
					t.Fatalf("error = %v, want ErrInvalidOption", err)
				}
				if !strings.Contains(err.Error(), tc.expectErr) {
					t.Fatalf("error %q does not mention %q", err, tc.expectErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if got != tc.expect {
				t.Fatalf("backend = %s, want %s", got, tc.expect)
			}
		})
	}
}

func TestAlignedStride(t *testing.T) {
	// The whole trap in one assertion: at 32bpp the aligned stride IS width*4,
	// which is why the assumption survives on Windows until a mapped D3D11
	// surface says otherwise.
	if got := AlignedStride(1920, 32); got != 1920*4 {
		t.Fatalf("AlignedStride(1920,32) = %d", got)
	}
	if got := AlignedStride(3, 24); got != 12 {
		t.Fatalf("AlignedStride(3,24) = %d, want 12 (9 bytes padded to a DWORD)", got)
	}
	if got := AlignedStride(0, 32); got != 0 {
		t.Fatalf("AlignedStride(0,32) = %d", got)
	}
	if got := AlignedStride(10, 0); got != 0 {
		t.Fatalf("AlignedStride(10,0) = %d", got)
	}
}

func TestDIBLayoutNormalize(t *testing.T) {
	// Top-down: the negative height this package always asks for.
	w, h, stride, bottomUp, err := DIBLayout{Width: 4, Height: -3, BitCount: 32}.Normalize()
	if err != nil || w != 4 || h != 3 || stride != 16 || bottomUp {
		t.Fatalf("top-down normalize = %d,%d,%d,%v,%v", w, h, stride, bottomUp, err)
	}
	// Bottom-up: the DEFAULT a Windows DIB has, and the one that must be
	// flagged rather than handed on.
	_, h, _, bottomUp, err = DIBLayout{Width: 4, Height: 3, BitCount: 32}.Normalize()
	if err != nil || h != 3 || !bottomUp {
		t.Fatalf("bottom-up normalize = %d,%v,%v", h, bottomUp, err)
	}
	// An explicit stride wider than the row — a mapped D3D11 staging texture
	// padded to 256 bytes — is kept, not recomputed.
	_, _, stride, _, err = DIBLayout{Width: 50, Height: -2, Stride: 256, BitCount: 32}.Normalize()
	if err != nil || stride != 256 {
		t.Fatalf("padded stride = %d, %v", stride, err)
	}

	bad := []struct {
		name string
		l    DIBLayout
		want string
	}{
		{"24bpp", DIBLayout{Width: 4, Height: -3, BitCount: 24}, "not supported"},
		{"zero depth", DIBLayout{Width: 4, Height: -3}, "not supported"},
		{"zero width", DIBLayout{Height: -3, BitCount: 32}, "non-positive DIB width"},
		{"negative width", DIBLayout{Width: -4, Height: -3, BitCount: 32}, "non-positive DIB width"},
		{"zero height", DIBLayout{Width: 4, BitCount: 32}, "zero DIB height"},
		{"huge width", DIBLayout{Width: MaxDimension + 1, Height: -1, BitCount: 32}, "exceeds the"},
		{"huge height", DIBLayout{Width: 1, Height: MaxDimension + 1, BitCount: 32}, "exceeds the"},
		{"huge negative height", DIBLayout{Width: 1, Height: -MaxDimension - 1, BitCount: 32}, "exceeds the"},
		{"narrow stride", DIBLayout{Width: 10, Height: -2, Stride: 20, BitCount: 32}, "narrower than"},
		{"unaligned stride", DIBLayout{Width: 10, Height: -2, Stride: 41, BitCount: 32}, "not a multiple of 4"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, err := tc.l.Normalize()
			if !errors.Is(err, ErrInvalidOption) {
				t.Fatalf("error = %v, want ErrInvalidOption", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestDIBLayoutSize(t *testing.T) {
	n, err := DIBLayout{Width: 10, Height: -4, BitCount: 32}.Size()
	if err != nil || n != 160 {
		t.Fatalf("Size = %d, %v", n, err)
	}
	if _, err := (DIBLayout{}).Size(); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("Size of a zero layout = %v", err)
	}
}

// gradient builds a buffer whose every row is filled with the row index, so a
// vertical flip is visible in a single byte per row.
func gradient(width, height, stride int) []byte {
	b := make([]byte, height*stride)
	for y := 0; y < height; y++ {
		for x := 0; x < stride; x++ {
			b[y*stride+x] = byte(y + 1)
		}
	}
	return b
}

func TestDIBLayoutFrameTopDown(t *testing.T) {
	l := DIBLayout{Width: 2, Height: -3, BitCount: 32}
	pix := gradient(2, 3, 8)
	at := time.Now()
	f, err := l.Frame(pix, 7, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.Width != 2 || f.Height != 3 || f.Stride != 8 || f.Seq != 7 || !f.At.Equal(at) {
		t.Fatalf("frame = %+v", f)
	}
	// Untouched: a top-down layout must not be flipped.
	if f.Row(0)[0] != 1 || f.Row(2)[0] != 3 {
		t.Fatalf("a top-down frame was flipped: rows %d..%d", f.Row(0)[0], f.Row(2)[0])
	}
	// The frame borrows: writing through Pix is visible in the source buffer.
	f.Pix[0] = 0xAA
	if pix[0] != 0xAA {
		t.Fatal("Frame.Pix is a copy, not a borrow")
	}
}

func TestDIBLayoutFrameBottomUpIsFlipped(t *testing.T) {
	// The defect this exists to stop: a bottom-up DIB handed straight to a
	// compositor draws the desktop upside down.
	l := DIBLayout{Width: 2, Height: 3, BitCount: 32}
	pix := gradient(2, 3, 8)
	f, err := l.Frame(pix, 1, time.Now(), make([]byte, 8))
	if err != nil {
		t.Fatal(err)
	}
	if f.Height != 3 {
		t.Fatalf("height = %d, want the absolute 3", f.Height)
	}
	if f.Row(0)[0] != 3 || f.Row(1)[0] != 2 || f.Row(2)[0] != 1 {
		t.Fatalf("rows after the flip = %d,%d,%d, want 3,2,1",
			f.Row(0)[0], f.Row(1)[0], f.Row(2)[0])
	}
}

func TestDIBLayoutFrameErrors(t *testing.T) {
	if _, err := (DIBLayout{}).Frame(nil, 0, time.Now(), nil); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("Frame of a zero layout = %v", err)
	}
	l := DIBLayout{Width: 4, Height: -4, BitCount: 32}
	_, err := l.Frame(make([]byte, 10), 0, time.Now(), nil)
	if !errors.Is(err, ErrShortBuffer) {
		t.Fatalf("Frame with a short buffer = %v, want ErrShortBuffer", err)
	}
	if !strings.Contains(err.Error(), "needs 64 bytes, got 10") {
		t.Fatalf("the short-buffer error does not say the sizes: %v", err)
	}
	// An over-long buffer is trimmed, not rejected: a ring slot is allowed to
	// be bigger than the frame it currently holds.
	f, err := l.Frame(make([]byte, 999), 0, time.Now(), nil)
	if err != nil || len(f.Pix) != 64 {
		t.Fatalf("Frame with a long buffer = %d bytes, %v", len(f.Pix), err)
	}
}

func TestFlipRows(t *testing.T) {
	// Nothing to do: fewer than two rows, or a nonsensical stride.
	one := []byte{1, 2, 3, 4}
	flipRows(one, 1, 4, nil)
	if one[0] != 1 {
		t.Fatal("a one-row image was modified")
	}
	flipRows(one, 2, 0, nil)
	if one[0] != 1 {
		t.Fatal("a zero-stride image was modified")
	}
	// An odd row count leaves the middle row alone.
	b := gradient(1, 5, 4)
	flipRows(b, 5, 4, nil)
	for i, want := range []byte{5, 4, 3, 2, 1} {
		if b[i*4] != want {
			t.Fatalf("row %d = %d, want %d", i, b[i*4], want)
		}
	}
	// A scratch buffer that is too short makes the flip allocate one, and the
	// result must be identical.
	c := gradient(1, 4, 4)
	flipRows(c, 4, 4, make([]byte, 1))
	for i, want := range []byte{4, 3, 2, 1} {
		if c[i*4] != want {
			t.Fatalf("short-scratch row %d = %d, want %d", i, c[i*4], want)
		}
	}
}

// paddedFrame builds a frame with a stride WIDER than width*4, which is what a
// mapped D3D11 staging texture looks like, and fills the padding with a
// poison value so any code that ignores stride is caught.
func paddedFrame(width, height, stride int) Frame {
	pix := make([]byte, height*stride)
	for i := range pix {
		pix[i] = 0xEE // poison
	}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			o := y*stride + x*4
			pix[o+0] = byte(x)     // B
			pix[o+1] = byte(y)     // G
			pix[o+2] = byte(x + y) // R
			pix[o+3] = byte(x ^ y) // A
		}
	}
	return Frame{Pix: pix, Width: width, Height: height, Stride: stride, Seq: 1, At: time.Now()}
}

func TestFrameValidAndRow(t *testing.T) {
	f := paddedFrame(4, 3, 32)
	if !f.Valid() {
		t.Fatal("a well-formed padded frame reported invalid")
	}
	if f.TightLen() != 48 {
		t.Fatalf("TightLen = %d, want 48", f.TightLen())
	}
	row := f.Row(1)
	if len(row) != 16 {
		t.Fatalf("Row length = %d, want the trimmed 16", len(row))
	}
	if row[1] != 1 {
		t.Fatalf("Row(1) is not row 1: %v", row[:4])
	}
	// Row must not expose the padding even by appending.
	if cap(row) != 16 {
		t.Fatalf("Row capacity = %d, want 16 so an append cannot reach the padding", cap(row))
	}
	for _, y := range []int{-1, 3, 99} {
		if f.Row(y) != nil {
			t.Fatalf("Row(%d) returned pixels", y)
		}
	}
	if (Frame{}).Row(0) != nil {
		t.Fatal("Row on an invalid frame returned pixels")
	}
	bad := []Frame{
		{},
		{Width: 1, Height: 0, Stride: 4, Pix: make([]byte, 4)},
		{Width: 0, Height: 1, Stride: 4, Pix: make([]byte, 4)},
		{Width: 4, Height: 1, Stride: 8, Pix: make([]byte, 4)},  // too few bytes
		{Width: 4, Height: 1, Stride: 4, Pix: make([]byte, 16)}, // stride < width*4
	}
	bad[4].Stride = 8
	bad[4].Width = 4
	bad = append(bad, Frame{Width: 4, Height: 1, Stride: 2, Pix: make([]byte, 16)})
	for i, f := range bad {
		if i == 4 {
			continue
		}
		if f.Valid() {
			t.Errorf("frame %d reported valid: %+v", i, f)
		}
	}
}

func TestFrameCopyTight(t *testing.T) {
	f := paddedFrame(4, 3, 32)
	dst := make([]byte, f.TightLen())
	n, err := f.CopyTight(dst)
	if err != nil || n != 48 {
		t.Fatalf("CopyTight = %d, %v", n, err)
	}
	for i, b := range dst {
		if b == 0xEE {
			t.Fatalf("CopyTight copied the row padding at byte %d", i)
		}
	}
	// The unpadded fast path.
	g := paddedFrame(4, 3, 16)
	n, err = g.CopyTight(dst)
	if err != nil || n != 48 {
		t.Fatalf("unpadded CopyTight = %d, %v", n, err)
	}
	if _, err := f.CopyTight(make([]byte, 10)); !errors.Is(err, ErrShortBuffer) {
		t.Fatalf("short CopyTight = %v", err)
	}
	if _, err := (Frame{}).CopyTight(dst); !errors.Is(err, ErrNoFrame) {
		t.Fatalf("CopyTight of an invalid frame = %v", err)
	}
}

func TestFrameNRGBA(t *testing.T) {
	f := paddedFrame(2, 2, 32)
	img, err := f.NRGBA()
	if err != nil {
		t.Fatal(err)
	}
	// (1,0) is B=1 G=0 R=1 A=1 in the source, so RGBA must be 1,0,1,1.
	got := img.Pix[0*img.Stride+1*4:]
	if got[0] != 1 || got[1] != 0 || got[2] != 1 || got[3] != 1 {
		t.Fatalf("BGRA->RGBA at (1,0) = %v, want [1 0 1 1]", got[:4])
	}
	// The honest conversion carries alpha through; a GDI capture leaves it at
	// zero, which is why NRGBAOpaque exists.
	op, err := f.NRGBAOpaque()
	if err != nil {
		t.Fatal(err)
	}
	for i := 3; i < len(op.Pix); i += 4 {
		if op.Pix[i] != 0xff {
			t.Fatalf("NRGBAOpaque left alpha at %d", op.Pix[i])
		}
	}
	if _, err := (Frame{}).NRGBA(); !errors.Is(err, ErrNoFrame) {
		t.Fatalf("NRGBA of an invalid frame = %v", err)
	}
	if _, err := (Frame{}).NRGBAOpaque(); !errors.Is(err, ErrNoFrame) {
		t.Fatalf("NRGBAOpaque of an invalid frame = %v", err)
	}
}

func TestFrameUniformAndDiffers(t *testing.T) {
	// The classic silent failure: an untouched buffer is uniformly zero.
	blank := Frame{Pix: make([]byte, 4*2*8), Width: 2, Height: 8, Stride: 8}
	px, uniform := blank.Uniform()
	if !uniform || px != [4]byte{} {
		t.Fatalf("a zero buffer = %v, uniform %v", px, uniform)
	}
	f := paddedFrame(4, 3, 32)
	if _, uniform := f.Uniform(); uniform {
		t.Fatal("a gradient reported uniform")
	}
	if _, uniform := (Frame{}).Uniform(); uniform {
		t.Fatal("an invalid frame reported uniform")
	}
	// A frame that differs only in the LAST pixel must still be caught.
	g := paddedFrame(4, 3, 32)
	if n, ok := f.Differs(g); !ok || n != 0 {
		t.Fatalf("two identical frames differ in %d pixels (ok=%v)", n, ok)
	}
	g.Pix[2*32+3*4] ^= 0xff
	n, ok := f.Differs(g)
	if !ok || n != 1 {
		t.Fatalf("one changed pixel = %d (ok=%v)", n, ok)
	}
	// Padding differences must NOT count: the two backends pad differently.
	h := paddedFrame(4, 3, 48)
	if n, ok := f.Differs(h); !ok || n != 0 {
		t.Fatalf("frames of different stride but the same pixels differ in %d (ok=%v)", n, ok)
	}
	if _, ok := f.Differs(paddedFrame(5, 3, 32)); ok {
		t.Fatal("frames of different width reported comparable")
	}
	if _, ok := f.Differs(Frame{}); ok {
		t.Fatal("an invalid frame reported comparable")
	}
	if _, ok := (Frame{}).Differs(f); ok {
		t.Fatal("an invalid receiver reported comparable")
	}
}

func TestStats(t *testing.T) {
	var zero Stats
	if zero.FPS() != 0 || zero.MeanCapture() != 0 {
		t.Fatal("the zero Stats must report zero rates")
	}
	if zero.MeanWait() != 0 {
		t.Fatal("the zero Stats must report no wait")
	}
	s := Stats{Frames: 4, Interval: 20 * time.Millisecond,
		CaptureTotal: 40 * time.Millisecond, WaitTotal: 200 * time.Millisecond}
	if got := s.FPS(); got != 50 {
		t.Fatalf("FPS = %v, want 50", got)
	}
	if got := s.MeanCapture(); got != 10*time.Millisecond {
		t.Fatalf("MeanCapture = %v, want 10ms", got)
	}
	// The read-back cost and the time the desktop took to change are separate
	// numbers on purpose: conflating them makes an idle change-driven backend
	// look ruinously expensive when it did almost no work.
	if got := s.MeanWait(); got != 50*time.Millisecond {
		t.Fatalf("MeanWait = %v, want 50ms", got)
	}
}

func TestHRESULT(t *testing.T) {
	if sOK.Failed() {
		t.Fatal("S_OK reported as a failure")
	}
	if sFalse.Failed() {
		t.Fatal("S_FALSE reported as a failure")
	}
	if !dxgiErrorUnsupported.Failed() {
		t.Fatal("DXGI_ERROR_UNSUPPORTED reported as a success")
	}
	if got := dxgiErrorAccessLost.Name(); got != "DXGI_ERROR_ACCESS_LOST" {
		t.Fatalf("Name = %q", got)
	}
	if got := dxgiErrorAccessLost.String(); got != "DXGI_ERROR_ACCESS_LOST (0x887a0026)" {
		t.Fatalf("String = %q", got)
	}
	if got := HRESULT(0x8000BEEF).String(); got != "0x8000beef" {
		t.Fatalf("unknown String = %q", got)
	}
	if got := HRESULT(0x8000BEEF).Name(); got != "" {
		t.Fatalf("unknown Name = %q", got)
	}
	// Every named code must be reachable through the table, or the table and
	// the constants have drifted apart.
	for code, name := range hresultNames {
		if code.Name() != name {
			t.Errorf("%#08x names %q, table says %q", uint32(code), code.Name(), name)
		}
	}
}

func TestCOMError(t *testing.T) {
	e := &COMError{HR: dxgiErrorUnsupported, Op: "IDXGIOutput1::DuplicateOutput", Detail: "adapter Foo"}
	msg := e.Error()
	for _, want := range []string{"DuplicateOutput", "DXGI_ERROR_UNSUPPORTED", "adapter Foo", "BackendGDI works there"} {
		if !strings.Contains(msg, want) {
			t.Errorf("%q does not mention %q", msg, want)
		}
	}
	if !errors.Is(e, ErrBackendUnavailable) {
		t.Fatal("DXGI_ERROR_UNSUPPORTED does not unwrap to ErrBackendUnavailable")
	}
	// An error with no operation and no detail still reads.
	bare := (&COMError{HR: eFail}).Error()
	if !strings.Contains(bare, "COM") || !strings.Contains(bare, "E_FAIL") {
		t.Fatalf("bare COMError = %q", bare)
	}
	if !strings.Contains((&COMError{HR: dxgiErrorNotCurrentlyAvail}).Error(), "already holds the duplication") {
		t.Fatal("DXGI_ERROR_NOT_CURRENTLY_AVAILABLE does not explain itself")
	}
	if !strings.Contains((&COMError{HR: dxgiErrorAccessLost}).Error(), "recoverable") {
		t.Fatal("DXGI_ERROR_ACCESS_LOST does not say it is recoverable")
	}
	if !strings.Contains((&COMError{HR: dxgiErrorSessionDisconnected}).Error(), "recoverable") {
		t.Fatal("DXGI_ERROR_SESSION_DISCONNECTED does not say it is recoverable")
	}

	for _, tc := range []struct {
		code HRESULT
		want error
	}{
		{eAccessDenied, ErrPermissionDenied},
		{dxgiErrorAccessDenied, ErrPermissionDenied},
		{dxgiErrorUnsupported, ErrBackendUnavailable},
		{dxgiErrorNotCurrentlyAvail, ErrBackendUnavailable},
		{eNotImpl, ErrBackendUnavailable},
		{dxgiErrorAccessLost, ErrAccessLost},
		{dxgiErrorSessionDisconnected, ErrAccessLost},
		{dxgiErrorDeviceRemoved, ErrAccessLost},
		{dxgiErrorDeviceReset, ErrAccessLost},
		{dxgiErrorDeviceHung, ErrAccessLost},
		{dxgiErrorWaitTimeout, ErrNoFrame},
		{dxgiErrorNotFound, ErrNotFound},
	} {
		if !errors.Is(&COMError{HR: tc.code}, tc.want) {
			t.Errorf("%s does not unwrap to %v", tc.code, tc.want)
		}
	}
	// A code with no sentinel unwraps to nothing rather than to something
	// misleading.
	if got := (&COMError{HR: eInvalidArg}).Unwrap(); got != nil {
		t.Fatalf("E_INVALIDARG unwrapped to %v", got)
	}
}

func TestHRHelper(t *testing.T) {
	if err := hr("op", sOK); err != nil {
		t.Fatalf("hr of S_OK = %v, want nil", err)
	}
	if err := hr("op", sFalse); err != nil {
		t.Fatalf("hr of S_FALSE = %v, want nil", err)
	}
	err := hr("IDXGIOutput::GetDesc", eFail)
	if err == nil || !strings.Contains(err.Error(), "GetDesc") {
		t.Fatalf("hr = %v", err)
	}
	var ce *COMError
	if !errors.As(err, &ce) || ce.Detail != "" {
		t.Fatalf("hr built %+v", ce)
	}
	err = hr("op", eFail, "because")
	if !errors.As(err, &ce) || ce.Detail != "because" {
		t.Fatalf("hr with a detail built %+v", ce)
	}
}

func TestSentinelsAreDistinct(t *testing.T) {
	all := []error{
		ErrUnsupported, ErrPermissionDenied, ErrNoDisplay, ErrNotFound, ErrClosed,
		ErrNoFrame, ErrInvalidOption, ErrShortBuffer, ErrBackendUnavailable, ErrAccessLost,
	}
	for i, a := range all {
		if !strings.HasPrefix(a.Error(), "screencapture: ") {
			t.Errorf("%v does not carry the package prefix", a)
		}
		for j, b := range all {
			if i != j && errors.Is(a, b) {
				t.Errorf("sentinel %d matches sentinel %d", i, j)
			}
		}
	}
}

// Frame must not allocate. The consumer's whole budget is 16.6 ms and it calls
// this every frame; an allocation here is a garbage-collection pause in a
// compositor.
func TestRingTakeDoesNotAllocate(t *testing.T) {
	r := newRing(3)
	f := paddedFrame(64, 64, 256)
	r.publish(0, f)
	if n := testing.AllocsPerRun(100, func() {
		r.take()
		r.publish(1, f)
		r.take()
		r.publish(0, f)
	}); n != 0 {
		t.Fatalf("the ring hand-off allocates %v times per round", n)
	}
}

func ExampleFrame_Row() {
	f := paddedFrame(2, 2, 64) // stride 64, not 8: the padding is real
	row := f.Row(1)
	fmt.Println(len(row), f.Stride, f.TightLen())
	// Output: 8 64 16
}
