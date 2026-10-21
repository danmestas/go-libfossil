//go:build unix

package testutil

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// StartServer runs `bin args...` as a long-lived server and stops it when the
// test ends. The server gets its own process group and cleanup kills the whole
// group before reaping the parent. `fossil server` forks a child per
// connection, so killing only the parent leaves a child that can still write
// journal/WAL files while t.TempDir()'s RemoveAll walks the directory, which
// fails cleanup with "directory not empty" (#352).
//
// Call it after t.TempDir() so its cleanup runs first (Cleanup is LIFO).
func StartServer(t *testing.T, bin string, args ...string) *exec.Cmd {
	t.Helper()
	if bin == "" {
		panic("testutil.StartServer: empty bin")
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s %v: %v", bin, args, err)
	}
	pgid := cmd.Process.Pid // Setpgid with Pgid 0 makes the child its own leader.
	t.Cleanup(func() {
		// SIGKILL cannot be caught, so no member can write after delivery.
		// ESRCH just means the group is already gone.
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = cmd.Wait()
		awaitGroupGone(pgid)
	})
	return cmd
}

// awaitGroupGone waits, for at most groupGoneTimeout, until no process is left
// in group pgid. Wait reaps only the leader; a per-connection child the signal
// has not reached yet can still be mid-write on a loaded machine, and
// RemoveAll then finds a file it did not list.
func awaitGroupGone(pgid int) {
	deadline := time.Now().Add(groupGoneTimeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

const groupGoneTimeout = 5 * time.Second
