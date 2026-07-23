package sync

import (
	"context"
	"fmt"

	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/uv"
	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/simio"
)

const (
	// DefaultMaxSend is the default byte budget per round for file payloads.
	DefaultMaxSend = 250000
	// MaxRounds caps the number of sync rounds before giving up.
	MaxRounds = 100
	// MaxGimmeBase is the minimum gimme cap per round.
	MaxGimmeBase = 200
)

// BuggifyChecker is an optional fault injection interface.
// Pass nil in production — implementations should be nil-safe.
type BuggifyChecker interface {
	Check(site string, probability float64) bool
}

// SyncOpts configures a sync session.
type SyncOpts struct {
	Push, Pull  bool   // enable push/pull directions (at least one must be true)
	ProjectCode string // repo's project-code — sent to identify the repository
	ServerCode  string // server-code from a previous session (cookie-like, speeds up sync)
	User        string // login user — empty means unauthenticated "nobody" sync
	Password    string // login password
	PeerID      string // identifies this leaf agent instance (for observability)
	MaxSend     int    // byte budget per round for file payloads (0 defaults to DefaultMaxSend)
	UV          bool   // enable unversioned file sync (wiki, forum, attachments)
	XTableSync  bool   // enable extension table sync (peer_registry, etc.) — only between EdgeSync peers
	Private     bool   // enable private artifact sync
	Env         *simio.Env     // nil defaults to RealEnv
	Buggify     BuggifyChecker // nil in production — used by DST for fault injection
	Observer    Observer       // nil defaults to no-op
	CkinLock     *CkinLockReq    // nil = no lock requested
	ContentCache *content.Cache  // nil = no caching (every Expand walks the full delta chain)
}

// CkinLockReq requests a server-side check-in lock.
type CkinLockReq struct {
	ParentUUID string
	ClientID   string
}

// SyncResult reports what happened during a sync.
type SyncResult struct {
	Rounds, FilesSent, FilesRecvd int
	UVFilesSent, UVFilesRecvd     int
	UVGimmesSent                  int
	BytesSent, BytesRecvd         int64
	ArtifactsLinked               int
	Errors                        []string
	CkinLockFail *CkinLockFail // nil = no conflict
}

// session holds the mutable state of a running sync.
type session struct {
	repo                *repo.Repo
	env                 *simio.Env
	opts                SyncOpts
	result              SyncResult
	cookie              string
	remoteHas           map[string]bool
	phantoms            map[string]bool
	pendingSend         map[string]bool
	filesRecvdLastRound int
	igotSentThisRound   int
	maxSend             int
	phantomAge          map[string]int // UUID -> consecutive rounds gimme'd without delivery
	uvHashSent          bool
	uvPushOK            bool
	uvPullOnly          bool
	uvToSend            map[string]bool // name -> true=full content, false=mtime-only
	uvGimmes            map[string]bool
	nUvGimmeSent        int
	nUvFileRcvd         int
	nGimmeRcvd          int // cumulative gimmes received across all rounds
	nextIsPrivate       bool // true when a PrivateCard precedes the next file/cfile
	roundStats          RoundStats
	xTableHashSent map[string]bool            // table -> true if xtable-hash pragma sent
	xTableGimmes   map[string]map[string]bool // table -> pkHash -> true
	xTableToSend   map[string]map[string]bool // table -> pkHash -> true
	xTableCache    map[string]*repo.TableDef  // cached table defs, lazily loaded
	dephantomizeHook    func(libfossil.FslID) // called after phantom→real transition
	cache               *content.Cache // nil = passthrough to content.Expand
}

func newSession(r *repo.Repo, opts SyncOpts) *session {
	if r == nil {
		panic("sync.newSession: r must not be nil")
	}
	ms := opts.MaxSend
	if ms <= 0 {
		ms = DefaultMaxSend
	}
	env := opts.Env
	if env == nil {
		env = simio.RealEnv()
	}
	s := &session{
		repo:           r,
		env:            env,
		opts:           opts,
		maxSend:        ms,
		remoteHas:      make(map[string]bool),
		phantoms:       make(map[string]bool),
		pendingSend:    make(map[string]bool),
		phantomAge:     make(map[string]int),
		xTableHashSent: make(map[string]bool),
		xTableGimmes:   make(map[string]map[string]bool),
		xTableToSend:   make(map[string]map[string]bool),
		cache:          opts.ContentCache,
	}

	// Pre-populate uvToSend with all local non-tombstone UV files.
	if opts.UV {
		if err := uv.EnsureSchema(r.DB()); err != nil {
			panic(fmt.Sprintf("sync.newSession: uv.EnsureSchema: %v", err))
		}
		entries, err := uv.List(r.DB())
		if err != nil {
			panic(fmt.Sprintf("sync.newSession: uv.List: %v", err))
		}
		s.uvToSend = make(map[string]bool)
		s.uvGimmes = make(map[string]bool)
		for _, e := range entries {
			// Include both live files and tombstones so deletions propagate.
			s.uvToSend[e.Name] = true
		}
	}

	return s
}

// getXTableDef returns a cached table definition, loading all tables on first miss.
func (s *session) getXTableDef(tableName string) (*repo.TableDef, error) {
	if s.xTableCache != nil {
		if def, ok := s.xTableCache[tableName]; ok {
			return def, nil
		}
	}
	// Cache miss — load all tables.
	tables, err := repo.ListSyncedTables(s.repo.DB())
	if err != nil {
		return nil, err
	}
	s.xTableCache = make(map[string]*repo.TableDef)
	for _, info := range tables {
		d := info.Def
		s.xTableCache[info.Name] = &d
	}
	if def, ok := s.xTableCache[tableName]; ok {
		return def, nil
	}
	return nil, nil
}

// Sync runs the client sync loop against the given transport.
// It returns once the protocol has converged or a fatal error occurs.
func Sync(ctx context.Context, r *repo.Repo, t Transport, opts SyncOpts) (result *SyncResult, err error) {
	if r == nil {
		panic("sync.Sync: r must not be nil")
	}
	if t == nil {
		panic("sync.Sync: t must not be nil")
	}
	defer func() {
		if err == nil && result == nil {
			panic("sync.Sync: result must not be nil on success")
		}
	}()

	obs := resolveObserver(opts.Observer)
	s := newSession(r, opts)

	ctx = obs.Started(ctx, SessionStart{
		Operation:   "sync",
		Push:        opts.Push,
		Pull:        opts.Pull,
		UV:          opts.UV,
		ProjectCode: opts.ProjectCode,
		PeerID:      opts.PeerID,
	})

	for cycle := 0; ; cycle++ {
		select {
		case <-ctx.Done():
			obs.Completed(ctx, sessionEndFromSync(&s.result, opts.ProjectCode), ctx.Err())
			return &s.result, ctx.Err()
		default:
		}
		if cycle >= MaxRounds {
			err := fmt.Errorf("sync: exceeded %d rounds", MaxRounds)
			obs.Completed(ctx, sessionEndFromSync(&s.result, opts.ProjectCode), err)
			return &s.result, err
		}

		roundCtx := obs.RoundStarted(ctx, cycle)
		s.roundStats = RoundStats{}

		req, err := s.buildRequest(cycle)
		if err != nil {
			obs.Error(roundCtx, err)
			obs.RoundCompleted(roundCtx, cycle, s.roundStats)
			obs.Completed(ctx, sessionEndFromSync(&s.result, opts.ProjectCode), err)
			return &s.result, fmt.Errorf("sync: buildRequest round %d: %w", cycle, err)
		}
		resp, err := t.Exchange(ctx, req)
		if err != nil {
			obs.Error(roundCtx, err)
			obs.RoundCompleted(roundCtx, cycle, s.roundStats)
			obs.Completed(ctx, sessionEndFromSync(&s.result, opts.ProjectCode), err)
			return &s.result, fmt.Errorf("sync: exchange round %d: %w", cycle, err)
		}

		sentBefore := s.result.FilesSent
		recvdBefore := s.result.FilesRecvd

		done, err := s.processResponse(resp)

		s.roundStats.FilesSent = s.result.FilesSent - sentBefore
		s.roundStats.FilesReceived = s.result.FilesRecvd - recvdBefore
		s.result.BytesSent += s.roundStats.BytesSent
		s.result.BytesRecvd += s.roundStats.BytesReceived

		if err != nil {
			obs.Error(roundCtx, err)
			obs.RoundCompleted(roundCtx, cycle, s.roundStats)
			obs.Completed(ctx, sessionEndFromSync(&s.result, opts.ProjectCode), err)
			return &s.result, fmt.Errorf("sync: processResponse round %d: %w", cycle, err)
		}
		s.result.Rounds = cycle + 1

		obs.RoundCompleted(roundCtx, cycle, s.roundStats)

		if done {
			break
		}
	}

	// Auto-crosslink after convergence. Context-aware for the same reason the
	// clone's sweep is (#120): this runs after the round loop, so it has no
	// round boundary of its own at which a cancelled context could be noticed.
	linked, xlinkErr := manifest.CrosslinkContext(ctx, s.repo)
	if xlinkErr != nil {
		obs.Completed(ctx, sessionEndFromSync(&s.result, opts.ProjectCode), xlinkErr)
		return &s.result, fmt.Errorf("sync: crosslink: %w", xlinkErr)
	}
	s.result.ArtifactsLinked = linked

	obs.Completed(ctx, sessionEndFromSync(&s.result, opts.ProjectCode), nil)
	return &s.result, nil
}

// sessionEndFromSync builds a SessionEnd from a SyncResult.
func sessionEndFromSync(r *SyncResult, projectCode string) SessionEnd {
	return SessionEnd{
		Operation:    "sync",
		Rounds:       r.Rounds,
		FilesSent:    r.FilesSent,
		FilesRecvd:   r.FilesRecvd,
		UVFilesSent:  r.UVFilesSent,
		UVFilesRecvd: r.UVFilesRecvd,
		UVGimmesSent: r.UVGimmesSent,
		BytesSent:    r.BytesSent,
		BytesRecvd:   r.BytesRecvd,
		ProjectCode:  projectCode,
		Errors:       r.Errors,
	}
}

// cardSummary returns a short string describing a card (for trace logging).
// codePrefix returns the first 8 characters of a code for trace logging,
// or the full string when shorter. Avoids slice-out-of-range panics when
// ServerCode/ProjectCode are empty (the no-codes-negotiated form).
func codePrefix(s string) string {
	if len(s) >= 8 {
		return s[:8]
	}
	return s
}

func cardSummary(c xfer.Card) string {
	switch v := c.(type) {
	case *xfer.PullCard:
		return fmt.Sprintf("srv=%s proj=%s", codePrefix(v.ServerCode), codePrefix(v.ProjectCode))
	case *xfer.PushCard:
		return fmt.Sprintf("srv=%s proj=%s", codePrefix(v.ServerCode), codePrefix(v.ProjectCode))
	case *xfer.IGotCard:
		return v.UUID[:16] + "..."
	case *xfer.GimmeCard:
		return v.UUID[:16] + "..."
	case *xfer.FileCard:
		return fmt.Sprintf("uuid=%s len=%d delta=%s", v.UUID[:16], len(v.Content), v.DeltaSrc)
	case *xfer.CFileCard:
		return fmt.Sprintf("uuid=%s len=%d delta=%s", v.UUID[:16], len(v.Content), v.DeltaSrc)
	case *xfer.LoginCard:
		return fmt.Sprintf("user=%s", v.User)
	case *xfer.CookieCard:
		return v.Value
	case *xfer.ErrorCard:
		return v.Message
	case *xfer.MessageCard:
		return v.Message
	case *xfer.PragmaCard:
		return v.Name
	default:
		return ""
	}
}

// cardsByType is a helper that filters cards by type for testing/debugging.
func cardsByType(msg *xfer.Message, ct xfer.CardType) []xfer.Card {
	var out []xfer.Card
	for _, c := range msg.Cards {
		if c.Type() == ct {
			out = append(out, c)
		}
	}
	return out
}
