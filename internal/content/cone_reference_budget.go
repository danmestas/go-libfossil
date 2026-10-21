package content

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/delta"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// ReferenceBudget bounds explicit preparation, not ordinary serving. Byte
// budgets count payload buffers, not Go allocator overhead or parser objects.
// The artifact/card caps separately bound that overhead. The caller must hold
// a stable database snapshot through preflight and materialization.
type ReferenceBudget struct {
	Artifacts     int
	StoredBytes   int64
	InflatedBytes int64
	ExpandedBytes int64
	Cards         int
}

type ReferenceWork struct {
	PayloadReads int
	InflateBytes int64
	ExpandBytes  int64
	Visits       int
}

type referenceBlob struct {
	rid, source libfossil.FslID
	uuid        string
	size        int64
	stored      int64
	inflated    int64
	payload     []byte
	data        []byte
	children    []int
	unreadable  bool
}

// LoadConeReferenceFactsBounded rejects total stored/declared expansion sizes
// before reading any content payload. Inflation uses a fixed, precharged
// buffer and verifies EOF; a lying zlib prefix cannot grow that allocation.
// Delta output length is checked before Apply can allocate its target.
func LoadConeReferenceFactsBounded(
	ctx context.Context, q db.Querier, budget ReferenceBudget,
) (*ConeReferenceFacts, ReferenceWork, error) {
	return loadConeReferenceFactsBounded(ctx, q, budget, false)
}

func loadConeReferenceFactsBounded(
	ctx context.Context, q db.Querier, budget ReferenceBudget, retainUnreadable bool,
) (*ConeReferenceFacts, ReferenceWork, error) {
	var work ReferenceWork
	blobs, names, err := referencePreflight(ctx, q, budget)
	if err != nil {
		return nil, work, err
	}
	facts := &ConeReferenceFacts{names: names, held: map[string]libfossil.FslID{},
		nodes: map[libfossil.FslID]*deck.Deck{}, byUUID: map[string]*deck.Deck{}}
	for i := range blobs {
		b := &blobs[i]
		if err := ctx.Err(); err != nil {
			return nil, work, err
		}
		if b.unreadable {
			if retainUnreadable {
				continue
			}
			return nil, work, fmt.Errorf("sparse preparation: invalid compressed prefix")
		}
		var packed []byte
		// length() was checked in the same transaction before this driver
		// allocation; the predicate also guards accidental unbound use.
		err := q.QueryRow("SELECT content FROM blob WHERE rid=? AND length(content)=?", b.rid, b.stored).Scan(&packed)
		if err != nil {
			return nil, work, err
		}
		work.PayloadReads++
		work.InflateBytes += b.inflated
		b.payload, err = referenceInflate(packed, int(b.inflated))
		if err != nil {
			if !retainUnreadable {
				return nil, work, err
			}
			b.unreadable = true
		}
	}
	if err := referenceMaterialize(ctx, blobs, facts, budget, &work); err != nil {
		return nil, work, err
	}
	return facts, work, nil
}

func referencePreflight(
	ctx context.Context, q db.Querier, budget ReferenceBudget,
) ([]referenceBlob, map[libfossil.FslID]string, error) {
	if ctx == nil || q == nil {
		panic("content.referencePreflight: nil context or querier")
	}
	if budget.Artifacts <= 0 || budget.Artifacts > coneReferenceNodes || budget.Cards <= 0 ||
		budget.StoredBytes <= 0 || budget.InflatedBytes <= 0 || budget.ExpandedBytes <= 0 {
		return nil, nil, fmt.Errorf("sparse preparation: invalid budget")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var count int64
	if err := q.QueryRow("SELECT count(*) FROM (SELECT rid FROM blob LIMIT ?)", budget.Artifacts+1).Scan(&count); err != nil {
		return nil, nil, err
	}
	if count > int64(budget.Artifacts) {
		return nil, nil, fmt.Errorf("sparse preparation: artifact budget exceeded")
	}
	blobs := make([]referenceBlob, 0, int(count))
	names := make(map[libfossil.FslID]string, int(count))
	rows, err := q.Query(`SELECT b.rid,substr(b.uuid,1,65),b.size,
		coalesce(length(b.content),0),substr(b.content,1,6),coalesce(d.srcid,0)
		FROM blob b LEFT JOIN delta d ON d.rid=b.rid ORDER BY b.rid`)
	if err != nil {
		return nil, nil, err
	}
	var stored, inflated, expanded int64
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, nil, errors.Join(err, rows.Close())
		}
		var b referenceBlob
		var prefix []byte
		if err := rows.Scan(&b.rid, &b.uuid, &b.size, &b.stored, &prefix, &b.source); err != nil {
			return nil, nil, errors.Join(err, rows.Close())
		}
		if len(names) >= budget.Artifacts || len(b.uuid) > 64 {
			return nil, nil, errors.Join(fmt.Errorf("sparse preparation: identity budget exceeded"), rows.Close())
		}
		names[b.rid] = b.uuid
		if b.size < 0 {
			continue
		}
		// Invalid zlib headers cannot justify an inflated-size reservation.
		// Ordinary discovery retains them as unknown without reading payloads.
		b.unreadable = len(prefix) != 6
		if !b.unreadable {
			b.unreadable = prefix[4]&15 != 8 || prefix[4]>>4 > 7 ||
				(binary.BigEndian.Uint16(prefix[4:])%31 != 0)
		}
		if !b.unreadable {
			b.inflated = int64(binary.BigEndian.Uint32(prefix[:4]))
		}
		if b.stored > budget.StoredBytes-stored || b.inflated > budget.InflatedBytes-inflated || b.size > budget.ExpandedBytes-expanded {
			return nil, nil, errors.Join(fmt.Errorf("sparse preparation: byte budget exceeded"), rows.Close())
		}
		stored += b.stored
		inflated += b.inflated
		expanded += b.size
		blobs = append(blobs, b)
	}
	return blobs, names, errors.Join(rows.Err(), rows.Close())
}

func referenceInflate(packed []byte, size int) ([]byte, error) {
	if len(packed) < 5 || size < 0 {
		return nil, fmt.Errorf("sparse preparation: invalid compressed bytes")
	}
	zr, err := zlib.NewReader(bytes.NewReader(packed[4:]))
	if err != nil {
		return nil, err
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(zr, data); err != nil {
		return nil, errors.Join(err, zr.Close())
	}
	var extra [1]byte
	n, err := zr.Read(extra[:])
	if n != 0 || err != io.EOF {
		return nil, errors.Join(fmt.Errorf("sparse preparation: compressed size mismatch"), zr.Close())
	}
	if err := zr.Close(); err != nil {
		return nil, err
	}
	return data, nil
}

func referenceMaterialize(
	ctx context.Context, blobs []referenceBlob, facts *ConeReferenceFacts,
	budget ReferenceBudget, work *ReferenceWork,
) error {
	index := make(map[libfossil.FslID]int, len(blobs))
	queue := make([]int, 0, len(blobs))
	for i := range blobs {
		index[blobs[i].rid] = i
	}
	for i := range blobs {
		if blobs[i].source == 0 {
			queue = append(queue, i)
		} else if parent, ok := index[blobs[i].source]; ok {
			blobs[parent].children = append(blobs[parent].children, i)
		}
	}
	aliases := 0
	for at := 0; at < len(queue); at++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if work.Visits >= budget.Artifacts {
			return fmt.Errorf("sparse preparation: visit budget exceeded")
		}
		work.Visits++
		b := &blobs[queue[at]]
		data := b.payload
		if b.source != 0 && !b.unreadable {
			base := blobs[index[b.source]].data
			size, err := delta.OutputSize(b.payload)
			if err != nil || size != uint64(b.size) || size > uint64(budget.ExpandedBytes-work.ExpandBytes) {
				return fmt.Errorf("sparse preparation: delta output budget or size mismatch")
			}
			if base == nil {
				data = nil
			} else {
				work.ExpandBytes += int64(size) // Before Apply allocates.
				var applyErr error
				data, applyErr = delta.Apply(base, b.payload)
				if applyErr != nil {
					data = nil // Corrupt content cannot prove outside membership.
				}
			}
		}
		if b.unreadable || int64(len(data)) != b.size || bindToUUID(b.rid, b.uuid, data) != nil {
			data = nil
		}
		b.data = data
		if data == nil {
			facts.unreadable = append(facts.unreadable, b.uuid)
		} else if err := referenceRecord(facts, b, &aliases, budget.Cards); err != nil {
			return err
		}
		queue = append(queue, b.children...)
	}
	// Missing-source/cyclic components cannot yield verified outside proofs.
	for i := range blobs {
		if blobs[i].data == nil {
			facts.unreadable = append(facts.unreadable, blobs[i].uuid)
		}
	}
	return nil
}

func referenceRecord(facts *ConeReferenceFacts, b *referenceBlob, aliases *int, max int) error {
	// Bound parser card allocations before Parse, including non-F cards.
	cards := bytes.Count(b.data, []byte{'\n'})
	if cards > max-*aliases {
		return fmt.Errorf("sparse preparation: card budget exceeded")
	}
	*aliases += cards
	facts.held[b.uuid] = b.rid
	d, err := deck.Parse(b.data)
	if err != nil {
		return nil // Ordinary file bytes do not provide reference facts.
	}
	d = referenceDeck(d)
	facts.records = append(facts.records, coneReferenceRecord{uuid: b.uuid, deck: d})
	if d.Type == deck.Submanifest {
		facts.byUUID[b.uuid] = d
		facts.nodes[b.rid] = d
	}
	if d.Type == deck.Checkin {
		facts.roots = append(facts.roots, d)
	}
	return nil
}
