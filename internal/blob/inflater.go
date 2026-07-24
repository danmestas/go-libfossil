package blob

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"

	"github.com/danmestas/go-libfossil/db"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/simio"
)

// Inflater loads a run of blobs through a single reusable zlib reader. The
// zero value is ready to use. It is not safe for concurrent use.
//
// It exists for delta-chain replay, where one Expand inflates every delta on a
// chain in sequence. Building a fresh zlib reader per link re-allocates a
// ~32 KiB flate history window and its machinery each time, and that per-link
// setup was the single largest allocation source in sub-64 KiB chain
// expansion. Resetting one reader instead pays the setup once for the whole
// chain. Each Load still returns a freshly allocated buffer the caller owns —
// only the reader is shared, so results never alias one another.
type Inflater struct {
	zr io.ReadCloser
}

// Load returns rid's fully-expanded content, decompressing through the reused
// reader. It behaves exactly like [Load] but amortizes the reader setup across
// calls.
func (inf *Inflater) Load(q db.Querier, rid libfossil.FslID) ([]byte, error) {
	if inf == nil {
		panic("blob.Inflater.Load: inf must not be nil")
	}
	return loadWith(q, rid, inf)
}

// decompress mirrors [Decompress] but reuses inf.zr across calls via zlib's
// Resetter, building the reader only on the first call. Reading to EOF
// verifies the Adler-32 checksum, so no Close is needed between resets.
func (inf *Inflater) decompress(data []byte) (result []byte, err error) {
	if data == nil {
		panic("blob.Inflater.decompress: data must not be nil")
	}
	defer func() {
		if err == nil && result == nil {
			panic("blob.Inflater.decompress: postcondition violated: result is nil with no error")
		}
	}()

	if len(data) < 5 {
		return nil, fmt.Errorf("zlib decompress: data too short (%d bytes)", len(data))
	}
	// Skip the 4-byte size prefix, matching Decompress.
	zlibData := data[4:]

	if inf.zr == nil {
		r, err := zlib.NewReader(bytes.NewReader(zlibData))
		if err != nil {
			return nil, fmt.Errorf("zlib decompress: %w", err)
		}
		inf.zr = r
	} else {
		if err := inf.zr.(zlib.Resetter).Reset(bytes.NewReader(zlibData), nil); err != nil {
			return nil, fmt.Errorf("zlib reset: %w", err)
		}
	}

	out, err := io.ReadAll(inf.zr)
	if err != nil {
		return nil, fmt.Errorf("zlib read: %w", err)
	}
	// BUGGIFY: truncate decompressed output to exercise partial-read handling,
	// mirroring Decompress so the reused-reader path is fuzzed the same way.
	if simio.Buggify(0.02) && len(out) > 1 {
		out = out[:len(out)/2]
	}
	return out, nil
}
