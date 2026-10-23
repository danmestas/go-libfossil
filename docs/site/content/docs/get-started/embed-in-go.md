---
title: Embed in Go
weight: 30
---

The `libfossil` package does what the command does, as typed Go values instead of command output. This program creates a repository, commits a file and reads the history back.

## Set up

```sh
mkdir notes && cd notes
go mod init example.com/notes
go get github.com/danmestas/go-libfossil@latest github.com/danmestas/go-libfossil/db/driver/modernc@latest
```

The blank import of `db/driver/modernc` registers the SQLite driver. A program registers exactly one; see [Install](/docs/get-started/install/) for the choice.

## Create, commit, read

```go
package main

import (
	"fmt"
	"log"

	"github.com/danmestas/go-libfossil"
	_ "github.com/danmestas/go-libfossil/db/driver/modernc"
)

func main() {
	r, err := libfossil.Create("notes.fossil", libfossil.CreateOpts{User: "ana"})
	if err != nil {
		log.Fatal(err)
	}
	defer r.Close()

	rid, uuid, err := r.Commit(libfossil.CommitOpts{
		Files:   []libfossil.FileToCommit{{Name: "README.md", Content: []byte("# notes\n")}},
		Comment: "first notes",
		User:    "ana",
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("committed", uuid[:10], "rid", rid)

	entries, err := r.Timeline(libfossil.TimelineOpts{Limit: 10})
	if err != nil {
		log.Fatal(err)
	}
	for _, e := range entries {
		fmt.Println(e.UUID[:10], e.User, e.Comment)
	}

	files, err := r.ListFiles(rid)
	if err != nil {
		log.Fatal(err)
	}
	for _, f := range files {
		fmt.Println(f.Name)
	}
}
```

```sh
go run .
```

```text
committed ee68ff62c5 rid 2
ee68ff62c5 ana first notes
README.md
```

`Commit` returns the check-in's row id in the `blob` table and its full name. `CommitOpts.Files` lists file contents directly; no checkout or working directory is involved. To commit on top of an existing check-in, set `ParentID`: the files you pass replace the parent's entries of the same name and the rest of the parent's tree is carried forward, as `fossil commit` does.

The file is an ordinary Fossil repository: `fossil timeline -R notes.fossil` shows the same check-in.

## Open an existing repository

```go
r, err := libfossil.Open("notes.fossil")
if err != nil {
	log.Fatal(err)
}
defer r.Close()

tip, err := r.BranchTip("trunk")
if err != nil {
	log.Fatal(err)
}
files, err := r.ListFiles(tip)
```

`Open` fails if the file does not exist; `Create` fails if it does. Reads through one `*Repo` may run concurrently; writes serialize in SQLite. Close the handle once, when you are done with it. [Production patterns](/docs/guides/production/) covers error handling, contexts and handle lifetime.

## Next

- [First sync](/docs/get-started/first-sync/): clone, pull and push over HTTP.
- [SDK reference](/docs/reference/sdk/libfossil/api/): every exported type and function.
- [Examples](/docs/examples/): runnable programs in the repository.
