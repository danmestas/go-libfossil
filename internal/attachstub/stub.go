// Package attachstub reads and writes the stub Quarry stores in place of an
// attachment's bytes (Quarry RFC 0016, "The stub"): a few lines of plain text
// naming a file by its SHA-256, kept in an ordinary blob that an A card names
// as its source, while the bytes live in a bucket the host binds. A fossil
// client that does not know the shape shows it as a small text file.
//
// The format is Quarry's internal/attach Stub, byte for byte. Parse is as
// strict as Quarry's parser and no stricter, so the two never disagree about
// which blobs are stubs.
package attachstub

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Header is the stub's first line, version included. A blob that does not
// start with it is a file, whatever else it says.
const Header = "quarry-attachment 1"

// MaxBytes bounds what Parse reads as a stub, as Quarry bounds it: the five
// lines run to a few hundred bytes at the longest name, so a stub is never
// larger and recognising one never reads more than this.
const MaxBytes = 1 << 10

// Ref names an attachment's bytes held outside the repository.
type Ref struct {
	// SHA256 is the bytes' SHA-256, 64 lowercase hex digits; the host's
	// bucket keys the object attachments/<SHA256>.
	SHA256 string
	// Size is the bytes' length.
	Size int64
	// Type is the mimetype the uploader declared.
	Type string
	// Name is the file's name.
	Name string
}

// Format returns ref's stub: the five lines in the RFC's order, each ending
// in one newline. It refuses a ref Parse would not read back as the same ref,
// so nothing written as a stub is later listed as a plain file.
func Format(ref Ref) ([]byte, error) {
	if !isSHA256(ref.SHA256) {
		return nil, fmt.Errorf("sha256 %q is not 64 lowercase hex digits", ref.SHA256)
	}
	if ref.Size < 0 {
		return nil, fmt.Errorf("size %d is negative", ref.Size)
	}
	if !singleLine(ref.Type) {
		return nil, fmt.Errorf("type %q is not one non-empty line", ref.Type)
	}
	if !singleLine(ref.Name) {
		return nil, fmt.Errorf("name %q is not one non-empty line", ref.Name)
	}
	var b strings.Builder
	b.WriteString(Header + "\n")
	b.WriteString("sha256 " + ref.SHA256 + "\n")
	b.WriteString("size " + strconv.FormatInt(ref.Size, 10) + "\n")
	b.WriteString("type " + ref.Type + "\n")
	b.WriteString("name " + ref.Name + "\n")
	if b.Len() > MaxBytes {
		return nil, fmt.Errorf("stub is %d bytes, over the %d a reader accepts", b.Len(), MaxBytes)
	}
	raw := []byte(b.String())
	if got, ok := Parse(raw); !ok || got != ref {
		panic("attachstub.Format: postcondition violated: stub does not parse back")
	}
	return raw, nil
}

// IsStub reports whether raw opens as a stub: its first line is Header. It
// does not say the stub is well formed; Parse does.
func IsStub(raw []byte) bool {
	return bytes.HasPrefix(raw, []byte(Header+"\n")) || string(raw) == Header
}

// Parse reads raw as a stub. ok is false when raw is not exactly one: the
// header, then each of sha256, size, type and name once, with no other line.
// One trailing newline is tolerated, because every editor adds one; a second
// is not, because a stub is written by a program and drift means the bytes
// are not what they claim.
func Parse(raw []byte) (ref Ref, ok bool) {
	ref, err := parse(raw)
	return ref, err == nil
}

func parse(raw []byte) (Ref, error) {
	if len(raw) > MaxBytes {
		return Ref{}, errors.New("too long")
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if lines[0] != Header {
		return Ref{}, errors.New("no header")
	}
	if len(lines) != 5 {
		return Ref{}, fmt.Errorf("%d lines, want 5", len(lines))
	}
	var ref Ref
	seen := make(map[string]bool, 4)
	for _, line := range lines[1:] {
		key, val, found := strings.Cut(line, " ")
		if !found || val == "" || seen[key] {
			return Ref{}, fmt.Errorf("line %q", line)
		}
		seen[key] = true
		switch key {
		case "sha256":
			if !isSHA256(val) {
				return Ref{}, fmt.Errorf("sha256 %q", val)
			}
			ref.SHA256 = val
		case "size":
			// The canonical decimal only: no sign, no leading zero.
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil || n < 0 || strconv.FormatInt(n, 10) != val {
				return Ref{}, fmt.Errorf("size %q", val)
			}
			ref.Size = n
		case "type":
			ref.Type = val
		case "name":
			ref.Name = val
		default:
			return Ref{}, fmt.Errorf("unknown key %q", key)
		}
	}
	if len(seen) != 4 {
		panic("attachstub.parse: four distinct known keys must have been seen")
	}
	return ref, nil
}

func isSHA256(s string) bool {
	return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == ""
}

// singleLine is what Format may put after a key: non-empty, with no line
// break. A carriage return is refused too, though Quarry's parser would keep
// it, so a stub written here never reads differently on a CRLF-minded tool.
func singleLine(s string) bool {
	return s != "" && !strings.ContainsAny(s, "\r\n")
}
