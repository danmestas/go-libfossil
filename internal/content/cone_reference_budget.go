package content

import (
	"bytes"
	"cmp"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/delta"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// ReferenceBudget bounds a whole-store reference load. Byte budgets count payload buffers, not Go allocator overhead or parser objects.
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
	PrefixReads  int
	PrefixBytes  int64
	PayloadReads int
	InflateBytes int64
	ExpandBytes  int64
	Visits       int
	// TreeReads and TreeBytes count a content horizon's manifest and
	// sub-manifest reads, charged as each one is expanded.
	TreeReads int
	TreeBytes int64
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
	pending     int // Children not yet expanded; data is released at zero.
	unreadable  bool
	member      bool // Parsed, not only expanded as a delta ancestor.
	verified    bool
}

// referenceReader reads raw artifacts in rounds. Every budget is charged
// across all rounds, and only for the blobs a round selects: its members
// and their delta ancestors.
type referenceReader struct {
	ctx              context.Context
	q                db.Querier
	budget           ReferenceBudget
	retainUnreadable bool
	held             []referenceBlob // Every held blob's metadata, by rid.
	index            map[libfossil.FslID]int
	facts            *ConeReferenceFacts
	work             ReferenceWork
	stored, expanded int64
	inflated         int64
	cards            int
}

// loadConeReferenceFactsBounded names every artifact, then reads raw bytes in
// at most three rounds: first everything the crosslink tables cannot answer,
// then the linked check-ins whose trees the first round showed mlink cannot
// reproduce, and, only if one of those proves unreadable, every remaining
// linked check-in. Within a round it proves stored bytes before prefix reads,
// which SQLite may materialize internally, and declared inflation before
// full driver payload reads. Inflation uses a fixed, precharged buffer and
// verifies EOF. Delta output size is checked before Apply can allocate.
func loadConeReferenceFactsBounded(
	ctx context.Context, q db.Querier, budget ReferenceBudget, retainUnreadable bool,
) (*ConeReferenceFacts, ReferenceWork, error) {
	s, work, err := loadReferenceState(ctx, q, budget, retainUnreadable)
	if err != nil {
		return nil, work, err
	}
	return s.facts, work, nil
}

// referenceState is a load's facts with the crosslink facts it decided by,
// which is what extending them needs (see extendReferenceState).
type referenceState struct {
	facts  *ConeReferenceFacts
	tables *referenceTables
	seen   map[[2]int64]bool // the (filename, file) pairs addLinkedFiles recorded
}

func loadReferenceState(
	ctx context.Context, q db.Querier, budget ReferenceBudget, retainUnreadable bool,
) (*referenceState, ReferenceWork, error) {
	r, err := referencePreflight(ctx, q, budget)
	if err != nil {
		return nil, ReferenceWork{}, err
	}
	r.retainUnreadable = retainUnreadable
	tables, err := loadReferenceTables(ctx, q)
	if err != nil {
		return nil, r.work, err
	}
	isLinked := func(rid libfossil.FslID) bool { _, ok := tables.linked[rid]; return ok }
	if _, err := r.read(func(rid libfossil.FslID) bool {
		return !tables.files[rid] && !isLinked(rid)
	}); err != nil {
		return nil, r.work, err
	}
	answer := tables.answerable(r.facts)
	failed, err := r.read(func(rid libfossil.FslID) bool { return isLinked(rid) && !answer[rid] })
	if err != nil {
		return nil, r.work, err
	}
	if failed {
		// answerable took every linked check-in to be readable.
		if _, err := r.read(func(rid libfossil.FslID) bool { return answer[rid] }); err != nil {
			return nil, r.work, err
		}
		answer = nil
	}
	s := &referenceState{facts: r.facts, tables: tables, seen: map[[2]int64]bool{}}
	if err := r.facts.addLinked(ctx, q, answer, nil, s.seen); err != nil {
		return nil, r.work, err
	}
	return s, r.work, nil
}

const referenceMetadataSQL = `SELECT b.rid,substr(b.uuid,1,65),b.size,
	CASE WHEN typeof(b.content)='blob' THEN length(b.content) ELSE -1 END,
	coalesce(d.srcid,0) FROM blob b LEFT JOIN delta d ON d.rid=b.rid ORDER BY b.rid`

// referencePreflight names every artifact. It reads no content, and charges
// nothing but the artifact budget: a round charges what it selects.
func referencePreflight(
	ctx context.Context, q db.Querier, budget ReferenceBudget,
) (*referenceReader, error) {
	if ctx == nil || q == nil {
		panic("content.referencePreflight: nil context or querier")
	}
	if budget.Artifacts <= 0 || budget.Artifacts > coneReferenceNodes || budget.Cards <= 0 ||
		budget.StoredBytes <= 0 || budget.InflatedBytes <= 0 || budget.ExpandedBytes <= 0 {
		return nil, fmt.Errorf("sparse preparation: invalid budget")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var count int64
	if err := q.QueryRow("SELECT count(*) FROM (SELECT rid FROM blob LIMIT ?)", budget.Artifacts+1).Scan(&count); err != nil {
		return nil, err
	}
	if count > int64(budget.Artifacts) {
		return nil, fmt.Errorf("sparse preparation: artifact budget exceeded")
	}
	r := &referenceReader{ctx: ctx, q: q, budget: budget,
		held: make([]referenceBlob, 0, int(count)), index: make(map[libfossil.FslID]int, int(count)),
		facts: &ConeReferenceFacts{names: make(map[libfossil.FslID]string, int(count)),
			held: map[string]libfossil.FslID{}, nodes: map[libfossil.FslID]*deck.Deck{},
			byUUID: map[string]*deck.Deck{}}}
	rows, err := q.Query(referenceMetadataSQL)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		var b referenceBlob
		if err := rows.Scan(&b.rid, &b.uuid, &b.size, &b.stored, &b.source); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		if len(r.facts.names) >= budget.Artifacts || len(b.uuid) > 64 {
			return nil, errors.Join(fmt.Errorf("sparse preparation: identity budget exceeded"), rows.Close())
		}
		r.facts.names[b.rid] = b.uuid
		if b.size >= 0 {
			r.index[b.rid] = len(r.held)
			r.held = append(r.held, b)
		}
	}
	return r, errors.Join(rows.Err(), rows.Close())
}

// referencePreflightFor is referencePreflight for an extension: names is
// every artifact's name, carried from the load being extended, and only the
// rids listed and the held delta ancestors their reads expand are given
// metadata, so a round can select from nothing else.
func referencePreflightFor(
	ctx context.Context, q db.Querier, budget ReferenceBudget, names map[libfossil.FslID]string,
	rids []libfossil.FslID,
) (*referenceReader, error) {
	if ctx == nil || q == nil || names == nil {
		panic("content.referencePreflightFor: nil context, querier or names")
	}
	if len(names) > budget.Artifacts {
		return nil, fmt.Errorf("sparse preparation: artifact budget exceeded")
	}
	r := &referenceReader{ctx: ctx, q: q, budget: budget, index: map[libfossil.FslID]int{},
		facts: &ConeReferenceFacts{names: names, held: map[string]libfossil.FslID{},
			nodes: map[libfossil.FslID]*deck.Deck{}, byUUID: map[string]*deck.Deck{}}}
	want := rids
	for len(want) > 0 {
		ids, err := ridsJSON(want)
		if err != nil {
			return nil, err
		}
		want = nil
		err = referenceRows(ctx, q, referenceMetadataForSQL, func(scan func(...any) error) error {
			var b referenceBlob
			if err := scan(&b.rid, &b.uuid, &b.size, &b.stored, &b.source); err != nil {
				return err
			}
			if _, seen := r.index[b.rid]; seen || b.size < 0 {
				return nil
			}
			if len(r.held) >= budget.Artifacts || len(b.uuid) > 64 {
				return fmt.Errorf("sparse preparation: identity budget exceeded")
			}
			r.index[b.rid] = len(r.held)
			r.held = append(r.held, b)
			if _, seen := r.index[b.source]; b.source != 0 && !seen {
				want = append(want, b.source)
			}
			return nil
		}, ids)
		if err != nil {
			return nil, err
		}
	}
	slices.SortFunc(r.held, func(a, b referenceBlob) int { return cmp.Compare(a.rid, b.rid) })
	for i := range r.held {
		r.index[r.held[i].rid] = i
	}
	return r, nil
}

// referenceMetadataForSQL is referenceMetadataSQL for the rids of the JSON
// array ?1.
const referenceMetadataForSQL = `SELECT b.rid,substr(b.uuid,1,65),b.size,
	CASE WHEN typeof(b.content)='blob' THEN length(b.content) ELSE -1 END,
	coalesce(d.srcid,0) FROM blob b LEFT JOIN delta d ON d.rid=b.rid
	WHERE b.rid IN (SELECT value FROM json_each(?1))`

// read reads the held blobs member selects, and parses them, reporting
// whether any member could not be read. Delta ancestors are expanded too,
// but they are not parsed: they are what the selection costs.
func (r *referenceReader) read(member func(libfossil.FslID) bool) (bool, error) {
	blobs, err := r.selectRound(member)
	if err != nil || len(blobs) == 0 {
		return false, err
	}
	if err := referencePrefixes(r.ctx, r.q, blobs, r.budget, &r.work, &r.inflated); err != nil {
		return false, err
	}
	if err := r.payloads(blobs); err != nil {
		return false, err
	}
	if err := r.materialize(blobs); err != nil {
		return false, err
	}
	failed := false
	for i := range blobs {
		failed = failed || blobs[i].member && !blobs[i].verified
	}
	return failed, nil
}

// selectRound copies out the members and their held delta ancestors, in rid
// order, and charges their stored and expanded bytes before any read.
func (r *referenceReader) selectRound(member func(libfossil.FslID) bool) ([]referenceBlob, error) {
	chosen := map[int]bool{}
	for i := range r.held {
		if !member(r.held[i].rid) {
			continue
		}
		for at, ok := i, true; ok && !chosen[at]; at, ok = r.index[r.held[at].source] {
			chosen[at] = true // Each step chooses a new blob, so this ends.
		}
	}
	blobs := make([]referenceBlob, 0, len(chosen))
	for i := range r.held {
		if !chosen[i] {
			continue
		}
		b := r.held[i]
		b.member = member(b.rid)
		b.unreadable = b.stored < 0
		if b.unreadable {
			b.stored = 0
		}
		if b.stored > r.budget.StoredBytes-r.stored || b.size > r.budget.ExpandedBytes-r.expanded {
			return nil, fmt.Errorf("sparse preparation: byte budget exceeded")
		}
		r.stored += b.stored
		r.expanded += b.size
		blobs = append(blobs, b)
	}
	return blobs, nil
}

func (r *referenceReader) payloads(blobs []referenceBlob) error {
	for i := range blobs {
		b := &blobs[i]
		if err := r.ctx.Err(); err != nil {
			return err
		}
		if b.unreadable {
			if r.retainUnreadable {
				continue
			}
			return fmt.Errorf("sparse preparation: invalid compressed prefix")
		}
		var packed []byte
		// Non-BLOB storage classes are unknown, never an allocation proof.
		// The same-snapshot predicate guards accidental unbound use too.
		err := r.q.QueryRow(`SELECT content FROM blob WHERE rid=?
			AND typeof(content)='blob' AND length(content)=?`, b.rid, b.stored).Scan(&packed)
		if err != nil {
			return err
		}
		r.work.PayloadReads++
		r.work.InflateBytes += b.inflated
		b.payload, err = referenceInflate(packed, int(b.inflated))
		if err != nil {
			if !r.retainUnreadable {
				return err
			}
			b.unreadable = true
		}
	}
	return nil
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

// materialize expands the round's blobs base first. A base's bytes are
// released once its last delta is applied, so a chain costs its width, not
// its length.
func (r *referenceReader) materialize(blobs []referenceBlob) error {
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
	for at := 0; at < len(queue); at++ {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		if r.work.Visits >= r.budget.Artifacts {
			return fmt.Errorf("sparse preparation: visit budget exceeded")
		}
		r.work.Visits++
		b := &blobs[queue[at]]
		data, err := r.expand(b, blobs, index)
		if err != nil {
			return err
		}
		b.payload = nil
		if b.unreadable || int64(len(data)) != b.size || bindToUUID(b.rid, b.uuid, data) != nil {
			data = nil
		}
		if data == nil {
			r.facts.unreadable = append(r.facts.unreadable, b.uuid)
		} else if err := referenceRecord(r.facts, b, data, &r.cards, r.budget.Cards); err != nil {
			return err
		}
		b.verified = data != nil
		b.pending = len(b.children)
		if b.pending > 0 {
			b.data = data
		}
		queue = append(queue, b.children...)
	}
	// Missing-source/cyclic components cannot yield verified outside proofs.
	for i := range blobs {
		if !blobs[i].verified {
			r.facts.unreadable = append(r.facts.unreadable, blobs[i].uuid)
		}
	}
	return nil
}

// expand applies b's delta to its base, then releases the base when b was
// the last delta waiting on it.
func (r *referenceReader) expand(b *referenceBlob, blobs []referenceBlob, index map[libfossil.FslID]int) ([]byte, error) {
	if b.source == 0 {
		return b.payload, nil
	}
	parent := &blobs[index[b.source]]
	base := parent.data
	parent.pending--
	if parent.pending == 0 {
		parent.data = nil
	}
	if b.unreadable {
		return nil, nil
	}
	size, err := delta.OutputSize(b.payload)
	if err != nil || size != uint64(b.size) || size > uint64(r.budget.ExpandedBytes-r.work.ExpandBytes) {
		return nil, fmt.Errorf("sparse preparation: delta output budget or size mismatch")
	}
	if base == nil {
		return nil, nil
	}
	r.work.ExpandBytes += int64(size) // Before Apply allocates.
	data, err := delta.Apply(base, b.payload)
	if err != nil {
		return nil, nil // Corrupt content cannot prove outside membership.
	}
	return data, nil
}

func referenceRecord(facts *ConeReferenceFacts, b *referenceBlob, data []byte, aliases *int, max int) error {
	facts.held[b.uuid] = b.rid
	if !b.member || !deck.HasValidZ(data) {
		return nil // Ordinary file bytes are never parsed; byte budgets bound them.
	}
	// Bound parser card allocations before Parse, including non-F cards.
	cards := bytes.Count(data, []byte{'\n'})
	if cards > max-*aliases {
		return fmt.Errorf("sparse preparation: card budget exceeded")
	}
	*aliases += cards
	d, err := deck.Parse(data)
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
