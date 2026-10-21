package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/delta"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/shun"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// Recovery is deliberately bounded independently of ordinary synchronization.
// Exceeding a bound refuses the operation; it never publishes a partial closure.
const (
	maxRequiredArtifacts  = 1 << 14
	maxRequiredBytes      = 64 << 20
	maxRequiredDepth      = 16
	maxRequiredDeltaDepth = 128
)

// FetchRequiredArtifacts resolves the S/B closure of explicitly named check-ins
// or sub-manifests. It does not change the store cone, discover a peer, or keep
// credentials. Callers must release their SQL and worktree locks before calling.
//
// All network work precedes one atomic store. Storage-delta bases are verified
// in memory and only structurally reachable artifacts are retained; unsolicited
// file payloads and inventory never enter the repository. At most MaxRounds
// exchanges, maxRequiredArtifacts names and maxRequiredBytes staged bytes are
// permitted. A caller retries its local operation once, revalidating its state.
func FetchRequiredArtifacts(
	ctx context.Context, r *repo.Repo, peer Transport, roots []string, opts SyncOpts,
) (*SyncResult, error) {
	return fetchRequired(ctx, r, peer, roots, opts, false)
}

// FetchRequiredFiles recovers only explicitly named file payloads. The caller
// derives these names from a fresh operation-specific covered file set; this
// does not authorize widening the store cone or ordinary inventory sync.
func FetchRequiredFiles(
	ctx context.Context, r *repo.Repo, peer Transport, names []string, opts SyncOpts,
) (*SyncResult, error) {
	return fetchRequired(ctx, r, peer, names, opts, true)
}

func fetchRequired(
	ctx context.Context, r *repo.Repo, peer Transport, roots []string, opts SyncOpts, files bool,
) (*SyncResult, error) {
	if ctx == nil || r == nil {
		panic("sync.FetchRequiredArtifacts: nil context or repo")
	}
	if err := requiredSyncOptions(opts); err != nil {
		return nil, err
	}
	f := requiredFetch{
		ctx: ctx, repo: r, opts: opts, data: make(map[string]*requiredArtifact), files: files,
	}
	var result SyncResult
	for round := 0; ; round++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		wanted, structural, err := f.closure(roots)
		if err != nil {
			return nil, err
		}
		if len(wanted) == 0 {
			checkins := roots
			if files {
				checkins = nil
			}
			if err := f.store(checkins, structural, &result); err != nil {
				return nil, err
			}
			result.AvoidSubmanifests = f.avoidSubmanifests
			return &result, nil
		}
		if peer == nil {
			a, err := content.LoadAbsencesContext(ctx, r.DB())
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("sparse required content: %d elided artifact(s); "+
				"no configured peer to resolve %d required artifact(s)",
				a.Count(content.Elided), len(wanted))
		}
		if round >= MaxRounds {
			return nil, fmt.Errorf("sparse required content: unavailable after %d rounds: %s",
				MaxRounds, wanted[0])
		}
		if err := f.exchange(peer, wanted); err != nil {
			return nil, err
		}
		result.Rounds++
	}
}

func requiredSyncOptions(opts SyncOpts) error {
	if opts.Push || opts.UV || opts.XTableSync || opts.ShareLinks || opts.MaxSend != 0 ||
		opts.PeerID != "" || opts.CkinLock != nil || opts.ContentCache != nil {
		return fmt.Errorf("sparse required content: recovery options must be read-only")
	}
	if opts.Observer != nil || opts.Buggify != nil {
		return fmt.Errorf("sparse required content: recovery instrumentation is unsupported")
	}
	return nil
}

type requiredArtifact struct {
	payload []byte
	basis   string
	full    []byte
	private bool
	local   bool
}

type requiredFetch struct {
	ctx               context.Context
	repo              *repo.Repo
	opts              SyncOpts
	data              map[string]*requiredArtifact
	bytes             int
	files             bool
	avoidSubmanifests bool
}

func (f *requiredFetch) exchange(peer Transport, wanted []string) error {
	if peer == nil {
		panic("sync.requiredFetch.exchange: nil peer")
	}
	if len(wanted) == 0 {
		panic("sync.requiredFetch.exchange: no required names")
	}
	s := newSession(f.repo, f.opts)
	if s.projectCode == "" {
		return errNoProjectCode("required content")
	}
	cards := []xfer.Card{versionPragma("client-version"), sendSubmanifests()}
	if f.opts.Private {
		cards = append(cards, &xfer.PragmaCard{Name: "send-private"})
	}
	// No cone pragmas: exact gimmes authorize only this request's coverage.
	cards = append(cards, &xfer.PullCard{ProjectCode: s.projectCode, ServerCode: s.serverCode()})
	requested := make(map[string]bool)
	for _, uuid := range wanted[:min(len(wanted), MaxGimmeBase)] {
		cards = append(cards, &xfer.GimmeCard{UUID: uuid})
		requested[uuid] = true
	}
	if f.opts.User != "" {
		login, signed, err := s.buildLoginCard(cards, s.projectCode)
		if err != nil {
			return err
		}
		cards = append([]xfer.Card{login}, signed...)
	}
	reply, err := peer.Exchange(f.ctx, &xfer.Message{Cards: cards})
	if err != nil {
		return fmt.Errorf("sparse required content: %w", err)
	}
	if err := f.ctx.Err(); err != nil {
		return err
	}
	if reply == nil {
		return fmt.Errorf("sparse required content: peer returned no reply")
	}
	accepted, err := f.receive(reply, requested, s.projectCode)
	if err != nil {
		return err
	}
	if accepted == 0 {
		return fmt.Errorf("sparse required content: configured peer cannot supply %s", wanted[0])
	}
	return nil
}

func (f *requiredFetch) checkName(uuid string) error {
	if err := f.ctx.Err(); err != nil {
		return err
	}
	if !hash.IsValidHash(uuid) {
		return fmt.Errorf("sparse required content: invalid artifact name %q", uuid)
	}
	barred, err := shun.IsShunned(f.repo.DB(), uuid)
	if err != nil {
		return err
	}
	if barred {
		return fmt.Errorf("sparse required content: required artifact %s is shunned", uuid)
	}
	return nil
}

func (f *requiredFetch) reserve(n int) error {
	if n > maxRequiredBytes-f.bytes {
		return fmt.Errorf("sparse required content: staging exceeds %d bytes", maxRequiredBytes)
	}
	f.bytes += n
	return nil
}

// load treats local corruption as an error, never as permission to replace
// bytes from another repository. Unavailable storage-delta targets may be fetched.
func (f *requiredFetch) load(uuid string, wanted map[string]bool) (*requiredArtifact, error) {
	if err := f.checkName(uuid); err != nil {
		return nil, err
	}
	if a := f.data[uuid]; a != nil {
		return a, nil
	}
	if len(f.data)+len(wanted) >= maxRequiredArtifacts {
		return nil, fmt.Errorf("sparse required content: artifact limit exceeded")
	}
	rid, held, err := content.CheckAvailableByUUID(f.repo.DB(), uuid)
	if err != nil {
		return nil, err
	}
	if !held {
		wanted[uuid] = true
		return nil, nil
	}
	if content.IsPrivate(f.repo.DB(), int64(rid)) && !f.opts.Private {
		return nil, fmt.Errorf("sparse required content: private artifact %s requires permission", uuid)
	}
	data, err := f.expandLocal(f.repo.DB(), rid)
	if err != nil {
		return nil, fmt.Errorf("sparse required content: expand %s: %w", uuid, err)
	}
	if !hashMatches(uuid, data) {
		return nil, fmt.Errorf("sparse required content: %s: %w", uuid, errHashMismatch)
	}
	a := &requiredArtifact{full: data, local: true}
	f.data[uuid] = a
	return a, nil
}

// resolve expands a bounded transport-delta chain on an explicit stack; its
// bases remain staging-only unless the structural walk independently needs them.
func (f *requiredFetch) resolve(uuid string, wanted map[string]bool) ([]byte, error) {
	path := make([]string, 0, maxRequiredDeltaDepth)
	seen := make(map[string]bool)
	name := uuid
	var full []byte
	for steps := 0; steps < maxRequiredDeltaDepth; steps++ {
		if seen[name] {
			return nil, fmt.Errorf("sparse required content: cyclic delta chain at %s", name)
		}
		seen[name] = true
		a, err := f.load(name, wanted)
		if err != nil || a == nil {
			return nil, err
		}
		if a.full != nil {
			full = a.full
			break
		}
		path = append(path, name)
		if a.basis == "" {
			full = a.payload
			break
		}
		name = a.basis
	}
	if full == nil {
		return nil, fmt.Errorf("sparse required content: excessive delta depth at %s", name)
	}
	for i := len(path) - 1; i >= 0; i-- {
		name = path[i]
		a := f.data[name]
		if a.basis != "" {
			var err error
			full, err = f.applyDelta(name, a.payload, full)
			if err != nil {
				return nil, err
			}
		}
		if !hashMatches(name, full) {
			return nil, fmt.Errorf("sparse required content: %s: %w", name, errHashMismatch)
		}
		a.full = full
	}
	return f.data[uuid].full, nil
}

func (f *requiredFetch) applyDelta(uuid string, payload, basis []byte) ([]byte, error) {
	n, err := delta.OutputSize(payload)
	if err != nil {
		return nil, fmt.Errorf("sparse required content: delta %s: %w", uuid, err)
	}
	if n > uint64(maxRequiredBytes-f.bytes) {
		return nil, fmt.Errorf("sparse required content: expanded delta exceeds staging budget")
	}
	if err := f.reserve(int(n)); err != nil {
		return nil, err
	}
	full, err := delta.Apply(basis, payload)
	if err != nil {
		return nil, fmt.Errorf("sparse required content: delta %s: %w", uuid, err)
	}
	return full, nil
}

type requiredFrame struct {
	uuid     string
	kind     deck.ArtifactType
	depth    int
	baseline bool
	exit     bool
}

func (f *requiredFetch) closure(roots []string) ([]string, map[string]bool, error) {
	if f.files {
		return f.payloads(roots)
	}
	if len(roots) > maxRequiredArtifacts {
		return nil, nil, fmt.Errorf("sparse required content: too many roots")
	}
	wanted, structural := make(map[string]bool), make(map[string]bool)
	active := make(map[string]bool)
	visited := make(map[requiredFrame]bool)
	stack := make([]requiredFrame, 0, len(roots))
	for _, uuid := range roots {
		stack = append(stack, requiredFrame{uuid: uuid, kind: -1})
	}
	const maxVisits = 1 << 20
	for visits := 0; len(stack) > 0; visits++ {
		if visits >= maxVisits {
			return nil, nil, fmt.Errorf("sparse required content: structural visit limit exceeded")
		}
		frame := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if frame.exit {
			delete(active, frame.uuid)
			continue
		}
		if active[frame.uuid] || frame.depth > maxRequiredDepth {
			return nil, nil, fmt.Errorf("sparse required content: cyclic or excessive S/B depth")
		}
		d, err := f.structuralDeck(frame, wanted)
		if err != nil {
			return nil, nil, err
		}
		key := requiredFrame{uuid: frame.uuid, baseline: frame.baseline}
		if d == nil || visited[key] {
			continue
		}
		visited[key] = true
		structural[frame.uuid], active[frame.uuid] = true, true
		frame.exit = true
		stack = append(stack, frame)
		// A baseline contributes its own file set, never another B chain.
		if d.B != "" && !frame.baseline {
			stack = append(stack, requiredFrame{uuid: d.B, kind: deck.Checkin, baseline: true})
		}
		if len(d.S) > maxVisits-len(stack) {
			return nil, nil, fmt.Errorf("sparse required content: structural stack limit exceeded")
		}
		for i := len(d.S) - 1; i >= 0; i-- {
			stack = append(stack, requiredFrame{
				uuid: d.S[i].UUID, kind: deck.Submanifest, depth: frame.depth + 1,
			})
		}
	}
	names := make([]string, 0, len(wanted))
	for uuid := range wanted {
		names = append(names, uuid)
	}
	sort.Strings(names)
	return names, structural, nil
}

func (f *requiredFetch) payloads(roots []string) ([]string, map[string]bool, error) {
	if len(roots) > maxRequiredArtifacts {
		return nil, nil, fmt.Errorf("sparse required content: too many payload roots")
	}
	wanted, required := make(map[string]bool), make(map[string]bool)
	for _, uuid := range roots {
		data, err := f.resolve(uuid, wanted)
		if err != nil {
			return nil, nil, err
		}
		if data != nil {
			required[uuid] = true
		}
	}
	names := make([]string, 0, len(wanted))
	for uuid := range wanted {
		names = append(names, uuid)
	}
	sort.Strings(names)
	return names, required, nil
}

func (f *requiredFetch) structuralDeck(
	frame requiredFrame, wanted map[string]bool,
) (*deck.Deck, error) {
	data, err := f.resolve(frame.uuid, wanted)
	if err != nil || data == nil {
		return nil, err
	}
	d, err := deck.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("sparse required content: parse %s: %w", frame.uuid, err)
	}
	if frame.kind >= 0 && d.Type != frame.kind {
		return nil, fmt.Errorf("sparse required content: wrong structural type at %s", frame.uuid)
	}
	if d.Type != deck.Checkin && d.Type != deck.Submanifest {
		return nil, fmt.Errorf("sparse required content: not a structural artifact: %s", frame.uuid)
	}
	return d, nil
}

func (f *requiredFetch) receive(
	reply *xfer.Message, requested map[string]bool, project string,
) (int, error) {
	accepted := 0
	private := false
	for _, card := range reply.Cards {
		if err := f.ctx.Err(); err != nil {
			return 0, err
		}
		var uuid, basis string
		var data []byte
		switch c := card.(type) {
		case *xfer.PrivateCard:
			private = true
			continue
		case *xfer.PragmaCard:
			if c.Name == "avoid-submanifests" {
				f.avoidSubmanifests = true
			}
			continue
		case *xfer.ErrorCard:
			return 0, fmt.Errorf("sparse required content: peer refused: %s", c.Message)
		case *xfer.UnknownCard, *xfer.BadCommandCard, *xfer.BadFileCard:
			return 0, fmt.Errorf("sparse required content: malformed peer reply")
		case *xfer.PushCard:
			if c.ProjectCode != project {
				return 0, fmt.Errorf("sparse required content: wrong project from peer")
			}
			continue
		case *xfer.FileCard:
			uuid, basis, data = c.UUID, c.DeltaSrc, c.Content
		case *xfer.CFileCard:
			uuid, basis, data = c.UUID, c.DeltaSrc, c.Content
		default:
			continue
		}
		isPrivate := private
		private = false
		if !requested[uuid] {
			continue
		}
		if isPrivate && !f.opts.Private {
			return 0, fmt.Errorf("sparse required content: private artifact %s requires permission", uuid)
		}
		if err := f.checkName(uuid); err != nil {
			return 0, err
		}
		if basis != "" {
			if err := f.checkName(basis); err != nil {
				return 0, err
			}
		}
		if f.data[uuid] != nil {
			continue
		}
		if len(f.data) >= maxRequiredArtifacts {
			return 0, fmt.Errorf("sparse required content: artifact limit exceeded")
		}
		if err := f.reserve(len(data)); err != nil {
			return 0, err
		}
		payload := append([]byte{}, data...)
		f.data[uuid] = &requiredArtifact{payload: payload, basis: basis, private: isPrivate}
		accepted++
	}
	return accepted, nil
}

// A held delta on an absent basis has a real row but no available bytes.
// Replace that storage representation with the independently verified full
// target, preserving its rid and rolling back the replacement on any failure.
func (f *requiredFetch) prepareTarget(tx *db.Tx, uuid string) error {
	var rid libfossil.FslID
	var size int64
	err := tx.QueryRow("SELECT rid,size FROM blob WHERE uuid=?", uuid).Scan(&rid, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if content.IsPrivate(tx, int64(rid)) && !f.opts.Private {
		return fmt.Errorf("sparse required content: required artifact %s became private", uuid)
	}
	_, held, err := content.CheckAvailableByUUID(tx, uuid)
	if err != nil || held || size < 0 {
		return err
	}
	if _, err := tx.Exec("DELETE FROM delta WHERE rid=?", rid); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE blob SET size=-1,content=NULL WHERE rid=?", rid); err != nil {
		return err
	}
	_, err = tx.Exec("INSERT OR IGNORE INTO phantom(rid) VALUES(?)", rid)
	return err
}

func (f *requiredFetch) storeArtifact(
	bound *repo.Repo, pass *receivePass, uuid string,
) (libfossil.FslID, error) {
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	barred, err := shun.IsShunned(bound.DB(), uuid)
	if err != nil {
		return 0, err
	}
	if barred {
		return 0, fmt.Errorf("sparse required content: required artifact %s became shunned", uuid)
	}
	if err := bound.WithTx(func(tx *db.Tx) error { return f.prepareTarget(tx, uuid) }); err != nil {
		return 0, err
	}
	a := f.data[uuid]
	vis := visibilityPublic
	if a.private {
		vis = visibilityPrivate
	}
	return storeReceived(f.ctx, bound, pass, uuid, "", a.full, nil, vis, nil)
}

func (f *requiredFetch) revalidate(bound *repo.Repo, uuid string) error {
	if err := f.ctx.Err(); err != nil {
		return err
	}
	barred, err := shun.IsShunned(bound.DB(), uuid)
	if err != nil {
		return err
	}
	if barred {
		return fmt.Errorf("sparse required content: required artifact %s became shunned", uuid)
	}
	rid, held, err := content.CheckAvailableByUUID(bound.DB(), uuid)
	if err != nil {
		return err
	}
	if !held {
		return fmt.Errorf("sparse required content: local readiness changed for %s; retry", uuid)
	}
	if content.IsPrivate(bound.DB(), int64(rid)) && !f.opts.Private {
		return fmt.Errorf("sparse required content: required artifact %s became private", uuid)
	}
	// Verification has its own fixed temporary budget; staged bytes remain
	// bounded separately while the transaction compares current durable inputs.
	verify := requiredFetch{ctx: f.ctx, repo: bound, opts: f.opts}
	data, err := verify.expandLocal(bound.DB(), rid)
	if err != nil {
		return err
	}
	if !hashMatches(uuid, data) {
		return fmt.Errorf("sparse required content: %s: %w", uuid, errHashMismatch)
	}
	return nil
}

func (f *requiredFetch) finishStore(
	bound *repo.Repo, roots, names []string, structural map[string]bool, result *SyncResult,
) error {
	return bound.WithTx(func(tx *db.Tx) error {
		for _, uuid := range names {
			rid, ok := blob.Exists(tx, uuid)
			if !ok {
				return fmt.Errorf("sparse required content: stored artifact unavailable: %s", uuid)
			}
			if err := noteReceived(tx, rid, f.data[uuid].full); err != nil {
				return err
			}
		}
		cache, rids, err := f.verifiedCache(tx, structural)
		if err != nil {
			return err
		}
		for _, uuid := range roots {
			d, err := deck.Parse(f.data[uuid].full)
			if err != nil {
				return err
			}
			if err := manifest.ValidateRequiredRoot(tx, cache, d); err != nil {
				return err
			}
		}
		if len(names) == 0 {
			return f.ctx.Err()
		}
		linked, err := manifest.LinkRequiredArtifacts(f.ctx, bound, cache, rids)
		if err != nil {
			return err
		}
		result.ArtifactsLinked = linked
		return f.ctx.Err()
	})
}

func (f *requiredFetch) verifiedCache(
	q db.Querier, structural map[string]bool,
) (*content.Cache, []libfossil.FslID, error) {
	cache := content.NewCache(maxRequiredBytes)
	names := make([]string, 0, len(structural))
	for uuid := range structural {
		names = append(names, uuid)
	}
	sort.Strings(names)
	rids := make([]libfossil.FslID, 0, len(names))
	for _, uuid := range names {
		var rid libfossil.FslID
		if err := q.QueryRow("SELECT rid FROM blob WHERE uuid=?", uuid).Scan(&rid); err != nil {
			return nil, nil, err
		}
		cache.Remember(rid, f.data[uuid].full)
		rids = append(rids, rid)
	}
	return cache, rids, nil
}

func (f *requiredFetch) store(
	roots []string, structural map[string]bool, result *SyncResult,
) error {
	names := make([]string, 0, len(structural))
	for uuid := range structural {
		if !f.data[uuid].local {
			names = append(names, uuid)
		}
	}
	sort.Strings(names)
	err := f.repo.WithBoundTx(func(bound *repo.Repo) error {
		pass := newReceivePass()
		for _, uuid := range names {
			if _, err := f.storeArtifact(bound, pass, uuid); err != nil {
				return err
			}
		}
		// Recheck local inputs under the publishing transaction: compaction or
		// a concurrent shun must not turn staged proof stale.
		for uuid := range structural {
			if err := f.revalidate(bound, uuid); err != nil {
				return err
			}
		}
		return f.finishStore(bound, roots, names, structural, result)
	})
	if err == nil {
		result.FilesRecvd = len(names)
	}
	return err
}
