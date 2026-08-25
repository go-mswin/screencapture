# go-mswin/screencapture

[![ci](https://github.com/go-mswin/screencapture/actions/workflows/ci.yml/badge.svg)](https://github.com/go-mswin/screencapture/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-mswin/screencapture.svg)](https://pkg.go.dev/github.com/go-mswin/screencapture)
[![Go Report Card](https://goreportcard.com/badge/github.com/go-mswin/screencapture)](https://goreportcard.com/report/github.com/go-mswin/screencapture)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD--3--Clause-blue.svg)](LICENSE)

Capture Windows displays and windows as raw BGRA pixels, in **pure Go with
`CGO_ENABLED=0`**. No cgo, no `exec.Command`, no PowerShell, no ffmpeg — DXGI
Desktop Duplication and GDI, reached through `golang.org/x/sys/windows` and
hand-built COM vtable calls.

It is the Windows sibling of [`go-macos/screencapture`](https://github.com/go-macos/screencapture)
and presents deliberately the **same shape**, so one consumer can drive both
platforms through near-identical adapters.

```go
ds, _ := screencapture.Displays(ctx)
s, _ := screencapture.CaptureDisplay(ctx, ds[0], screencapture.Options{FPS: 60})
defer s.Close()

for {
    f, fresh := s.Frame()   // 16 ns, 0 allocations, borrowed bytes
    if fresh {
        composite(f.Pix, f.Width, f.Height, f.Stride) // BGRA, top-down
    }
}
```

## Two backends

| | `BackendDuplication` | `BackendGDI` |
|---|---|---|
| API | `IDXGIOutputDuplication` | `BitBlt` / `PrintWindow` into a DIB section |
| Change-driven | **yes** — a still desktop delivers nothing | no — every tick costs a full read-back |
| Whole display | yes | yes |
| One window | no | **yes** (through DWM, even when occluded) |
| Resample | no | yes (`StretchBlt`, HALFTONE) |
| Cursor | no (delivered as separate metadata) | **yes** (`DrawIconEx`) |
| Over RDP / no GPU / session 0 | often refused | **works** |
| Copies on the hot path | **none** (mapped staging texture) | **none** (aliased DIB section) |

`BackendAuto`, the zero value, uses duplication when the request allows it and
falls back to GDI when it does not or when the adapter refuses.
`Stream.Backend()` and `Stream.Note()` say which happened and why.

## The two contract details that matter

**Stride is carried, never assumed.** `Frame.Stride` is the number of *bytes*
per row and is not necessarily `Width*4`. A DIB section at 32bpp happens to
land on exactly `Width*4`, which is precisely why the assumption survives long
enough on Windows to break somewhere else: a mapped D3D11 staging texture
reports whatever `RowPitch` the driver chose, commonly the width rounded up to
256 bytes. Index with `Stride`, or use `Frame.Row(y)`.

**Bottom-up DIBs are normalised inside the package.** A Windows DIB is
bottom-up by *default*: `BITMAPINFOHEADER.biHeight` is positive and row 0 is
the *bottom* row. This package always asks for a top-down DIB (a negative
`biHeight`) and flips anything that arrives bottom-up anyway. **Every `Frame`
handed to a consumer is top-down, row 0 at the top.** The rule lives in one
exported, fully tested place: `DIBLayout`.

**`Frame()` does not allocate.** Measured on a real Windows machine:

```
BenchmarkFrame-4    225069488    16.11 ns/op    0 B/op    0 allocs/op
```

It hands back a borrowed view of the capture buffer — the DIB section GDI
blitted into, or the mapped staging texture — valid until the next `Frame`,
`WaitFrame` or `Close`. A ring of buffers (`Options.QueueDepth`, minimum 3)
guarantees the capture never writes into the one you are holding.

## Pixel format

`FormatBGRA`: four bytes per pixel, blue, green, red, alpha. It is what Windows
produces natively at both ends — `DXGI_FORMAT_B8G8R8A8_UNORM` from duplication
and a 32bpp `BI_RGB` DIB from GDI — so nothing is converted on the hot path.

GDI's `BitBlt` does **not** fill the alpha channel; a screen capture comes back
with alpha 0. `Frame.NRGBA()` carries that through honestly, which makes a
saved PNG invisible, so `Frame.NRGBAOpaque()` sits next to it for anything you
intend to look at.

## Differences from the macOS sibling

Everything else is deliberately identical. These are the places Windows
genuinely differs:

- **`Options.Backend` is new.** macOS has one capture route; Windows has two
  with materially different properties.
- **`Authorized()` and `RequestAuthorization()` are trivially true.** Windows
  has no screen-recording permission gate at all. The failures that *do* happen
  are different in kind — a service or CI runner has no interactive desktop —
  and surface as errors from the capture call.
- **`ErrAccessLost` is new.** A duplication stream is torn down by the OS on a
  mode change, a UAC secure-desktop transition or a session switch. It
  re-establishes itself; the sentinel exists so a consumer *can* know.
- **`Options.ExcludeWindows` holds HWNDs** and is implemented with
  `SetWindowDisplayAffinity(WDA_EXCLUDEFROMCAPTURE)`, which the OS only permits
  on windows the calling process owns. Excluding someone else's window is
  `ErrPermissionDenied`, not a silent no-op. The affinity is restored on
  `Close`.
- **`Display.Width`/`Height` are device-independent pixels** at the monitor's
  DPI, the closest Windows equivalent of macOS points;
  `PixelWidth`/`PixelHeight` are real device pixels, read with the process
  marked per-monitor-DPI-aware.
- **`Rect` is integer** (Win32 `RECT` is), and its origin can be negative: the
  virtual screen's origin is the top-left of the *primary* display.
- **GDI cannot tell you nothing changed**, so with `BackendGDI` the freshness
  flag from `Frame()` is true on every tick. Only duplication is change-driven.

## Reuse

Built on [`go-mswin/win32`](https://github.com/go-mswin/win32), the fleet's
owned CGO-free Win32 foundation: its shared `User32`/`Gdi32`/`Kernel32` lazy DLL
handles, its `BitmapInfoHeader`, its `PackBGRA`. The capture-specific
procedures, and the dxgi/d3d11/shcore/dwmapi libraries win32 does not carry, are
bound here off those same handles rather than behind a second set of
`NewLazySystemDLL` calls.

### Why there is no `RtlMoveMemory` on the hot path

The fleet's usual way to read OS memory from Go is to copy it with
`RtlMoveMemory`, so `go vet`'s `unsafeptr` check never sees a `uintptr` turned
back into a pointer. This package cannot afford that: a copy of a 4K frame is
milliseconds out of a 16.6 ms budget, every frame.

The way out is to never hold the address as a `uintptr` at all.
`CreateDIBSection`, `ID3D11DeviceContext::Map` and
`IDXGIOutputDuplication::MapDesktopSurface` all write their result *through* a
caller-supplied pointer, so the destination is declared as a `*byte` or an
`unsafe.Pointer` and the OS fills it in. No `uintptr`-to-pointer conversion ever
happens in Go, `unsafeptr` has nothing to complain about, and `unsafe.Slice`
then makes a borrowed view with no copy. `go vet` is clean for `GOOS=windows` on
both amd64 and arm64.

## Verification

`cmd/wsccheck` is the protocol, not a demo. It enumerates, captures, and then
asserts the things that separate a working capture from the silent failures: the
frame is not uniformly one colour, it is the size that was asked for, the
content **changes** between frames, and `Frame()` allocates nothing. With
`-animate` it opens its own window and repaints it, so the test does not depend
on a human moving a mouse.

```
wsccheck -list
wsccheck -capture -animate -out C:\proof
wsccheck -capture -backend gdi -frames 300 -out C:\proof
```

The live Go suite is behind `//go:build windows && integration` plus
`SCREENCAPTURE_LIVE=1`:

```
set SCREENCAPTURE_LIVE=1
go test -tags integration -run Live -v .
go test -tags integration -run XXX -bench . -benchmem .
```

**Run it in an interactive session.** A process reached over ssh on Windows is
in session 0: it still reports a display (a 1024x768 phantom) and a capture of
it *succeeds* while showing nothing. Launch into session 1 with
`schtasks /ru <desktop user> /it`.

### What was proven, and where

On a Windows 11 ARM64 QEMU VM (`ramfb` + Microsoft Basic Display Driver,
800x600, no GPU) — full record in
[`testdata/artifacts/PROOF-2026-08-25.txt`](testdata/artifacts/PROOF-2026-08-25.txt):

- **A GDI capture matched QEMU's own independent `screendump` of the
  framebuffer in 0 of 480000 pixels.** An instrument that is not this package
  agrees exactly.
- **Duplication and GDI produced byte-identical PNGs** of the same still
  desktop.
- **The content changes**, driven from outside the process by QEMU's virtual
  keyboard: duplication 45 of 50 frames differed with 141 correctly-idle polls;
  GDI 15 of 300 (the desktop only changed 15 times); an occluded animated
  window 120 of 120.
- `Frame()`: **16.11 ns/op, 0 allocs/op** over 225 million iterations.
- The live suite: **7 of 7 PASS**, including `GetMonitorInfoW` accepting the Go
  `MONITORINFOEXW` (it rejects a wrong `cbSize`, so acceptance is direct
  evidence the struct layout is right).

Artefacts: [`display-duplication.png`](testdata/artifacts/display-duplication.png)
(the desktop through `IDXGIOutputDuplication`) and
[`window-printwindow.png`](testdata/artifacts/window-printwindow.png) (a fully
occluded animated window through `PrintWindow(PW_RENDERFULLCONTENT)`).

### What is NOT proven

State plainly rather than implied:

- **No real 4K screen read-back has been measured.** The only Windows machine
  available has an emulated 800x600 basic display adapter. Its screen read-back
  costs 23.6 ms for 0.48 Mpx (81 MB/s) — that number describes QEMU's ramfb, not
  Windows, and extrapolating it to 4K would be describing the wrong thing. What
  *is* measured is the DIB-section write path alone, memory to memory:
  **2.17 ms at 3840x2160** (15.3 GB/s), which fits a 16.6 ms budget with room.
  The real cost on real hardware is that plus a real read-back across the
  display driver.
- **The duplication GPU path is untested.** The VM has no GPU, so
  `DXGI_OUTDUPL_DESC.DesktopImageInSystemMemory` is true and duplication runs
  through `MapDesktopSurface`. The `CopyResource` + staging-texture path — the
  one every machine with a real GPU takes — compiles, is `vet`-clean, and has
  its structure layouts asserted, but has never executed.
- **Multi-monitor has not been exercised.** The adapter/output matching by GDI
  device name is written for it and is the reason `Display.AdapterIndex` exists,
  but the VM has one display.
- **Rotated displays are reported, not corrected.** `Display.Rotation` carries
  what DXGI says; a rotated panel's frames arrive in the panel's native
  orientation.
- **`ExcludeWindows` has not been exercised live.**
- **HDR / 10-bit surfaces are refused**, not converted: duplication reporting a
  format other than `B8G8R8A8_UNORM` is `ErrBackendUnavailable`.

## Testing

100% statement coverage on every portable file — `screencapture.go`,
`structs.go`, `ring.go`, `screencapture_other.go` — enforced in CI on Linux.
That covers option validation, backend selection, stride and bottom-up
normalisation, the C structure layouts, HRESULT-to-sentinel mapping and the
whole borrow ring. The Win32 and COM bindings cannot be covered without a
desktop, and a total-coverage gate would either be a lie or force a session a
runner cannot hold.

## Install

```
go get github.com/go-mswin/screencapture
```

Requires Go 1.26 and, at run time, Windows 8 or later on amd64 or arm64.
Compiles (and reports `ErrUnsupported`) everywhere else.

## Licence

BSD-3-Clause. See [LICENSE](LICENSE).
