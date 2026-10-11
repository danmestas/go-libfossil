package conformance

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/testutil"
)

// fossilReference runs fossil(1) on the same input a case gives
// go-libfossil, so every case's expectation is checked against fossil on
// every run rather than once, when the case was written.
type fossilReference struct {
	bin   string
	dir   string
	count int
}

// newFossilReference finds fossil(1). Without it the cases still run against
// go-libfossil, but nothing checks their expectations; CI sets
// REQUIRE_FOSSIL_BIN=1 so a missing binary fails there instead.
func newFossilReference(t *testing.T) *fossilReference {
	t.Helper()
	bin := testutil.FossilBinary()
	if bin == "" {
		if os.Getenv("REQUIRE_FOSSIL_BIN") == "1" {
			t.Fatal("REQUIRE_FOSSIL_BIN=1 but no fossil binary found")
		}
		t.Log("fossil not found: case expectations are not checked against fossil(1)")
		return nil
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &fossilReference{bin: bin, dir: dir}
}

// command builds a fossil(1) command whose global configuration lives in
// the reference's own directory. fossil records every checkout it opens,
// and more, in a per-user database (~/.fossil or ~/.config/fossil.db); a
// test must not write there. The last value of a repeated variable wins.
func (f *fossilReference) command(ctx context.Context, args ...string) *exec.Cmd {
	home := filepath.Join(f.dir, "home")
	cmd := exec.CommandContext(ctx, f.bin, args...)
	cmd.Env = append(os.Environ(), "HOME="+home, "FOSSIL_HOME="+home,
		"XDG_CONFIG_HOME="+home, "USER=u")
	return cmd
}

// parseArtifact reports whether fossil's manifest parser accepts data, and
// fossil's reason when it does not. test-parse-manifest exits 0 either way,
// so the verdict is read from its output.
func (f *fossilReference) parseArtifact(data []byte) (accepted bool, reason string, err error) {
	if f == nil {
		panic("conformance: parseArtifact without fossil")
	}
	f.count++
	path := filepath.Join(f.dir, fmt.Sprintf("artifact-%d", f.count))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return false, "", err
	}
	output, err := f.command(context.Background(), "test-parse-manifest", path).CombinedOutput()
	if err != nil {
		return false, "", fmt.Errorf("fossil test-parse-manifest: %v: %s", err, output)
	}
	text := string(output)
	if strings.Contains(text, "manifest_parse() worked") {
		return true, "", nil
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "ERROR: ") {
			return false, strings.TrimPrefix(line, "ERROR: "), nil
		}
	}
	return false, "", fmt.Errorf("fossil test-parse-manifest: unrecognized output %q", text)
}

// withZ appends the Z card for body: the MD5 of every byte before it. Cases
// build artifacts without one so that a single edit never also breaks the
// checksum, which would make any rejection look deserved.
func withZ(body string) []byte {
	if body != "" && !strings.HasSuffix(body, "\n") {
		panic("conformance: artifact body must end in a newline")
	}
	return []byte(body + zCard(body))
}

// zCard is the Z card line for data: "Z", a space, the lowercase MD5 of
// data, LF. Unlike withZ it accepts any data, for cases that put the Z
// card somewhere a well-formed artifact would not.
func zCard(data string) string {
	sum := md5.Sum([]byte(data))
	return "Z " + hex.EncodeToString(sum[:]) + "\n"
}
