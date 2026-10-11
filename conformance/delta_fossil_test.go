package conformance

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// fossil's delta functions live in its SQL layer, which wants a repository
// even though these never read one. test-delta-apply cannot serve: it exits 0
// and writes an empty file whether it applied the delta or rejected it.

// deltaRepo returns the scratch repository the SQL functions run against,
// creating it on first use.
func (f *fossilReference) deltaRepo() (string, error) {
	path := filepath.Join(f.dir, "delta-scratch.fossil")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if output, err := f.command(context.Background(), "new", path).CombinedOutput(); err != nil {
		return "", fmt.Errorf("fossil new: %v: %s", err, output)
	}
	return path, nil
}

// blobLiteral is b as an SQL hex literal. Bytes travel as hex so that NUL,
// quotes and the bytes 0x80-0xFF reach fossil unchanged.
func blobLiteral(b []byte) string {
	return "x'" + hex.EncodeToString(b) + "'"
}

// deltaSQL evaluates one SELECT expression. fossil reports a rejected delta
// as "Error: corrupt fossil delta", with exit status 0; any other failure,
// such as the crash on a zero-length blob, is an error, not a verdict.
func (f *fossilReference) deltaSQL(expr string) (out string, rejected bool, err error) {
	if f == nil {
		panic("conformance: deltaSQL without fossil")
	}
	repo, err := f.deltaRepo()
	if err != nil {
		return "", false, err
	}
	output, runErr := f.command(context.Background(), "sql", "-R", repo, "SELECT "+expr+";").
		CombinedOutput()
	text := strings.TrimSpace(string(output))
	if strings.Contains(text, "corrupt fossil delta") {
		return "", true, nil
	}
	if runErr == nil {
		return text, false, nil
	}
	return "", false, fmt.Errorf("fossil sql %.60s: %v: %s", expr, runErr, text)
}

// refuseEmptyDelta stops a zero-length delta on its way to fossil, whose
// delta_apply and delta_output_size crash on one (SIGSEGV).
func refuseEmptyDelta(d []byte) error {
	if len(d) == 0 {
		return fmt.Errorf("conformance: a zero-length delta must not be sent to fossil")
	}
	return nil
}

// applyDelta is delta_apply(src, d): the output, or rejected.
func (f *fossilReference) applyDelta(src, d []byte) (out []byte, accepted bool, err error) {
	if err := refuseEmptyDelta(d); err != nil {
		return nil, false, err
	}
	text, rejected, err := f.deltaSQL(fmt.Sprintf("hex(delta_apply(%s,%s))",
		blobLiteral(src), blobLiteral(d)))
	if err != nil || rejected {
		return nil, false, err
	}
	out, err = hex.DecodeString(text)
	return out, err == nil, err
}

// deltaSize is delta_output_size(d). fossil keeps the size in a signed int,
// so a header of 2^31 or more is reported as rejected.
func (f *fossilReference) deltaSize(d []byte) (size uint64, accepted bool, err error) {
	if err := refuseEmptyDelta(d); err != nil {
		return 0, false, err
	}
	text, rejected, err := f.deltaSQL(fmt.Sprintf("delta_output_size(%s)", blobLiteral(d)))
	if err != nil || rejected {
		return 0, false, err
	}
	size, err = strconv.ParseUint(text, 10, 64)
	return size, err == nil, err
}

// createDelta is delta_create(src, tgt), fossil's own encoder.
func (f *fossilReference) createDelta(src, tgt []byte) ([]byte, error) {
	text, rejected, err := f.deltaSQL(fmt.Sprintf("hex(delta_create(%s,%s))",
		blobLiteral(src), blobLiteral(tgt)))
	if err != nil {
		return nil, err
	}
	if rejected {
		return nil, fmt.Errorf("fossil delta_create rejected its input")
	}
	return hex.DecodeString(text)
}
