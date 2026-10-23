//go:build !js

package clearsign

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/danmestas/go-libfossil/internal/deck"
)

// runClearsign runs COMMAND OUTFILE INFILE and returns OUTFILE.
func runClearsign(ctx context.Context, command string, plain []byte) ([]byte, error) {
	return inTempDir(func(dir string) ([]byte, error) {
		in, out := filepath.Join(dir, "manifest"), filepath.Join(dir, "signed")
		if err := os.WriteFile(in, plain, 0o600); err != nil {
			return nil, err
		}
		if err := runShell(ctx, command+" "+quote(out)+" "+quote(in)); err != nil {
			return nil, err
		}
		return readBounded(out)
	})
}

// runDetached runs COMMAND INFILE, as ssh-keygen -Y sign is run, and returns
// the INFILE.sig it writes.
func runDetached(ctx context.Context, command string, plain []byte) ([]byte, error) {
	return inTempDir(func(dir string) ([]byte, error) {
		in := filepath.Join(dir, "manifest")
		if err := os.WriteFile(in, plain, 0o600); err != nil {
			return nil, err
		}
		if err := runShell(ctx, command+" "+quote(in)); err != nil {
			return nil, err
		}
		return readBounded(in + ".sig")
	})
}

// inTempDir gives fn a private directory, removed afterwards: the names it
// hands the signer are its own, never caller input.
func inTempDir(fn func(dir string) ([]byte, error)) ([]byte, error) {
	dir, err := os.MkdirTemp("", "libfossil-sign-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	return fn(dir)
}

// runShell runs script through the shell, as fossil's system() and zit run
// pgp-command, so a setting such as "-f ~/.ssh/id_ed25519" expands.
func runShell(ctx context.Context, script string) error {
	shell, flag := "/bin/sh", "-c"
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/C"
	}
	cmd := exec.CommandContext(ctx, shell, flag, script)
	ownGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{buf: &stderr, max: 4096}
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func quote(path string) string {
	if runtime.GOOS == "windows" {
		return `"` + path + `"`
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

func readBounded(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxSignedBytes {
		return nil, fmt.Errorf("%s is %d bytes, over the %d-byte bound",
			filepath.Base(path), info.Size(), maxSignedBytes)
	}
	return os.ReadFile(path)
}

// verifyPGP has gpg verify signed and checks three things gpg alone does
// not: the signature is by the key fpr names and by no other, it is the only
// signature, and the text gpg verified is byte for byte the text a reader
// parses. The last matters because a clear-signature ignores trailing white
// space, so bytes can change, and with them the artifact's name, while the
// signature still verifies; for an artifact with a Z card the card catches
// that too, and this check covers every artifact.
func verifyPGP(ctx context.Context, fpr string, signed []byte) error {
	cmd := exec.CommandContext(ctx, "gpg", "--batch", "--no-tty", "--status-fd", "2",
		"--output", "-", "--decrypt")
	ownGroup(cmd)
	cmd.Stdin = bytes.NewReader(signed)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedWriter{buf: &stdout, max: maxSignedBytes}
	cmd.Stderr = &limitedWriter{buf: &stderr, max: 64 << 10}
	cmd.WaitDelay = 5 * time.Second
	runErr := cmd.Run()
	status := gpgStatus(stderr.String())
	// gpg that judged no signature (absent, killed at the deadline, unable to
	// read its home) has said nothing about this artifact: that is
	// ErrCannotVerify, which a caller must not take for a bad signature.
	if !slices.ContainsFunc(status, func(f []string) bool { return len(f) > 0 && f[0] == "NEWSIG" }) {
		return fmt.Errorf("%w: gpg: %v: %s", ErrCannotVerify, runErr, gpgMessage(stderr.String()))
	}
	if runErr != nil {
		return fmt.Errorf("%w: gpg: %v: %s", ErrBadSignature, runErr, gpgMessage(stderr.String()))
	}
	if err := checkGPGStatus(status, fpr); err != nil {
		return err
	}
	// Nothing may follow the signature: gpg ignores trailing bytes, which
	// would give a validly signed text other names.
	if !bytes.HasSuffix(signed, []byte("-----END PGP SIGNATURE-----\n")) {
		return fmt.Errorf("%w: bytes follow the PGP signature", ErrBadSignature)
	}
	if !bytes.Equal(stdout.Bytes(), deck.StripClearsign(signed)) {
		return fmt.Errorf("%w: the signed text differs from the artifact's", ErrBadSignature)
	}
	return nil
}

// gpgStatus returns gpg's --status-fd lines, keyword first.
func gpgStatus(stderr string) [][]string {
	var lines [][]string
	for _, line := range strings.Split(stderr, "\n") {
		if rest, ok := strings.CutPrefix(line, "[GNUPG:] "); ok {
			lines = append(lines, strings.Fields(rest))
		}
	}
	return lines
}

// gpgMessage is gpg's human output, without its status lines.
func gpgMessage(stderr string) string {
	var kept []string
	for _, line := range strings.Split(stderr, "\n") {
		if line != "" && !strings.HasPrefix(line, "[GNUPG:] ") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "; ")
}

func checkGPGStatus(status [][]string, fpr string) error {
	var sigs, good int
	var valid bool
	for _, f := range status {
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "NEWSIG":
			sigs++
		case "GOODSIG":
			good++
		case "BADSIG", "ERRSIG", "EXPSIG", "EXPKEYSIG", "REVKEYSIG":
			return fmt.Errorf("%w: gpg reports %s", ErrBadSignature, f[0])
		case "VALIDSIG":
			// VALIDSIG <fpr> <date> <ts> <expire> <ver> <rsv> <pk> <hash> <class> [<primary-fpr>]
			valid = valid || (len(f) > 1 && f[1] == fpr) || (len(f) > 10 && f[10] == fpr)
		}
	}
	if sigs != 1 || good != 1 {
		return fmt.Errorf("%w: want one good signature, gpg reports %d signatures, %d good",
			ErrBadSignature, sigs, good)
	}
	if !valid {
		return fmt.Errorf("%w: not signed by %s", ErrBadSignature, fpr)
	}
	return nil
}

// limitedWriter keeps the first max bytes written and drops the rest.
type limitedWriter struct {
	buf *bytes.Buffer
	max int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
