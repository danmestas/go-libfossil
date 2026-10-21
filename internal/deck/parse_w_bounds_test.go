package deck

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"testing"
)

// Use valid framing and a real Z checksum so oversized sizes reach parseW,
// rather than being rejected by a different validation stage.
func TestParseWRejectsOversizedBody(t *testing.T) {
	for _, size := range []string{
		"18446744073709551615",
		"18446744073709551616",
		"30000000000000000000",
		"9223372036854775807",
		"4294967295",
		"1000",
	} {
		t.Run(size, func(t *testing.T) {
			if _, err := Parse(wBoundsArtifact(size, nil)); err == nil {
				t.Fatal("accepted a W body larger than the artifact")
			}
		})
	}
}

// The overflow guard must preserve Fossil's leading-decimal token semantics,
// including empty payloads and a token whose decimal prefix is empty.
func TestParseWBoundsPreserveValidBodies(t *testing.T) {
	for _, tc := range []struct {
		size string
		body []byte
	}{
		{"0", nil},
		{"abc", nil},
		{"1x", []byte("x")},
		{"3", []byte("abc")},
	} {
		t.Run(tc.size, func(t *testing.T) {
			d, err := Parse(wBoundsArtifact(tc.size, tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if d.Type != Wiki || !bytes.Equal(d.W, tc.body) {
				t.Fatalf("type=%v body=%q, want wiki body=%q", d.Type, d.W, tc.body)
			}
		})
	}
}

func wBoundsArtifact(size string, body []byte) []byte {
	prefix := []byte(fmt.Sprintf("D 2026-10-20T00:00:00.000\nL Page\nU user\nW %s\n", size))
	prefix = append(prefix, body...)
	prefix = append(prefix, '\n')
	sum := md5.Sum(prefix)
	return append(prefix, []byte(fmt.Sprintf("Z %x\n", sum))...)
}
