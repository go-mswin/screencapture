// Copyright (c) the go-mswin/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

// Command pngdiff compares two PNG files pixel by pixel and reports how many
// pixels differ.
//
// It exists so that the claims in testdata/artifacts/PROOF-2026-08-25.txt can
// be CHECKED rather than believed. The strongest line in that record is that a
// capture taken by this package and a screendump taken by QEMU — an instrument
// that is not this package at all — agree in zero pixels out of 480000. A
// reader should be able to run that comparison themselves, on the committed
// files, without writing a decoder first:
//
//	go run ./cmd/pngdiff testdata/artifacts/display-gdi.png \
//	                     testdata/artifacts/qemu-screendump-control.png
//
// It exits 0 when the images are identical and 1 when they are not, so it also
// works as a gate.
//
// Only the colour channels are compared. The two files come from different
// producers — one from a BGRA frame, one from a PPM — and their alpha
// channels carry different conventions; comparing alpha would report a
// difference that is about file formats rather than about pixels. Use -alpha
// to compare it anyway.
package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	_ "image/png"
	"io"
	"os"
)

// osExit is a seam so the exit path can be tested.
var osExit = os.Exit

func main() { osExit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run compares the two named files and returns the process exit status: 0 for
// identical, 1 for different, 2 for a usage or I/O failure.
func run(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("pngdiff", flag.ContinueOnError)
	fs.SetOutput(errOut)
	alpha := fs.Bool("alpha", false, "compare the alpha channel too")
	rows := fs.Bool("rows", false, "also report the differing pixels per band of rows")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(errOut, "usage: pngdiff [-alpha] [-rows] a.png b.png")
		return 2
	}
	a, err := load(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	b, err := load(fs.Arg(1))
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	ab, bb := a.Bounds(), b.Bounds()
	if ab.Dx() != bb.Dx() || ab.Dy() != bb.Dy() {
		fmt.Fprintf(errOut, "different sizes: %dx%d and %dx%d\n",
			ab.Dx(), ab.Dy(), bb.Dx(), bb.Dy())
		return 2
	}

	diff, maxDev, band := compare(a, b, *alpha)
	total := ab.Dx() * ab.Dy()
	fmt.Fprintf(out, "%s vs %s: %d of %d pixels differ (%.2f%%), max deviation %d\n",
		fs.Arg(0), fs.Arg(1), diff, total, 100*float64(diff)/float64(total), maxDev)
	if *rows {
		for i, n := range band {
			fmt.Fprintf(out, "  rows %4d-%4d : %d differing\n", i*bandRows, (i+1)*bandRows, n)
		}
	}
	if diff == 0 {
		return 0
	}
	return 1
}

// bandRows is how many image rows one -rows band covers. Localising a
// difference to a band is what distinguishes "two instruments disagree" from
// "two moments of the same screen": chrome that agrees exactly while only a
// content area differs is the second one.
const bandRows = 50

// compare counts differing pixels, the largest single-channel deviation, and
// the per-band distribution.
//
// Both pixels are converted to NRGBA — NON-premultiplied — before anything is
// compared. color.Color.RGBA() returns ALPHA-PREMULTIPLIED channels, so two
// pixels of the same colour at different opacities come back with different
// red, green and blue values; comparing those would make an alpha difference
// leak into the colour comparison and defeat the point of -alpha being
// opt-in. It cost nothing on the committed artefacts, which are fully opaque,
// and would have been silently wrong on the first window capture with a
// transparent corner.
func compare(a, b image.Image, alpha bool) (diff, maxDev int, band []int) {
	ab, bb := a.Bounds(), b.Bounds()
	band = make([]int, (ab.Dy()+bandRows-1)/bandRows)
	for y := 0; y < ab.Dy(); y++ {
		for x := 0; x < ab.Dx(); x++ {
			p := color.NRGBAModel.Convert(a.At(ab.Min.X+x, ab.Min.Y+y)).(color.NRGBA)
			q := color.NRGBAModel.Convert(b.At(bb.Min.X+x, bb.Min.Y+y)).(color.NRGBA)
			d := chanDev(p.R, q.R)
			if v := chanDev(p.G, q.G); v > d {
				d = v
			}
			if v := chanDev(p.B, q.B); v > d {
				d = v
			}
			if alpha {
				if v := chanDev(p.A, q.A); v > d {
					d = v
				}
			}
			if d > 0 {
				diff++
				band[y/bandRows]++
				if d > maxDev {
					maxDev = d
				}
			}
		}
	}
	return diff, maxDev, band
}

// chanDev is the absolute difference of two 8-bit channel values.
func chanDev(a, b uint8) int {
	if a > b {
		return int(a) - int(b)
	}
	return int(b) - int(a)
}

// load decodes a PNG.
func load(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return img, nil
}
