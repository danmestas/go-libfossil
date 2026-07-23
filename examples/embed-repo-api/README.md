# embed-repo-api

A minimal example showing how to embed `libfossil` as a library in a Go
application. It walks the basic repo lifecycle:

1. Register the default pure-Go SQLite driver via blank import
   (`_ "github.com/danmestas/go-libfossil/db/driver/modernc"`).
2. Create a new `.fossil` repository in a temp directory with
   `libfossil.Create`.
3. Close the handle returned by `Create` and re-open the file from disk
   with `libfossil.Open` to show the two calls are distinct.
4. Make one commit via `repo.Commit`, then read back:
   - the `project-code` config value (`repo.Config`)
   - the timeline of checkins (`repo.Timeline`)
   - the files in the manifest (`repo.ListFiles`)
5. Close the repo and remove the temp directory.

## Running

This example lives inside the root `github.com/danmestas/go-libfossil` module,
so no separate `go.mod` is needed.

From the repo root:

```
go run ./examples/embed-repo-api
```

or:

```
go build ./examples/embed-repo-api
```

Expected output includes log lines for the created repo path, the commit
RID and UUID, the `project-code`, one timeline entry, and one file entry
(`hello.txt`).
