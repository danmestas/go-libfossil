package xfer

import (
	"bytes"
	"testing"
)

// TestUVNameFossilized pins the NAME of uvfile, uvigot and uvgimme as
// fossilized on the wire (sync-protocol-01 Section 7.3 and Appendix B): a
// name with a space goes out escaped and comes back whole. fossil(1) 2.28
// sends the name raw and refuses an escaped one; the draft governs.
func TestUVNameFossilized(t *testing.T) {
	const name = "a b.txt"
	const hash = "1f09d30c707d53f3d16c530dd73d70a6ce7596a9"
	for _, tc := range []struct {
		card Card
		wire string
	}{
		{&UVFileCard{Name: name, MTime: 1, Hash: "-", Flags: 1},
			`uvfile a\sb.txt 1 - 0 1` + "\n"},
		{&UVIGotCard{Name: name, MTime: 1, Hash: hash, Size: 2},
			`uvigot a\sb.txt 1 ` + hash + " 2\n"},
		{&UVGimmeCard{Name: name}, `uvgimme a\sb.txt` + "\n"},
	} {
		var buf bytes.Buffer
		if err := EncodeCard(&buf, tc.card); err != nil {
			t.Fatal(err)
		}
		if buf.String() != tc.wire {
			t.Errorf("wire %q, want %q", buf.String(), tc.wire)
		}
		msg, err := DecodeUncompressed([]byte(tc.wire))
		if err != nil {
			t.Fatalf("%q: %v", tc.wire, err)
		}
		var got string
		switch c := msg.Cards[0].(type) {
		case *UVFileCard:
			got = c.Name
		case *UVIGotCard:
			got = c.Name
		case *UVGimmeCard:
			got = c.Name
		default:
			t.Fatalf("%q decoded as %T", tc.wire, c)
		}
		if got != name {
			t.Errorf("%q: NAME %q, want %q", tc.wire, got, name)
		}
	}
}
