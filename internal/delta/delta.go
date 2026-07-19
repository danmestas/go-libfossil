package delta

import (
	"errors"
	"fmt"
	"math"
)

var (
	ErrInvalidDelta = errors.New("delta: invalid format")
	ErrChecksum     = errors.New("delta: checksum mismatch")
)

// maxDeltaTargetSize bounds the target length declared in a delta header.
// It is far larger than any real Fossil artifact — its purpose is to
// reject an obviously-hostile or malformed claim (wire data, never
// trusted) before it reaches an allocation or a size column, not to
// constrain legitimate content.
const maxDeltaTargetSize = 1 << 32 // 4 GiB

var digits = [128]int{
	-1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1,
	-1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1,
	-1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1,
	0, 1, 2, 3, 4, 5, 6, 7, 8, 9, -1, -1, -1, -1, -1, -1,
	-1, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24,
	25, 26, 27, 28, 29, 30, 31, 32, 33, 34, 35, -1, -1, -1, -1, 36,
	-1, 37, 38, 39, 40, 41, 42, 43, 44, 45, 46, 47, 48, 49, 50, 51,
	52, 53, 54, 55, 56, 57, 58, 59, 60, 61, 62, -1, -1, -1, 63, -1,
}

type reader struct {
	data []byte
	pos  int
}

func (r *reader) getInt() (uint64, error) {
	if r == nil {
		panic("delta.getInt: receiver must not be nil")
	}
	var v uint64
	started := false
	for r.pos < len(r.data) {
		c := r.data[r.pos]
		if c >= 128 || digits[c] < 0 {
			break
		}
		d := uint64(digits[c])
		// Reject rather than wrap: v*64+d overflowing uint64 would wrap
		// silently, and a wrapped value can land anywhere — including
		// exactly math.MaxUint64, which casts to int64(-1), the phantom
		// sentinel used throughout blob.size. Wire data gets no benefit
		// of the doubt here.
		if v > (math.MaxUint64-d)/64 {
			return 0, fmt.Errorf("%w: integer overflow at pos %d", ErrInvalidDelta, r.pos)
		}
		v = v*64 + d
		r.pos++
		started = true
	}
	if !started {
		return 0, fmt.Errorf("%w: expected integer at pos %d", ErrInvalidDelta, r.pos)
	}
	return v, nil
}

func (r *reader) getChar() (byte, error) {
	if r == nil {
		panic("delta.getChar: receiver must not be nil")
	}
	if r.pos >= len(r.data) {
		return 0, fmt.Errorf("%w: unexpected end at pos %d", ErrInvalidDelta, r.pos)
	}
	c := r.data[r.pos]
	r.pos++
	return c, nil
}

// parseTargetLen reads the "<len>\n" header that begins every delta,
// without touching anything past it. Apply continues on into the command
// stream from the returned reader position; OutputSize stops here.
func parseTargetLen(r *reader) (uint64, error) {
	targetLen, err := r.getInt()
	if err != nil {
		return 0, err
	}
	term, err := r.getChar()
	if err != nil {
		return 0, err
	}
	if term != '\n' {
		return 0, fmt.Errorf("%w: expected newline after target length", ErrInvalidDelta)
	}
	if targetLen > maxDeltaTargetSize {
		return 0, fmt.Errorf("%w: target length %d exceeds maximum %d", ErrInvalidDelta, targetLen, uint64(maxDeltaTargetSize))
	}
	return targetLen, nil
}

// OutputSize returns the reconstructed size of a delta's target, read from
// the delta header alone. It does not require or touch the source content,
// so a delta's expanded size is known even when its base is not yet
// available — mirrors Fossil's delta_output_size (src/delta.c). Returns an
// error (never panics) on malformed or empty input: deltaBytes is wire
// data, not a programmer-controlled argument, so a malformed value is an
// input-validation failure, not an assertion failure.
func OutputSize(deltaBytes []byte) (uint64, error) {
	if len(deltaBytes) == 0 {
		return 0, fmt.Errorf("%w: empty delta", ErrInvalidDelta)
	}
	return parseTargetLen(&reader{data: deltaBytes})
}

// Apply reconstructs target data from source and delta.
// Variable naming follows fossil/src/delta.c for cross-reference:
//   cnt = count, offset = source offset, cmd = command byte
func Apply(source, delta []byte) (result []byte, err error) {
	if source == nil {
		panic("delta.Apply: source must not be nil")
	}
	defer func() {
		if err == nil && result == nil {
			panic("delta.Apply: postcondition violated: result is nil with no error")
		}
	}()

	if len(delta) == 0 {
		return nil, fmt.Errorf("%w: empty delta", ErrInvalidDelta)
	}

	r := &reader{data: delta}

	targetLen, err := parseTargetLen(r)
	if err != nil {
		return nil, err
	}

	output := make([]byte, 0, targetLen)

	for r.pos < len(r.data) {
		cnt, err := r.getInt()
		if err != nil {
			return nil, err
		}
		cmd, err := r.getChar()
		if err != nil {
			return nil, err
		}

		switch cmd {
			case '@':
			// Fossil delta format: count@offset, (first int = byte count, second = source offset)
			offset, err := r.getInt()
			if err != nil {
				return nil, err
			}
			term, err := r.getChar()
			if err != nil {
				return nil, err
			}
			if term != ',' {
				return nil, fmt.Errorf("%w: expected comma in copy command", ErrInvalidDelta)
			}
			// Bounds check before casting to int
			if offset > uint64(len(source)) || cnt > uint64(len(source)) {
				return nil, fmt.Errorf("%w: copy offset/count overflow", ErrInvalidDelta)
			}
			if int(offset+cnt) > len(source) {
				return nil, fmt.Errorf("%w: copy exceeds source bounds (offset=%d, cnt=%d, srclen=%d)",
					ErrInvalidDelta, offset, cnt, len(source))
			}
			output = append(output, source[offset:offset+cnt]...)

		case ':':
			// Bounds check before casting to int
			if cnt > uint64(len(r.data)) {
				return nil, fmt.Errorf("%w: insert count overflow", ErrInvalidDelta)
			}
			if r.pos+int(cnt) > len(r.data) {
				return nil, fmt.Errorf("%w: insert exceeds delta bounds", ErrInvalidDelta)
			}
			output = append(output, r.data[r.pos:r.pos+int(cnt)]...)
			r.pos += int(cnt)

		case ';':
			if uint64(len(output)) != targetLen {
				return nil, fmt.Errorf("%w: output size %d != target size %d",
					ErrInvalidDelta, len(output), targetLen)
			}
			if cnt != uint64(Checksum(output)) {
				return nil, fmt.Errorf("%w: expected %d, got %d",
					ErrChecksum, Checksum(output), cnt)
			}
			return output, nil

		default:
			return nil, fmt.Errorf("%w: unknown command '%c' at pos %d",
				ErrInvalidDelta, cmd, r.pos-1)
		}
	}

	return nil, fmt.Errorf("%w: missing terminator", ErrInvalidDelta)
}

// Checksum computes Fossil's delta checksum, matching delta.c's checksum().
// Sum of 4-byte big-endian words, with trailing bytes in big-endian position.
func Checksum(data []byte) uint32 {
	if data == nil {
		panic("delta.Checksum: data must not be nil")
	}
	var sum uint32
	i := 0
	n := len(data)

	for n >= 4 {
		sum += uint32(data[i])<<24 | uint32(data[i+1])<<16 | uint32(data[i+2])<<8 | uint32(data[i+3])
		i += 4
		n -= 4
	}

	switch n {
	case 3:
		sum += uint32(data[i+2]) << 8
		fallthrough
	case 2:
		sum += uint32(data[i+1]) << 16
		fallthrough
	case 1:
		sum += uint32(data[i]) << 24
	}

	return sum
}

const zDigitsEnc = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ_abcdefghijklmnopqrstuvwxyz~"

type hashEntry struct {
	offset int
	next   int
}

// Create generates a delta that transforms source into target.
// Variable naming follows fossil/src/delta.c for cross-reference:
//   nHash = NHASH, ei = entry index, ml = match length, tPos = target position, sOff = source offset
func Create(source, target []byte) (result []byte) {
	if source == nil {
		panic("delta.Create: source must not be nil")
	}
	if target == nil {
		panic("delta.Create: target must not be nil")
	}
	defer func() {
		if len(result) == 0 {
			panic("delta.Create: postcondition violated: result is empty")
		}
	}()

	if len(target) == 0 {
		var buf []byte
		buf = appendInt(buf, 0)
		buf = append(buf, '\n')
		buf = appendInt(buf, uint64(Checksum(target)))
		buf = append(buf, ';')
		return buf
	}

	if len(source) < 16 {
		return createInsertAll(target)
	}

	heads, entries, mask := buildHashTable(source)
	return emitMatches(source, target, heads, entries, mask)
}

func buildHashTable(source []byte) (heads []int, entries []hashEntry, mask int) {
	const nHash = 16

	tableSize := len(source) / nHash
	if tableSize < 64 {
		tableSize = 64
	}
	for tableSize&(tableSize-1) != 0 {
		tableSize &= tableSize - 1
	}
	tableSize <<= 1
	mask = tableSize - 1

	heads = make([]int, tableSize)
	entries = make([]hashEntry, 0, len(source)/nHash)

	for i := 0; i+nHash <= len(source); i += nHash {
		h := rollingHash(source[i : i+nHash])
		idx := int(h) & mask
		entries = append(entries, hashEntry{offset: i, next: heads[idx] - 1})
		heads[idx] = len(entries)
	}

	return heads, entries, mask
}

func emitMatches(source, target []byte, heads []int, entries []hashEntry, mask int) []byte {
	const nHash = 16

	var buf []byte
	buf = appendInt(buf, uint64(len(target)))
	buf = append(buf, '\n')

	var pendingInsert []byte
	tPos := 0

	flushInsert := func() {
		if len(pendingInsert) > 0 {
			buf = appendInt(buf, uint64(len(pendingInsert)))
			buf = append(buf, ':')
			buf = append(buf, pendingInsert...)
			pendingInsert = pendingInsert[:0]
		}
	}

	for tPos < len(target) {
		bestLen := 0
		bestOff := 0

		if tPos+nHash <= len(target) {
			h := rollingHash(target[tPos : tPos+nHash])
			idx := int(h) & mask
			ei := heads[idx]
			for ei > 0 {
				e := entries[ei-1]
				sOff := e.offset

				if sOff+nHash <= len(source) && matchLen(source[sOff:], target[tPos:]) >= nHash {
					ml := matchLen(source[sOff:], target[tPos:])
					if ml > bestLen {
						bestLen = ml
						bestOff = sOff
					}
				}
				ei = e.next + 1
			}
		}

		if bestLen >= nHash {
			flushInsert()
			// Fossil delta format: count@offset,
			buf = appendInt(buf, uint64(bestLen))
			buf = append(buf, '@')
			buf = appendInt(buf, uint64(bestOff))
			buf = append(buf, ',')
			tPos += bestLen
		} else {
			pendingInsert = append(pendingInsert, target[tPos])
			tPos++
		}
	}

	flushInsert()
	buf = appendInt(buf, uint64(Checksum(target)))
	buf = append(buf, ';')
	return buf
}

func createInsertAll(target []byte) []byte {
	if len(target) == 0 {
		panic("delta.createInsertAll: target length must be > 0")
	}
	var buf []byte
	buf = appendInt(buf, uint64(len(target)))
	buf = append(buf, '\n')
	buf = appendInt(buf, uint64(len(target)))
	buf = append(buf, ':')
	buf = append(buf, target...)
	buf = appendInt(buf, uint64(Checksum(target)))
	buf = append(buf, ';')
	return buf
}

func appendInt(buf []byte, v uint64) []byte {
	if v == 0 {
		return append(buf, '0')
	}
	var tmp [13]byte
	i := len(tmp)
	for v > 0 {
		i--
		tmp[i] = zDigitsEnc[v&0x3f]
		v >>= 6
	}
	return append(buf, tmp[i:]...)
}

func rollingHash(data []byte) uint32 {
	if len(data) == 0 {
		panic("delta.rollingHash: data length must be > 0")
	}
	var h uint32
	for _, b := range data {
		h = h*37 + uint32(b)
	}
	return h
}

func matchLen(a, b []byte) int {
	if a == nil {
		panic("delta.matchLen: a must not be nil")
	}
	if b == nil {
		panic("delta.matchLen: b must not be nil")
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
