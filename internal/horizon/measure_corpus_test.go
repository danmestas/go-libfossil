package horizon_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/horizon"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// stat counts artifacts with their stored (compressed, possibly delta) and
// expanded byte sizes.
type stat struct {
	n        int
	stored   int64
	expanded int64
}

func (s *stat) add(o stat) {
	s.n += o.n
	s.stored += o.stored
	s.expanded += o.expanded
}

// TestCorpusHorizonPayoff measures draft-fossil-horizon-00 Appendix A step 4
// on a real repository: whether the manifests below C are a meaningful share
// of what a content horizon still transfers. For C at several depths back
// along trunk's primary-parent chain it compares the content horizon's
// transfer with the structural horizon's, under both held-set rules. It is
// skipped unless LIBFOSSIL_CORPUS names a repository; it works on a copy.
func TestCorpusHorizonPayoff(t *testing.T) {
	corpus := os.Getenv("LIBFOSSIL_CORPUS")
	if corpus == "" {
		t.Skip("set LIBFOSSIL_CORPUS to a .fossil repository to run")
	}
	work := filepath.Join(t.TempDir(), "corpus.fossil")
	if err := measureCopy(work, corpus); err != nil {
		t.Fatalf("copy corpus: %v", err)
	}
	r, err := repo.Open(work)
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	q := r.DB()
	ctx := context.Background()
	stopHeap := measureHeapPeak()
	start := time.Now()

	m := newMeasure(t, q)
	t.Logf("repository: dense clone %s", m.dense)
	t.Logf("check-in manifests %s; sub-manifest nodes %s", m.checkins, m.nodes)
	t.Logf("other structure (control, wiki, ticket, event, forum, attachment) %s; clusters %s; unparsed non-file %s",
		m.other, m.clusters, m.unparsed)

	chain, depths := m.trunkChain()
	t.Logf("trunk chain: %d check-ins from the tip", len(chain))
	for _, d := range depths {
		if d.index >= len(chain) {
			t.Logf("depth %s: chain too short, skipped", d.label)
			continue
		}
		c := chain[d.index]
		for _, rule := range []cone.HorizonRule{cone.RuleDescendants, cone.RuleLeaves} {
			k, err := horizon.HeldSet(ctx, q, c, rule)
			if err != nil {
				t.Fatalf("held set %s: %v", d.label, err)
			}
			m.report(t, d.label, rule, k)
		}
	}
	peak := stopHeap()
	t.Logf("runtime %v, peak heap %d MiB", time.Since(start).Round(time.Second), peak>>20)
}

type depth struct {
	label string
	index int
}

type measure struct {
	t  *testing.T
	q  db.Querier
	dc *content.Cache

	dense, checkins, nodes, other, clusters, unparsed stat
	uuidLen                                           int

	// seedBytes is the estimated size of a seed's T cards per check-in.
	seedBytes map[libfossil.FslID]int64
	mtime     map[libfossil.FslID]float64
	primary   map[libfossil.FslID]libfossil.FslID
	tip       libfossil.FslID
}

func newMeasure(t *testing.T, q db.Querier) *measure {
	t.Helper()
	m := &measure{t: t, q: q, dc: content.NewCache(64 << 20),
		seedBytes: map[libfossil.FslID]int64{}, mtime: map[libfossil.FslID]float64{},
		primary: map[libfossil.FslID]libfossil.FslID{}}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(q.QueryRow(`SELECT count(*), coalesce(sum(length(content)),0), coalesce(sum(size),0)
		FROM blob WHERE size>=0`).Scan(&m.dense.n, &m.dense.stored, &m.dense.expanded))
	must(q.QueryRow(`SELECT count(*), coalesce(sum(length(b.content)),0), coalesce(sum(b.size),0)
		FROM blob b WHERE b.size>=0 AND b.rid IN (SELECT objid FROM event WHERE type='ci')`).
		Scan(&m.checkins.n, &m.checkins.stored, &m.checkins.expanded))
	must(q.QueryRow(`SELECT length(uuid) FROM blob WHERE size>=0 LIMIT 1`).Scan(&m.uuidLen))

	// Every non-check-in artifact no file version names is parsed, to tell
	// clusters (never transferred by a horizon) from the rest of the
	// structure.
	rows, err := q.Query(`SELECT rid, size, length(content) FROM blob
		WHERE size>=0 AND rid NOT IN (SELECT objid FROM event WHERE type='ci')
		AND rid NOT IN (SELECT fid FROM mlink WHERE fid>0)`)
	must(err)
	type leftover struct {
		rid  libfossil.FslID
		size int64
		sto  int64
	}
	var left []leftover
	for rows.Next() {
		var l leftover
		must(rows.Scan(&l.rid, &l.size, &l.sto))
		left = append(left, l)
	}
	must(rows.Close())
	for _, l := range left {
		s := stat{1, l.sto, l.size}
		d := m.parse(l.rid)
		switch {
		case d == nil:
			m.unparsed.add(s)
		case d.Type == deck.Cluster:
			m.clusters.add(s)
		case d.Type == deck.Submanifest:
			m.nodes.add(s)
		default:
			m.other.add(s)
		}
	}
	rows, err = q.Query(`SELECT t.rid, g.tagname, coalesce(t.value,'') FROM tagxref t
		JOIN tag g ON g.tagid=t.tagid WHERE t.tagtype>0
		AND t.rid IN (SELECT objid FROM event WHERE type='ci')`)
	must(err)
	for rows.Next() {
		var rid libfossil.FslID
		var name, value string
		must(rows.Scan(&rid, &name, &value))
		// "T +name uuid value\n"
		n := int64(len("T +") + len(name) + 1 + m.uuidLen + 1)
		if value != "" {
			n += int64(1 + len(value))
		}
		m.seedBytes[rid] += n
	}
	must(rows.Close())
	rows, err = q.Query(`SELECT objid, mtime FROM event WHERE type='ci'`)
	must(err)
	for rows.Next() {
		var rid libfossil.FslID
		var mt float64
		must(rows.Scan(&rid, &mt))
		m.mtime[rid] = mt
	}
	must(rows.Close())
	rows, err = q.Query(`SELECT cid, pid FROM plink WHERE isprim`)
	must(err)
	for rows.Next() {
		var cid, pid libfossil.FslID
		must(rows.Scan(&cid, &pid))
		m.primary[cid] = pid
	}
	must(rows.Close())
	return m
}

// parse returns the artifact rid holds, or nil when it does not parse.
func (m *measure) parse(rid libfossil.FslID) *deck.Deck {
	data, err := m.dc.Expand(m.q, rid)
	if err != nil || !deck.HasValidZ(data) {
		return nil
	}
	d, err := deck.Parse(data)
	if err != nil {
		return nil
	}
	return d
}

// trunkChain returns trunk's primary-parent chain from its tip, and the
// chain indexes of the measured horizons: 10, 100 and 1000 check-ins back,
// and the check-in nearest one year before the tip by event mtime.
func (m *measure) trunkChain() ([]libfossil.FslID, []depth) {
	var tip libfossil.FslID
	if err := m.q.QueryRow(`SELECT e.objid FROM event e
		JOIN tagxref x ON x.rid=e.objid AND x.tagtype>0 AND x.value='trunk'
		JOIN tag g ON g.tagid=x.tagid AND g.tagname='branch'
		WHERE e.type='ci' ORDER BY e.mtime DESC LIMIT 1`).Scan(&tip); err != nil {
		m.t.Fatalf("trunk tip: %v", err)
	}
	var chain []libfossil.FslID
	for rid := tip; rid > 0 && len(chain) <= len(m.primary)+1; rid = m.primary[rid] {
		chain = append(chain, rid)
	}
	year, best := 0, -1.0
	for i, rid := range chain {
		d := m.mtime[rid] - (m.mtime[tip] - 365)
		if d < 0 {
			d = -d
		}
		if best < 0 || d < best {
			year, best = i, d
		}
	}
	return chain, []depth{{"10 back", 10}, {"100 back", 100}, {"1000 back", 1000},
		{fmt.Sprintf("1 year (%d back)", year), year}}
}

// reach is what a walk from K touches: the check-in manifests and
// sub-manifest nodes reached through K, B and S cards, and the blobs those
// name.
type reach struct {
	pieces stat
	files  stat
	absent int
}

func (m *measure) reach(k []libfossil.FslID) reach {
	var out reach
	visited := map[string]bool{}
	files := map[string]bool{}
	var pending []string
	push := func(uuid string) {
		if uuid != "" && !visited[uuid] {
			visited[uuid] = true
			pending = append(pending, uuid)
		}
	}
	for _, rid := range k {
		var uuid string
		if err := m.q.QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
			m.t.Fatalf("K member %d: %v", rid, err)
		}
		push(uuid)
	}
	for len(pending) > 0 {
		uuid := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		var rid libfossil.FslID
		var s stat
		s.n = 1
		err := m.q.QueryRow("SELECT rid, size, length(content) FROM blob WHERE uuid=? AND size>=0", uuid).
			Scan(&rid, &s.expanded, &s.stored)
		if err != nil {
			out.absent++
			continue
		}
		out.pieces.add(s)
		d := m.parse(rid)
		if d == nil || d.Type != deck.Checkin && d.Type != deck.Submanifest {
			continue
		}
		for _, f := range d.F {
			if f.UUID != "" {
				files[f.UUID] = true
			}
		}
		push(d.B)
		for _, n := range d.S {
			push(n.UUID)
		}
	}
	for uuid := range files {
		var s stat
		s.n = 1
		if err := m.q.QueryRow("SELECT size, length(content) FROM blob WHERE uuid=? AND size>=0", uuid).
			Scan(&s.expanded, &s.stored); err != nil {
			out.absent++
			continue
		}
		out.files.add(s)
	}
	return out
}

func (m *measure) report(t *testing.T, label string, rule cone.HorizonRule, k []libfossil.FslID) {
	t.Helper()
	r := m.reach(k)
	seed := stat{n: 1, expanded: 100}
	for _, rid := range k {
		seed.expanded += m.seedBytes[rid]
	}
	seed.stored = seed.expanded
	all := stat{m.checkins.n + m.nodes.n, m.checkins.stored + m.nodes.stored, m.checkins.expanded + m.nodes.expanded}
	below := stat{all.n - r.pieces.n, all.stored - r.pieces.stored, all.expanded - r.pieces.expanded}
	content, structural := m.other, m.other
	content.add(all)
	content.add(r.files)
	structural.add(r.pieces)
	structural.add(r.files)
	structural.add(seed)
	name := "descendants"
	if rule == cone.RuleLeaves {
		name = "leaves"
	}
	share := func(c, s int64) float64 { return 100 * float64(c-s) / float64(c) }
	t.Logf("[%s, %s rule] K=%d absent=%d", label, name, len(k), r.absent)
	t.Logf("    below horizon (manifests+nodes not reached) %s", below)
	t.Logf("    content horizon    %s", content)
	t.Logf("    structural horizon %s", structural)
	t.Logf("    share (content-structural)/content: %.1f%% stored, %.1f%% expanded; dense clone %s",
		share(content.stored, structural.stored), share(content.expanded, structural.expanded), m.dense)
}

func (s stat) String() string {
	return fmt.Sprintf("%d artifacts, %.1f MiB stored, %.1f MiB expanded",
		s.n, float64(s.stored)/(1<<20), float64(s.expanded)/(1<<20))
}

func measureCopy(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// measureHeapPeak samples the live heap until stopped, which returns the peak.
func measureHeapPeak() func() uint64 {
	var mu sync.Mutex
	var max uint64
	done := make(chan struct{})
	var wg sync.WaitGroup
	sample := func() {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		mu.Lock()
		if ms.HeapAlloc > max {
			max = ms.HeapAlloc
		}
		mu.Unlock()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				sample()
			}
		}
	}()
	return func() uint64 {
		sample()
		close(done)
		wg.Wait()
		return max
	}
}
