package sync

import (
	"context"
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
func FetchRequiredArtifacts(ctx context.Context, r *repo.Repo, peer Transport, roots []string, opts SyncOpts) (*SyncResult, error) {
	if ctx == nil || r == nil {
		panic("sync.FetchRequiredArtifacts: nil context or repo")
	}
	f := requiredFetch{ctx: ctx, repo: r, opts: opts, data: make(map[string]*requiredArtifact)}
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
			if err := f.store(roots, structural, &result); err != nil {
				return nil, err
			}
			return &result, nil
		}
		if peer == nil {
			a, err := content.LoadAbsencesContext(ctx, r.DB())
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("sparse required content: %d elided artifact(s); no configured peer to resolve %d required artifact(s)", a.Count(content.Elided), len(wanted))
		}
		if round >= MaxRounds {
			return nil, fmt.Errorf("sparse required content: unavailable after %d rounds: %s", MaxRounds, wanted[0])
		}
		s := newSession(r, opts)
		cards := []xfer.Card{versionPragma("client-version"), sendSubmanifests()}
		if opts.Private {
			cards = append(cards, &xfer.PragmaCard{Name: "send-private"})
		}
		if s.projectCode == "" {
			return nil, errNoProjectCode("required content")
		}
		// No cone pragmas: the explicit gimme set, not a lasting policy
		// change, authorizes this request's structural coverage.
		cards = append(cards, &xfer.PullCard{ProjectCode: s.projectCode, ServerCode: s.serverCode()})
		requested := make(map[string]bool)
		for _, uuid := range wanted[:min(len(wanted), MaxGimmeBase)] {
			cards = append(cards, &xfer.GimmeCard{UUID: uuid})
			requested[uuid] = true
		}
		if opts.User != "" {
			login, signed, err := s.buildLoginCard(cards, s.projectCode)
			if err != nil {
				return nil, err
			}
			cards = append([]xfer.Card{login}, signed...)
		}
		reply, err := peer.Exchange(ctx, &xfer.Message{Cards: cards})
		if err != nil {
			return nil, fmt.Errorf("sparse required content: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if reply == nil {
			return nil, fmt.Errorf("sparse required content: peer returned no reply")
		}
		accepted, err := f.receive(reply, requested, s.projectCode)
		if err != nil {
			return nil, err
		}
		result.Rounds++
		if accepted == 0 {
			return nil, fmt.Errorf("sparse required content: configured peer cannot supply required artifact %s", wanted[0])
		}
	}
}

type requiredArtifact struct {
	payload []byte
	basis   string
	full    []byte
	private bool
	local   bool
}

type requiredFetch struct {
	ctx   context.Context
	repo  *repo.Repo
	opts  SyncOpts
	data  map[string]*requiredArtifact
	bytes int
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

// resolve expands transport deltas without storing their bases. Local corruption
// is an error, not a reason to silently replace bytes from another repository.
func (f *requiredFetch) resolve(uuid string, wanted map[string]bool, active map[string]bool) ([]byte, error) {
	if err := f.checkName(uuid); err != nil {
		return nil, err
	}
	if active[uuid] || len(active) >= maxRequiredDeltaDepth {
		return nil, fmt.Errorf("sparse required content: cyclic or excessive delta chain at %s", uuid)
	}
	a := f.data[uuid]
	if a == nil {
		if len(f.data)+len(wanted) >= maxRequiredArtifacts {
			return nil, fmt.Errorf("sparse required content: artifact limit %d exceeded", maxRequiredArtifacts)
		}
		if rid, held := content.AvailableByUUID(f.repo.DB(), uuid); held {
			if content.IsPrivate(f.repo.DB(), int64(rid)) && !f.opts.Private {
				return nil, fmt.Errorf("sparse required content: private artifact %s requires private permission", uuid)
			}
			data, err := content.Expand(f.repo.DB(), rid)
			if err != nil {
				return nil, fmt.Errorf("sparse required content: expand %s: %w", uuid, err)
			}
			if !hashMatches(uuid, data) {
				return nil, fmt.Errorf("sparse required content: %s: %w", uuid, errHashMismatch)
			}
			if err := f.reserve(len(data)); err != nil {
				return nil, err
			}
			a = &requiredArtifact{full: data, local: true}
			f.data[uuid] = a
		} else {
			wanted[uuid] = true
			return nil, nil
		}
	}
	if a.full != nil {
		return a.full, nil
	}
	full := a.payload
	if a.basis != "" {
		active[uuid] = true
		basis, err := f.resolve(a.basis, wanted, active)
		delete(active, uuid)
		if err != nil || basis == nil {
			return nil, err
		}
		n, err := delta.OutputSize(a.payload)
		if err != nil {
			return nil, fmt.Errorf("sparse required content: delta %s: %w", uuid, err)
		}
		if n > uint64(maxRequiredBytes-f.bytes) {
			return nil, fmt.Errorf("sparse required content: expanded delta exceeds staging budget")
		}
		if err := f.reserve(int(n)); err != nil {
			return nil, err
		}
		full, err = delta.Apply(basis, a.payload)
		if err != nil {
			return nil, fmt.Errorf("sparse required content: delta %s: %w", uuid, err)
		}
	}
	if !hashMatches(uuid, full) {
		return nil, fmt.Errorf("sparse required content: %s: %w", uuid, errHashMismatch)
	}
	a.full = full
	return full, nil
}

func (f *requiredFetch) closure(roots []string) ([]string, map[string]bool, error) {
	wanted, structural := make(map[string]bool), make(map[string]bool)
	active := make(map[string]bool)
	var visit func(string, deck.ArtifactType, int, bool) error
	visit = func(uuid string, kind deck.ArtifactType, depth int, baseline bool) error {
		if active[uuid] || depth > maxRequiredDepth {
			return fmt.Errorf("sparse required content: cyclic or excessive S/B depth at %s", uuid)
		}
		data, err := f.resolve(uuid, wanted, make(map[string]bool))
		if err != nil || data == nil {
			return err
		}
		d, err := deck.Parse(data)
		if err != nil {
			return fmt.Errorf("sparse required content: parse %s: %w", uuid, err)
		}
		if (kind >= 0 && d.Type != kind) || (d.Type != deck.Checkin && d.Type != deck.Submanifest) {
			return fmt.Errorf("sparse required content: %s has the wrong structural artifact type", uuid)
		}
		if structural[uuid] {
			return nil
		}
		structural[uuid], active[uuid] = true, true
		defer delete(active, uuid)
		for _, s := range d.S {
			if err := visit(s.UUID, deck.Submanifest, depth+1, false); err != nil {
				return err
			}
		}
		// A delta's baseline contributes its own file set, never another B
		// chain (artifact-format-00 8.3). Its tree starts at depth zero.
		if d.B != "" && !baseline {
			return visit(d.B, deck.Checkin, 0, true)
		}
		return nil
	}
	if len(roots) > maxRequiredArtifacts {
		return nil, nil, fmt.Errorf("sparse required content: too many roots")
	}
	for _, uuid := range roots {
		if err := visit(uuid, -1, 0, false); err != nil {
			return nil, nil, err
		}
	}
	names := make([]string, 0, len(wanted))
	for uuid := range wanted {
		names = append(names, uuid)
	}
	sort.Strings(names)
	return names, structural, nil
}

func (f *requiredFetch) receive(reply *xfer.Message, requested map[string]bool, project string) (int, error) {
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
		case *xfer.ErrorCard:
			return 0, fmt.Errorf("sparse required content: peer refused: %s", c.Message)
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
			return 0, fmt.Errorf("sparse required content: private artifact %s requires private permission", uuid)
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

func (f *requiredFetch) store(roots []string, structural map[string]bool, result *SyncResult) error {
	names := make([]string, 0, len(structural))
	for uuid := range structural {
		if !f.data[uuid].local {
			names = append(names, uuid)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return f.ctx.Err()
	}
	err := f.repo.WithBoundTx(func(bound *repo.Repo) error {
		pass := newReceivePass()
		var filled []libfossil.FslID
		for _, uuid := range names {
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
			a := f.data[uuid]
			vis := visibilityPublic
			if a.private {
				vis = visibilityPrivate
			}
			rid, err := storeReceived(f.ctx, bound, pass, uuid, "", a.full, nil, vis, nil)
			if err != nil {
				return err
			}
			if rid > 0 {
				filled = append(filled, rid)
			}
		}
		// Recheck all local structural inputs under the publishing transaction;
		// a concurrent compaction or shun must not turn staged proof stale.
		for uuid := range structural {
			if err := f.ctx.Err(); err != nil {
				return err
			}
			barred, err := shun.IsShunned(bound.DB(), uuid)
			if err != nil || barred {
				if err != nil {
					return err
				}
				return fmt.Errorf("sparse required content: required artifact %s became shunned", uuid)
			}
			rid, held := content.AvailableByUUID(bound.DB(), uuid)
			if !held {
				return fmt.Errorf("sparse required content: local readiness changed for %s; retry", uuid)
			}
			if _, err := content.Expand(bound.DB(), rid); err != nil {
				return err
			}
		}
		return bound.WithTx(func(tx *db.Tx) error {
			for _, uuid := range names {
				rid, _ := blob.Exists(tx, uuid)
				if err := noteReceived(tx, rid, f.data[uuid].full); err != nil {
					return err
				}
			}
			manifest.AfterDephantomizeBatch(f.ctx, bound, filled)
			for _, uuid := range roots {
				d, err := deck.Parse(f.data[uuid].full)
				if err != nil {
					return err
				}
				if d.Type == deck.Checkin {
					rid, _ := blob.Exists(tx, uuid)
					if _, err := manifest.ListFiles(bound, rid); err != nil {
						return err
					}
				}
			}
			return f.ctx.Err()
		})
	})
	if err == nil {
		result.FilesRecvd = len(names)
	}
	return err
}
