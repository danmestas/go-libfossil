package content

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
)

// The metadata-only pass must prove total stored bytes first: substr(BLOB) can
// materialize the entire value inside SQLite even though it returns six bytes.
// Charge those reads separately from later full driver-payload reads.
func referencePrefixes(
	ctx context.Context, q db.Querier, blobs []referenceBlob,
	budget ReferenceBudget, work *ReferenceWork, inflated *int64,
) error {
	for i := range blobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		b := &blobs[i]
		if b.unreadable {
			continue
		}
		if b.stored > budget.StoredBytes-work.PrefixBytes {
			return fmt.Errorf("sparse preparation: prefix byte budget exceeded")
		}
		work.PrefixReads++
		work.PrefixBytes += b.stored
		stored, prefix, err := blob.StoredHead(q, b.rid, 6)
		if err != nil {
			return err
		}
		if stored != b.stored {
			return fmt.Errorf("sparse preparation: stored bytes changed")
		}
		b.unreadable = len(prefix) != 6
		if !b.unreadable {
			b.unreadable = prefix[4]&15 != 8 || prefix[4]>>4 > 7 ||
				binary.BigEndian.Uint16(prefix[4:])%31 != 0
		}
		if b.unreadable {
			continue
		}
		b.inflated = int64(binary.BigEndian.Uint32(prefix[:4]))
		if b.inflated > budget.InflatedBytes-*inflated {
			return fmt.Errorf("sparse preparation: inflation budget exceeded")
		}
		*inflated += b.inflated
	}
	return nil
}
