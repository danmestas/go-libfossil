// Example: embed libfossil as a library in a Go application.
//
// Demonstrates the minimal lifecycle of a Fossil repository:
// create, open, write a commit, read the timeline, list files, close, clean up.
package main

import (
	"log"
	"os"
	"path/filepath"

	"github.com/danmestas/libfossil"
	// Blank-import the default pure-Go SQLite driver. libfossil requires
	// at least one driver to be registered; this one uses modernc.org/sqlite.
	_ "github.com/danmestas/libfossil/db/driver/modernc"
)

func main() {
	// 1. Pick a temp path for the .fossil file and ensure we clean it up.
	tmpDir, err := os.MkdirTemp("", "libfossil-embed-")
	if err != nil {
		log.Fatalf("mkdir temp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	repoPath := filepath.Join(tmpDir, "demo.fossil")

	// 2. Create a new repo. Create returns (*Repo, error) and opens the
	//    repo on success, but we close it here to demonstrate a clean
	//    open-from-disk step below.
	created, err := libfossil.Create(repoPath, libfossil.CreateOpts{User: "demo"})
	if err != nil {
		log.Fatalf("create: %v", err)
	}
	if err := created.Close(); err != nil {
		log.Fatalf("close after create: %v", err)
	}
	log.Printf("created repo at %s", repoPath)

	// 3. Open the repo fresh from disk.
	repo, err := libfossil.Open(repoPath)
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer repo.Close()

	// 4a. Write a commit so we have something to read.
	rid, uuid, err := repo.Commit(libfossil.CommitOpts{
		Files: []libfossil.FileToCommit{
			{Name: "hello.txt", Content: []byte("hello from libfossil\n")},
		},
		Comment: "initial commit",
		User:    "demo",
	})
	if err != nil {
		log.Fatalf("commit: %v", err)
	}
	log.Printf("commit rid=%d uuid=%s", rid, uuid)

	// 4b. Read the config — every repo has a project-code.
	projectCode, err := repo.Config("project-code")
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("project-code: %s", projectCode)

	// 4c. Read the repository-wide timeline (newest first).
	entries, err := repo.Timeline(libfossil.TimelineOpts{Limit: 10})
	if err != nil {
		log.Fatalf("timeline: %v", err)
	}
	for _, e := range entries {
		log.Printf("timeline: %s %s by %s — %s", e.Time.Format("2006-01-02 15:04:05"), e.UUID[:12], e.User, e.Comment)
	}

	// 4d. List files in the manifest we just wrote.
	files, err := repo.ListFiles(rid)
	if err != nil {
		log.Fatalf("list files: %v", err)
	}
	for _, f := range files {
		log.Printf("file: %s (%s)", f.Name, f.UUID[:12])
	}

	// 5. Close happens via defer above. 6. temp dir is removed via defer.
	log.Println("done")
}
