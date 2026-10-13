package xfer

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const sizeTestHash = "0123456789abcdef0123456789abcdef01234567"

// badSizeTokens are tokens that strconv.Atoi accepts or that overflow, none
// of which is the draft's `decimal = 1*DIGIT` within fossil's 32-bit int.
var badSizeTokens = []string{
	"-1", "-0", "+5", "-2147483648", "0x10", "1e3", "5x", "５",
	"2147483648", "99999999999999999999",
}

// sizeCardTemplates holds one card line per size-bearing token; %s marks the
// token under test. Each is followed by enough payload bytes that a decoder
// accepting the token would have something to read.
var sizeCardTemplates = []string{
	"file " + sizeTestHash + " %s\n",
	"file " + sizeTestHash + " " + sizeTestHash + " %s\n",
	"cfile " + sizeTestHash + " %s 4\n",
	"cfile " + sizeTestHash + " 4 %s\n",
	"cfile " + sizeTestHash + " " + sizeTestHash + " %s 4\n",
	"config /project %s\n",
	"uvfile name.txt 0 " + sizeTestHash + " %s 0\n",
	"uvfile name.txt 0 " + sizeTestHash + " 4 %s\n",
	"schema t 1 " + sizeTestHash + " 0 %s\n",
	"xrow t " + sizeTestHash + " 0 %s\n",
	"xdelete t " + sizeTestHash + " 0 %s\n",
}

// TestDecodeRejectsNonDecimalSizes checks that every size token on a
// payload-bearing card is held to the draft's decimal grammar. Method: put
// each bad token into each size position and decode. A negative size used to
// reach readPayload's assertion and panic the process reading a peer's
// message; a signed or hex size was read as a size. None may be used as a
// size: the card must not parse as a valid payload card. §5.2 no longer fails
// the whole message for a bad card, so a file/cfile/uvfile line comes back as
// a *BadFileCard, a config line as a *BadCommandCard, and schema/xrow/xdelete
// (go-libfossil's own table-sync cards) still as a decode error.
func TestDecodeRejectsNonDecimalSizes(t *testing.T) {
	for _, tmpl := range sizeCardTemplates {
		for _, tok := range badSizeTokens {
			line := strings.Replace(tmpl, "%s", tok, 1)
			data := []byte(line + "abcdefgh\n")
			t.Run(strings.TrimSpace(line), func(t *testing.T) {
				defer func() {
					if p := recover(); p != nil {
						t.Fatalf("Decode panicked: %v", p)
					}
				}()
				msg, err := Decode(data, ContentTypeUncompressed)
				if err != nil {
					return // a decode error is one valid refusal
				}
				if len(msg.Cards) == 0 {
					t.Fatalf("Decode accepted size token %q with no card", tok)
				}
				switch msg.Cards[0].(type) {
				case *BadFileCard, *BadCommandCard:
					// refused as a bad card, which is what §5.2 now does
				default:
					t.Fatalf("Decode accepted size token %q as %T", tok, msg.Cards[0])
				}
			})
		}
	}
}

// allocatedBy reports the bytes f allocates on the heap.
func allocatedBy(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// allocBudget is far above what decoding a few-kilobyte message costs and
// far below the sizes these tests declare.
const allocBudget = 16 << 20

// TestDecodeHugeDeclaredSizeIsNotAllocated checks that a declared size is
// not trusted for allocation. Method: declare ~2 GiB on each payload card but
// send a few bytes, and measure what decoding allocates. Allocating the
// declared size up front lets a few bytes from a peer cost gigabytes.
func TestDecodeHugeDeclaredSizeIsNotAllocated(t *testing.T) {
	const huge = "2147483647"
	lines := []string{
		"file " + sizeTestHash + " " + huge + "\n",
		"cfile " + sizeTestHash + " 4 " + huge + "\n",
		"config /project " + huge + "\n",
		"uvfile name.txt 0 " + sizeTestHash + " " + huge + " 0\n",
	}
	for _, line := range lines {
		t.Run(strings.TrimSpace(line), func(t *testing.T) {
			data := []byte(line + "short payload")
			var msg *Message
			var err error
			n := allocatedBy(func() { msg, err = Decode(data, ContentTypeUncompressed) })
			if err == nil && !endsInBadCard(msg) {
				t.Fatal("Decode accepted a truncated payload")
			}
			if n > allocBudget {
				t.Fatalf("decoding %d bytes allocated %d bytes", len(data), n)
			}
		})
	}
}

// endsInBadCard reports whether a message's last card is one the decoder
// could not read: a payload cut short is a malformed file, cfile or uvfile
// line (Section 5.1), not a decode error.
func endsInBadCard(msg *Message) bool {
	if msg == nil || len(msg.Cards) == 0 {
		return false
	}
	_, ok := msg.Cards[len(msg.Cards)-1].(*BadFileCard)
	return ok
}

// zlibZeros compresses n zero bytes without holding them in memory.
func zlibZeros(t *testing.T, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	chunk := make([]byte, 64<<10)
	for written := 0; written < n; written += len(chunk) {
		if _, err := zw.Write(chunk[:min(len(chunk), n-written)]); err != nil {
			t.Fatalf("zlib write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	return buf.Bytes()
}

// TestDecodeCFileInflationIsBounded checks that a cfile cannot inflate past
// the size its card and prefix declare. Method: send a small zlib stream of
// 256 MiB of zeros under cards declaring a few bytes, in the plain and the
// prefixed form and as a delta, and measure what decoding allocates. Without
// a bound, ~256 KiB on the wire costs 256 MiB, and a message of such cards
// costs a multiple of that.
func TestDecodeCFileInflationIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("compresses 256 MiB")
	}
	bomb := zlibZeros(t, 256<<20)
	prefixed := binary.BigEndian.AppendUint32(nil, 16)
	prefixed = append(prefixed, bomb...)
	cases := []struct {
		name    string
		header  string
		payload []byte
	}{
		{"plain", "cfile " + sizeTestHash + " 16", bomb},
		{"prefixed", "cfile " + sizeTestHash + " 16", prefixed},
		{"delta", "cfile " + sizeTestHash + " " + sizeTestHash + " 16", bomb},
		{"delta prefixed", "cfile " + sizeTestHash + " " + sizeTestHash + " 16", prefixed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var data bytes.Buffer
			data.WriteString(tc.header)
			data.WriteString(" ")
			data.WriteString(strconv.Itoa(len(tc.payload)))
			data.WriteString("\n")
			data.Write(tc.payload)
			var err error
			n := allocatedBy(func() { _, err = Decode(data.Bytes(), ContentTypeUncompressed) })
			if err == nil {
				t.Fatal("Decode accepted a cfile inflating past its declared size")
			}
			if n > allocBudget+uint64(data.Len())*2 {
				t.Fatalf("decoding %d bytes allocated %d bytes", data.Len(), n)
			}
		})
	}
}

// TestDecodeCFileBudgetSpansMessage checks that cfile inflation is budgeted
// across a whole message, not per card. Method: decode two full cfiles, each
// inflating to exactly its own USIZE, under a budget that covers one of them.
// Each card alone is consistent with its declaration, so only a message-wide
// budget stops a message of many such cards from costing a multiple of it.
func TestDecodeCFileBudgetSpansMessage(t *testing.T) {
	const usize = 48 << 10
	payload := zlibZeros(t, usize)
	card := "cfile " + sizeTestHash + " " + strconv.Itoa(usize) + " " +
		strconv.Itoa(len(payload)) + "\n" + string(payload)

	if _, err := decodeMessage([]byte(card), usize); err != nil {
		t.Fatalf("one card within the budget: %v", err)
	}
	_, err := decodeMessage([]byte(card+card), usize+usize/2)
	if err == nil {
		t.Fatal("two cards over the message budget were accepted")
	}
	if !strings.Contains(err.Error(), "decode card 1") {
		t.Fatalf("budget did not stop the second card: %v", err)
	}
}

// FuzzDecode checks that no message a peer can send makes the decoder panic.
// A malformed message must come back as an error; the decoder runs on both
// sides of a sync, so a panic here stops a client or a server's handler.
func FuzzDecode(f *testing.F) {
	seeds := []string{
		"igot " + sizeTestHash + "\ngimme " + sizeTestHash + "\n",
		"file " + sizeTestHash + " 3\nabc",
		"file " + sizeTestHash + " " + sizeTestHash + " 3\nabc",
		"file " + sizeTestHash + " -1\nabc",
		"config /project 3\nabc\n",
		"uvfile a.txt 0 " + sizeTestHash + " 3 0\nabc",
		"uvfile a.txt 0 - 0 1\n",
		"xrow t " + sizeTestHash + " 0 2\n{}\n",
		"clone 3 1\nclone_seqno 5\npragma client-version 22800\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s), false)
	}
	msg := &Message{Cards: []Card{
		&CFileCard{UUID: sizeTestHash, Content: []byte("hello")},
		&CFileCard{UUID: sizeTestHash, DeltaSrc: sizeTestHash, USize: 9, Content: []byte("5\n0:;")},
		&FileCard{UUID: sizeTestHash, Content: []byte("x")},
	}}
	raw, err := msg.EncodeUncompressed()
	if err != nil {
		f.Fatalf("encode seed: %v", err)
	}
	f.Add(raw, false)
	compressed, err := msg.Encode()
	if err != nil {
		f.Fatalf("encode seed: %v", err)
	}
	f.Add(compressed, true)
	f.Fuzz(func(t *testing.T, data []byte, isCompressed bool) {
		contentType := ContentTypeUncompressed
		if isCompressed {
			contentType = ContentTypeCompressed
		}
		// Only a panic fails; any error is the decoder doing its job.
		_, _ = Decode(data, contentType)
	})
}
