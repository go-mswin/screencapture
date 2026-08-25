// Copyright (c) the go-mswin/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

package screencapture

import (
	"unsafe"

	"github.com/go-mswin/win32"
)

// The Win32, DXGI and Direct3D 11 structures this package passes across the
// syscall boundary, mirrored in Go.
//
// They live in an UNTAGGED file on purpose. They are plain Go structs with no
// Windows import, so they compile — and are size-checked by the test suite —
// on every GOOS. A field of the wrong width or a missing pad byte does not
// fail to build and does not fail to run: it silently shifts every subsequent
// field, so a texture description asks for the wrong format and a duplication
// descriptor reports the wrong size. The only defence is to assert the C
// layout, which structs_test.go does against the documented sizes.
//
// Every one of these is 64-bit-only, which this package is: Windows on
// x86-64 and arm64. Both have 8-byte pointers and the same alignment rules, so
// one set of expected sizes covers both.

// point and rect are go-mswin/win32's POINT and RECT. They are ALIASES, not
// redeclarations: a second copy of a structure the OS writes into is a second
// place for its layout to drift.
type (
	point = win32.Point
	rect  = win32.Rect
)

// toRect converts a Win32 RECT (left, top, right, bottom) to the exported
// origin-plus-size form.
func toRect(r rect) Rect {
	return Rect{X: int(r.Left), Y: int(r.Top), W: int(r.Width()), H: int(r.Height())}
}

// monitorInfoEx mirrors MONITORINFOEXW. CbSize must be set to the struct's own
// size before GetMonitorInfoW is called, which is how the OS tells the EX form
// from the plain one.
type monitorInfoEx struct {
	CbSize    uint32
	RcMonitor rect
	RcWork    rect
	DwFlags   uint32
	SzDevice  [32]uint16
}

// monitorinfoPrimary is MONITORINFOF_PRIMARY.
const monitorinfoPrimary = 0x00000001

// bitmapInfoHeader mirrors BITMAPINFOHEADER. A NEGATIVE BiHeight requests a
// top-down DIB, which is the only thing this package ever asks for.
//
// It is declared here rather than reused from go-mswin/win32 because
// CreateDIBSection needs a BITMAPINFO — a header immediately followed by a
// colour table — and Go gives no way to guarantee that adjacency across two
// types. See bitmapInfo.
type bitmapInfoHeader struct {
	BiSize          uint32
	BiWidth         int32
	BiHeight        int32
	BiPlanes        uint16
	BiBitCount      uint16
	BiCompression   uint32
	BiSizeImage     uint32
	BiXPelsPerMeter int32
	BiYPelsPerMeter int32
	BiClrUsed       uint32
	BiClrImportant  uint32
}

// bitmapInfo mirrors BITMAPINFO: a BITMAPINFOHEADER followed by its colour
// table. At 32bpp with BI_RGB the table is unused, but CreateDIBSection reads
// the three DWORDs anyway when BI_BITFIELDS is in play, so the space is
// declared rather than left off the end of an allocation.
type bitmapInfo struct {
	Header bitmapInfoHeader
	Colors [3]uint32
}

// newBitmapInfo fills in a top-down 32bpp BI_RGB BITMAPINFO for a width and
// height given as POSITIVE pixel counts. The negative BiHeight is applied here,
// in one place, so no call site has to remember the sign convention.
func newBitmapInfo(width, height int) bitmapInfo {
	var bi bitmapInfo
	bi.Header.BiSize = uint32(unsafe.Sizeof(bitmapInfoHeader{}))
	bi.Header.BiWidth = int32(width)
	bi.Header.BiHeight = int32(-height) // negative → top-down, row 0 at the top
	bi.Header.BiPlanes = 1
	bi.Header.BiBitCount = 32
	bi.Header.BiCompression = biRGB
	bi.Header.BiSizeImage = uint32(AlignedStride(width, 32) * height)
	return bi
}

// layout describes the DIB this BITMAPINFO asks for, in the same convention
// the OS states it, so the normalisation path can be exercised against a real
// header rather than a hand-written one.
func (bi bitmapInfo) layout() DIBLayout {
	return DIBLayout{
		Width:    int(bi.Header.BiWidth),
		Height:   int(bi.Header.BiHeight),
		BitCount: int(bi.Header.BiBitCount),
	}
}

// GDI constants. The ones go-mswin/win32 owns are ALIASED rather than
// restated, so there is one place for each value to be wrong in.
const (
	biRGB        = win32.BIRGB
	dibRGBColors = win32.DIBRGBColors
	srcCopy      = win32.SRCCOPY
	captureBLT   = win32.CaptureBLT
	halftone     = win32.Halftone
	// pwRenderFullContent is PW_RENDERFULLCONTENT, which makes PrintWindow go
	// through DWM and so capture a window that is occluded or hardware
	// composited. Without it a modern window prints blank.
	pwRenderFullContent = 0x00000002
	// diNormal is DI_NORMAL for DrawIconEx.
	diNormal = 0x0003
	// cursorShowing is CURSOR_SHOWING in CURSORINFO.Flags.
	cursorShowing = 0x00000001
	// wdaNone and wdaExcludeFromCapture are the SetWindowDisplayAffinity
	// values. WDA_EXCLUDEFROMCAPTURE (Windows 10 2004 and later) removes the
	// window from captures while leaving it visible on screen; the older
	// WDA_MONITOR blanks it on screen too, which is not what anyone wants.
	wdaNone               = 0x00000000
	wdaExcludeFromCapture = 0x00000011
	// dwmwaCloaked is DWMWA_CLOAKED for DwmGetWindowAttribute.
	dwmwaCloaked = 14
	// dwmwaExtendedFrameBounds is DWMWA_EXTENDED_FRAME_BOUNDS: the rectangle
	// the user actually sees, without the invisible DWM resize border that
	// GetWindowRect includes.
	dwmwaExtendedFrameBounds = 9
	// mdtEffectiveDPI is MDT_EFFECTIVE_DPI for GetDpiForMonitor.
	mdtEffectiveDPI = 0
	// dpiAwarenessContextPerMonitorAwareV2 is
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2, the handle value (-4) that
	// SetProcessDpiAwarenessContext takes. Without it every monitor
	// measurement comes back in virtualised coordinates and a capture of a
	// scaled display is silently the wrong size.
	dpiAwarenessContextPerMonitorAwareV2 = ^uintptr(3) // -4
)

// cursorInfo mirrors CURSORINFO. CbSize must be set before GetCursorInfo.
type cursorInfo struct {
	CbSize      uint32
	Flags       uint32
	HCursor     uintptr
	PtScreenPos point
}

// iconInfo is go-mswin/win32's ICONINFO, used to recover a cursor's hotspot so
// the pointer is drawn where it actually points rather than with its top-left
// corner at the hotspot.
type iconInfo = win32.IconInfo

// guid mirrors GUID, passed by pointer to QueryInterface and
// CreateDXGIFactory1. An interface identity that is one byte wrong resolves to
// nothing at all and reads as "this interface is not supported", which is why
// its layout is asserted rather than assumed.
type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// dxgiRational mirrors DXGI_RATIONAL.
type dxgiRational struct{ Numerator, Denominator uint32 }

// dxgiModeDesc mirrors DXGI_MODE_DESC.
type dxgiModeDesc struct {
	Width            uint32
	Height           uint32
	RefreshRate      dxgiRational
	Format           uint32
	ScanlineOrdering uint32
	Scaling          uint32
}

// dxgiOutputDesc mirrors DXGI_OUTPUT_DESC. DeviceName is the GDI device name
// (`\\.\DISPLAY1`), which is the only reliable way to match a DXGI output to
// the monitor GetMonitorInfo described.
type dxgiOutputDesc struct {
	DeviceName         [32]uint16
	DesktopCoordinates rect
	AttachedToDesktop  int32
	Rotation           uint32
	Monitor            uintptr
}

// dxgiAdapterDesc1 mirrors DXGI_ADAPTER_DESC1.
type dxgiAdapterDesc1 struct {
	Description           [128]uint16
	VendorID              uint32
	DeviceID              uint32
	SubSysID              uint32
	Revision              uint32
	DedicatedVideoMemory  uint64
	DedicatedSystemMemory uint64
	SharedSystemMemory    uint64
	AdapterLUID           struct {
		LowPart  uint32
		HighPart int32
	}
	Flags uint32
}

// dxgiAdapterFlagSoftware is DXGI_ADAPTER_FLAG_SOFTWARE: the Microsoft Basic
// Render Driver (WARP). It is a real adapter with no outputs attached, so it
// is enumerated and then skipped.
const dxgiAdapterFlagSoftware = 2

// dxgiOutduplDesc mirrors DXGI_OUTDUPL_DESC.
//
// DesktopImageInSystemMemory being non-zero means the desktop is already in
// system memory and MapDesktopSurface can read it without a texture copy at
// all. It is only ever true on a basic display driver, which is exactly the
// case a virtual machine hits, so this package checks it rather than assuming
// the texture path.
type dxgiOutduplDesc struct {
	ModeDesc                   dxgiModeDesc
	Rotation                   uint32
	DesktopImageInSystemMemory int32
}

// dxgiOutduplPointerPosition mirrors DXGI_OUTDUPL_POINTER_POSITION.
type dxgiOutduplPointerPosition struct {
	Position point
	Visible  int32
}

// dxgiOutduplFrameInfo mirrors DXGI_OUTDUPL_FRAME_INFO.
//
// AccumulatedFrames == 0 is the "nothing on the desktop changed, only the
// pointer moved" signal. Treating it as a frame is how a duplication capture
// turns into a busy loop that never stops.
type dxgiOutduplFrameInfo struct {
	LastPresentTime           int64
	LastMouseUpdateTime       int64
	AccumulatedFrames         uint32
	RectsCoalesced            int32
	ProtectedContentMaskedOut int32
	PointerPosition           dxgiOutduplPointerPosition
	TotalMetadataBufferSize   uint32
	PointerShapeBufferSize    uint32
}

// dxgiMappedRect mirrors DXGI_MAPPED_RECT, what MapDesktopSurface fills in.
// PBits is declared as an unsafe.Pointer, not a uintptr: the OS writes a
// pointer there, and reading it back as a pointer means no uintptr-to-pointer
// conversion ever happens in Go, which is what keeps go vet's unsafeptr check
// quiet without a copy through RtlMoveMemory.
type dxgiMappedRect struct {
	Pitch int32
	_     int32 // the C struct pads here on 64-bit before the pointer
	PBits unsafe.Pointer
}

// d3d11SampleDesc mirrors DXGI_SAMPLE_DESC.
type d3d11SampleDesc struct{ Count, Quality uint32 }

// d3d11Texture2DDesc mirrors D3D11_TEXTURE2D_DESC.
type d3d11Texture2DDesc struct {
	Width          uint32
	Height         uint32
	MipLevels      uint32
	ArraySize      uint32
	Format         uint32
	SampleDesc     d3d11SampleDesc
	Usage          uint32
	BindFlags      uint32
	CPUAccessFlags uint32
	MiscFlags      uint32
}

// stagingDesc turns a captured texture's description into the staging texture
// that can be mapped for CPU reads: same size and format, no mip levels, no
// binding, no sharing, one sample.
//
// Getting this wrong is the classic Desktop Duplication mistake: CopyResource
// silently does nothing when the two descriptions disagree, so the map
// succeeds and every frame is the untouched buffer — a perfectly stable image
// of nothing.
func stagingDesc(src d3d11Texture2DDesc) d3d11Texture2DDesc {
	return d3d11Texture2DDesc{
		Width:          src.Width,
		Height:         src.Height,
		MipLevels:      1,
		ArraySize:      1,
		Format:         src.Format,
		SampleDesc:     d3d11SampleDesc{Count: 1, Quality: 0},
		Usage:          d3d11UsageStaging,
		BindFlags:      0,
		CPUAccessFlags: d3d11CPUAccessRead,
		MiscFlags:      0,
	}
}

// d3d11MappedSubresource mirrors D3D11_MAPPED_SUBRESOURCE. PData is an
// unsafe.Pointer for the same reason as dxgiMappedRect.PBits.
type d3d11MappedSubresource struct {
	PData      unsafe.Pointer
	RowPitch   uint32
	DepthPitch uint32
}

// Direct3D 11 and DXGI enumeration values.
const (
	d3d11SDKVersion    = 7
	d3d11UsageStaging  = 3
	d3d11CPUAccessRead = 0x20000
	d3d11MapRead       = 1

	d3dDriverTypeUnknown  = 0
	d3dDriverTypeHardware = 1
	d3dDriverTypeWARP     = 5

	// d3d11CreateDeviceBGRASupport is D3D11_CREATE_DEVICE_BGRA_SUPPORT. The
	// duplication surface is BGRA, so the device must admit to supporting it.
	d3d11CreateDeviceBGRASupport = 0x20

	// The feature levels this package will accept, newest first.
	// Duplication needs nothing beyond 9.1, so the list is only about not
	// refusing a device that is perfectly capable of a CopyResource.
	featureLevel111 = 0xb100
	featureLevel110 = 0xb000
	featureLevel101 = 0xa100
	featureLevel100 = 0xa000
	featureLevel93  = 0x9300
	featureLevel92  = 0x9200
	featureLevel91  = 0x9100

	// dxgiFormatB8G8R8A8Unorm is what Desktop Duplication always produces.
	// Anything else is a surprise this package refuses rather than
	// misinterprets.
	dxgiFormatB8G8R8A8Unorm = 87
)

// featureLevels is the list handed to D3D11CreateDevice, newest first.
var featureLevels = [...]uint32{
	featureLevel111, featureLevel110, featureLevel101,
	featureLevel100, featureLevel93, featureLevel92, featureLevel91,
}
