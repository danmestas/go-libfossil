//go:build unix

package testutil

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
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
	})
	return cmd
}
