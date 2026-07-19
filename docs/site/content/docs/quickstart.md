---
title: Quickstart
weight: 10
---

A five-minute tour of libfossil: clone a repo and read its history from Go.

## Install

```sh
go install github.com/danmestas/libfossil/cmd/libfossil@latest
libfossil --help
```

The binary is pure-Go and CGo-free — drop it on any host.

## Clone a repository

Clone using the Go API:

```go
package main

import (
    "context"
    "log"

    "github.com/danmestas/libfossil"
)

func main() {
    t := libfossil.NewHTTPTransport("https://example.com/repo.fossil")
    repo, _, err := libfossil.Clone(context.Background(), "./repo.fossil", t, libfossil.CloneOpts{})
    if err != nil {
        log.Fatal(err)
    }
    defer repo.Close()
}
```

## Open from Go

After cloning (above), open the same `.fossil` file from Go and read its history:

```go
package main

import (
    "fmt"
    "log"

    "github.com/danmestas/libfossil"
)

func main() {
    repo, err := libfossil.Open("repo.fossil")
    if err != nil {
        log.Fatal(err)
    }
    defer repo.Close()

    entries, err := repo.Timeline(libfossil.TimelineOpts{Limit: 10})
    if err != nil {
        log.Fatal(err)
    }
    for _, e := range entries {
        fmt.Println(e.UUID, e.Comment)
    }
}
```

## Sync over HTTP

```go
package main

import (
    "context"
    "log"

    "github.com/danmestas/libfossil"
)

func main() {
    repo, err := libfossil.Open("repo.fossil")
    if err != nil {
        log.Fatal(err)
    }
    defer repo.Close()

    t := libfossil.NewHTTPTransport("https://example.com/repo.fossil")
    if _, err := repo.Sync(context.Background(), t, libfossil.SyncOpts{Pull: true}); err != nil {
        log.Fatal(err)
    }
}
```

Or use the simpler `Pull` wrapper for pull-only workflows:

```go
_, err := repo.Pull(context.Background(), "https://example.com/repo.fossil", libfossil.PullOpts{})
```

## Next steps

- [Architecture](../architecture) — how the pieces fit
- [Production patterns](../production) — error handling, context, telemetry beyond `log.Fatal`
- [Extension Points](../extension-points) — observers for telemetry, custom SQLite drivers
- [Migration from Fossil](../migration-from-fossil) — command mapping from upstream
- [SDK reference](../reference/sdk) — every package, every exported symbol
