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

// Apply reconstructs target data from source and delta, allocating a fresh
// output buffer. It is ApplyInto with no buffer to reuse.
func Apply(source, delta []byte) (result []byte, err error) {
	return ApplyInto(nil, source, delta)
}

// ApplyInto reconstructs target data from source and delta into dst, reusing
// dst's capacity when it is large enough. dst must be empty (len(dst) == 0);
// pass dst[:0] to reuse a buffer from a previous call. A nil dst is the
// fresh-allocation path and behaves exactly like Apply.
//
// Reuse is what makes a delta-chain replay allocate a constant number of
// output buffers instead of one per link: the caller ping-pongs two buffers,
// each link's target reusing the buffer that held the target two links back.
// The reused capacity is proven prior work, not a wire claim, so it is a safe
// starting size — unlike targetLen, which is a header value ApplyInto never
// trusts to allocate from (see the deltaInitialCap comment below).
//
// The returned slice may share dst's backing array; the caller must not hold
// a reference to dst afterwards expecting it to be independent.
//
// Variable naming follows fossil/src/delta.c for cross-reference:
//   cnt = count, offset = source offset, cmd = command byte
func ApplyInto(dst, source, delta []byte) (result []byte, err error) {
	if source == nil {
		panic("delta.ApplyInto: source must not be nil")
	}
	if len(dst) != 0 {
		panic("delta.ApplyInto: dst must be empty (pass dst[:0])")
	}
	defer func() {
		if err == nil && result == nil {
			panic("delta.ApplyInto: postcondition violated: result is nil with no error")
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

	// The initial capacity is deliberately NOT targetLen: targetLen is a
	// claim read from the delta's own header, and maxDeltaTargetSize
	// (which parseTargetLen already bounds it to) exists to reject
	// obviously-hostile claims, not to size a safe allocation from — a
	// handful of wire bytes can declare a target near that bound. Once a
	// delta can be stored unverified (see blob.StoreDeltaRaw), a peer
	// plants one such delta and every later Expand of that row repeats
	// the attempt: a durable, remotely-planted allocation, not a one-off
	// parse spike. Starting small and growing tracks actual work
	// performed instead of trusting the claim.
	const deltaInitialCap = 64 * 1024 // 64 KiB; upgraded once real work is proven (see growOutputCap)
	var output []byte
	capped := false
	if cap(dst) == 0 {
		initialCap := targetLen
		if initialCap > deltaInitialCap {
			initialCap = deltaInitialCap
			capped = true
		}
		output = make([]byte, 0, initialCap)
	} else {
		// A reused buffer's capacity is proven prior work: reconstructing an
		// adjacent revision produced it, so it is a legitimate starting size
		// even above deltaInitialCap. Only when this target outgrows the
		// inherited buffer does the one-time upgrade fire, and it still sizes
		// off len(delta) rather than the targetLen header (see growOutputCap).
		output = dst[:0]
		capped = uint64(cap(output)) < targetLen
	}

	// grownPastInitialCap upgrades output's capacity exactly once, the
	// first time a command actually executes. Go's growth factor for
	// large slices is ~1.25x, not 2x, so climbing from a 64 KiB cap to a
	// multi-megabyte target purely via append's own doubling lands near
	// 5x the final size in cumulative allocation, not ~1x. A single
	// upgrade sized off len(delta) — bytes the peer actually had to send,
	// not the unverified targetLen header — restores near-exact sizing
	// for honest deltas without reopening the tiny-payload-huge-header
	// attack: a hostile delta with no real commands never reaches this
	// upgrade at all (see TestApply_TinyPayloadHugeHeaderBoundsAllocation).
	grownPastInitialCap := false

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
			if uint64(len(output))+cnt > targetLen {
				return nil, fmt.Errorf("%w: output size %d exceeds declared target size %d during copy command",
					ErrInvalidDelta, uint64(len(output))+cnt, targetLen)
			}
			output = append(output, source[offset:offset+cnt]...)
			if capped && !grownPastInitialCap {
				output = growOutputCap(output, targetLen, len(delta))
				grownPastInitialCap = true
			}

		case ':':
			// Bounds check before casting to int
			if cnt > uint64(len(r.data)) {
				return nil, fmt.Errorf("%w: insert count overflow", ErrInvalidDelta)
			}
			if r.pos+int(cnt) > len(r.data) {
				return nil, fmt.Errorf("%w: insert exceeds delta bounds", ErrInvalidDelta)
			}
			if uint64(len(output))+cnt > targetLen {
				return nil, fmt.Errorf("%w: output size %d exceeds declared target size %d during insert command",
					ErrInvalidDelta, uint64(len(output))+cnt, targetLen)
			}
			output = append(output, r.data[r.pos:r.pos+int(cnt)]...)
			r.pos += int(cnt)
			if capped && !grownPastInitialCap {
				output = growOutputCap(output, targetLen, len(delta))
				grownPastInitialCap = true
			}

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

// deltaGrowthFactor bounds the one-time capacity upgrade growOutputCap
// performs against len(delta): the multiple is generous enough to reach
// most legitimate targets in a single reallocation, while still scaling
// with bytes the peer actually transmitted rather than with an unverified
// header claim.
const deltaGrowthFactor = 64

// growOutputCap upgrades output's capacity once a command has proven the
// delta contains real work, from the conservative 64 KiB initial cap to
// min(targetLen, deltaGrowthFactor*len(delta)). targetLen is already
// bounded by maxDeltaTargetSize and, from this point in Apply, output can
// never be allowed past it either (see the per-command bound checks
// above) — so this upgrade can only reduce the number of later appends,
// never let output outgrow what was already going to be permitted.
func growOutputCap(output []byte, targetLen uint64, deltaLen int) []byte {
	next := uint64(deltaLen) * deltaGrowthFactor
	if next > targetLen {
		next = targetLen
	}
	if next <= uint64(cap(output)) {
		return output
	}
	grown := make([]byte, len(output), next)
	copy(grown, output)
	return grown
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
