// Copyright (c) the go-mswin/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

package screencapture

import (
	"testing"
	"unsafe"

	"github.com/go-mswin/win32"
)

// The C layout of every structure that crosses the syscall boundary.
//
// A field of the wrong width or a missing pad byte does not fail to build and
// does not fail to run: it silently shifts every field after it, so a texture
// description asks for the wrong format and a duplication descriptor reports
// the wrong size — and the capture then produces a plausible-looking image of
// nothing. Asserting the size is the only defence, and it works on every GOOS
// because these are plain Go structs.
//
// The expected values are the 64-bit ones, which is the only Windows this
// package targets (amd64 and arm64 agree on all of them).
func TestStructSizes(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"POINT", unsafe.Sizeof(point{}), 8},
		{"RECT", unsafe.Sizeof(rect{}), 16},
		{"BITMAPINFOHEADER", unsafe.Sizeof(bitmapInfoHeader{}), 40},
		{"BITMAPINFO", unsafe.Sizeof(bitmapInfo{}), 52},
		{"CURSORINFO", unsafe.Sizeof(cursorInfo{}), 24},
		{"ICONINFO", unsafe.Sizeof(iconInfo{}), 32},
		{"DXGI_RATIONAL", unsafe.Sizeof(dxgiRational{}), 8},
		{"DXGI_MODE_DESC", unsafe.Sizeof(dxgiModeDesc{}), 28},
		{"DXGI_OUTPUT_DESC", unsafe.Sizeof(dxgiOutputDesc{}), 96},
		{"DXGI_ADAPTER_DESC1", unsafe.Sizeof(dxgiAdapterDesc1{}), 312},
		{"DXGI_OUTDUPL_DESC", unsafe.Sizeof(dxgiOutduplDesc{}), 36},
		{"DXGI_OUTDUPL_POINTER_POSITION", unsafe.Sizeof(dxgiOutduplPointerPosition{}), 12},
		{"DXGI_OUTDUPL_FRAME_INFO", unsafe.Sizeof(dxgiOutduplFrameInfo{}), 48},
		{"DXGI_MAPPED_RECT", unsafe.Sizeof(dxgiMappedRect{}), 16},
		{"DXGI_SAMPLE_DESC", unsafe.Sizeof(d3d11SampleDesc{}), 8},
		{"D3D11_TEXTURE2D_DESC", unsafe.Sizeof(d3d11Texture2DDesc{}), 44},
		{"D3D11_MAPPED_SUBRESOURCE", unsafe.Sizeof(d3d11MappedSubresource{}), 16},
		{"GUID", unsafe.Sizeof(guid{}), 16},
	} {
		if tc.got != tc.want {
			t.Errorf("sizeof(%s) = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

// The field offsets that matter most: the ones the OS writes into, where a
// shift produces a plausible wrong answer rather than a crash.
func TestStructOffsets(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		// The layout is win32's to declare now, and win32 asserts it too. It
		// is asserted HERE as well because this package DEPENDS on it: an
		// upstream field added in the wrong place would move szDevice, every
		// display would come back with a mangled device name, and nothing in
		// this repository would say why.
		{"MONITORINFOEXW.szDevice", unsafe.Offsetof(win32.MonitorInfoEx{}.SzDevice), 40},
		{"DXGI_OUTPUT_DESC.Monitor", unsafe.Offsetof(dxgiOutputDesc{}.Monitor), 88},
		{"DXGI_OUTDUPL_DESC.DesktopImageInSystemMemory",
			unsafe.Offsetof(dxgiOutduplDesc{}.DesktopImageInSystemMemory), 32},
		{"DXGI_OUTDUPL_FRAME_INFO.AccumulatedFrames",
			unsafe.Offsetof(dxgiOutduplFrameInfo{}.AccumulatedFrames), 16},
		{"DXGI_MAPPED_RECT.pBits", unsafe.Offsetof(dxgiMappedRect{}.PBits), 8},
		{"D3D11_TEXTURE2D_DESC.Usage", unsafe.Offsetof(d3d11Texture2DDesc{}.Usage), 28},
		{"D3D11_MAPPED_SUBRESOURCE.RowPitch",
			unsafe.Offsetof(d3d11MappedSubresource{}.RowPitch), 8},
		{"BITMAPINFO.Colors", unsafe.Offsetof(bitmapInfo{}.Colors), 40},
	} {
		if tc.got != tc.want {
			t.Errorf("offsetof(%s) = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

func TestRectHelpers(t *testing.T) {
	// A monitor above and to the left of the primary one: negative origin,
	// positive size.
	r := rect{Left: -1920, Top: -1080, Right: 0, Bottom: 0}
	if r.Width() != 1920 || r.Height() != 1080 {
		t.Fatalf("width/height = %d/%d", r.Width(), r.Height())
	}
	if got := toRect(r); got != (Rect{X: -1920, Y: -1080, W: 1920, H: 1080}) {
		t.Fatalf("toRect = %s", got)
	}
}

func TestNewBitmapInfoIsTopDown(t *testing.T) {
	bi := newBitmapInfo(1920, 1080)
	if bi.Header.BiSize != 40 {
		t.Fatalf("biSize = %d, want the BITMAPINFOHEADER size 40", bi.Header.BiSize)
	}
	// The sign convention in one assertion: NEGATIVE height means row 0 is the
	// TOP row, which is the only thing this package ever asks GDI for.
	if bi.Header.BiHeight != -1080 {
		t.Fatalf("biHeight = %d, want -1080 so the DIB is top-down", bi.Header.BiHeight)
	}
	if bi.Header.BiWidth != 1920 || bi.Header.BiPlanes != 1 || bi.Header.BiBitCount != 32 {
		t.Fatalf("header = %+v", bi.Header)
	}
	if bi.Header.BiCompression != biRGB {
		t.Fatalf("biCompression = %d, want BI_RGB", bi.Header.BiCompression)
	}
	if got := bi.Header.BiSizeImage; got != 1920*4*1080 {
		t.Fatalf("biSizeImage = %d", got)
	}
	// The layout it describes must normalise as top-down with no flip.
	w, h, stride, bottomUp, err := bi.layout().Normalize()
	if err != nil || w != 1920 || h != 1080 || stride != 1920*4 || bottomUp {
		t.Fatalf("layout normalize = %d,%d,%d,%v,%v", w, h, stride, bottomUp, err)
	}
}

func TestStagingDesc(t *testing.T) {
	// CopyResource between mismatched descriptions silently does NOTHING, so
	// the staging description has to agree with the source on size and format
	// and disagree on everything else.
	src := d3d11Texture2DDesc{
		Width: 3840, Height: 2160, MipLevels: 1, ArraySize: 1,
		Format: dxgiFormatB8G8R8A8Unorm, SampleDesc: d3d11SampleDesc{Count: 1},
		Usage: 0, BindFlags: 0x20, CPUAccessFlags: 0, MiscFlags: 0x800,
	}
	got := stagingDesc(src)
	if got.Width != src.Width || got.Height != src.Height || got.Format != src.Format {
		t.Fatalf("staging disagrees with the source: %+v", got)
	}
	if got.Usage != d3d11UsageStaging {
		t.Fatalf("usage = %d, want D3D11_USAGE_STAGING", got.Usage)
	}
	if got.CPUAccessFlags != d3d11CPUAccessRead {
		t.Fatalf("cpu access = %#x, want D3D11_CPU_ACCESS_READ", got.CPUAccessFlags)
	}
	if got.BindFlags != 0 || got.MiscFlags != 0 {
		t.Fatalf("a staging texture must bind and share nothing: %+v", got)
	}
	if got.MipLevels != 1 || got.ArraySize != 1 || got.SampleDesc.Count != 1 {
		t.Fatalf("staging must be a single unsampled surface: %+v", got)
	}
}

func TestFeatureLevelsAreNewestFirst(t *testing.T) {
	for i := 1; i < len(featureLevels); i++ {
		if featureLevels[i] >= featureLevels[i-1] {
			t.Fatalf("feature level %d (%#x) is not below %#x",
				i, featureLevels[i], featureLevels[i-1])
		}
	}
	if featureLevels[len(featureLevels)-1] != featureLevel91 {
		t.Fatal("the list must reach down to 9.1: duplication needs nothing more")
	}
}

func TestDPIAwarenessContextValue(t *testing.T) {
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 is the HANDLE value -4. It is
	// written as a bit expression so it is right on both 64-bit targets, and
	// asserted because a wrong value silently leaves the process DPI-unaware
	// and every capture of a scaled display the wrong size.
	//
	// The constant is win32's now. It stays asserted here because THIS package
	// is the one whose captures come back wrong if it drifts, and a dependency
	// one may not check is a dependency one may not rely on.
	if got := win32.DPIAwarenessPerMonitorV2; got != ^uintptr(0)-3 {
		t.Fatalf("DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = %#x, want -4", got)
	}
}
