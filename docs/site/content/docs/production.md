---
title: Production patterns
weight: 60
---

The [Quickstart](./quickstart) uses `log.Fatal` to keep the snippets short. This page shows the patterns you'll actually want when libfossil is embedded in a service.

## Error handling

Don't `log.Fatal` from a long-running process — return errors up to a caller that can log, retry, or fail the request.

```go
func openRepo(path string) (*libfossil.Repo, error) {
    repo, err := libfossil.Open(path)
    if err != nil {
        return nil, fmt.Errorf("open %s: %w", path, err)
    }
    return repo, nil
}
```

The errors you get back are wrapped Go errors; use `errors.Is` and `errors.As` against the sentinel errors exported from the [`libfossil` package](./reference/sdk/libfossil/api/) when you need to branch on a specific failure (e.g., "repo not found" vs. "schema mismatch").

## Context cancellation

`Sync`, `Pull`, and `Clone` accept a `context.Context`. Pass a request-scoped context with a deadline so a stalled remote doesn't tie up your goroutine forever.

```go
ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
defer cancel()

if _, err := repo.Sync(ctx, transport, libfossil.SyncOpts{Pull: true}); err != nil {
    return fmt.Errorf("sync: %w", err)
}
```

The HTTP transport honors the deadline at the network boundary; the rest of the sync state machine checks `ctx.Done()` between rounds.

## Telemetry

Wire an observer to your Repo's operations to surface sync rounds, checkout events, and per-error events to your existing logging or metrics stack. See [Extension Points](./extension-points) for the full interface, and [`observer/otel`](./reference/sdk/observer/otel/api/) for an OpenTelemetry-ready implementation that emits spans + counters.

```go
import (
    libfossil "github.com/danmestas/go-libfossil"
    otelobs "github.com/danmestas/go-libfossil/observer/otel"
)

obs := otelobs.NewSyncObserver()
_, err := repo.Sync(ctx, transport, libfossil.SyncOpts{
    Pull:     true,
    Observer: obs,
})
```

`observer/otel` is a separate Go module so the OpenTelemetry SDK doesn't leak into consumers that don't want it.

## Repo handle lifetime

A `*Repo` wraps a SQLite database connection pool. Open it once at service startup, keep it for the lifetime of the process, and `Close()` it during shutdown — don't open and close per request.

```go
type service struct {
    repo *libfossil.Repo
}

func newService(path string) (*service, error) {
    repo, err := libfossil.Open(path)
    if err != nil {
        return nil, err
    }
    return &service{repo: repo}, nil
}

func (s *service) Shutdown() error {
    return s.repo.Close()
}
```

Concurrent reads through one Repo are safe — SQLite handles the locking. Writes serialize at the database level; if your service has heavy write traffic, queue write operations through a single goroutine rather than relying on lock contention to back-pressure.

## Driver selection

For services targeting Linux/macOS/Windows, the default `modernc` driver is the right choice — pure Go, static binaries, no surprises. Switch to `ncruces` only when you're shipping a WASI/browser binary or when you need ncruces-specific behavior:

```go
import _ "github.com/danmestas/go-libfossil/db/driver/ncruces"
// instead of:
// import _ "github.com/danmestas/go-libfossil/db/driver/modernc"
```

Drivers are mutually exclusive — a process can register exactly one. See [`db`](./reference/sdk/db/api/) for the registry contract.
