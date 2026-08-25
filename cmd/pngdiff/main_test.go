// Copyright (c) the go-mswin/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePNG makes a test image; fn decides each pixel.
func writePNG(t *testing.T, name string, w, h int, fn func(x, y int) color.NRGBA) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, fn(x, y))
		}
	}
	path := filepath.Join(t.TempDir(), name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	return path
}

func gradient(x, y int) color.NRGBA {
	return color.NRGBA{R: uint8(x), G: uint8(y), B: uint8(x ^ y), A: 255}
}

func TestIdenticalImagesExitZero(t *testing.T) {
	a := writePNG(t, "a.png", 8, 8, gradient)
	b := writePNG(t, "b.png", 8, 8, gradient)
	var out, errOut bytes.Buffer
	if got := run([]string{a, b}, &out, &errOut); got != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", got, errOut.String())
	}
	if !strings.Contains(out.String(), "0 of 64 pixels differ") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestOneChangedPixelIsFound(t *testing.T) {
	a := writePNG(t, "a.png", 8, 8, gradient)
	b := writePNG(t, "b.png", 8, 8, func(x, y int) color.NRGBA {
		c := gradient(x, y)
		if x == 3 && y == 5 {
			c.R ^= 0xff
		}
		return c
	})
	var out, errOut bytes.Buffer
	if got := run([]string{b, a}, &out, &errOut); got != 1 {
		t.Fatalf("exit = %d, want 1", got)
	}
	if !strings.Contains(out.String(), "1 of 64 pixels differ") {
		t.Fatalf("output = %q", out.String())
	}
	// The deviation must be reported on the 0..255 scale a reader expects, not
	// on the 16-bit one image.Image.RGBA returns. Pixel (3,5) has R=3 in a and
	// 3^0xff=252 in b, so the deviation is 249.
	if !strings.Contains(out.String(), "max deviation 249") {
		t.Fatalf("deviation not on the 0..255 scale: %q", out.String())
	}
}

// Alpha is ignored by default on purpose: the two artefacts come from
// different producers (a BGRA frame and a PPM) whose alpha conventions differ,
// and comparing it would report a difference that is about file formats rather
// than about pixels.
func TestAlphaIsIgnoredUnlessAsked(t *testing.T) {
	a := writePNG(t, "a.png", 4, 4, func(x, y int) color.NRGBA {
		return color.NRGBA{R: 10, G: 20, B: 30, A: 255}
	})
	b := writePNG(t, "b.png", 4, 4, func(x, y int) color.NRGBA {
		return color.NRGBA{R: 10, G: 20, B: 30, A: 128}
	})
	var out, errOut bytes.Buffer
	if got := run([]string{a, b}, &out, &errOut); got != 0 {
		t.Fatalf("alpha-only difference reported without -alpha: exit %d, %q", got, out.String())
	}
	out.Reset()
	if got := run([]string{"-alpha", a, b}, &out, &errOut); got != 1 {
		t.Fatalf("-alpha did not report the difference: exit %d, %q", got, out.String())
	}
}

// Localising a difference to a band of rows is what distinguishes "two
// instruments disagree" from "two moments of the same screen".
func TestRowBands(t *testing.T) {
	a := writePNG(t, "a.png", 4, 120, func(x, y int) color.NRGBA {
		return color.NRGBA{A: 255}
	})
	b := writePNG(t, "b.png", 4, 120, func(x, y int) color.NRGBA {
		c := color.NRGBA{A: 255}
		if y >= 60 && y < 70 {
			c.R = 255
		}
		return c
	})
	var out, errOut bytes.Buffer
	if got := run([]string{"-rows", a, b}, &out, &errOut); got != 1 {
		t.Fatalf("exit = %d", got)
	}
	s := out.String()
	if !strings.Contains(s, "rows    0-  50 : 0 differing") {
		t.Errorf("band 0 not reported clean: %q", s)
	}
	if !strings.Contains(s, "rows   50- 100 : 40 differing") {
		t.Errorf("band 1 not reported: %q", s)
	}
	if !strings.Contains(s, "rows  100- 150 : 0 differing") {
		t.Errorf("band 2 not reported clean: %q", s)
	}
}

func TestUsageErrors(t *testing.T) {
	var out, errOut bytes.Buffer
	if got := run([]string{"only-one.png"}, &out, &errOut); got != 2 {
		t.Fatalf("one argument: exit = %d, want 2", got)
	}
	if !strings.Contains(errOut.String(), "usage:") {
		t.Fatalf("no usage line: %q", errOut.String())
	}
	errOut.Reset()
	if got := run([]string{"-nonsense", "a", "b"}, &out, &errOut); got != 2 {
		t.Fatalf("bad flag: exit = %d, want 2", got)
	}
}

func TestIOErrors(t *testing.T) {
	a := writePNG(t, "a.png", 4, 4, gradient)
	var out, errOut bytes.Buffer
	if got := run([]string{"missing.png", a}, &out, &errOut); got != 2 {
		t.Fatalf("missing first file: exit = %d, want 2", got)
	}
	errOut.Reset()
	if got := run([]string{a, "missing.png"}, &out, &errOut); got != 2 {
		t.Fatalf("missing second file: exit = %d, want 2", got)
	}
	// A file that exists but is not a PNG must fail as a decode error, not be
	// silently treated as an empty image.
	bad := filepath.Join(t.TempDir(), "bad.png")
	if err := os.WriteFile(bad, []byte("not a png"), 0o644); err != nil {
		t.Fatal(err)
	}
	errOut.Reset()
	if got := run([]string{bad, a}, &out, &errOut); got != 2 {
		t.Fatalf("non-PNG: exit = %d, want 2", got)
	}
	if !strings.Contains(errOut.String(), "bad.png") {
		t.Fatalf("the decode error does not name the file: %q", errOut.String())
	}
}

func TestSizeMismatch(t *testing.T) {
	a := writePNG(t, "a.png", 4, 4, gradient)
	b := writePNG(t, "b.png", 5, 4, gradient)
	var out, errOut bytes.Buffer
	if got := run([]string{a, b}, &out, &errOut); got != 2 {
		t.Fatalf("exit = %d, want 2", got)
	}
	if !strings.Contains(errOut.String(), "different sizes: 4x4 and 5x4") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestChanDev(t *testing.T) {
	if got := chanDev(255, 0); got != 255 {
		t.Fatalf("chanDev(255,0) = %d, want 255", got)
	}
	if got := chanDev(0, 255); got != 255 {
		t.Fatalf("chanDev(0,255) = %d, want 255", got)
	}
	if got := chanDev(128, 128); got != 0 {
		t.Fatalf("chanDev(equal) = %d", got)
	}
}

// The regression this file's compare() comment is about: color.Color.RGBA()
// premultiplies, so a half-transparent pixel of the SAME colour comes back
// with different red, green and blue. Comparing those would report a colour
// difference that is really an alpha difference — and would have made -alpha
// meaningless while looking perfectly healthy on the opaque artefacts.
func TestAlphaDoesNotLeakIntoColour(t *testing.T) {
	opaque := writePNG(t, "o.png", 2, 2, func(x, y int) color.NRGBA {
		return color.NRGBA{R: 200, G: 100, B: 50, A: 255}
	})
	faint := writePNG(t, "f.png", 2, 2, func(x, y int) color.NRGBA {
		return color.NRGBA{R: 200, G: 100, B: 50, A: 1}
	})
	var out, errOut bytes.Buffer
	if got := run([]string{opaque, faint}, &out, &errOut); got != 0 {
		t.Fatalf("an alpha-only difference was reported as a colour difference: %q", out.String())
	}
}

// The committed artefacts must keep proving what the record says they prove.
// This is the claim from PROOF-2026-08-25.txt sections 1 and 2, run against
// the files a reader actually has.
func TestCommittedArtifactsStillAgree(t *testing.T) {
	const dir = "../../testdata/artifacts/"
	for _, pair := range [][2]string{
		{"display-gdi.png", "qemu-screendump-control.png"},
		{"display-duplication.png", "qemu-screendump-control.png"},
		{"display-gdi.png", "display-duplication.png"},
	} {
		a, b := dir+pair[0], dir+pair[1]
		if _, err := os.Stat(a); err != nil {
			t.Skipf("artefacts not present: %v", err)
		}
		var out, errOut bytes.Buffer
		if got := run([]string{a, b}, &out, &errOut); got != 0 {
			t.Errorf("%s vs %s: exit %d\n%s%s", pair[0], pair[1], got, out.String(), errOut.String())
			continue
		}
		t.Log(strings.TrimSpace(out.String()))
	}
}

// The largest deviation must be the largest across ALL channels, not just the
// first one that differs. A max that only ever looks at red under-reports
// every green or blue shift.
func TestMaxDeviationScansEveryChannel(t *testing.T) {
	base := color.NRGBA{R: 10, G: 10, B: 10, A: 200}
	for _, tc := range []struct {
		name  string
		other color.NRGBA
		args  []string
		want  string
	}{
		{"green", color.NRGBA{R: 10, G: 210, B: 10, A: 200}, nil, "max deviation 200"},
		{"blue", color.NRGBA{R: 10, G: 10, B: 160, A: 200}, nil, "max deviation 150"},
		{"alpha", color.NRGBA{R: 10, G: 10, B: 10, A: 50}, []string{"-alpha"}, "max deviation 150"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := writePNG(t, "a.png", 2, 2, func(x, y int) color.NRGBA { return base })
			b := writePNG(t, "b.png", 2, 2, func(x, y int) color.NRGBA { return tc.other })
			var out, errOut bytes.Buffer
			if got := run(append(append([]string{}, tc.args...), a, b), &out, &errOut); got != 1 {
				t.Fatalf("exit = %d, want 1: %q %q", got, out.String(), errOut.String())
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Fatalf("output %q does not contain %q", out.String(), tc.want)
			}
		})
	}
}

// main() is a one-line seam over run(); exercising it keeps the exit path from
// being the only untested statement in the command.
func TestMainUsesTheExitSeam(t *testing.T) {
	saved := osExit
	defer func() { osExit = saved }()
	got := -1
	osExit = func(code int) { got = code }
	savedArgs := os.Args
	defer func() { os.Args = savedArgs }()
	os.Args = []string{"pngdiff"} // no operands: usage error
	main()
	if got != 2 {
		t.Fatalf("main exited %d, want 2", got)
	}
}
