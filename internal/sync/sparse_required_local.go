package sync

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

type requiredLocalFrame struct {
	rid  libfossil.FslID
	uuid string
	size int64
}

// Local bases are untrusted too. Account for compressed buffers, inflated
// programs and delta outputs before allocating any of them, not after Expand.
func (f *requiredFetch) expandLocal(q db.Querier, rid libfossil.FslID) ([]byte, error) {
	if q == nil || rid <= 0 {
		panic("sync.requiredFetch.expandLocal: nil querier or nonpositive rid")
	}
	var path []requiredLocalFrame
	seen := make(map[libfossil.FslID]bool)
	for depth := 0; rid > 0; depth++ {
		if depth >= maxRequiredDeltaDepth || seen[rid] {
			return nil, fmt.Errorf("sparse required content: cyclic or excessive local delta depth")
		}
		seen[rid] = true
		frame := requiredLocalFrame{rid: rid}
		var source libfossil.FslID
		err := q.QueryRow(`SELECT b.uuid,b.size,COALESCE(d.srcid,0)
			FROM blob b LEFT JOIN delta d ON d.rid=b.rid WHERE b.rid=?`, rid).
			Scan(&frame.uuid, &frame.size, &source)
		if err != nil {
			return nil, err
		}
		if err := f.checkName(frame.uuid); err != nil {
			return nil, err
		}
		if content.IsPrivate(q, int64(rid)) && !f.opts.Private {
			return nil, fmt.Errorf("sparse required content: private local delta basis requires permission")
		}
		if frame.size < 0 || frame.size > int64(maxRequiredBytes-f.bytes) {
			return nil, fmt.Errorf("sparse required content: local size exceeds staging budget")
		}
		path = append(path, frame)
		rid = source
	}
	var full []byte
	for i := len(path) - 1; i >= 0; i-- {
		frame := path[i]
		payload, err := f.localPayload(q, frame)
		if err != nil {
			return nil, err
		}
		if i == len(path)-1 {
			full = payload
		} else {
			full, err = f.applyDelta(frame.uuid, payload, full)
			if err != nil {
				return nil, err
			}
		}
		if int64(len(full)) != frame.size || !hashMatches(frame.uuid, full) {
			return nil, fmt.Errorf("sparse required content: invalid local size or hash for %s", frame.uuid)
		}
	}
	return full, nil
}

func (f *requiredFetch) localPayload(q db.Querier, frame requiredLocalFrame) ([]byte, error) {
	if err := f.ctx.Err(); err != nil {
		return nil, err
	}
	var packedSize int64
	var prefix []byte
	if err := q.QueryRow("SELECT length(content),substr(content,1,4) FROM blob WHERE rid=?",
		frame.rid).Scan(&packedSize, &prefix); err != nil {
		return nil, err
	}
	if len(prefix) != 4 || packedSize < 5 {
		return nil, fmt.Errorf("sparse required content: invalid local compression header")
	}
	plainSize := int64(binary.BigEndian.Uint32(prefix))
	remaining := int64(maxRequiredBytes - f.bytes)
	if packedSize > remaining || plainSize >= remaining-packedSize {
		return nil, fmt.Errorf("sparse required content: local inflation exceeds staging budget")
	}
	if err := f.reserve(int(packedSize + plainSize + 1)); err != nil {
		return nil, err
	}
	var packed []byte
	err := q.QueryRow(`SELECT content FROM blob WHERE rid=? AND size=?
		AND length(content)=? AND substr(content,1,4)=?`,
		frame.rid, frame.size, packedSize, prefix).Scan(&packed)
	if err != nil {
		return nil, err
	}
	return requiredInflate(packed, int(plainSize))
}

func requiredInflate(packed []byte, size int) ([]byte, error) {
	reader, err := zlib.NewReader(bytes.NewReader(packed[4:]))
	if err != nil {
		return nil, err
	}
	// Read one extra byte to detect a forged low prefix without expanding the
	// unbounded remainder. Reaching EOF also verifies the zlib checksum.
	plain := make([]byte, size+1)
	n, readErr := io.ReadFull(reader, plain)
	closeErr := reader.Close()
	if readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		if readErr == nil {
			return nil, fmt.Errorf("sparse required content: local compression length mismatch")
		}
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if n != size {
		return nil, fmt.Errorf("sparse required content: local compression length mismatch")
	}
	return plain[:size], nil
}
