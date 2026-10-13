//go:build !unix

package testutil

import (
	"os"
	"os/exec"
	"testing"
)

// StartServer is the non-unix fallback: no process groups, so only the parent
// is killed and reaped. The #352 race therefore remains on these platforms.
func StartServer(t *testing.T, bin string, args ...string) *exec.Cmd {
	t.Helper()
	if bin == "" {
		panic("testutil.StartServer: empty bin")
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s %v: %v", bin, args, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}
