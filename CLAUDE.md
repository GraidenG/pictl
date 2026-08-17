# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`pictl` is a Go program that runs on a Raspberry Pi and drives GPIO lines wired to a PC's
front-panel header, so the machine can be powered on/off/reset remotely and its power LED read back.
It is early-stage: `main.go` currently just initializes the pins and closes them again. The comment
above `main` ("Initialize HTTP API server and goroutines") is a statement of intent — no HTTP server
or goroutine supervision exists yet.

## Commands

```bash
go build ./...            # compile
go vet ./...              # static checks
go test ./...             # no test files exist yet
go test -run '^TestName$' ./pinctl/   # single test, once tests exist
GOOS=linux GOARCH=arm64 go build -o pictl .   # cross-compile for the Pi
```

## Running it

The program cannot run on the development machine: `pinctl.Initialize` requests real lines from
`gpiochip0` via the Linux GPIO character device, so it needs Pi hardware (and either root or
membership in the `gpio` group). Compilation and `go vet` work fine locally.

The IDE is configured for remote development against the Pi over SSH
(`.idea/remote-targets.xml`: `indigo@pictl-via-proxy:3322`, rsync to `~/pictl`), so the normal loop
is edit locally, sync, build and run on the target.

## Architecture

Everything hardware-facing lives in `pinctl/pins.go`, built on `github.com/warthog618/go-gpiocdev`.
The package exposes three package-level pins as vars — `PowerSwitch`, `ResetSwitch` (output),
`PowerLED` (input) — populated by `Initialize()`. Callers use the exported methods on those vars;
there is no per-instance construction, and `Initialize()` must run before anything else.

Pin offsets are `rpi.GPIO*` constants at the top of the file; changing the wiring means changing
those constants. All lines are requested with `WithConsumer("pictl")` so the kernel attributes
ownership (visible in `gpioinfo`).

Two invariants in this package are deliberate and easy to break accidentally:

- **A press is exclusive, not queued.** `outputPin.press` uses `mu.TryLock()`; a concurrent press
  returns `ErrButtonAlreadyPressed` (match with `errors.Is`) instead of waiting. The call blocks for
  the full press duration — 500ms short, 6s long — so callers that must stay responsive need their
  own goroutine.
- **Failing to release a pin is fatal on purpose.** If `setLow` fails, the code tries to close the
  line so the kernel drives it low; if that also fails it calls `log.Fatalf`. Crashing is preferred
  over leaving a power button virtually held down. Keep that escalation path intact when touching
  output handling.

`requestPin` appends every successfully opened line to the package-level `pins` slice, which
`CloseAll` walks; `CloseAll` joins all close errors and retains only the pins that failed to close,
so it is safe to retry.

Note `inputPin.currentStatus` is never written — nothing subscribes to edge events yet, so
`PowerLED.GetStatus()` always reports `false`. Wiring that up (via `gpiocdev.WithEventHandler` or a
poll loop) is outstanding work, not a bug to paper over at the call site. Also be aware `pin` is
copied by value into both the exported vars and the `pins` slice; that works only because `line` is
a pointer, so don't add value-typed mutable state to `pin` itself.

## Repo state

This directory is not a git repository. Don't assume version control exists as a safety net when
making large edits.
