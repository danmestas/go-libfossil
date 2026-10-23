//go:build !js && !windows

// Package signtest makes throwaway signing keys for tests: an ssh-keygen key,
// or a gpg key in a private GNUPGHOME. A test is skipped when the tool is
// not installed.
package signtest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// SSHKey makes an ssh-keygen key of type kind ("ed25519", "ecdsa", "rsa") and
// returns the pgp-command that signs with it in namespace and its public key
// line.
func SSHKey(t testing.TB, kind, namespace string) (command, public string) {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not installed")
	}
	path := filepath.Join(t.TempDir(), "id")
	keygen := exec.Command("ssh-keygen", "-q", "-t", kind, "-N", "", "-C", "origin", "-f", path)
	if out, err := keygen.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen -t %s: %v: %s", kind, err, out)
	}
	pub, err := os.ReadFile(path + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	return "ssh-keygen -q -Y sign -n " + namespace + " -f '" + path + "'", strings.TrimSpace(string(pub))
}

// GPGKey makes a gpg home holding one ed25519 signing key, sets GNUPGHOME
// for the test, and returns the pgp-command that signs with it without a
// passphrase prompt and its fingerprint.
func GPGKey(t *testing.T) (command, fingerprint string) {
	t.Helper()
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not installed")
	}
	// gpg-agent walks every possible descriptor when it starts, so under a
	// huge open-file limit (a macOS shell can set 2^31) it never finishes
	// starting. Setting the limit here also stops Go restoring the original
	// one for the processes it starts.
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err == nil && lim.Cur > 4096 {
		lim.Cur = 4096
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
			t.Fatalf("lower the open-file limit for gpg-agent: %v", err)
		}
	}
	// A short home, since the agent's socket path must fit in sun_path.
	home, err := os.MkdirTemp("", "gpg")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GNUPGHOME", home)
	t.Cleanup(func() {
		_ = exec.Command("gpgconf", "--homedir", home, "--kill", "all").Run()
		os.RemoveAll(home)
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	gen := exec.CommandContext(ctx, "gpg", "--batch", "--quiet", "--pinentry-mode", "loopback",
		"--passphrase", "", "--quick-gen-key", "Origin <origin@example.org>", "ed25519", "sign", "never")
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("gpg --quick-gen-key: %v: %s", err, out)
	}
	out, err := exec.Command("gpg", "--list-keys", "--with-colons").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Split(line, ":"); len(f) > 9 && f[0] == "fpr" {
			return "gpg --batch --pinentry-mode loopback --passphrase '' --clearsign -o", f[9]
		}
	}
	t.Fatal("gpg listed no fingerprint")
	return "", ""
}
