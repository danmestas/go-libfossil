//go:build unix && !js

package clearsign

import (
	"os/exec"
	"syscall"
)

// ownGroup runs cmd in a process group of its own and has cancellation kill
// the whole group, so a signer the shell started (gpg, ssh-keygen, an agent
// prompt) dies with it at the deadline instead of outliving the shell.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
