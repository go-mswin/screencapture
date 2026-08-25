// Copyright (c) the go-mswin/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build windows

package screencapture

// DXGI Desktop Duplication, driven through hand-built COM vtable calls.
//
// # Why by hand
//
// IDXGIOutputDuplication is COM, and pure Go has no COM. What it does have is
// syscall.SyscallN and a well-defined memory layout: a COM interface pointer
// points at a structure whose first word is the address of an array of
// function pointers, in DECLARATION ORDER, base interfaces first. So a method
// call is a slot lookup and a SyscallN with `this` as the first argument.
//
// The entire risk of the technique is in the slot numbers. A wrong one does
// not crash: it calls a different method of the same object, which returns a
// plausible HRESULT and leaves the out-parameter untouched. Every slot below
// is written out with the full inheritance chain that produces it.
//
// # The two read-back paths
//
// DXGI_OUTDUPL_DESC.DesktopImageInSystemMemory decides which one runs:
//
//   - FALSE (a real GPU): the acquired frame is a GPU texture. It is copied
//     into a staging texture with CPU read access, the duplication frame is
//     released immediately, and the staging texture is MAPPED and left mapped.
//     The consumer then borrows the driver's own memory — no copy anywhere on
//     the hot path. The staging textures are a ring, so the one being written
//     is never the one being read.
//
//   - TRUE (a basic display driver, which is what a virtual machine has): the
//     desktop is already in system memory and MapDesktopSurface reads it
//     without any texture at all. That mapping is only valid between
//     AcquireNextFrame and ReleaseFrame, so this path DOES copy the frame out,
//     once, into the ring. It is the slower of the two and it is the one a VM
//     takes; the numbers in the README say which was measured where.

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Vtable slots. The inheritance chain that produces each number is spelled out
// because an off-by-one here is silent.
const (
	// IUnknown: QueryInterface(0) AddRef(1) Release(2)
	// IDXGIObject: SetPrivateData(3) SetPrivateDataInterface(4)
	//              GetPrivateData(5) GetParent(6)
	// IDXGIFactory: EnumAdapters(7) MakeWindowAssociation(8)
	//               GetWindowAssociation(9) CreateSwapChain(10)
	//               CreateSoftwareAdapter(11)
	// IDXGIFactory1: EnumAdapters1(12) IsCurrent(13)
	slotFactory1EnumAdapters1 = 12

	// IDXGIObject(3..6) then IDXGIAdapter: EnumOutputs(7) GetDesc(8)
	// CheckInterfaceSupport(9); IDXGIAdapter1: GetDesc1(10)
	slotAdapterEnumOutputs = 7
	slotAdapter1GetDesc1   = 10

	// IDXGIObject(3..6) then IDXGIOutput: GetDesc(7) GetDisplayModeList(8)
	// FindClosestMatchingMode(9) WaitForVBlank(10) TakeOwnership(11)
	// ReleaseOwnership(12) GetGammaControlCapabilities(13) SetGammaControl(14)
	// GetGammaControl(15) SetDisplaySurface(16) GetDisplaySurfaceData(17)
	// GetFrameStatistics(18); IDXGIOutput1: GetDisplayModeList1(19)
	// FindClosestMatchingMode1(20) GetDisplaySurfaceData1(21)
	// DuplicateOutput(22)
	slotOutputGetDesc         = 7
	slotOutput1DuplicateOutpt = 22

	// IDXGIObject(3..6) then IDXGIOutputDuplication: GetDesc(7)
	// AcquireNextFrame(8) GetFrameDirtyRects(9) GetFrameMoveRects(10)
	// GetFramePointerShape(11) MapDesktopSurface(12) UnMapDesktopSurface(13)
	// ReleaseFrame(14)
	slotDuplGetDesc            = 7
	slotDuplAcquireNextFrame   = 8
	slotDuplMapDesktopSurface  = 12
	slotDuplUnMapDesktopSurfce = 13
	slotDuplReleaseFrame       = 14

	// IUnknown(0..2) then ID3D11Device: CreateBuffer(3) CreateTexture1D(4)
	// CreateTexture2D(5) …
	slotDeviceCreateTexture2D = 5

	// IUnknown(0..2), ID3D11DeviceChild: GetDevice(3) GetPrivateData(4)
	// SetPrivateData(5) SetPrivateDataInterface(6), then
	// ID3D11DeviceContext: VSSetConstantBuffers(7) PSSetShaderResources(8)
	// PSSetShader(9) PSSetSamplers(10) VSSetShader(11) DrawIndexed(12)
	// Draw(13) Map(14) Unmap(15) … CopySubresourceRegion(46) CopyResource(47)
	slotContextMap          = 14
	slotContextUnmap        = 15
	slotContextCopyResource = 47

	// IUnknown(0..2), ID3D11DeviceChild(3..6), ID3D11Resource: GetType(7)
	// SetEvictionPriority(8) GetEvictionPriority(9), ID3D11Texture2D:
	// GetDesc(10)
	slotTexture2DGetDesc = 10
)

// dxgiOutputRef locates one display in the DXGI enumeration.
type dxgiOutputRef struct {
	adapter  int
	output   int
	rotation Rotation
}

// dxgiOutputsByDeviceName enumerates every adapter and every output on it, and
// indexes them by the GDI device name they report. Matching by NAME rather
// than by position is what makes a multi-adapter machine come out right: on a
// laptop with a discrete GPU the built-in panel and an external monitor are
// routinely on different adapters, and output 0 of adapter 1 is not the second
// monitor.
//
// It never returns an error: dxgi.dll not loading, or an adapter refusing
// enumeration, only means those displays cannot be duplicated, and they are
// still perfectly capturable with GDI. Every caller treats a missing entry that
// way.
func dxgiOutputsByDeviceName() map[string]dxgiOutputRef {
	out := map[string]dxgiOutputRef{}
	factory, err := createDXGIFactory1()
	if err != nil {
		return out
	}
	defer release(factory)
	for ai := 0; ; ai++ {
		adapter, code := enumAdapter(factory, ai)
		if adapter == nil {
			_ = code // DXGI_ERROR_NOT_FOUND ends the enumeration
			break
		}
		// The Microsoft Basic Render Driver is a real adapter with no outputs;
		// enumerating it costs nothing and finds nothing, but skipping it
		// keeps the indices meaningful.
		if isSoftwareAdapter(adapter) {
			release(adapter)
			continue
		}
		for oi := 0; ; oi++ {
			output, _ := enumOutput(adapter, oi)
			if output == nil {
				break
			}
			var desc dxgiOutputDesc
			r, _, _ := syscall.SyscallN(slot(output, slotOutputGetDesc),
				uintptr(output), uintptr(unsafe.Pointer(&desc)))
			if !HRESULT(r).Failed() && desc.AttachedToDesktop != 0 {
				name := utf16ToString(desc.DeviceName[:])
				out[name] = dxgiOutputRef{adapter: ai, output: oi, rotation: Rotation(desc.Rotation)}
			}
			release(output)
		}
		release(adapter)
	}
	return out
}

// createDXGIFactory1 creates the factory the adapter enumeration hangs off.
func createDXGIFactory1() (unsafe.Pointer, error) {
	if err := modDXGI.Load(); err != nil {
		return nil, fmt.Errorf("%w: dxgi.dll: %v", ErrBackendUnavailable, err)
	}
	if err := procCreateDXGIFactory1.Find(); err != nil {
		return nil, fmt.Errorf("%w: CreateDXGIFactory1: %v", ErrBackendUnavailable, err)
	}
	var f unsafe.Pointer
	r, _, _ := procCreateDXGIFactory1.Call(
		uintptr(unsafe.Pointer(&iidIDXGIFactory1)), uintptr(unsafe.Pointer(&f)))
	if err := hr("CreateDXGIFactory1", HRESULT(r)); err != nil {
		return nil, err
	}
	if f == nil {
		return nil, hr("CreateDXGIFactory1", eFail)
	}
	return f, nil
}

// enumAdapter returns adapter i, or nil when the enumeration is exhausted.
func enumAdapter(factory unsafe.Pointer, i int) (unsafe.Pointer, HRESULT) {
	var a unsafe.Pointer
	r, _, _ := syscall.SyscallN(slot(factory, slotFactory1EnumAdapters1),
		uintptr(factory), uintptr(uint32(i)), uintptr(unsafe.Pointer(&a)))
	if HRESULT(r).Failed() {
		return nil, HRESULT(r)
	}
	return a, HRESULT(r)
}

// enumOutput returns output i of an adapter, or nil when exhausted.
func enumOutput(adapter unsafe.Pointer, i int) (unsafe.Pointer, HRESULT) {
	var o unsafe.Pointer
	r, _, _ := syscall.SyscallN(slot(adapter, slotAdapterEnumOutputs),
		uintptr(adapter), uintptr(uint32(i)), uintptr(unsafe.Pointer(&o)))
	if HRESULT(r).Failed() {
		return nil, HRESULT(r)
	}
	return o, HRESULT(r)
}

// isSoftwareAdapter reports whether an adapter is the Microsoft Basic Render
// Driver (WARP), which carries no outputs.
func isSoftwareAdapter(adapter unsafe.Pointer) bool {
	var d dxgiAdapterDesc1
	r, _, _ := syscall.SyscallN(slot(adapter, slotAdapter1GetDesc1),
		uintptr(adapter), uintptr(unsafe.Pointer(&d)))
	if HRESULT(r).Failed() {
		return false
	}
	return d.Flags&dxgiAdapterFlagSoftware != 0
}

// adapterDescription is an adapter's human-readable name, for logs and for the
// report a consumer prints when duplication is refused.
func adapterDescription(adapter unsafe.Pointer) string {
	var d dxgiAdapterDesc1
	r, _, _ := syscall.SyscallN(slot(adapter, slotAdapter1GetDesc1),
		uintptr(adapter), uintptr(unsafe.Pointer(&d)))
	if HRESULT(r).Failed() {
		return "unknown adapter"
	}
	return strings.TrimSpace(utf16ToString(d.Description[:]))
}

// duplicator is a live Desktop Duplication on one output, with the D3D11
// device it needs and the staging textures it reads back through.
type duplicator struct {
	device   unsafe.Pointer // ID3D11Device
	context  unsafe.Pointer // ID3D11DeviceContext
	output   unsafe.Pointer // IDXGIOutput1
	dupl     unsafe.Pointer // IDXGIOutputDuplication
	adapterN string

	width, height int
	// systemMemory is DXGI_OUTDUPL_DESC.DesktopImageInSystemMemory: the
	// desktop can be mapped without a texture copy, and the mapping is only
	// valid while the frame is held.
	systemMemory bool

	// staging holds one ID3D11Texture2D per ring slot, created lazily the
	// first time a slot is filled because the texture's description must match
	// the acquired frame's, which is only known once a frame has arrived.
	staging []unsafe.Pointer
	mapped  []bool

	// copyBuf holds one ring slot's worth of bytes per slot for the
	// system-memory path, which cannot lend the OS's mapping out.
	copyBuf [][]byte

	// held says a duplication frame is acquired and not yet released. It is
	// tracked rather than inferred because releasing a frame twice is an
	// error and forgetting to release one wedges the duplication.
	held bool

	// lost records that the duplication was torn down and must be rebuilt
	// before the next acquire.
	lost bool

	// firstDone gates the LastPresentTime idle test: the very first frame
	// after DuplicateOutput legitimately reports a zero present time and is
	// still the whole desktop, so it must not be discarded as idle.
	firstDone bool

	// closeOnce makes teardown idempotent.
	closeOnce sync.Once
}

// newDuplicator brings up a D3D11 device on the display's own adapter and
// starts duplicating its output.
//
// The device is created with D3D_DRIVER_TYPE_UNKNOWN, which is REQUIRED when
// an adapter is passed: naming both an adapter and a driver type is
// E_INVALIDARG, and the mistake reads as "duplication is unsupported here".
func newDuplicator(d Display, slots int) (*duplicator, error) {
	if !d.Duplicable() {
		return nil, fmt.Errorf("%w: display %q was not matched to a DXGI output",
			ErrBackendUnavailable, d.DeviceName)
	}
	if err := modD3D11.Load(); err != nil {
		return nil, fmt.Errorf("%w: d3d11.dll: %v", ErrBackendUnavailable, err)
	}
	if err := procD3D11CreateDevice.Find(); err != nil {
		return nil, fmt.Errorf("%w: D3D11CreateDevice: %v", ErrBackendUnavailable, err)
	}
	factory, err := createDXGIFactory1()
	if err != nil {
		return nil, err
	}
	defer release(factory)

	adapter, code := enumAdapter(factory, d.AdapterIndex)
	if adapter == nil {
		return nil, hr("IDXGIFactory1::EnumAdapters1", code,
			fmt.Sprintf("adapter %d for display %q", d.AdapterIndex, d.DeviceName))
	}
	defer release(adapter)
	output, code := enumOutput(adapter, d.OutputIndex)
	if output == nil {
		return nil, hr("IDXGIAdapter::EnumOutputs", code,
			fmt.Sprintf("output %d of adapter %d", d.OutputIndex, d.AdapterIndex))
	}
	defer release(output)

	out1, err := queryInterface(output, &iidIDXGIOutput1, "IDXGIOutput::QueryInterface(IDXGIOutput1)")
	if err != nil {
		return nil, err
	}

	dup := &duplicator{output: out1, adapterN: adapterDescription(adapter)}
	dup.staging = make([]unsafe.Pointer, slots)
	dup.mapped = make([]bool, slots)
	dup.copyBuf = make([][]byte, slots)

	if err := dup.createDevice(adapter); err != nil {
		dup.Close()
		return nil, err
	}
	if err := dup.duplicate(); err != nil {
		dup.Close()
		return nil, err
	}
	return dup, nil
}

// createDevice creates the D3D11 device and immediate context on one adapter.
func (p *duplicator) createDevice(adapter unsafe.Pointer) error {
	var dev, ctx unsafe.Pointer
	var level uint32
	r, _, _ := procD3D11CreateDevice.Call(
		uintptr(adapter),
		uintptr(d3dDriverTypeUnknown), // REQUIRED with an explicit adapter
		0,                             // no software rasteriser module
		uintptr(d3d11CreateDeviceBGRASupport),
		uintptr(unsafe.Pointer(&featureLevels[0])),
		uintptr(len(featureLevels)),
		uintptr(d3d11SDKVersion),
		uintptr(unsafe.Pointer(&dev)),
		uintptr(unsafe.Pointer(&level)),
		uintptr(unsafe.Pointer(&ctx)),
	)
	if err := hr("D3D11CreateDevice", HRESULT(r), "adapter "+p.adapterN); err != nil {
		return err
	}
	if dev == nil || ctx == nil {
		return hr("D3D11CreateDevice", eFail, "no device returned")
	}
	p.device, p.context = dev, ctx
	return nil
}

// duplicate starts (or restarts) the duplication and reads its description.
func (p *duplicator) duplicate() error {
	var dupl unsafe.Pointer
	r, _, _ := syscall.SyscallN(slot(p.output, slotOutput1DuplicateOutpt),
		uintptr(p.output), uintptr(p.device), uintptr(unsafe.Pointer(&dupl)))
	if err := hr("IDXGIOutput1::DuplicateOutput", HRESULT(r), "adapter "+p.adapterN); err != nil {
		return err
	}
	if dupl == nil {
		return hr("IDXGIOutput1::DuplicateOutput", eFail, "no duplication returned")
	}
	p.dupl = dupl
	p.held, p.lost, p.firstDone = false, false, false

	// GetDesc returns void, so there is no HRESULT to check here: the only
	// evidence it worked is the values it wrote.
	var desc dxgiOutduplDesc
	syscall.SyscallN(slot(dupl, slotDuplGetDesc), uintptr(dupl), uintptr(unsafe.Pointer(&desc)))
	p.width, p.height = int(desc.ModeDesc.Width), int(desc.ModeDesc.Height)
	p.systemMemory = desc.DesktopImageInSystemMemory != 0
	if p.width <= 0 || p.height <= 0 {
		return fmt.Errorf("%w: duplication reported a %dx%d desktop",
			ErrBackendUnavailable, p.width, p.height)
	}
	if desc.ModeDesc.Format != dxgiFormatB8G8R8A8Unorm && desc.ModeDesc.Format != 0 {
		return fmt.Errorf("%w: duplication reported DXGI format %d, not B8G8R8A8_UNORM (%d)",
			ErrBackendUnavailable, desc.ModeDesc.Format, dxgiFormatB8G8R8A8Unorm)
	}
	return nil
}

// Size is the duplicated desktop's pixel size.
func (p *duplicator) Size() (int, int) { return p.width, p.height }

// Adapter names the GPU the duplication runs on.
func (p *duplicator) Adapter() string { return p.adapterN }

// Path names the read-back route in use, for the report.
func (p *duplicator) Path() string {
	if p.systemMemory {
		return "MapDesktopSurface (desktop already in system memory)"
	}
	return "staging texture (CopyResource + Map)"
}

// releaseFrame gives an acquired duplication frame back. Holding one blocks
// the next acquire, so every path out of Capture goes through here.
func (p *duplicator) releaseFrame() {
	if !p.held || p.dupl == nil {
		return
	}
	syscall.SyscallN(slot(p.dupl, slotDuplReleaseFrame), uintptr(p.dupl))
	p.held = false
}

// reset tears the duplication down so the next Capture rebuilds it. It is what
// DXGI_ERROR_ACCESS_LOST is answered with.
func (p *duplicator) reset() {
	p.releaseFrame()
	if p.dupl != nil {
		release(p.dupl)
		p.dupl = nil
	}
	p.lost = true
}

// Capture acquires one frame into ring slot i and returns its layout, or
// [ErrNoFrame] when nothing changed within the timeout.
//
// The returned slice ALIASES either the mapped staging texture (the GPU path)
// or the ring's own buffer (the system-memory path). It stays valid until the
// same slot is captured into again, which the ring guarantees does not happen
// while a consumer holds it.
func (p *duplicator) Capture(i int, timeout time.Duration) ([]byte, DIBLayout, time.Duration, time.Duration, error) {
	if p.lost || p.dupl == nil {
		if err := p.duplicate(); err != nil {
			return nil, DIBLayout{}, 0, 0, err
		}
	}
	// A frame from the previous round must be released before the next
	// acquire; AcquireNextFrame returns DXGI_ERROR_INVALID_CALL otherwise.
	p.releaseFrame()

	ms := uint32(timeout / time.Millisecond)
	var info dxgiOutduplFrameInfo
	var resource unsafe.Pointer
	// AcquireNextFrame BLOCKS until the desktop changes, so the time it takes
	// is a property of the desktop, not of this package. It is measured
	// separately and never folded into the read-back cost.
	waitStart := time.Now()
	r, _, _ := syscall.SyscallN(slot(p.dupl, slotDuplAcquireNextFrame),
		uintptr(p.dupl), uintptr(ms),
		uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&resource)))
	waited := time.Since(waitStart)
	code := HRESULT(r)
	switch {
	case code == dxgiErrorWaitTimeout:
		// Nothing changed. This is the normal, correct outcome on a still
		// desktop and is not a failure.
		return nil, DIBLayout{}, 0, waited, ErrNoFrame
	case code.Failed():
		err := hr("IDXGIOutputDuplication::AcquireNextFrame", code)
		if isAccessLost(code) {
			p.reset()
		}
		return nil, DIBLayout{}, 0, waited, err
	}
	p.held = true

	// LastPresentTime == 0 means the desktop image did NOT change and only the
	// pointer moved. The very first frame after DuplicateOutput reports zero
	// too and IS the whole desktop, so it is let through once.
	if info.LastPresentTime == 0 && p.firstDone {
		if resource != nil {
			release(resource)
		}
		p.releaseFrame()
		return nil, DIBLayout{}, 0, waited, ErrNoFrame
	}

	readStart := time.Now()
	var pix []byte
	var layout DIBLayout
	var err error
	if p.systemMemory {
		pix, layout, err = p.captureSystemMemory(i)
	} else {
		pix, layout, err = p.captureTexture(i, resource)
	}
	if resource != nil {
		release(resource)
	}
	if err != nil {
		p.releaseFrame()
		var ce *COMError
		if errors.As(err, &ce) && isAccessLost(ce.HR) {
			p.reset()
		}
		return nil, DIBLayout{}, time.Since(readStart), waited, err
	}
	p.firstDone = true
	if p.systemMemory {
		// The mapping was only valid while the frame was held, and the bytes
		// have already been copied out, so the frame goes back now.
		p.releaseFrame()
	}
	return pix, layout, time.Since(readStart), waited, nil
}

// captureSystemMemory reads the desktop through MapDesktopSurface, which is
// only available on a basic display driver. The mapping dies with the frame,
// so this path copies — once, into the ring's own buffer.
func (p *duplicator) captureSystemMemory(i int) ([]byte, DIBLayout, error) {
	var m dxgiMappedRect
	r, _, _ := syscall.SyscallN(slot(p.dupl, slotDuplMapDesktopSurface),
		uintptr(p.dupl), uintptr(unsafe.Pointer(&m)))
	if err := hr("IDXGIOutputDuplication::MapDesktopSurface", HRESULT(r)); err != nil {
		return nil, DIBLayout{}, err
	}
	defer syscall.SyscallN(slot(p.dupl, slotDuplUnMapDesktopSurfce), uintptr(p.dupl))
	if m.PBits == nil || m.Pitch <= 0 {
		return nil, DIBLayout{}, hr("IDXGIOutputDuplication::MapDesktopSurface", eFail,
			fmt.Sprintf("pitch %d", m.Pitch))
	}
	stride := int(m.Pitch)
	n := stride * p.height
	// PBits arrives as a POINTER the OS wrote, never as a uintptr, so
	// unsafe.Slice here involves no uintptr-to-pointer conversion.
	src := unsafe.Slice((*byte)(m.PBits), n)
	if len(p.copyBuf[i]) < n {
		p.copyBuf[i] = make([]byte, n)
	}
	copy(p.copyBuf[i], src)
	// The desktop surface is top-down, so the layout says so with a negative
	// height and no flip is needed.
	return p.copyBuf[i][:n], DIBLayout{Width: p.width, Height: -p.height, Stride: stride, BitCount: 32}, nil
}

// captureTexture copies the acquired GPU texture into ring slot i's staging
// texture and maps it, leaving it mapped so the consumer can borrow it.
func (p *duplicator) captureTexture(i int, resource unsafe.Pointer) ([]byte, DIBLayout, error) {
	if resource == nil {
		return nil, DIBLayout{}, hr("IDXGIOutputDuplication::AcquireNextFrame", eFail,
			"no desktop resource returned")
	}
	tex, err := queryInterface(resource, &iidID3D11Texture2D,
		"IDXGIResource::QueryInterface(ID3D11Texture2D)")
	if err != nil {
		return nil, DIBLayout{}, err
	}
	defer release(tex)

	var desc d3d11Texture2DDesc
	syscall.SyscallN(slot(tex, slotTexture2DGetDesc), uintptr(tex), uintptr(unsafe.Pointer(&desc)))
	if desc.Format != dxgiFormatB8G8R8A8Unorm {
		return nil, DIBLayout{}, fmt.Errorf(
			"%w: the duplicated texture is DXGI format %d, not B8G8R8A8_UNORM (%d)",
			ErrBackendUnavailable, desc.Format, dxgiFormatB8G8R8A8Unorm)
	}

	// Unmap and drop a staging texture whose size no longer matches — which
	// happens after a display mode change — then create the one this frame
	// needs. CopyResource between mismatched descriptions silently does
	// NOTHING, so this check is what stands between the consumer and a stream
	// of untouched buffers.
	if p.staging[i] != nil && !p.stagingMatches(i, desc) {
		p.dropStaging(i)
	}
	if p.staging[i] == nil {
		sd := stagingDesc(desc)
		var st unsafe.Pointer
		r, _, _ := syscall.SyscallN(slot(p.device, slotDeviceCreateTexture2D),
			uintptr(p.device), uintptr(unsafe.Pointer(&sd)), 0, uintptr(unsafe.Pointer(&st)))
		if err := hr("ID3D11Device::CreateTexture2D(staging)", HRESULT(r)); err != nil {
			return nil, DIBLayout{}, err
		}
		p.staging[i] = st
	} else if p.mapped[i] {
		syscall.SyscallN(slot(p.context, slotContextUnmap), uintptr(p.context), uintptr(p.staging[i]), 0)
		p.mapped[i] = false
	}

	// CopyResource returns void; a failure shows up as a D3D debug-layer
	// message nobody reads, which is why the description check above is not
	// optional.
	syscall.SyscallN(slot(p.context, slotContextCopyResource),
		uintptr(p.context), uintptr(p.staging[i]), uintptr(tex))

	var m d3d11MappedSubresource
	r, _, _ := syscall.SyscallN(slot(p.context, slotContextMap),
		uintptr(p.context), uintptr(p.staging[i]), 0,
		uintptr(d3d11MapRead), 0, uintptr(unsafe.Pointer(&m)))
	if err := hr("ID3D11DeviceContext::Map(staging)", HRESULT(r)); err != nil {
		return nil, DIBLayout{}, err
	}
	if m.PData == nil || m.RowPitch == 0 {
		return nil, DIBLayout{}, hr("ID3D11DeviceContext::Map(staging)", eFail,
			fmt.Sprintf("row pitch %d", m.RowPitch))
	}
	p.mapped[i] = true
	stride := int(m.RowPitch)
	h := int(desc.Height)
	// PData is a pointer the driver wrote, so no uintptr is involved and the
	// slice is a true borrow: nothing is copied on this path at all.
	pix := unsafe.Slice((*byte)(m.PData), stride*h)
	return pix, DIBLayout{Width: int(desc.Width), Height: -h, Stride: stride, BitCount: 32}, nil
}

// stagingMatches reports whether slot i's staging texture still has the size
// and format a newly acquired frame needs.
func (p *duplicator) stagingMatches(i int, want d3d11Texture2DDesc) bool {
	var have d3d11Texture2DDesc
	syscall.SyscallN(slot(p.staging[i], slotTexture2DGetDesc),
		uintptr(p.staging[i]), uintptr(unsafe.Pointer(&have)))
	return have.Width == want.Width && have.Height == want.Height && have.Format == want.Format
}

// dropStaging unmaps and destroys one slot's staging texture.
func (p *duplicator) dropStaging(i int) {
	if p.staging[i] == nil {
		return
	}
	if p.mapped[i] {
		syscall.SyscallN(slot(p.context, slotContextUnmap), uintptr(p.context), uintptr(p.staging[i]), 0)
		p.mapped[i] = false
	}
	release(p.staging[i])
	p.staging[i] = nil
}

// Close releases every COM object. It is idempotent.
func (p *duplicator) Close() {
	p.closeOnce.Do(func() {
		p.releaseFrame()
		for i := range p.staging {
			p.dropStaging(i)
		}
		if p.dupl != nil {
			release(p.dupl)
			p.dupl = nil
		}
		if p.output != nil {
			release(p.output)
			p.output = nil
		}
		if p.context != nil {
			release(p.context)
			p.context = nil
		}
		if p.device != nil {
			release(p.device)
			p.device = nil
		}
	})
}

// isAccessLost reports whether a code means the duplication was torn down and
// must be rebuilt rather than that the capture failed.
func isAccessLost(c HRESULT) bool {
	switch c {
	case dxgiErrorAccessLost, dxgiErrorSessionDisconnected,
		dxgiErrorDeviceRemoved, dxgiErrorDeviceReset, dxgiErrorDeviceHung,
		dxgiErrorInvalidCall:
		return true
	}
	return false
}
