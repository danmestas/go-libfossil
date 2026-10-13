//go:build unix

package testutil

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A forked child must not outlive StartServer's cleanup (#352).
func TestStartServerKillsForkedChildren(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Run("server", func(t *testing.T) {
		script := `sleep 300 & echo $! > "$1"; wait`
		StartServer(t, "/bin/sh", "-c", script, "sh", pidFile)
		waitForFile(t, pidFile)
	})
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse child pid %q: %v", raw, err)
	}
	// Signal 0 probes existence; SIGKILL delivery is asynchronous, so poll.
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("forked child %d outlived the server cleanup", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) != "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
