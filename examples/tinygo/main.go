// Example: the whole public API, sync included, built with TinyGo for the
// js/wasm target, as an application ships go-libfossil to a browser or an
// edge runtime.
//
// It imports no SQLite driver. A TinyGo host supplies one for its own
// storage (a database/sql driver over the host's SQLite) and registers it;
// the ncruces driver, which embeds SQLite as a wasm module run by an
// interpreter, is for standard Go builds. CI builds this example with TinyGo
// so the module keeps compiling there.
//
// It imports sshverify, which links SSH signature verification, so the
// build covers a store whose horizon-seed-key is an SSH key. An application
// that never verifies an SSH signature leaves it out and is smaller.
package main

import (
	"context"
	"log"

	"github.com/danmestas/go-libfossil"
	_ "github.com/danmestas/go-libfossil/sshverify"
)

func main() {
	r, err := libfossil.Open("notes.fossil")
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer r.Close()
	t := libfossil.NewHTTPTransport("https://example.org/notes")
	res, err := r.Sync(context.Background(), t, libfossil.SyncOpts{Pull: true})
	if err != nil {
		log.Fatalf("sync: %v", err)
	}
	log.Printf("pulled %d artifacts in %d rounds", res.FilesRecvd, res.Rounds)
}
