package xfer

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/hash"
)

// maxPayloadSize is the largest size token a card may carry. Fossil parses
// these tokens into a C int (xfer.c via blob_is_int), so no fossil peer can
// send or store a larger artifact; a larger token is not a size at all.
const maxPayloadSize = 1<<31 - 1

// payloadChunk is how much of a payload is allocated before its bytes arrive.
// A declared size is the peer's claim, so memory grows with bytes actually
// read rather than with the claim.
const payloadChunk = 64 << 10

// parseSize parses a card's size token. The sync draft's grammar makes every
// size `decimal = 1*DIGIT` (Appendix B), and fossil reads it with blob_is_int,
// which accepts digits only; strconv.Atoi alone would also take a sign. A bad
// token is the peer's error, returned as one, and never reaches readPayload's
// assertion.
func parseSize(tok string) (int, error) {
	if !isDecimal(tok) {
		return 0, fmt.Errorf("%q is not a decimal", tok)
	}
	n, err := strconv.ParseInt(tok, 10, 64)
	if err != nil || n > maxPayloadSize {
		return 0, fmt.Errorf("%q exceeds %d", tok, maxPayloadSize)
	}
	return int(n), nil
}

// readPayload reads exactly size bytes from the reader. Its buffer grows with
// the bytes read, doubling up to size, so a size that the message does not
// back fails on a short read after allocating about what was sent.
func readPayload(r *bufio.Reader, size int) ([]byte, error) {
	if r == nil {
		panic("xfer.readPayload: r must not be nil")
	}
	// Callers pass only sizes from parseSize; a peer cannot reach this.
	if size < 0 || size > maxPayloadSize {
		panic("xfer.readPayload: size out of range")
	}
	buf := make([]byte, 0, min(size, payloadChunk))
	for len(buf) < size {
		if len(buf) == cap(buf) {
			buf = slices.Grow(buf, min(size, 2*cap(buf))-len(buf))
		}
		n, err := io.ReadFull(r, buf[len(buf):min(size, cap(buf))])
		buf = buf[:len(buf)+n]
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return nil, fmt.Errorf("read payload: %w", err)
		}
	}
	return buf, nil
}

// cutShort reports whether a payload read failed because the body ended
// before the payload's declared count. Section 5.1 of sync-protocol-00 makes
// such a card a handler failure that names the card's malformed-line error, so the decoder yields a BadFileCard
// and stops, leaving the cards before it in effect. fossil(1) instead keeps
// the bytes present (blob_extract clamps the count); -00 is followed here.
func cutShort(err error) bool {
	return errors.Is(err, io.ErrUnexpectedEOF)
}

// readPayloadWithTrailingNewline reads size bytes then consumes one trailing \n.
func readPayloadWithTrailingNewline(r *bufio.Reader, size int) ([]byte, error) {
	buf, err := readPayload(r, size)
	if err != nil {
		return nil, err
	}
	b, err := r.ReadByte()
	if err == nil && b != '\n' {
		r.UnreadByte()
	}
	return buf, nil
}

// uvFileOmitsContent returns true if the flags indicate no binary payload follows.
func uvFileOmitsContent(flags int) bool {
	return flags&0x0001 != 0 || flags&0x0004 != 0
}

// DecodeCard reads one card from r. It skips comment lines (starting with #)
// and empty lines. Returns io.EOF when the reader is exhausted.
// Unrecognized command words produce an *UnknownCard (not an error).
// A cfile card may inflate to at most MaxCFileInflateBytes.
func DecodeCard(r *bufio.Reader) (Card, error) {
	budget := int64(MaxCFileInflateBytes)
	return decodeCard(r, &budget)
}

// decodeCard reads one card as DecodeCard does, charging any cfile payload's
// inflated size to budget, which the caller shares across a message.
func decodeCard(r *bufio.Reader, budget *int64) (Card, error) {
	if r == nil {
		panic("xfer.decodeCard: r must not be nil")
	}
	if budget == nil || *budget < 0 {
		panic("xfer.decodeCard: budget must be a non-negative count")
	}
	for {
		rawLine, err := r.ReadString('\n')
		// Handle final line without trailing newline
		if err != nil && err != io.EOF {
			return nil, err
		}
		line := strings.TrimRight(rawLine, "\n\r")

		// EOF with no data left
		if err == io.EOF && line == "" {
			return nil, io.EOF
		}

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			if err == io.EOF {
				return nil, io.EOF
			}
			continue
		}

		// rawLine keeps its terminating LF so a BadCommandCard quotes the
		// line exactly as fossil's %F(blob_str(&xfer.line)) does.
		return parseLine(r, rawLine, line, budget)
	}
}

// cardTokenWindow is how many tokens a receiver reads from a card line.
// Canonical fossil tokenizes each line into a fixed Blob aToken[6]
// (src/xfer.c), so a card is six tokens at most and a seventh and beyond are
// never seen -- the §3.9 window that makes a surplus token harmless.
const cardTokenWindow = 6

// isCardSpace reports whether c separates card tokens, matching fossil's
// fossil_isspace (src/blob.c): ASCII space and the controls \t through \r.
// A byte at or above 0x80, such as a UTF-8 no-break space, is not a
// separator, so a token may carry one.
func isCardSpace(c byte) bool {
	return c == ' ' || (c >= '\t' && c <= '\r')
}

// tokenizeCard splits a card line into at most cardTokenWindow tokens on runs
// of card whitespace, as fossil's blob_tokenize fills aToken[6] with
// blob_token. Tokens past the sixth are dropped, not merged into the last.
func tokenizeCard(line string) []string {
	tokens := make([]string, 0, cardTokenWindow)
	i, n := 0, len(line)
	for len(tokens) < cardTokenWindow {
		for i < n && isCardSpace(line[i]) {
			i++
		}
		if i >= n {
			break
		}
		start := i
		for i < n && !isCardSpace(line[i]) {
			i++
		}
		tokens = append(tokens, line[start:i])
	}
	return tokens
}

// parseLine dispatches a trimmed, non-empty line to the appropriate card
// parser, reading at most the §3.9 token window. A line no rule accepts
// becomes a *BadCommandCard, and a file/cfile/uvfile line that cannot be
// framed a *BadFileCard; neither is an error, so one malformed card no longer
// fails the whole message. The reader r is passed through for payload cards.
func parseLine(r *bufio.Reader, rawLine, line string, budget *int64) (Card, error) {
	fields := tokenizeCard(line)
	if len(fields) == 0 {
		return &BadCommandCard{Line: rawLine}, nil
	}
	cmd := fields[0]
	args := fields[1:]

	switch cmd {
	case "file":
		return parseFile(r, args)
	case "cfile":
		return parseCFile(r, args, budget)
	case "uvfile":
		return parseUVFile(r, args)
	case "config":
		return parseConfig(r, rawLine, args)
	case "schema":
		return parseSchema(r, args)
	case "xrow":
		return parseXRow(r, args)
	case "xdelete":
		return parseXDelete(r, args)
	}
	if c := parseControlCard(cmd, args); c != nil {
		return keepRawLine(c, rawLine), nil
	}
	return &BadCommandCard{Line: rawLine}, nil
}

// keepRawLine records the line as received on the cards a server answers with
// "bad command: LINE" when a client sends them (§5.2). fossil quotes
// blob_str(&xfer.line) verbatim, so re-encoding the parsed card would alter a
// spelling such as "clone_seqno 05" or a run of spaces.
func keepRawLine(c Card, rawLine string) Card {
	if !strings.HasSuffix(rawLine, "\n") {
		rawLine += "\n"
	}
	switch c := c.(type) {
	case *CloneSeqNoCard:
		c.Raw = rawLine
	case *ErrorCard:
		c.Raw = rawLine
	case *MessageCard:
		c.Raw = rawLine
	case *UnknownCard:
		c.Raw = rawLine
	}
	return c
}

// parseControlCard reads a card that carries no payload. It returns nil when
// the line is a bad command -- an unrecognized operator, or a known one whose
// arity breaks its rule -- which the caller turns into a *BadCommandCard. The
// §3.9 window already dropped any token past the sixth, so a card that accepts
// a fixed arity silently tolerates a surplus token (igot, private).
func parseControlCard(cmd string, args []string) Card {
	switch cmd {
	case "igot":
		if len(args) < 1 {
			return nil
		}
		c := &IGotCard{UUID: args[0]}
		if len(args) >= 2 && args[1] == "1" {
			c.IsPrivate = true
		}
		return c
	case "gimme":
		if len(args) != 1 {
			return nil
		}
		return &GimmeCard{UUID: args[0]}
	// push/pull: canonical (src/xfer.c) accepts them only at nToken==3 and
	// reads the project code it validates out of the second argument.
	case "push":
		if len(args) != 2 {
			return nil
		}
		return &PushCard{ServerCode: args[0], ProjectCode: args[1]}
	case "pull":
		if len(args) != 2 {
			return nil
		}
		return &PullCard{ServerCode: args[0], ProjectCode: args[1]}
	case "cookie":
		if len(args) != 1 {
			return nil
		}
		return &CookieCard{Value: args[0]}
	case "reqconfig":
		if len(args) != 1 {
			return nil
		}
		return &ReqConfigCard{Name: args[0]}
	case "private":
		// Canonical matches `private` on its keyword alone; a surplus token
		// is past the window and harmless.
		return &PrivateCard{}
	case "clone":
		return parseClone(args)
	case "clone_seqno":
		return parseCloneSeqNo(args)
	case "uvgimme":
		if len(args) != 1 {
			return nil
		}
		return &UVGimmeCard{Name: deck.FossilDecode(args[0])}
	case "pragma":
		return parsePragma(args)
	case "login":
		if len(args) != 3 {
			return nil
		}
		return &LoginCard{User: deck.FossilDecode(args[0]), Nonce: args[1], Signature: args[2]}
	case "error":
		if len(args) != 1 {
			return nil
		}
		return &ErrorCard{Message: deck.FossilDecode(args[0])}
	case "message":
		if len(args) != 1 {
			return nil
		}
		return &MessageCard{Message: deck.FossilDecode(args[0])}
	case "uvigot":
		return parseUVIGot(args)
	case "xigot":
		return parseXIGot(args)
	case "xgimme":
		return parseXGimme(args)
	}
	return nil
}

// isDecimal reports whether s matches the spec's `decimal = 1*DIGIT`
// production (Appendix B). Tokens such as "-1" parse as integers but are
// not decimal, and several card rules turn on that distinction rather than
// on the parsed value.
func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseClone returns nil for a line no clone rule accepts, so the caller
// makes it a bad command. A clone with two tokens neither of which parses as
// an integer degrades to an *UnknownCard rather than a bad command: §8.1
// withholds its fatal from a non-decimal SEQNO, and treating the whole
// message as bad would be harsher still.
func parseClone(args []string) Card {
	if len(args) != 0 && len(args) != 2 {
		return nil
	}
	if len(args) == 0 {
		return &CloneCard{}
	}
	v, vErr := strconv.Atoi(args[0])
	s, sErr := strconv.Atoi(args[1])
	if vErr != nil || sErr != nil {
		return &UnknownCard{Command: "clone", Args: args}
	}
	// §8.1 keys its fatal on a digit-only SEQNO and explicitly withholds it
	// from one that fails digit-only recognition, so record whether this
	// token qualifies rather than only what it parsed to.
	return &CloneCard{Version: v, SeqNo: s, SeqNoIsDecimal: isDecimal(args[1])}
}

// parseCloneSeqNo degrades a non-decimal NEXT to an *UnknownCard: §3.2 records
// NEXT only when it is decimal and §8.2 makes a visible but non-decimal NEXT a
// receiver-tolerance exception, so the token is kept but denied any effect on
// the cursor (a CloneSeqNoCard would be recorded).
func parseCloneSeqNo(args []string) Card {
	if len(args) != 1 {
		return nil
	}
	if !isDecimal(args[0]) {
		return &UnknownCard{Command: "clone_seqno", Args: args}
	}
	s, _ := strconv.Atoi(args[0]) // isDecimal guarantees this parses
	return &CloneSeqNoCard{SeqNo: s}
}

func parsePragma(args []string) Card {
	if len(args) < 1 {
		return nil
	}
	c := &PragmaCard{Name: args[0]}
	if len(args) > 1 {
		c.Values = args[1:]
	}
	return c
}

func parseUVIGot(args []string) Card {
	if len(args) != 4 {
		return nil
	}
	mtime, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return nil
	}
	size, err := strconv.Atoi(args[3])
	if err != nil {
		return nil
	}
	return &UVIGotCard{Name: deck.FossilDecode(args[0]), MTime: mtime, Hash: args[2], Size: size}
}

// --- Payload card parsers ---

// parseFile decodes: file UUID SIZE \n CONTENT  or  file UUID DELTASRC SIZE \n CONTENT
// No trailing newline after CONTENT.
func parseFile(r *bufio.Reader, args []string) (Card, error) {
	if len(args) != 2 && len(args) != 3 {
		return &BadFileCard{Message: "malformed file line"}, nil
	}
	c := &FileCard{UUID: args[0]}
	var sizeStr string
	if len(args) == 3 {
		c.DeltaSrc = args[1]
		sizeStr = args[2]
	} else {
		sizeStr = args[1]
	}
	size, err := parseSize(sizeStr)
	if err != nil {
		// A bad SIZE leaves the payload unframeable: fossil's xfer_accept_file
		// writes "malformed file line" and page_xfer breaks.
		return &BadFileCard{Message: "malformed file line"}, nil
	}
	content, err := readPayload(r, size)
	if cutShort(err) {
		return &BadFileCard{Message: "malformed file line"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("xfer: file payload: %w", err)
	}
	c.Content = content
	return c, nil
}

// parseCFile decodes: cfile UUID USIZE CSIZE \n ZCONTENT
// or: cfile UUID DELTASRC USIZE CSIZE \n ZCONTENT
// No trailing newline after ZCONTENT.
func parseCFile(r *bufio.Reader, args []string, budget *int64) (Card, error) {
	if len(args) != 3 && len(args) != 4 {
		return &BadFileCard{Message: "malformed cfile line"}, nil
	}
	c := &CFileCard{UUID: args[0]}
	var usizeStr, csizeStr string
	if len(args) == 4 {
		c.DeltaSrc = args[1]
		usizeStr = args[2]
		csizeStr = args[3]
	} else {
		usizeStr = args[1]
		csizeStr = args[2]
	}
	usize, err := parseSize(usizeStr)
	if err != nil {
		return &BadFileCard{Message: "malformed cfile line"}, nil
	}
	csize, err := parseSize(csizeStr)
	if err != nil {
		return &BadFileCard{Message: "malformed cfile line"}, nil
	}
	compressed, err := readPayload(r, csize)
	if cutShort(err) {
		return &BadFileCard{Message: "malformed cfile line"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("xfer: cfile payload: %w", err)
	}
	// Decompress zlib. Fossil's server may send blobs in its blob format
	// ([4-byte BE size prefix][zlib data]) or plain zlib. Try plain first
	// (matches our own encoder), fall back to skipping a 4-byte prefix
	// (matches Fossil's send_compressed_file which sends raw blob content).
	// decompressCFile also hands back storedBlob: the same bytes recast
	// into Fossil's on-disk blob format, built without a second zlib pass,
	// so a receive path can persist storedBlob verbatim instead of
	// recompressing decompressed content and getting different bytes.
	limit := min(cfileInflateLimit(usize, c.DeltaSrc != ""), *budget)
	decompressed, storedBlob, err := decompressCFile(compressed, limit)
	if err != nil {
		return nil, err
	}
	*budget -= int64(len(decompressed))
	// USIZE is metadata, not a frame boundary. fossil's
	// xfer_accept_compressed_file reads CSIZE bytes and never checks that the
	// inflated size equals USIZE; the content is bound by its hash at storage
	// time instead. So a USIZE that disagrees with the inflated size (a sender
	// counting a trailing byte, say) is carried through, not rejected (§7.2).
	c.USize = usize
	c.Content = decompressed
	c.StoredBlob = storedBlob
	return c, nil
}

// parseConfig decodes: config NAME SIZE \n CONTENT <1 byte>.
//
// Canonical (src/xfer.c) enters its config branch only at nToken==3 with a
// non-negative integer SIZE; otherwise the line is a bad command and no
// payload is read -- so a wrong arity or a signed SIZE degrades here to a
// *BadCommandCard, leaving the next line to the decoder.
//
// After the SIZE payload canonical does blob_seek(xfer.pIn, 1, BLOB_SEEK_CUR):
// it steps over exactly one byte whatever it is, not only a LF. A sender
// writes a LF there, but the receiver does not require one.
//
// A config payload cut short by the end of the body still fails the whole
// decode (an HTTP 400 upstream), unlike file, cfile and uvfile: Section 5.1
// names handler texts only for those three, so this is a deliberate choice,
// not an oversight; the cards before it are not kept.
func parseConfig(r *bufio.Reader, rawLine string, args []string) (Card, error) {
	if len(args) != 2 {
		return &BadCommandCard{Line: rawLine}, nil
	}
	name := args[0]
	size, err := parseSize(args[1])
	if err != nil {
		return &BadCommandCard{Line: rawLine}, nil
	}
	content, err := readPayload(r, size)
	if err != nil {
		return nil, fmt.Errorf("xfer: config payload: %w", err)
	}
	// Step over the one trailing byte, as canonical does. An EOF here (the
	// payload ended the message) is fine: there was nothing to step over.
	if _, err := r.ReadByte(); err != nil && err != io.EOF {
		return nil, fmt.Errorf("xfer: config trailing byte: %w", err)
	}
	return &ConfigCard{Name: name, Content: content}, nil
}

// parseUVFile decodes: uvfile NAME MTIME HASH SIZE FLAGS \n CONTENT
// FLAGS bits: 0x0001 = deleted, 0x0004 = content omitted. When set, no payload follows.
func parseUVFile(r *bufio.Reader, args []string) (Card, error) {
	// A malformed uvfile line leaves its payload unframeable: fossil's
	// xfer_accept_unversioned_file writes "malformed uvfile line" and
	// page_xfer breaks. Its checks are reproduced here -- arity, a NAME that
	// is a strict simple pathname, a non-negative MTIME, a HASH that is an
	// artifact name or "-", and SIZE and FLAGS that are non-negative
	// decimals (parseSize enforces the last). NAME is fossilized (draft
	// Section 7.3), so it is checked defossilized; fossil(1) checks the token
	// as sent and refuses any escape, sending names raw.
	if len(args) != 5 {
		return &BadFileCard{Message: "malformed uvfile line"}, nil
	}
	name := deck.FossilDecode(args[0])
	if !deck.StrictPathname(name) ||
		(args[2] != "-" && !hash.IsValidHash(args[2])) {
		return &BadFileCard{Message: "malformed uvfile line"}, nil
	}
	mtime, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || mtime < 0 {
		return &BadFileCard{Message: "malformed uvfile line"}, nil
	}
	size, err := parseSize(args[3])
	if err != nil {
		return &BadFileCard{Message: "malformed uvfile line"}, nil
	}
	flags, err := parseSize(args[4])
	if err != nil {
		return &BadFileCard{Message: "malformed uvfile line"}, nil
	}
	c := &UVFileCard{
		Name:  name,
		MTime: mtime,
		Hash:  args[2],
		Size:  size,
		Flags: flags,
	}
	if !uvFileOmitsContent(flags) {
		content, err := readPayload(r, size)
		if cutShort(err) {
			return &BadFileCard{Message: "malformed uvfile line"}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("xfer: uvfile payload: %w", err)
		}
		c.Content = content
	}
	return c, nil
}

// cfileInflateLimit bounds how far a cfile payload may inflate. Without it,
// a few kilobytes of zlib on the wire inflate to gigabytes, since zlib reaches
// about 1000:1. A full artifact must inflate to exactly usize. A delta is
// bounded by usize too: fossil's delta_create and this module's delta.Create
// emit a copy only for a match of 16 bytes or more, which costs no more than
// the bytes it replaces, so a delta exceeds its target by little more than its
// header and checksum. The margin below is far wider than that.
func cfileInflateLimit(usize int, isDelta bool) int64 {
	if usize < 0 || usize > maxPayloadSize {
		panic("xfer.cfileInflateLimit: usize out of range")
	}
	if !isDelta {
		return int64(usize)
	}
	return int64(usize) + int64(usize)/2 + 64
}

// decompressCFile decompresses a cfile card's wire payload and also returns
// storedBlob: the same bytes recast into Fossil's on-disk blob format
// ([4-byte BE uncompressed-size prefix][zlib data]), built without ever
// invoking zlib as a writer. This lets a caller persist storedBlob directly
// as blob.content -- reproducing whatever bytes the sender's own storage
// held -- instead of decompressing here and recompressing later, which
// would depend on this process's zlib writer parameters rather than the
// sender's. Neither form may inflate past limit; the prefixed form may not
// inflate past its prefix either, as fossil's blob_uncompress sizes its
// output buffer from that prefix.
func decompressCFile(data []byte, limit int64) (decompressed []byte, storedBlob []byte, err error) {
	// Try plain zlib, which this module's encoder sent before it adopted
	// fossil's prefixed form.
	out, overLimit, readErr := inflateBounded(data, limit)
	if readErr == nil {
		// data carries no size prefix. Fossil's on-disk format needs
		// one, so prepend it -- the zlib bytes themselves pass through
		// unchanged, so this is bookkeeping, not recompression.
		var buf bytes.Buffer
		if err := binary.Write(&buf, binary.BigEndian, uint32(len(out))); err != nil {
			return nil, nil, fmt.Errorf("xfer: cfile size prefix: %w", err)
		}
		buf.Write(data)
		return out, buf.Bytes(), nil
	}
	// Try with 4-byte size prefix skip (Fossil blob format). Fossil's
	// send_compressed_file transmits the source's own blob.content bytes
	// unmodified, so data IS already the canonical on-disk form here --
	// nothing needs to be rebuilt.
	if len(data) > 4 {
		prefix := int64(binary.BigEndian.Uint32(data[:4]))
		out, over, readErr := inflateBounded(data[4:], min(limit, prefix))
		if readErr == nil {
			return out, data, nil
		}
		overLimit = overLimit || over
	}
	if overLimit {
		return nil, nil, fmt.Errorf(
			"xfer: cfile payload inflates past %d bytes, its declared size or the message budget",
			limit)
	}
	return nil, nil, fmt.Errorf(
		"xfer: cfile zlib init: could not decompress payload (%d bytes)", len(data))
}

// inflateBounded inflates one zlib stream, reading at most limit+1 bytes of
// output so a stream claiming more stops there. overLimit reports that case;
// a stream that is not zlib, or is corrupt, returns only an error.
func inflateBounded(data []byte, limit int64) (out []byte, overLimit bool, err error) {
	zr, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, false, err
	}
	defer zr.Close()
	out, err = io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(out)) > limit {
		return nil, true, fmt.Errorf("inflates past %d bytes", limit)
	}
	return out, false, nil
}

func parseSchema(r *bufio.Reader, args []string) (Card, error) {
	if len(args) != 5 {
		return nil, fmt.Errorf("xfer: schema requires 5 args, got %d", len(args))
	}
	version, err := strconv.Atoi(args[1])
	if err != nil {
		return nil, fmt.Errorf("xfer: schema version: %w", err)
	}
	mtime, err := strconv.ParseInt(args[3], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("xfer: schema mtime: %w", err)
	}
	size, err := parseSize(args[4])
	if err != nil {
		return nil, fmt.Errorf("xfer: schema size: %w", err)
	}
	const maxSchemaSize = 64 << 20 // 64 MiB — schema payloads should be small JSON.
	if size > maxSchemaSize {
		return nil, fmt.Errorf("xfer: schema size out of bounds: %d", size)
	}
	content, err := readPayloadWithTrailingNewline(r, size)
	if err != nil {
		return nil, fmt.Errorf("xfer: schema payload: %w", err)
	}
	return &SchemaCard{Table: args[0], Version: version, Hash: args[2], MTime: mtime, Content: content}, nil
}

// parseXIGot and parseXGimme return nil for a malformed line, which the
// caller turns into a bad command, as for the other payload-free cards.
func parseXIGot(args []string) Card {
	if len(args) != 3 {
		return nil
	}
	mtime, err := strconv.ParseInt(args[2], 10, 64)
	if err != nil {
		return nil
	}
	return &XIGotCard{Table: args[0], PKHash: args[1], MTime: mtime}
}

func parseXGimme(args []string) Card {
	if len(args) != 2 {
		return nil
	}
	return &XGimmeCard{Table: args[0], PKHash: args[1]}
}

func parseXDelete(r *bufio.Reader, args []string) (Card, error) {
	if len(args) != 4 {
		return nil, fmt.Errorf("xfer: xdelete requires 4 args, got %d", len(args))
	}
	mtime, err := strconv.ParseInt(args[2], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("xfer: xdelete mtime: %w", err)
	}
	size, err := parseSize(args[3])
	if err != nil {
		return nil, fmt.Errorf("xfer: xdelete size: %w", err)
	}
	const maxXDeleteSize = 64 << 20 // 64 MiB — PK data should be tiny JSON
	if size > maxXDeleteSize {
		return nil, fmt.Errorf("xfer: xdelete size out of range: %d", size)
	}
	pkData, err := readPayloadWithTrailingNewline(r, size)
	if err != nil {
		return nil, fmt.Errorf("xfer: xdelete payload: %w", err)
	}
	return &XDeleteCard{Table: args[0], PKHash: args[1], MTime: mtime, PKData: pkData}, nil
}

func parseXRow(r *bufio.Reader, args []string) (Card, error) {
	if len(args) != 4 {
		return nil, fmt.Errorf("xfer: xrow requires 4 args, got %d", len(args))
	}
	mtime, err := strconv.ParseInt(args[2], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("xfer: xrow mtime: %w", err)
	}
	size, err := parseSize(args[3])
	if err != nil {
		return nil, fmt.Errorf("xfer: xrow size: %w", err)
	}
	const maxXRowSize = 64 << 20 // 64 MiB — row payloads should be small JSON.
	if size > maxXRowSize {
		return nil, fmt.Errorf("xfer: xrow size out of bounds: %d", size)
	}
	content, err := readPayloadWithTrailingNewline(r, size)
	if err != nil {
		return nil, fmt.Errorf("xfer: xrow payload: %w", err)
	}
	return &XRowCard{Table: args[0], PKHash: args[1], MTime: mtime, Content: content}, nil
}
