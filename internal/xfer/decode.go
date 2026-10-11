package xfer

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/danmestas/go-libfossil/internal/deck"
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
		line, err := r.ReadString('\n')
		// Handle final line without trailing newline
		if err != nil && err != io.EOF {
			return nil, err
		}
		line = strings.TrimRight(line, "\n\r")

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

		card, parseErr := parseLine(r, line, budget)
		if parseErr != nil {
			return nil, parseErr
		}
		return card, nil
	}
}

// parseLine dispatches a trimmed, non-empty line to the appropriate card parser.
// The reader r is passed through for payload cards that need to read binary data.
func parseLine(r *bufio.Reader, line string, budget *int64) (Card, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return nil, fmt.Errorf("xfer: empty line after split")
	}
	cmd := fields[0]
	args := fields[1:]

	switch cmd {
	case "igot":
		return parseIGot(args)
	case "gimme":
		return parseGimme(args)
	case "push":
		return parsePush(args)
	case "pull":
		return parsePull(args)
	case "cookie":
		return parseCookie(args)
	case "reqconfig":
		return parseReqConfig(args)
	case "private":
		return parsePrivate(args)
	case "clone":
		return parseClone(args)
	case "clone_seqno":
		return parseCloneSeqNo(args)
	case "uvgimme":
		return parseUVGimme(args)
	case "pragma":
		return parsePragma(args)
	case "login":
		return parseLogin(args)
	case "error":
		return parseError(args)
	case "message":
		return parseMessage(args)
	case "uvigot":
		return parseUVIGot(args)
	case "file":
		return parseFile(r, args)
	case "cfile":
		return parseCFile(r, args, budget)
	case "config":
		return parseConfig(r, args)
	case "uvfile":
		return parseUVFile(r, args)
	case "schema":
		return parseSchema(r, args)
	case "xigot":
		return parseXIGot(args)
	case "xgimme":
		return parseXGimme(args)
	case "xrow":
		return parseXRow(r, args)
	case "xdelete":
		return parseXDelete(r, args)
	default:
		return &UnknownCard{Command: cmd, Args: args}, nil
	}
}

func parseIGot(args []string) (Card, error) {
	if len(args) < 1 || len(args) > 2 {
		return nil, fmt.Errorf("xfer: igot requires 1-2 args, got %d", len(args))
	}
	c := &IGotCard{UUID: args[0]}
	if len(args) == 2 && args[1] == "1" {
		c.IsPrivate = true
	}
	return c, nil
}

func parseGimme(args []string) (Card, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("xfer: gimme requires 1 arg, got %d", len(args))
	}
	return &GimmeCard{UUID: args[0]}, nil
}

// parsePush parses "push <server-code> <project-code>".
//
// Canonical (src/xfer.c) accepts push and pull only at nToken==3, and reads
// the project code — the value it validates, and the value a cloning client
// adopts as its own — out of the second argument. Anything shorter is not a
// push card canonical would act on, so it is an error here rather than a
// guess about which code the sender meant.
func parsePush(args []string) (Card, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("xfer: push requires 2 args (server-code project-code), got %d", len(args))
	}
	return &PushCard{ServerCode: args[0], ProjectCode: args[1]}, nil
}

// parsePull parses "pull <server-code> <project-code>".
//
// Same arg order and arity as push; see parsePush.
func parsePull(args []string) (Card, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("xfer: pull requires 2 args (server-code project-code), got %d", len(args))
	}
	return &PullCard{ServerCode: args[0], ProjectCode: args[1]}, nil
}

func parseCookie(args []string) (Card, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("xfer: cookie requires 1 arg, got %d", len(args))
	}
	return &CookieCard{Value: args[0]}, nil
}

func parseReqConfig(args []string) (Card, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("xfer: reqconfig requires 1 arg, got %d", len(args))
	}
	return &ReqConfigCard{Name: args[0]}, nil
}

func parsePrivate(args []string) (Card, error) {
	if len(args) != 0 {
		return nil, fmt.Errorf("xfer: private takes 0 args, got %d", len(args))
	}
	return &PrivateCard{}, nil
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

func parseClone(args []string) (Card, error) {
	if len(args) != 0 && len(args) != 2 {
		return nil, fmt.Errorf("xfer: clone requires 0 or 2 args, got %d", len(args))
	}
	if len(args) == 0 {
		return &CloneCard{}, nil
	}
	// §8.1 withholds its fatal from a SEQNO that fails the digit-only rule
	// (isDecimal below) but a token strconv.Atoi cannot parse at all -- e.g.
	// "abc" or "3x" -- is rarer still and outside anything §8.1 discusses.
	// Erroring the whole message for it is harsher than the fatal the spec
	// was careful to withhold, so degrade the same way parseCloneSeqNo does
	// for clone_seqno: carry the raw tokens forward as an UnknownCard rather
	// than failing every later card in the message.
	v, vErr := strconv.Atoi(args[0])
	s, sErr := strconv.Atoi(args[1])
	if vErr != nil || sErr != nil {
		return &UnknownCard{Command: "clone", Args: args}, nil
	}
	return &CloneCard{
		Version: v,
		SeqNo:   s,
		// §8.1 keys its fatal on a digit-only SEQNO and explicitly withholds
		// it from one that fails digit-only recognition, so record whether
		// this token qualifies rather than only what it parsed to.
		SeqNoIsDecimal: isDecimal(args[1]),
	}, nil
}

func parseCloneSeqNo(args []string) (Card, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("xfer: clone_seqno requires 1 arg, got %d", len(args))
	}
	if !isDecimal(args[0]) {
		// §3.2 records NEXT only when it is decimal, and §8.2 makes a visible
		// but non-decimal NEXT a receiver-tolerance exception that leaves the
		// recorded sequence unchanged without stopping later reply cards.
		// Surfacing it as an unknown card keeps the token while denying it any
		// effect on the cursor — a CloneSeqNoCard here would be recorded.
		return &UnknownCard{Command: "clone_seqno", Args: args}, nil
	}
	s, err := strconv.Atoi(args[0])
	if err != nil {
		return nil, fmt.Errorf("xfer: clone_seqno: %w", err)
	}
	return &CloneSeqNoCard{SeqNo: s}, nil
}

func parseUVGimme(args []string) (Card, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("xfer: uvgimme requires 1 arg, got %d", len(args))
	}
	return &UVGimmeCard{Name: args[0]}, nil
}

func parsePragma(args []string) (Card, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("xfer: pragma requires at least 1 arg, got %d", len(args))
	}
	c := &PragmaCard{Name: args[0]}
	if len(args) > 1 {
		c.Values = args[1:]
	}
	return c, nil
}

func parseLogin(args []string) (Card, error) {
	if len(args) != 3 {
		return nil, fmt.Errorf("xfer: login requires 3 args, got %d", len(args))
	}
	return &LoginCard{
		User:      deck.FossilDecode(args[0]),
		Nonce:     args[1],
		Signature: args[2],
	}, nil
}

func parseError(args []string) (Card, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("xfer: error requires 1 arg, got %d", len(args))
	}
	return &ErrorCard{Message: deck.FossilDecode(args[0])}, nil
}

func parseMessage(args []string) (Card, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("xfer: message requires 1 arg, got %d", len(args))
	}
	return &MessageCard{Message: deck.FossilDecode(args[0])}, nil
}

func parseUVIGot(args []string) (Card, error) {
	if len(args) != 4 {
		return nil, fmt.Errorf("xfer: uvigot requires 4 args, got %d", len(args))
	}
	mtime, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("xfer: uvigot mtime: %w", err)
	}
	size, err := strconv.Atoi(args[3])
	if err != nil {
		return nil, fmt.Errorf("xfer: uvigot size: %w", err)
	}
	return &UVIGotCard{
		Name:  args[0],
		MTime: mtime,
		Hash:  args[2],
		Size:  size,
	}, nil
}

// --- Payload card parsers ---

// parseFile decodes: file UUID SIZE \n CONTENT  or  file UUID DELTASRC SIZE \n CONTENT
// No trailing newline after CONTENT.
func parseFile(r *bufio.Reader, args []string) (Card, error) {
	if len(args) != 2 && len(args) != 3 {
		return nil, fmt.Errorf("xfer: file requires 2-3 args, got %d", len(args))
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
		return nil, fmt.Errorf("xfer: file size: %w", err)
	}
	content, err := readPayload(r, size)
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
		return nil, fmt.Errorf("xfer: cfile requires 3-4 args, got %d", len(args))
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
		return nil, fmt.Errorf("xfer: cfile usize: %w", err)
	}
	csize, err := parseSize(csizeStr)
	if err != nil {
		return nil, fmt.Errorf("xfer: cfile csize: %w", err)
	}
	compressed, err := readPayload(r, csize)
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
	// For non-delta cfiles, decompressed size == usize (full content).
	// For delta cfiles, decompressed size is the delta payload size, which
	// is smaller than usize (the fully expanded content size). Fossil's
	// send_compressed_file sets usize = blob.size (full content) regardless.
	// Only validate for non-delta cards where the sizes must match.
	if c.DeltaSrc == "" && len(decompressed) != usize {
		return nil, fmt.Errorf("xfer: cfile usize mismatch: header says %d, got %d", usize, len(decompressed))
	}
	c.USize = usize
	c.Content = decompressed
	c.StoredBlob = storedBlob
	return c, nil
}

// parseConfig decodes: config NAME SIZE \n CONTENT \n
// Note the trailing \n after CONTENT.
func parseConfig(r *bufio.Reader, args []string) (Card, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("xfer: config requires 2 args, got %d", len(args))
	}
	name := args[0]
	size, err := parseSize(args[1])
	if err != nil {
		return nil, fmt.Errorf("xfer: config size: %w", err)
	}
	content, err := readPayloadWithTrailingNewline(r, size)
	if err != nil {
		return nil, fmt.Errorf("xfer: config payload: %w", err)
	}
	return &ConfigCard{Name: name, Content: content}, nil
}

// parseUVFile decodes: uvfile NAME MTIME HASH SIZE FLAGS \n CONTENT
// FLAGS bits: 0x0001 = deleted, 0x0004 = content omitted. When set, no payload follows.
func parseUVFile(r *bufio.Reader, args []string) (Card, error) {
	if len(args) != 5 {
		return nil, fmt.Errorf("xfer: uvfile requires 5 args, got %d", len(args))
	}
	mtime, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("xfer: uvfile mtime: %w", err)
	}
	size, err := parseSize(args[3])
	if err != nil {
		return nil, fmt.Errorf("xfer: uvfile size: %w", err)
	}
	// FLAGS share the size grammar: the draft has receivers reject a
	// negative FLAGS.
	flags, err := parseSize(args[4])
	if err != nil {
		return nil, fmt.Errorf("xfer: uvfile flags: %w", err)
	}
	c := &UVFileCard{
		Name:  args[0],
		MTime: mtime,
		Hash:  args[2],
		Size:  size,
		Flags: flags,
	}
	if !uvFileOmitsContent(flags) {
		content, err := readPayload(r, size)
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

func parseXIGot(args []string) (Card, error) {
	if len(args) != 3 {
		return nil, fmt.Errorf("xfer: xigot requires 3 args, got %d", len(args))
	}
	mtime, err := strconv.ParseInt(args[2], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("xfer: xigot mtime: %w", err)
	}
	return &XIGotCard{Table: args[0], PKHash: args[1], MTime: mtime}, nil
}

func parseXGimme(args []string) (Card, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("xfer: xgimme requires 2 args, got %d", len(args))
	}
	return &XGimmeCard{Table: args[0], PKHash: args[1]}, nil
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
