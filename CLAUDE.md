# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`pictl` is a Go program that runs on a Raspberry Pi and drives GPIO lines wired to a PC's
front-panel header, so the machine can be powered on/off/reset remotely and its power LED read back.
`main.go` initializes the pins, starts a background LED sync goroutine, and serves an HTTP API plus
a small operator web page over a Unix socket.

## Commands

```bash
go build ./...            # compile
go vet ./...              # static checks — copylocks matters here, see Architecture
gofmt -l .                # should print nothing
go test ./...             # no test files exist yet
go test -run '^TestName$' ./pinctl/   # single test, once tests exist
GOOS=linux GOARCH=arm GOARM=6 go build -o pictl .   # cross-compile for the Pi (32-bit ARM)
```

## Running it

The program cannot run on the development machine: `pinctl.Initialize` requests real lines from
`gpiochip0` via the Linux GPIO character device, so it needs Pi hardware (and either root or
membership in the `gpio` group). Compilation, `go vet` and `gofmt` work fine locally.

The IDE is configured for remote development against the Pi over SSH
(`.idea/remote-targets.xml`: `indigo@pictl-via-proxy:3322`, rsync to `~/pictl`), so the normal loop
is edit locally, sync, build and run on the target.

It listens on a Unix socket (`socketPath` in `main.go`, currently `/home/indigo/pictl.sock`), chmod
`0660` — the socket's group owner is the access control, so there is no auth in the program itself.
Nothing reaches it over the network without a reverse proxy in front. To poke it by hand:

```bash
curl --unix-socket /home/indigo/pictl.sock -X POST http://localhost/api/power/short
```

### What is testable off-hardware

Anything that dereferences `pin.line` panics on a dev machine, which covers every press path. But
`GetHandler()` builds the route table without touching hardware, and `CancelPress` only reads a
struct field, so routing and the cancel endpoints are exercisable with `httptest` locally. The
press/release paths need a real Pi.

## Architecture

Two packages: `pinctl` owns the hardware, `server` owns HTTP. `server` imports `pinctl`; nothing
imports `server` except `main`.

### pinctl

Everything hardware-facing lives in `pinctl/pins.go`, built on `github.com/warthog618/go-gpiocdev`.
The package exposes three package-level pins as vars — `PowerSwitch`, `ResetSwitch` (output),
`PowerLED` (input) — populated by `Initialize()`. Callers use the exported methods on those vars;
there is no per-instance construction, and `Initialize()` must run before anything else.

Pin offsets are `rpi.GPIO*` constants at the top of the file; changing the wiring means changing
those constants. All lines are requested with `WithConsumer("pictl")` so the kernel attributes
ownership (visible in `gpioinfo`).

Invariants that are deliberate and easy to break accidentally:

- **A press is exclusive, not queued.** `outputPin.press` uses `mu.TryLock()`; a concurrent press
  returns `ErrButtonAlreadyPressed` (match with `errors.Is`) instead of waiting. The call blocks for
  the full press duration — 500ms short, 6s long — so callers that must stay responsive need their
  own goroutine.
- **Failing to release a pin is fatal on purpose.** If `setLow` fails, the code tries to close the
  line so the kernel drives it low; if that also fails it calls `log.Fatalf`. Crashing is preferred
  over leaving a power button virtually held down. Keep that escalation path intact when touching
  output handling.
- **Cancelling shortens a press; it never skips the release.** `press` selects on `ctx.Done()` vs
  the duration and runs `setLow` on both paths. A cancel that returned early would strand the line
  high — the exact failure the previous invariant exists to prevent.
- **`pressCtxCancel != nil` means "a press is in flight."** `press` stores the cancel func on entry
  and nils it in a `defer`. `CancelPress` depends on that to return `ErrNoPressToCancel` honestly
  instead of firing a cancel func whose press already finished. Dropping the defer makes the cancel
  endpoint report success while the machine sits idle, and `go vet`'s `lostcancel` will not catch it
  because the func escapes into a struct field.

**Two mutexes, and the order matters.** `mu` is held for the entire press duration (up to 6s);
`cancelMu` guards only the `pressCtxCancel` field. They cannot be merged: `CancelPress` runs from
another goroutine *while* `mu` is held, so guarding the cancel func with `mu` deadlocks. Never take
`mu` while holding `cancelMu` and no cycle is possible.

**Copying rules.** `pin` is copied by value into both the exported vars and the `pins` slice; that
works only because `line` is a pointer, so don't add value-typed mutable state to `pin` itself.
`outputPin` contains mutexes and must never be copied at all — always use pointer receivers and
address the package vars directly. `go vet`'s copylocks is the safety net; heed it.

`requestPin` appends every successfully opened line to the package-level `pins` slice, which
`CloseAll` walks; `CloseAll` joins all close errors and retains only the pins that failed to close,
so it is safe to retry.

`PowerLED` is kept current two ways: `gpiocdev.WithEventHandler(PowerLED.handleEdge)` updates
`currentStatus` on every edge, and `SyncValue` re-reads the line on a ticker (30s from `main`) as a
belt-and-braces guard against a missed event. `currentStatus` is an `atomic.Bool` so readers never
block on an incoming update.

### server

`server/server.go` holds the route table and handlers; `server/web/index.html` is the operator page,
`go:embed`ed so the binary is self-contained.

Routes: `GET /api/status`, `GET /api/events/status` (SSE, 250ms ticker), `POST /api/power/short`,
`POST /api/power/long`, `POST /api/power/cancel`, `POST /api/reset`, `POST /api/reset/cancel`, and
`GET /` serving the embedded page.

- **Handlers do not watch `r.Context()`.** A client disconnecting must not abandon a press half-done.
  The `press` helper runs the action to completion and only then maps the result to a status code.
- **Status mapping is via `errors.Is` on the `pinctl` sentinels.** `ErrButtonAlreadyPressed` and
  `ErrNoPressToCancel` are both 409 — the latter usually means the press ended on its own between
  the operator deciding to cancel and the request landing, which is a race, not a failure. Clients
  are expected to treat it as a non-event.
- **`GET /` is a catch-all**, so an unknown or wrong-method `GET /api/...` falls through to the file
  server and returns 404 rather than 405. Don't write tests expecting 405 on GET.

### The web page

`index.html` is plain JS, no build step — edit and re-embed by rebuilding. Its `ACTIONS` array
duplicates the routes *and* the press durations from `pinctl`; nothing enforces the match, and a
wrong `ms` desyncs the fill animation from the real press. Keep them in step, or add the durations
to `/api/status` and delete the duplication.

Each action button is a three-phase control: idle → arming (a local `COUNTDOWN_MS` countdown, during
which **nothing has been sent** and clicking aborts purely client-side) → pressing (clicking POSTs
the pin's `cancelPath`). Buttons sharing a `group` share a pin, so the group is disabled for the
whole cycle while the active button stays live as the cancel control.

## Repo state

This is a git repository on branch `master`, with no remote configured — commits are local only, so
there is no push to recover from. There are no tests yet, so `go build`, `go vet` and a run on the
Pi are the only verification available.