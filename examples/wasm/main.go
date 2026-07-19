// Example: build libfossil for a generic WASI Preview 1 wasm target.
//
// Target: GOOS=wasip1 GOARCH=wasm. The pure-Go modernc.org/sqlite driver
// uses syscalls not available under wasip1, so this example uses the
// ncruces driver, which ships SQLite itself as WebAssembly and is the
// supported backend for wasip1/js builds.
package main

import (
	"log"
	"os"
	"path/filepath"

	"github.com/danmestas/libfossil"
	_ "github.com/danmestas/libfossil/db/driver/ncruces"
)

func main() {
	// Use a fixed relative path inside whatever directory the WASI runtime
	// mapped in (e.g. wasmtime --dir=.). os.MkdirTemp is unreliable on
	// some WASI runtimes, so clear any stale file up front.
	repoPath := filepath.Join(".", "demo.fossil")
	_ = os.Remove(repoPath)
	defer os.Remove(repoPath)

	created, err := libfossil.Create(repoPath, libfossil.CreateOpts{User: "wasi"})
	if err != nil {
		log.Fatalf("create: %v", err)
	}
	if err := created.Close(); err != nil {
		log.Fatalf("close after create: %v", err)
	}
	log.Printf("created repo at %s", repoPath)

	repo, err := libfossil.Open(repoPath)
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer repo.Close()

	// Every fresh repo has an auto-generated project-code in the config table.
	projectCode, err := repo.Config("project-code")
	if err != nil {
		log.Fatalf("config project-code: %v", err)
	}
	log.Printf("project-code: %s", projectCode)

	// Commit one file to exercise blob insert, manifest parse, and deltas
	// on top of the ncruces SQLite VFS.
	rid, uuid, err := repo.Commit(libfossil.CommitOpts{
		Files:   []libfossil.FileToCommit{{Name: "hello.txt", Content: []byte("hello from wasip1\n")}},
		Comment: "initial commit from wasi",
		User:    "wasi",
	})
	if err != nil {
		log.Fatalf("commit: %v", err)
	}
	log.Printf("commit rid=%d uuid=%s", rid, uuid)

	entries, err := repo.Timeline(libfossil.TimelineOpts{Limit: 10})
	if err != nil {
		log.Fatalf("timeline: %v", err)
	}
	for _, e := range entries {
		log.Printf("timeline: %s %s — %s", e.Time.Format("2006-01-02 15:04:05"), e.UUID[:12], e.Comment)
	}

	files, err := repo.ListFiles(rid)
	if err != nil {
		log.Fatalf("list files: %v", err)
	}
	for _, f := range files {
		log.Printf("file: %s (%s)", f.Name, f.UUID[:12])
	}
	log.Println("done")
}
