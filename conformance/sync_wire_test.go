package conformance

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// The sync cases read every request and reply with the reader in this file,
// not with internal/xfer: the codec under test must not judge its own
// output. The reader knows only what the draft's Appendix B says about
// framing: which cards carry a byte-counted payload and where its size is.

// Media types of the sync draft, Section 4.1.
const (
	typeCompressed   = "application/x-fossil"
	typeDebug        = "application/x-fossil-debug"
	typeUncompressed = "application/x-fossil-uncompressed"
)

// wireCard is one line of a sync body, with the payload that follows it.
type wireCard struct {
	line    string
	payload []byte
	// padded is set when a payload is followed by an empty line: the LF a
	// cfile sender adds after a payload that does not end in one.
	padded bool
}

// op is the card's operator: its first SP-separated token.
func (c wireCard) op() string {
	op, _, _ := strings.Cut(c.line, " ")
	return op
}

// tokens splits the line on SP, as a canonical sender separates them.
func (c wireCard) tokens() []string { return strings.Split(c.line, " ") }

// compress builds the Section 4.1 container: the 4-byte big-endian length
// of raw, then a zlib stream at the given level.
func compress(raw []byte, level int) []byte {
	var out bytes.Buffer
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(raw)))
	out.Write(size[:])
	w, err := zlib.NewWriterLevel(&out, level)
	if err != nil {
		panic("conformance: zlib level " + strconv.Itoa(level))
	}
	if _, err := w.Write(raw); err != nil {
		panic("conformance: zlib write: " + err.Error())
	}
	if err := w.Close(); err != nil {
		panic("conformance: zlib close: " + err.Error())
	}
	return out.Bytes()
}

// decompress reads a Section 4.1 container and checks its length field.
func decompress(body []byte) ([]byte, error) {
	if len(body) < 6 {
		return nil, fmt.Errorf("compressed body of %d bytes", len(body))
	}
	if body[4] != 0x78 {
		return nil, fmt.Errorf("byte 4 is %#x, not a zlib header", body[4])
	}
	r, err := zlib.NewReader(bytes.NewReader(body[4:]))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if n := binary.BigEndian.Uint32(body[:4]); int(n) != len(raw) {
		return nil, fmt.Errorf("length field %d, %d bytes inflated", n, len(raw))
	}
	return raw, nil
}

// bodyText returns a body's card text, decompressing it when its media
// type says it is compressed.
func bodyText(ctype string, body []byte) ([]byte, error) {
	if len(body) == 0 {
		return nil, nil
	}
	mt, _, _ := strings.Cut(ctype, ";")
	if strings.EqualFold(strings.TrimSpace(mt), typeCompressed) {
		return decompress(body)
	}
	return body, nil
}

// parseCards splits card text into cards, stepping over each payload by
// its declared size. Comment lines are cards too; empty lines are not.
func parseCards(text []byte) ([]wireCard, error) {
	var cards []wireCard
	r := bufio.NewReader(bytes.NewReader(text))
	for {
		line, err := r.ReadString('\n')
		if err == io.EOF && line == "" {
			return cards, nil
		}
		if err != nil && err != io.EOF {
			return nil, err
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			continue
		}
		card := wireCard{line: line}
		size, err := payloadSize(card)
		if err != nil {
			return nil, err
		}
		if size > 0 {
			card.payload = make([]byte, size)
			if _, err := io.ReadFull(r, card.payload); err != nil {
				return nil, fmt.Errorf("%q: payload: %v", line, err)
			}
			next, err := r.Peek(1)
			card.padded = err == nil && next[0] == '\n'
		}
		cards = append(cards, card)
	}
}

// payloadSize is how many bytes follow a card line (Appendix B.6, B.7).
// config carries one more byte, its trailing LF, read with the payload.
func payloadSize(c wireCard) (int, error) {
	tok := c.tokens()
	var field string
	switch {
	case c.op() == "file" && (len(tok) == 3 || len(tok) == 4):
		field = tok[len(tok)-1]
	case c.op() == "cfile" && (len(tok) == 4 || len(tok) == 5):
		field = tok[len(tok)-1]
	case c.op() == "config" && len(tok) == 3:
		n, err := strconv.Atoi(tok[2])
		return n + 1, err
	case c.op() == "uvfile" && len(tok) == 6:
		flags, err := strconv.Atoi(tok[5])
		if err != nil || flags&5 != 0 {
			return 0, err
		}
		field = tok[4]
	default:
		return 0, nil
	}
	n, err := strconv.Atoi(field)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%q: size %q", c.line, field)
	}
	return n, nil
}

// lines returns the card lines of cards, payloads left out.
func lines(cards []wireCard) []string {
	out := make([]string, len(cards))
	for i, c := range cards {
		out[i] = c.line
	}
	return out
}

// find returns the cards whose line starts with prefix.
func find(cards []wireCard, prefix string) []wireCard {
	var out []wireCard
	for _, c := range cards {
		if strings.HasPrefix(c.line, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// index is the position of the first card whose line starts with prefix,
// or -1.
func index(cards []wireCard, prefix string) int {
	for i, c := range cards {
		if strings.HasPrefix(c.line, prefix) {
			return i
		}
	}
	return -1
}

// errorTexts returns the decoded text of every error card.
func errorTexts(cards []wireCard) []string {
	var out []string
	for _, c := range find(cards, "error ") {
		out = append(out, defossilize(strings.TrimPrefix(c.line, "error ")))
	}
	return out
}

// defossilize undoes fossil's escaping (draft-fossil-artifact-format
// Section 4), which TEXT and NAME tokens carry.
func defossilize(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			out.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 's':
			out.WriteByte(' ')
		case 'n':
			out.WriteByte('\n')
		case 't':
			out.WriteByte('\t')
		case 'r':
			out.WriteByte('\r')
		case 'v':
			out.WriteByte('\v')
		case 'f':
			out.WriteByte('\f')
		case '0':
			out.WriteByte(0)
		default:
			out.WriteByte(s[i])
		}
	}
	return out.String()
}

// fossilize escapes s the way a card argument is written.
func fossilize(s string) string {
	r := strings.NewReplacer("\\", `\\`, " ", `\s`, "\n", `\n`, "\t", `\t`,
		"\r", `\r`, "\v", `\v`, "\f", `\f`, "\x00", `\0`)
	return r.Replace(s)
}

// sha1Hex is the lowercase hex SHA-1 of data.
func sha1Hex(data string) string {
	sum := sha1.Sum([]byte(data))
	return hex.EncodeToString(sum[:])
}
