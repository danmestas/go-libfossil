package blob

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/danmestas/go-libfossil/simio"
)

// Compress produces Fossil-compatible compressed blob content:
// [4-byte big-endian uncompressed size][zlib-compressed data].
// This matches Fossil's blob_compress() in src/blob.c.
func Compress(data []byte) (result []byte, err error) {
	if data == nil {
		panic("blob.Compress: data must not be nil")
	}
	defer func() {
		if err == nil && len(result) == 0 {
			panic("blob.Compress: postcondition violated: result is empty with no error")
		}
	}()

	var buf bytes.Buffer
	// 4-byte big-endian uncompressed size prefix.
	if err := binary.Write(&buf, binary.BigEndian, uint32(len(data))); err != nil {
		return nil, fmt.Errorf("write size prefix: %w", err)
	}
	w := zlib.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		return nil, fmt.Errorf("zlib compress: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("zlib close: %w", err)
	}
	return buf.Bytes(), nil
}

// EncodeForStorage returns the bytes to write into blob.content for data.
// When verbatim is non-nil, it is already data re-expressed in Fossil's
// on-disk blob format (e.g. bytes received over the wire that were already
// encoded that way) and is returned as-is, with no zlib pass of our own.
// When verbatim is nil, data is compressed fresh via Compress.
//
// Centralizing the choice here -- rather than in each receive-path caller --
// means a caller with wire-verbatim bytes on hand never has to decide
// whether re-encoding is safe to skip; it just offers the bytes and this
// function decides. Locally authored content, which never has verbatim
// bytes to offer, is unaffected: it always takes the Compress path.
func EncodeForStorage(data []byte, verbatim []byte) ([]byte, error) {
	if data == nil {
		panic("blob.EncodeForStorage: data must not be nil")
	}
	if verbatim != nil {
		if len(verbatim) == 0 {
			panic("blob.EncodeForStorage: verbatim must not be empty when non-nil")
		}
		return verbatim, nil
	}
	return Compress(data)
}

// Decompress handles Fossil's compressed blob format:
// [4-byte big-endian uncompressed size][zlib-compressed data].
// The 4-byte prefix is skipped before decompressing.
func Decompress(data []byte) (result []byte, err error) {
	if data == nil {
		panic("blob.Decompress: data must not be nil")
	}
	defer func() {
		if err == nil && result == nil {
			panic("blob.Decompress: postcondition violated: result is nil with no error")
		}
	}()

	if len(data) < 5 {
		return nil, fmt.Errorf("zlib decompress: data too short (%d bytes)", len(data))
	}
	// Skip the 4-byte size prefix.
	zlibData := data[4:]
	r, err := zlib.NewReader(bytes.NewReader(zlibData))
	if err != nil {
		return nil, fmt.Errorf("zlib decompress: %w", err)
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("zlib read: %w", err)
	}
	// BUGGIFY: truncate decompressed output to exercise partial-read handling.
	if simio.Buggify(0.02) && len(out) > 1 {
		out = out[:len(out)/2]
	}
	return out, nil
}
