---
title: Extension Points
weight: 40
---

libfossil exposes three surfaces for consumers that want to instrument, extend, or replace pieces of the stack:

1. **Observer interfaces** for sync and checkout lifecycle events (`SyncObserver`, `CheckoutObserver`).
2. **SQLite driver registration** (`db.Register`) — pick the SQLite backend at link time.
3. **The `db.DB` abstraction** — an opaque wrapper around `database/sql` with Fossil-specific helpers. Treat it as internal; it is less stable than the observer and driver surfaces.

## Observer interfaces

Observers are the supported way to attach telemetry, logging, or metrics to a `Repo` without forking or wrapping the public API. Both interfaces are defined in the root `github.com/danmestas/go-libfossil` package and are context-free by design so that they can be reused across synchronous and asynchronous callers.

### `SyncObserver`

Source: `observer.go`.

```go
type SyncObserver interface {
    Started(info SessionStart)
    RoundStarted(round int)
    RoundCompleted(round int, stats RoundStats)
    Completed(info SessionEnd)
    Error(err error)
    HandleStarted(info HandleStart)
    HandleCompleted(info HandleEnd)
    TableSyncStarted(info TableSyncStart)
    TableSyncCompleted(info TableSyncEnd)
}
```

| Method | Fires when |
|---|---|
| `Started` | A client-side sync or clone session begins, before any rounds run. |
| `RoundStarted` | A new sync round starts (there are typically multiple rounds per session). |
| `RoundCompleted` | A sync round finishes — `RoundStats` carries per-round totals. |
| `Completed` | The sync/clone session ends successfully. |
| `Error` | The sync machinery reports a per-error event (protocol or transport). |
| `HandleStarted` | Server-side: an inbound sync/clone HTTP request is accepted. |
| `HandleCompleted` | Server-side: an inbound sync/clone request finishes. |
| `TableSyncStarted` | An extension table sync (for example `peer_registry`) is about to run. |
| `TableSyncCompleted` | An extension table sync finishes. |

Libfossil ships `NopSyncObserver()` and `StdoutSyncObserver()` as drop-in implementations.

### `CheckoutObserver`

Source: `observer.go`.

```go
type CheckoutObserver interface {
    ExtractStarted(info ExtractStart)
    ExtractFileCompleted(name string, change UpdateChange)
    ExtractCompleted(info ExtractEnd)
    ScanStarted(dir string)
    ScanCompleted(info ScanEnd)
    CommitStarted(info CommitStart)
    CommitCompleted(info CommitEnd)
    Error(err error)
}
```

| Method | Fires when |
|---|---|
| `ExtractStarted` | An extract or update begins writing files from a checkin into the working tree. |
| `ExtractFileCompleted` | One file has been written — `change` is `ChangeAdded`, `ChangeModified`, or `ChangeDeleted`. |
| `ExtractCompleted` | The extract/update finishes. |
| `ScanStarted` | A working-tree scan (status/change detection) begins for `dir`. |
| `ScanCompleted` | The working-tree scan finishes. |
| `CommitStarted` | A checkin commit begins after staging. |
| `CommitCompleted` | The commit lands — `info.UUID` and `info.RID` identify the new checkin. |
| `Error` | A per-error event fires from the checkout pipeline. |

### Minimal logging observer

```go
package main

import (
    "log"

    libfossil "github.com/danmestas/go-libfossil"
)

type logSync struct{ libfossil.SyncObserver } // embed nop for defaults

func (logSync) Started(info libfossil.SessionStart) {
    log.Printf("sync start project=%s push=%v pull=%v", info.ProjectCode, info.Push, info.Pull)
}

func (logSync) RoundCompleted(round int, s libfossil.RoundStats) {
    log.Printf("round %d sent=%d recv=%d", round, s.FilesSent, s.FilesRecvd)
}

func newLogSyncObserver() libfossil.SyncObserver {
    return logSync{SyncObserver: libfossil.NopSyncObserver()}
}
```

The embedded `NopSyncObserver()` provides no-op implementations of the methods you do not override, so you only need to define the events you care about. The same pattern applies to `CheckoutObserver` via `NopCheckoutObserver()`.

For a full real-world observer that emits spans and counters, see `observer/otel/otel.go`. It implements both interfaces and is the reference you should copy from when wiring up your own telemetry backend.

## Adding an OpenTelemetry (or other) observer

`observer/otel` lives in its own Go module (separate `go.mod`) so that the OpenTelemetry SDK dependency does not leak into consumers that do not want it. To use it, import the subpackage explicitly and hand the observer to each `Repo` operation that accepts one (`SyncOpts.Observer`, `CloneOpts.Observer`, `HandleOpts.Observer`, `CheckoutCreateOpts.Observer`, `CheckoutOpenOpts.Observer`).

```go
import (
    libfossil "github.com/danmestas/go-libfossil"
    "github.com/danmestas/go-libfossil/observer/otel"
)

obs := otel.NewSyncObserver() // implements libfossil.SyncObserver
_, err := repo.Sync(ctx, transport, libfossil.SyncOpts{
    Pull:     true,
    Observer: obs,
})
```

`otel.NewCheckoutObserver()` returns the matching `CheckoutObserver`. Any third-party telemetry library (Prometheus, Datadog, zap, etc.) should be vendored the same way: a sibling module that imports `libfossil` and implements the observer interfaces.

## SQLite driver interface

Source: `db/register.go`, `db/db.go`, `db/doc.go`.

libfossil never imports a SQLite driver directly. Instead, the `db` package exposes a single registration hook that a driver package calls from its `init()`:

```go
// db/register.go
type DriverConfig struct {
    Name     string
    BuildDSN func(path string, pragmas map[string]string) string
}

func Register(cfg DriverConfig)
```

Each `db.Open(path)` / `db.OpenWith(path, cfg)` call routes through the currently registered driver. Exactly one driver may be registered per process — `Register` panics if called twice, if `Name` is empty, or if `BuildDSN` is nil. Likewise, calling `Open` with no driver registered panics with a message pointing at the driver import paths.

The driver contract is intentionally small:

- **`Name`**: the name passed to `sql.Open`. The driver package is responsible for registering this name with `database/sql` (typically via a blank import of the underlying SQLite library).
- **`BuildDSN(path, pragmas)`**: return a DSN string that opens `path` and applies the given pragmas. libfossil calls `db.DefaultPragmas()` and merges any caller overrides before handing them to `BuildDSN`.

### Built-in drivers

| Driver | Import | When to use |
|---|---|---|
| `modernc` | `_ "github.com/danmestas/go-libfossil/db/driver/modernc"` | Default. Pure Go, no cgo, works on every `GOOS`/`GOARCH` libfossil supports. |
| `ncruces` | `_ "github.com/danmestas/go-libfossil/db/driver/ncruces"` | Uses `github.com/ncruces/go-sqlite3` (WASM-based SQLite). Pick this for WASM builds or environments where you need the ncruces feature set. |

### Registering a custom driver

Use this pattern when you want to wrap an instrumented SQLite build, redirect to a network-attached SQLite, or inject additional pragmas. Model the package after `db/driver/modernc/modernc.go`:

```go
package mydriver

import (
    "fmt"
    "strings"

    "github.com/danmestas/go-libfossil/db"
    _ "example.com/my/sqlite" // registers "mysqlite" with database/sql
)

func init() {
    db.Register(db.DriverConfig{
        Name:     "mysqlite",
        BuildDSN: buildDSN,
    })
}

func buildDSN(path string, pragmas map[string]string) string {
    var parts []string
    for k, v := range pragmas {
        parts = append(parts, fmt.Sprintf("_pragma=%s(%s)", k, v))
    }
    if len(parts) == 0 {
        return path
    }
    return fmt.Sprintf("file:%s?%s", path, strings.Join(parts, "&"))
}
```

Consumers then blank-import your package (`_ "example.com/mydriver"`) exactly like they would for `modernc` or `ncruces`.

## The `DB` abstraction

`github.com/danmestas/go-libfossil/db` wraps `*sql.DB` with Fossil-aware helpers: DSN construction, default-pragma setup, WAL/nolock handling for WASM, transaction scoping via `DB.WithTx`, and the `Querier` interface (satisfied by both `DB` and `Tx`) so repository code can be written transaction-agnostic. The exported surface is stable for reading — `SqlDB()`, `Path()`, `Driver()`, `Exec`/`Query*` — but the struct itself is treated as internal: the public `Repo` API is the intended extension point. Prefer writing a custom driver (above) over implementing `DB` yourself. See `db/db.go`, `db/config.go`, and `db/scan.go` for the source of truth.
