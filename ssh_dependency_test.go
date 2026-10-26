package libfossil

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	// The signing tests verify SSH signatures in process, which the
	// library links only for a program that imports sshverify.
	_ "github.com/danmestas/go-libfossil/sshverify"
)

// sshImportPath is the in-process SSH signature code: x/crypto/ssh and the
// ECDSA, Ed25519 and RSA implementations it links.
const sshImportPath = "golang.org/x/crypto/ssh"

// goListDeps returns the import paths of pkg's dependency closure, built for
// goos/goarch, or for the host when goos is empty.
func goListDeps(t *testing.T, goos, goarch, pkg string) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", "-f={{.ImportPath}}", pkg)
	cmd.Env = os.Environ()
	if goos != "" {
		cmd.Env = append(cmd.Env, "GOOS="+goos, "GOARCH="+goarch)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", pkg, err, out)
	}
	return strings.Fields(string(out))
}

// TestSSHVerifyIsOptIn: the packages an embedder links (the library, db and
// simio) carry no SSH cryptography, for the host or for js/wasm, so a
// program that never verifies an SSH signature does not carry it; the
// sshverify package links it, and the libfossil command, which verifies,
// imports it.
func TestSSHVerifyIsOptIn(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go tool")
	}
	for _, target := range [][2]string{{"", ""}, {"js", "wasm"}} {
		for _, pkg := range []string{".", "./db", "./simio"} {
			if slices.Contains(goListDeps(t, target[0], target[1], pkg), sshImportPath) {
				t.Errorf("%s (GOOS=%q) links %s; import sshverify to opt in instead",
					pkg, target[0], sshImportPath)
			}
		}
	}
	for _, pkg := range []string{"./sshverify", "./cmd/libfossil"} {
		if !slices.Contains(goListDeps(t, "", "", pkg), sshImportPath) {
			t.Errorf("%s does not link %s", pkg, sshImportPath)
		}
	}
}
