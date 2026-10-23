---
title: First sync
weight: 40
---

Two repositories exchange check-ins with Fossil's sync protocol over HTTP. `libfossil repo serve` is a sync server; `Clone`, `Repo.Sync` and `Repo.Pull` are the client. Either side can be stock `fossil`. [Sync](/docs/concepts/sync/) explains the protocol.

## Serve a repository

```sh
libfossil -R notes.fossil repo serve
```

```text
serving /home/ana/notes.fossil on http://127.0.0.1:8080 (Ctrl-C to stop)
```

`--addr` changes the bind address. The server answers sync requests only; it has no web interface.

An unauthenticated client gets the capabilities of the `nobody` user, which a new repository sets as `fossil new` does: clone and read, no check-in. To let a client push, give a user the check-in capability `i` and a password:

```sh
libfossil -R notes.fossil repo user add --cap=i bo
libfossil -R notes.fossil repo user passwd bo
```

Both commands print the user's generated password.

## Clone

From the command line, with either tool:

```sh
libfossil repo clone http://127.0.0.1:8080 copy.fossil
fossil clone http://127.0.0.1:8080 copy2.fossil
```

```text
Cloned into copy.fossil
  Rounds:       2
  Blobs:        2
  Project-code: ec1581be7066e95dd3b9ee14b91ab3f258221ce4
```

`repo clone` also clones part of a repository: `--cone` by path and `--horizon` by age. See [Sparse cones](/docs/concepts/sparse-cones/) and [Horizons](/docs/concepts/horizons/).

## Clone, commit, push and pull from Go

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/danmestas/go-libfossil"
	_ "github.com/danmestas/go-libfossil/db/driver/modernc"
)

func main() {
	ctx := context.Background()
	url := "http://127.0.0.1:8080"

	r, res, err := libfossil.Clone(ctx, "copy.fossil", libfossil.NewHTTPTransport(url), libfossil.CloneOpts{})
	if err != nil {
		log.Fatal(err)
	}
	defer r.Close()
	fmt.Println("cloned", res.BlobsRecvd, "blobs in", res.Rounds, "rounds")

	tip, err := r.BranchTip("trunk")
	if err != nil {
		log.Fatal(err)
	}
	if _, _, err := r.Commit(libfossil.CommitOpts{
		Files:    []libfossil.FileToCommit{{Name: "TODO.md", Content: []byte("- ship\n")}},
		Comment:  "add a todo",
		User:     "bo",
		ParentID: tip,
	}); err != nil {
		log.Fatal(err)
	}

	pushed, err := r.Sync(ctx, libfossil.NewHTTPTransport(url), libfossil.SyncOpts{
		Push: true, Pull: true, User: "bo", Password: os.Getenv("NOTES_PASSWORD"),
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("sent", pushed.FilesSent, "received", pushed.FilesRecvd)

	pulled, err := r.Pull(ctx, url, libfossil.PullOpts{})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("pull rounds", pulled.Rounds)
}
```

```sh
NOTES_PASSWORD=... go run .
```

```text
cloned 2 blobs in 2 rounds
sent 2 received 0
pull rounds 1
```

`Clone` creates the file and fails if it exists. `Sync` with `Push` and `Pull` runs one session in both directions until neither side has anything the other lacks; `Pull` is a pull-only shorthand that takes a URL. Without `User`, a session runs as `nobody`. Pass a context with a deadline in a service: a stalled peer otherwise holds the call open.

The commit made in the clone went to the origin:

```sh
libfossil -R notes.fossil repo timeline
```

```text
fb5af4c078  2026-10-23 21:02  bo  add a todo
ee68ff62c5  2026-10-23 21:02  ana  first notes
```

## Next

- [Sync](/docs/concepts/sync/): the cards and rounds behind these calls.
- [Production patterns](/docs/guides/production/): contexts, errors and telemetry for a long-running service.
- [Signing](/docs/concepts/signing/): signed check-ins and signed horizon seeds.
