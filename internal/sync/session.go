package sync

import (
	"context"
	"fmt"
	"github.com/danmestas/go-libfossil/db"

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
	Push, Pull   bool           // enable push/pull directions (at least one must be true)
	ProjectCode  string         // repo's project-code — sent to identify the repository
	ServerCode   string         // server-code from a previous session (cookie-like, speeds up sync)
	User         string         // login user — empty means unauthenticated "nobody" sync
	Password     string         // login password
	PeerID       string         // identifies this leaf agent instance (for observability)
	MaxSend      int            // byte budget per round for file payloads (0 defaults to DefaultMaxSend)
	UV           bool           // enable unversioned file sync (wiki, forum, attachments)
	XTableSync   bool           // enable extension table sync (peer_registry, etc.) — only between EdgeSync peers
	Private      bool           // enable private artifact sync
	ShareLinks   bool           // share-links mode: request and keep links (§9.5); pull only
	Env          *simio.Env     // nil defaults to RealEnv
	Buggify      BuggifyChecker // nil in production — used by DST for fault injection
	Observer     Observer       // nil defaults to no-op
	CkinLock     *CkinLockReq   // nil = no lock requested
	ContentCache *content.Cache // nil = no caching; rid-keyed, so not across a rollback
	// Origin names the peer this session addresses, keying the horizon
	// cursor a horizon store keeps per origin (draft-fossil-horizon-00
	// §6.2.6). Empty keeps no cursor: every request announces in full.
	Origin string
	// MergeBaseOf, when set, asks the origin for the merge base it names
	// (draft-fossil-horizon-00 §6.2.4), once, in the first request; it needs
	// Pull, and one or two secondaries. The answer is in the result.
	MergeBaseOf MergeBaseQuery
}

// CkinLockReq requests a server-side check-in lock.
type CkinLockReq struct {
	ParentUUID string
	ClientID   string
}

// SyncResult reports what happened during a sync.
type SyncResult struct {
	Elided int // artifacts deliberately absent under the store cone
	// ConeServed counts the replies whose peer acknowledged applying the
	// store cone (pragma cone-served, sparse-cone §6.3). The other rounds of
	// a sparse store were served densely and filtered locally.
	ConeServed int
	// HorizonServed counts the replies whose origin served the store's
	// horizon at either level (pragma horizon-served, horizon §6.2.5). The
	// other rounds were served densely and classified locally (§6.3).
	HorizonServed int
	// StructuralServed counts the replies that served the structural level.
	// HorizonFallback reports a structural store that fell back to the
	// content horizon for good: its origin did not serve the structural
	// level with a seed in the first reply, or the seed failed its
	// provenance (§5.1, §5.2.4, §6.3).
	StructuralServed              int
	HorizonFallback               bool
	Rounds, FilesSent, FilesRecvd int
	UVFilesSent, UVFilesRecvd     int
	UVGimmesSent                  int
	BytesSent, BytesRecvd         int64
	ArtifactsLinked               int
	Errors                        []string
	CkinLockFail                  *CkinLockFail // nil = no conflict
	// HorizonHistoryURL is the interface outside the protocol at which the
	// origin says history below a horizon can be queried (horizon-served
	// URL, §6.2.5), the last a reply named; "" when none did.
	HorizonHistoryURL string
	// HorizonRemedy is set when HorizonFallback is: it names C, the
	// manifests below it the store now wants, and where they are held, the
	// history interface a reply named when one did (§6.3, §6.4).
	HorizonRemedy string
	// AvoidSubmanifests reports that this exchange received pragma
	// avoid-submanifests (draft-fossil-submanifests-00 §7.2). Sync makes no
	// commit; this observation does not bind a later local commit.
	AvoidSubmanifests bool
	// MergeBaseAnswered reports that the origin answered MergeBaseOf, and
	// MergeBase is its answer: the base's hash, or "" when the two share no
	// ancestor.
	MergeBaseAnswered bool
	MergeBase         string
}

// session holds the mutable state of a running sync.
type session struct {
	ctx            context.Context
	unjudged       unjudged // deltas stored unreadable, counted once judged
	sparseReceived map[string]bool
	horizon        bool // the store cone has a horizon: no cluster is asked for
	structural     structuralClient
	// reloadPhantoms makes the next phantom load ask for every wanted
	// phantom, announced or not: a structural store that fell back wants the
	// manifests below its horizon, which an origin announced while they were
	// still elided (horizon §6.3).
	reloadPhantoms bool
	// The horizon cursor (draft-fossil-horizon-00 §6.2.6), kept per origin
	// under coneDigest: cursor is the record this session started from,
	// startPosition this store's position then, and peerCursor the last
	// cursor a reply handed out. coneDigest is empty when none is kept.
	coneDigest        string
	cursor            cursorRecord
	startPosition     int64
	peerCursor        int64
	repo              *repo.Repo
	env               *simio.Env
	opts              SyncOpts
	projectCode       string // resolved once: SyncOpts.ProjectCode, else the repo's own
	result            SyncResult
	cookie            string
	remoteHas         map[string]bool
	phantoms          map[string]bool
	pendingSend       map[string]bool
	mxPhantomReq      int // gimme cards allowed per request; fossil's mxPhantomReq
	igotSentThisRound int
	maxSend           int
	phantomAge        map[string]int // UUID -> consecutive rounds gimme'd without delivery
	uvHashSent        bool
	uvPushOK          bool
	uvPullOnly        bool
	uvToSend          map[string]bool // name -> true=full content, false=mtime-only
	uvGimmes          map[string]bool
	nUvGimmeSent      int
	nUvFileRcvd       int
	nGimmeRcvd        int  // cumulative gimmes received across all rounds
	nextIsPrivate     bool // true when a PrivateCard precedes the next file/cfile
	// pushStopped is set the moment the server's "message pull only ..." is
	// read (§10.2): the client keeps pulling but answers no further gimme, as
	// fossil's client_sync clears SYNC_PUSH mid-reply, so a gimme after the
	// message in the same reply goes unanswered.
	pushStopped bool
	// pushCardDropped lags pushStopped by one request. fossil emits the next
	// request's push card before it reads the reply that stops the push, so
	// the request built right after the message still carries its push card;
	// only the one after that drops it. buildRequest promotes pushStopped into
	// this after emitting each request's push card.
	pushCardDropped bool
	// visibilityUpdates buffers this round's igot-announced status changes so
	// they are written in one transaction rather than one per card.
	visibilityUpdates []visibilityUpdate
	// newPhantoms buffers this round's igot phantoms for the same reason.
	newPhantoms []phantomUpdate
	// peerVersion is the server's release from server-version; before one
	// arrives it is this library's own, as in fossil's client_sync (§7.6).
	peerVersion int
	// loginCookieOK is set once a server-version passes the §6.5 gate:
	// later requests may then log in through the x-f-l-c cookie.
	loginCookieOK bool
	// sha3Refusals counts gimmes for SHA3 content withheld from an older
	// server; the next request carries one refusal error card for each.
	sha3Refusals int
	// smGate is the draft-fossil-submanifests-00 §7.2 transfer gate and
	// smRefusals the gimmes it withheld this round: the next request
	// carries one refusal card for them, and the result records each.
	smGate     submanifestGate
	smRefusals int
	// peerReadsTrees is set once a reply carries send-submanifests (§7.2);
	// until then the gate treats the server as one that reads no tree.
	peerReadsTrees bool
	// peerDate is the server's check-in date from server-version: 0 before
	// one arrives, or when it carries no DATE and TIME, which no date gate
	// passes (§8.6).
	peerDate int
	// privateIGots are the private artifacts a gimme asked for outside a
	// private sync; the next request answers each with igot HASH 1 (§8.6).
	privateIGots []string
	// dephantomized buffers the phantoms this reply filled, whose
	// after-dephantomize cascade runs once at the end of the reply.
	dephantomized []libfossil.FslID
	// pass is the current reply's verification pass (§7.7).
	pass             *receivePass
	roundStats       RoundStats
	xTableHashSent   map[string]bool            // table -> true if xtable-hash pragma sent
	xTableGimmes     map[string]map[string]bool // table -> pkHash -> true
	xTableToSend     map[string]map[string]bool // table -> pkHash -> true
	xTableCache      map[string]*repo.TableDef  // cached table defs, lazily loaded
	dephantomizeHook func(libfossil.FslID)      // called after phantom→real transition
	cache            *content.Cache             // nil = passthrough to content.Expand
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
	// A repo knows its own project code, so a caller holding one never has to
	// supply it. An explicit SyncOpts.ProjectCode still wins; a repo with no
	// stored code leaves this empty, and the sites that need one say so.
	projectCode := opts.ProjectCode
	if projectCode == "" {
		projectCode, _ = r.Config("project-code")
	}

	s := &session{
		repo:           r,
		env:            env,
		opts:           opts,
		projectCode:    projectCode,
		maxSend:        ms,
		remoteHas:      make(map[string]bool),
		phantoms:       make(map[string]bool),
		mxPhantomReq:   MaxGimmeBase,
		pendingSend:    make(map[string]bool),
		phantomAge:     make(map[string]int),
		peerVersion:    releaseVersion,
		xTableHashSent: make(map[string]bool),
		xTableGimmes:   make(map[string]map[string]bool),
		xTableToSend:   make(map[string]map[string]bool),
		cache:          opts.ContentCache,
	}

	// Pre-populate uvToSend with the local files, tombstones left out, as
	// fossil seeds uv_tosend from rows whose hash is not NULL (§8.5.1). A
	// local deletion newer than the server's copy is queued when its
	// uvigot shows that.
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
			if e.Hash != "" {
				s.uvToSend[e.Name] = true
			}
		}
	}

	// A gate whose indicator cannot be read is armed: it then classifies
	// every artifact an older peer asks for, which costs time but can never
	// hand that peer a tree.
	gate, err := newSubmanifestGate(r.DB())
	if err != nil {
		gate = submanifestGate{armed: true}
	}
	s.smGate = gate
	if c, err := content.StoreCone(r.DB()); err == nil && c != nil {
		s.sparseReceived = map[string]bool{}
		_, s.horizon = c.Horizon()
		if s.horizon && opts.Origin != "" {
			s.coneDigest = c.Digest()
		}
	}

	return s
}

// errNoProjectCode reports that neither the caller nor the repo could supply
// the project code the named wire element needs.
func errNoProjectCode(what string) error {
	return fmt.Errorf("sync: %s requires a project-code: none in SyncOpts.ProjectCode and none stored in the repo's config", what)
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
		// The invariant below is only meaningful on a normal return: while a
		// panic unwinds, err and result are both nil and asserting here would
		// replace the real cause with this one.
		if p := recover(); p != nil {
			panic(p)
		}
		if err == nil && result == nil {
			panic("sync.Sync: result must not be nil on success")
		}
	}()

	if opts.MergeBaseOf.Primary != "" && !opts.MergeBaseOf.valid() {
		return nil, fmt.Errorf("sync: a merge-base query names full hashes and one or two secondaries (horizon §6.2.4): %q",
			opts.MergeBaseOf.hashes())
	}

	obs := resolveObserver(opts.Observer)
	var clock simio.Clock
	if opts.Env != nil {
		clock = opts.Env.Clock
	}
	r, err = withClientReceipt(r, opts.User, clock)
	if err != nil {
		return nil, fmt.Errorf("sync: %w", err)
	}
	s := newSession(r, opts)
	s.ctx = ctx
	if s.structural, err = newStructuralClient(r.DB()); err != nil {
		return &s.result, fmt.Errorf("sync: %w", err)
	}
	if err := s.seedPhantoms(); err != nil {
		return &s.result, fmt.Errorf("sync: load phantoms: %w", err)
	}
	if err := s.loadCursor(); err != nil {
		return &s.result, err
	}

	ctx = obs.Started(ctx, SessionStart{
		Operation:   "sync",
		Push:        opts.Push,
		Pull:        opts.Pull,
		UV:          opts.UV,
		ProjectCode: s.projectCode,
		PeerID:      opts.PeerID,
	})

	for cycle := 0; ; cycle++ {
		select {
		case <-ctx.Done():
			obs.Completed(ctx, sessionEndFromSync(&s.result, s.projectCode), ctx.Err())
			return &s.result, ctx.Err()
		default:
		}
		if cycle >= MaxRounds {
			err := fmt.Errorf("sync: exceeded %d rounds", MaxRounds)
			obs.Completed(ctx, sessionEndFromSync(&s.result, s.projectCode), err)
			return &s.result, err
		}

		roundCtx := obs.RoundStarted(ctx, cycle)
		s.roundStats = RoundStats{}

		req, err := s.buildRequest(cycle)
		if err != nil {
			obs.Error(roundCtx, err)
			obs.RoundCompleted(roundCtx, cycle, s.roundStats)
			obs.Completed(ctx, sessionEndFromSync(&s.result, s.projectCode), err)
			return &s.result, fmt.Errorf("sync: buildRequest round %d: %w", cycle, err)
		}
		resp, err := exchangeWithRetry(ctx, t, req, s.env.Clock, obs)
		if err != nil {
			obs.Error(roundCtx, err)
			obs.RoundCompleted(roundCtx, cycle, s.roundStats)
			obs.Completed(ctx, sessionEndFromSync(&s.result, s.projectCode), err)
			return &s.result, fmt.Errorf("sync: exchange round %d: %w", cycle, err)
		}
		notePeer(s.repo, t)

		sentBefore := s.result.FilesSent
		recvdBefore := s.result.FilesRecvd

		done, err := s.processResponse(ctx, resp)

		s.roundStats.FilesSent = s.result.FilesSent - sentBefore
		s.roundStats.FilesReceived = s.result.FilesRecvd - recvdBefore
		s.result.BytesSent += s.roundStats.BytesSent
		s.result.BytesRecvd += s.roundStats.BytesReceived

		if err != nil {
			obs.Error(roundCtx, err)
			obs.RoundCompleted(roundCtx, cycle, s.roundStats)
			obs.Completed(ctx, sessionEndFromSync(&s.result, s.projectCode), err)
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
	// fossil runs manifest_crosslink_end inside each round's transaction
	// (xfer.c); here each sweep batch repairs in its own transaction, so a
	// cancelled sweep cannot roll back a committed reply. The phantom-fill
	// cascades need no repair here: each cascade batch
	// repairs leaves and propagated tags before it commits, as fossil's
	// per-round manifest_crosslink_end does (src/xfer.c), so a round that fails
	// after earlier rounds' cascades committed leaves no check-in with an event
	// row and no leaf row (repo-state-01 Section 7.3, RS-7.3-4).
	linked, xlinkErr := manifest.CrosslinkContext(ctx, s.repo)
	if xlinkErr != nil {
		obs.Completed(ctx, sessionEndFromSync(&s.result, s.projectCode), xlinkErr)
		return &s.result, fmt.Errorf("sync: crosslink: %w", xlinkErr)
	}
	s.result.ArtifactsLinked = linked
	// Horizon §5.2.3: a delta stored unreadable is counted once the rounds
	// and the crosslink have judged it, and only if it was not a seed.
	kept, err := s.unjudged.settle(s.repo.DB())
	if err != nil {
		obs.Completed(ctx, sessionEndFromSync(&s.result, s.projectCode), err)
		return &s.result, fmt.Errorf("sync: %w", err)
	}
	s.result.FilesRecvd += kept
	if s.sparseReceived != nil {
		err := s.repo.WithTx(func(tx *db.Tx) error {
			var err error
			s.result.Elided, err = filterSparseReceived(ctx, tx, s.sparseReceived, s.cache)
			return err
		})
		if err != nil {
			return &s.result, fmt.Errorf("sync: sparse receive filtering: %w", err)
		}
	}
	// Horizon §5.2.3: the seed applies to what the closing crosslink linked.
	fell, err := s.structural.apply(s.repo)
	if err != nil {
		return &s.result, fmt.Errorf("sync: horizon seed: %w", err)
	}
	s.result.HorizonFallback = s.result.HorizonFallback || fell
	if s.result.HorizonRemedy, err = fallbackRemedy(s.repo.DB(), s.result.HorizonFallback, s.result.HorizonHistoryURL); err != nil {
		return &s.result, fmt.Errorf("sync: %w", err)
	}
	if err := s.recordCursor(); err != nil {
		return &s.result, err
	}

	obs.Completed(ctx, sessionEndFromSync(&s.result, s.projectCode), nil)
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
