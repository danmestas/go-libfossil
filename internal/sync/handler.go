package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/danmestas/go-libfossil/internal/auth"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// DefaultCloneBatchBytes bounds the wire bytes a single clone round emits.
//
// A clone batch is bounded by output size, never by artifact count. A count
// cannot adapt to artifact size: 200 tiny control artifacts and 200 large
// blobs produce wildly different messages, and neither corresponds to what
// the transport can carry. §8.2 leaves the bound entirely to the server
// ("clients MUST NOT infer count or size from range"), so the only real
// constraint is that the response stays a size a server can hold and that a
// repository drains in rounds proportional to its bytes, not its artifacts.
//
// The value is tuned, not principled, and is deliberately larger than
// canonical's 5,000,000 max-download. Canonical's clone retransmits each
// artifact's stored compressed form (§7.2), so its 5 MB covers 5 MB of
// repository. This clone path emits fully expanded artifacts instead, which
// on real repositories runs 80x to 220x the stored bytes -- libfossil's own
// repository is 13.3 MB stored and 1.08 GB expanded. Applying canonical's
// constant to that quantity is a dimensional error: it needs 184 rounds
// where the old 200-artifact bound needed 87, breaking clones that work
// today. Rounds to drain, against the client's MaxRounds of 100:
//
//	repo       artifacts  expanded  count-200   5 MB   16 MB
//	libfossil     17.2k    1.08 GB         87    184      65
//	fossil        67.5k    8.66 GB        338   1125     473
//	sqlite         131k    13.1 GB        655   2502     809
//
// So this replaces an artifact-count ceiling with an expanded-bytes ceiling.
// It is a better ceiling -- it tracks the quantity the transport actually
// carries, and it beats the count bound on every corpus measured -- but
// fossil- and sqlite-sized repositories still cannot be cloned from a
// libfossil server within MaxRounds. They could not before either. Only
// retransmitting stored content per §7.2 removes the ceiling rather than
// moving it; at that point wire bytes and repository bytes become the same
// quantity and this drops to canonical's 5,000,000.
//
// Note this bounds a round at budget plus one artifact, not at budget: the
// artifact that crosses the budget is sent whole (see emitCloneBatch), so a
// repository holding one 2 GB artifact still emits a 2 GB round. Measured
// worst case is ~25 MB across the three repositories above, since the
// largest single artifacts are 9.4 MB (libfossil) and 17.0 MB (sqlite).
// Against the count bound's unbounded 44.9 MB that is a large reduction, but
// it is a reduction and not a cap, and concurrent clones multiply it.
const DefaultCloneBatchBytes = 16_000_000

// A round this server emits must stay inside the bound the client applies when
// it decompresses one, or a clone between two libfossil peers fails on a
// message this same code produced (issue #104). The budget is charged before
// each artifact and the artifact that crosses it is sent whole, so a round can
// reach the budget plus one artifact; requiring two budgets of room reserves an
// artifact allowance equal to the budget itself. Raising DefaultCloneBatchBytes
// past half of xfer.MaxDecompressedBytes underflows this unsigned expression
// and fails the build rather than the clone.
const _ = uint(xfer.MaxDecompressedBytes - 2*DefaultCloneBatchBytes)

// cloneCardOverheadBytes over-approximates the non-payload bytes of an
// encoded file card -- keyword, hash, decimal length, and newline come to
// about 58 -- so that a repository of zero-length artifacts still paginates
// instead of emitting the whole blob table into a single response. The slack
// also absorbs the PrivateCard that may precede a card, which is not charged
// separately. Over-charging is the safe direction and cannot be defeated: a
// corpus of nothing but zero-length artifacts caps at ~167,000 cards per
// round, about 9.7 MB of actual output, still inside the budget.
const cloneCardOverheadBytes = 96

// cloneCapPrefix tags a CookieCard value that carries the upper rid bound
// of a clone session. The handler captures max(rid) on the first clone round
// and echoes it via CookieCard; the client returns it on every subsequent
// round so the (otherwise stateless) server can scope its blob query and
// converge even if the hub is being written to during the clone session.
const cloneCapPrefix = "lf-clone-cap:"

// parseCloneCap returns the snapshot rid encoded in a CookieCard value,
// or (0, false) if the value isn't a clone-cap cookie. Non-matching
// cookies (e.g. arbitrary sync session cookies) leave the bound at zero,
// which preserves pre-snapshot behavior for forward/back compat.
func parseCloneCap(v string) (int, bool) {
	if !strings.HasPrefix(v, cloneCapPrefix) {
		return 0, false
	}
	rid, err := strconv.Atoi(v[len(cloneCapPrefix):])
	if err != nil || rid < 0 {
		return 0, false
	}
	return rid, true
}

// HandleFunc is the server-side sync handler signature.
// Transport listeners call this with decoded requests and write back the response.
type HandleFunc func(ctx context.Context, r *repo.Repo, req *xfer.Message) (*xfer.Message, error)

// HandleOpts configures optional behavior for HandleSync.
type HandleOpts struct {
	Buggify      BuggifyChecker // nil in production.
	Observer     Observer       // nil defaults to no-op.
	ContentCache *content.Cache // nil = no caching.
}

// HandleSync processes an incoming xfer request and produces a response.
// Stateless per-round — the client drives convergence.
func HandleSync(ctx context.Context, r *repo.Repo, req *xfer.Message) (*xfer.Message, error) {
	return HandleSyncWithOpts(ctx, r, req, HandleOpts{})
}

// HandleSyncWithOpts processes an incoming xfer request with optional
// fault injection. Used by DST harness; production callers use HandleSync.
func HandleSyncWithOpts(ctx context.Context, r *repo.Repo, req *xfer.Message, opts HandleOpts) (*xfer.Message, error) {
	if r == nil {
		panic("sync.HandleSync: r must not be nil")
	}
	if req == nil {
		panic("sync.HandleSync: req must not be nil")
	}

	obs := resolveObserver(opts.Observer)
	ctx = obs.HandleStarted(ctx, HandleStart{
		Operation: detectOperation(req),
	})

	h := &handler{repo: r, buggify: opts.Buggify, cache: opts.ContentCache}
	resp, err := h.process(ctx, req)
	if err == nil && resp == nil {
		panic("sync.HandleSync: resp must not be nil on success")
	}

	obs.HandleCompleted(ctx, HandleEnd{
		CardsProcessed: len(req.Cards),
		FilesSent:      h.filesSent,
		FilesReceived:  h.filesRecvd,
		Err:            err,
	})
	return resp, err
}

// detectOperation checks request cards to determine if this is a clone or sync.
func detectOperation(req *xfer.Message) string {
	for _, c := range req.Cards {
		if _, ok := c.(*xfer.CloneCard); ok {
			return "clone"
		}
	}
	return "sync"
}

// remoteHasEntry records that the client announced a blob via igot.
type remoteHasEntry struct {
	isPrivate bool // IsPrivate flag from the client's igot card
}

// handler holds per-request state while processing cards.
type handler struct {
	repo          *repo.Repo
	buggify       BuggifyChecker
	resp          []xfer.Card
	pushOK        bool // client sent a valid push card
	pullOK        bool // client sent a valid pull card
	cloneMode     bool // client sent a clone card
	fatal         bool // a card rule ended the request; resp holds only its error
	cloneSeq      int  // rid of the next blob to send, from the client's clone card
	cloneSnapMax  int  // upper rid bound for this clone session (from cookie); 0 = capture fresh in emitCloneBatch
	uvCatalogSent bool // true after sending UV catalog
	reqClusters   bool // client sent pragma req-clusters
	filesSent     int  // files sent in response (for observer)
	filesRecvd    int  // files received from client (for observer)
	syncPrivate   bool // true if pragma send-private was accepted
	nextIsPrivate bool // true if a private card precedes the next file/cfile
	syncedTables  map[string]*SyncedTable // cached table definitions
	xrowsSent     int  // table sync rows sent
	xrowsRecvd    int  // table sync rows received
	cache         *content.Cache             // nil = passthrough to content.Expand
	remoteHas     map[string]remoteHasEntry // UUIDs the client announced via igot (mirrors Fossil's onremote table)

	// Auth state
	user   string // verified username ("nobody" if no login card)
	caps   string // capability string from user table
	authed bool   // whether login card was verified
}

func (h *handler) initAuth() {
	h.user = "nobody"
	h.caps = ""
	h.authed = false
	var caps string
	err := h.repo.DB().QueryRow("SELECT cap FROM user WHERE login='nobody'").Scan(&caps)
	if err == nil {
		h.caps = caps
	}
}

func (h *handler) handleLoginCard(c *xfer.LoginCard) {
	var projectCode string
	if err := h.repo.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&projectCode); err != nil {
		h.resp = append(h.resp, &xfer.ErrorCard{Message: "authentication failed"})
		return
	}
	u, err := auth.VerifyLogin(h.repo.DB(), projectCode, c)
	if err != nil {
		h.resp = append(h.resp, &xfer.ErrorCard{Message: "authentication failed"})
		return
	}
	h.user = u.Login
	h.caps = u.Cap
	h.authed = true
}

func (h *handler) process(_ context.Context, req *xfer.Message) (*xfer.Message, error) {
	// Initialize auth state from nobody user.
	h.initAuth()

	// Load synced tables.
	if err := h.loadSyncedTables(); err != nil {
		return nil, err
	}

	// First pass: resolve login cards before other control cards.
	for _, card := range req.Cards {
		if lc, ok := card.(*xfer.LoginCard); ok {
			h.handleLoginCard(lc)
		}
	}

	// Second pass: process other control cards with capability checks.
	for _, card := range req.Cards {
		if _, ok := card.(*xfer.LoginCard); ok {
			continue // Already processed.
		}
		h.handleControlCard(card)
		if h.fatal {
			return &xfer.Message{Cards: h.resp}, nil
		}
	}

	// Emit PushCard with project-code/server-code so the clone client can
	// identify the repo. Only in clone mode — sync clients already have
	// codes, and real Fossil treats server-sent "push" as unknown during sync.
	if h.cloneMode {
		var projectCode, serverCode string
		_ = h.repo.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&projectCode)
		_ = h.repo.DB().QueryRow("SELECT value FROM config WHERE name='server-code'").Scan(&serverCode)
		if projectCode != "" {
			h.resp = append(h.resp, &xfer.PushCard{
				ProjectCode: projectCode,
				ServerCode:  serverCode,
			})
		}
	}

	// Process data cards and emit response blobs.
	if err := h.processDataCards(req.Cards); err != nil {
		return nil, err
	}

	// Crosslink any newly-received manifests into the relational tables
	// (event/leaf/plink/mlink/tagxref). Without this the receiving repo
	// stores blobs durably but exposes nothing on the timeline, fork
	// detection sees stale leaf state, and the clone protocol cannot
	// traverse manifest parents (mlink is empty) — symptoms documented
	// in agent-infra trial 2026-04-25 finding #3.
	//
	// Only walk the crosslink scanner when we accepted files this round;
	// pure pull/igot rounds add nothing relational to update.
	if h.filesRecvd > 0 {
		if _, err := manifest.Crosslink(h.repo); err != nil {
			return nil, fmt.Errorf("HandleSync: crosslink: %w", err)
		}
	}

	return &xfer.Message{Cards: h.resp}, nil
}

// processDataCards handles file, igot, gimme, and other data cards in the
// correct order, then emits igot/clone batches. Extracted from process() to
// keep each function under 70 lines.
func (h *handler) processDataCards(cards []xfer.Card) error {
	// File cards (and private prefix) first so blobs are stored before
	// IGotCard checks blob.Exists. Without this, a request containing
	// both IGotCard and FileCard for the same blob produces a spurious
	// GimmeCard — the IGotCard runs before the FileCard stores it.
	for _, card := range cards {
		switch card.(type) {
		case *xfer.FileCard, *xfer.CFileCard, *xfer.PrivateCard:
			if err := h.handleDataCard(card); err != nil {
				return err
			}
		}
	}
	// Remaining data cards (igot, gimme, etc.).
	for _, card := range cards {
		switch card.(type) {
		case *xfer.FileCard, *xfer.CFileCard, *xfer.PrivateCard:
			continue // Already handled above.
		default:
			if err := h.handleDataCard(card); err != nil {
				return err
			}
		}
	}

	// If pull was requested, emit igot for all non-phantom blobs.
	if h.pullOK {
		if err := h.emitIGots(); err != nil {
			return err
		}
		if err := h.emitXIGots(); err != nil {
			return err
		}
	}

	// If clone, emit paginated file cards.
	if h.cloneMode {
		if err := h.emitCloneBatch(); err != nil {
			return err
		}
	}
	return nil
}

func (h *handler) handleControlCard(card xfer.Card) {
	switch c := card.(type) {
	case *xfer.LoginCard:
		return // Already processed in first pass (initAuth/handleLoginCard).
	case *xfer.PragmaCard:
		if c.Name == "uv-hash" && len(c.Values) >= 1 {
			if err := h.handlePragmaUVHash(c.Values[0]); err != nil {
				h.resp = append(h.resp, &xfer.ErrorCard{
					Message: fmt.Sprintf("uv-hash: %v", err),
				})
			}
		} else if c.Name == "xtable-hash" && len(c.Values) >= 2 {
			h.handlePragmaXTableHash(c.Values[0], c.Values[1])
		}
		if c.Name == "req-clusters" {
			h.reqClusters = true
		}
		if c.Name == "ci-lock" && len(c.Values) >= 2 {
			fail := processCkinLock(h.repo.DB(), c.Values[0], c.Values[1], h.user, DefaultCkinLockTimeout)
			if fail != nil {
				h.resp = append(h.resp, &xfer.PragmaCard{
					Name:   "ci-lock-fail",
					Values: []string{fail.HeldBy, fmt.Sprintf("%d", fail.Since.Unix())},
				})
			}
		}
		if c.Name == "send-private" {
			if auth.CanSyncPrivate(h.caps) {
				h.syncPrivate = true
			} else {
				h.resp = append(h.resp, &xfer.ErrorCard{
					Message: "not authorized to sync private content",
				})
			}
		}
		// Acknowledge client-version, ignore other unknown pragmas.
	case *xfer.PushCard:
		if auth.CanPush(h.caps) {
			h.pushOK = true
		} else {
			h.resp = append(h.resp, &xfer.ErrorCard{
				Message: "push denied: insufficient capabilities",
			})
		}
	case *xfer.PullCard:
		if auth.CanPull(h.caps) {
			h.pullOK = true
		} else {
			h.resp = append(h.resp, &xfer.ErrorCard{
				Message: "pull denied: insufficient capabilities",
			})
		}
	case *xfer.CloneCard:
		if !auth.CanClone(h.caps) {
			h.resp = append(h.resp, &xfer.ErrorCard{
				Message: "clone denied: insufficient capabilities",
			})
			return
		}
		// §8.1: an otherwise authorized three-token clone with VERSION >= 2
		// and a digit-only SEQNO of zero or less clears accumulated output,
		// rolls back, emits this error, and parses no later request card.
		//
		// The mandated rollback is satisfied vacuously: no card handled
		// before processDataCards writes anything, so returning here leaves
		// nothing to undo. That depends on an ordering nothing enforces —
		// the day a control card starts writing (§3.5's ci-lock is the
		// obvious candidate, since a lock taken ahead of this card would
		// survive the abort), this needs a real transaction instead.
		if c.SeqNoIsDecimal && c.Version >= 2 && c.SeqNo <= 0 {
			h.resp = []xfer.Card{&xfer.ErrorCard{Message: "invalid clone sequence number"}}
			h.fatal = true
			return
		}
		h.cloneMode = true
		// The pagination cursor rides on the clone card and nowhere else
		// (§8.1; canonical xfer.c:1553 reads token 2 of `clone VERSION
		// SEQNO`). A request carrying no usable cursor — a bare `clone`, or
		// a SEQNO that fails digit-only recognition — starts from the first
		// rid. §8.1 withholds the fatal above from both of those cases.
		h.cloneSeq = c.SeqNo
		if h.cloneSeq <= 0 {
			h.cloneSeq = 1
		}
	case *xfer.CloneSeqNoCard:
		// Deliberately ignored. clone_seqno is a reply-only card, so no
		// conforming client sends one; canonical answers `bad command`. We
		// stay tolerant rather than strict, which is why the model server in
		// TestCloneMultiBatchAgainstCanonicalServer rejects it and we do not.
	case *xfer.CookieCard:
		// Clone clients echo the cap cookie on every round. Sync sessions also
		// use cookies opaquely; non-matching values leave cloneSnapMax at zero.
		if rid, ok := parseCloneCap(c.Value); ok {
			h.cloneSnapMax = rid
		}
	case *xfer.SchemaCard:
		h.handleSchemaCard(c)
	}
}

func (h *handler) handleDataCard(card xfer.Card) error {
	switch c := card.(type) {
	case *xfer.IGotCard:
		return h.handleIGot(c)
	case *xfer.GimmeCard:
		return h.handleGimme(c)
	case *xfer.FileCard:
		return h.handleFile(c.UUID, c.DeltaSrc, c.Content)
	case *xfer.CFileCard:
		return h.handleFile(c.UUID, c.DeltaSrc, c.Content)
	case *xfer.PrivateCard:
		if !auth.CanSyncPrivate(h.caps) {
			h.resp = append(h.resp, &xfer.ErrorCard{
				Message: "not authorized to sync private content",
			})
			h.nextIsPrivate = false
		} else {
			h.nextIsPrivate = true
		}
		return nil
	case *xfer.ReqConfigCard:
		return h.handleReqConfig(c)
	case *xfer.UVIGotCard:
		return h.handleUVIGot(c)
	case *xfer.UVGimmeCard:
		return h.handleUVGimme(c)
	case *xfer.UVFileCard:
		return h.handleUVFile(c)
	case *xfer.XIGotCard:
		return h.handleXIGot(c)
	case *xfer.XGimmeCard:
		return h.handleXGimme(c)
	case *xfer.XRowCard:
		return h.handleXRow(c)
	case *xfer.XDeleteCard:
		return h.handleXDelete(c)
	}
	return nil
}

func (h *handler) handleIGot(c *xfer.IGotCard) error {
	if c == nil {
		panic("handler.handleIGot: c must not be nil")
	}
	_, exists := blob.Exists(h.repo.DB(), c.UUID)
	if exists {
		// Record that the client has this blob so emitIGots can skip it.
		// Mirrors Fossil's remote_has() → onremote table (xfer.c:1471).
		if h.remoteHas == nil {
			h.remoteHas = make(map[string]remoteHasEntry)
		}
		h.remoteHas[c.UUID] = remoteHasEntry{isPrivate: c.IsPrivate}
		return nil
	}
	// Server requests the missing blob whenever either side has expressed
	// interest in transferring data. Pre-fix this gate was just !pullOK,
	// which silently dropped server gimmes when a client called
	// SyncOpts{Push:true, Pull:false}: the client emits igot cards from
	// sendUnclustered every round regardless of Pull, so the server
	// would see the announcements but never request the blobs and the
	// loop would converge with the server holding only what the client
	// pushed proactively. Mirrors fossil-scm/c xfer.c, which generates
	// gimmes from igot cards as long as either direction is active.
	if !h.pushOK && !h.pullOK {
		return nil
	}
	if c.IsPrivate && !h.syncPrivate {
		return nil // not authorized — don't request
	}
	h.resp = append(h.resp, &xfer.GimmeCard{UUID: c.UUID})
	return nil
}

func (h *handler) handleGimme(c *xfer.GimmeCard) error {
	if c == nil {
		panic("handler.handleGimme: c must not be nil")
	}
	// BUGGIFY: 5% chance skip sending a file to test client retry.
	if h.buggify != nil && h.buggify.Check("handler.handleGimme.skip", 0.05) {
		return nil
	}
	rid, ok := content.AvailableByUUID(h.repo.DB(), c.UUID)
	if !ok {
		return nil // blob not found — not fatal, skip.
	}
	isPriv := content.IsPrivate(h.repo.DB(), int64(rid))
	if isPriv && !h.syncPrivate {
		return nil // private blob, client not authorized — skip.
	}
	data, err := h.cache.Expand(h.repo.DB(), rid)
	if err != nil {
		h.resp = append(h.resp, &xfer.ErrorCard{
			Message: fmt.Sprintf("expand %s: %v", c.UUID, err),
		})
		return nil
	}
	if isPriv {
		// BUGGIFY: 5% chance skip the PrivateCard prefix — client should
		// treat the file as public; next sync round corrects the status.
		if h.buggify == nil || !h.buggify.Check("handler.handleGimme.dropPrivateCard", 0.05) {
			h.resp = append(h.resp, &xfer.PrivateCard{})
		}
	}
	h.resp = append(h.resp, &xfer.FileCard{UUID: c.UUID, Content: data})
	h.filesSent++
	return nil
}

func (h *handler) handleFile(uuid, deltaSrc string, payload []byte) error {
	if uuid == "" {
		panic("handler.handleFile: uuid must not be empty")
	}
	if !h.pushOK {
		h.resp = append(h.resp, &xfer.ErrorCard{
			Message: fmt.Sprintf("file %s rejected: no push card", uuid),
		})
		return nil
	}
	// BUGGIFY: 3% chance reject a valid file to test client re-push.
	if h.buggify != nil && h.buggify.Check("handler.handleFile.reject", 0.03) {
		h.resp = append(h.resp, &xfer.ErrorCard{
			Message: fmt.Sprintf("buggify: rejected file %s", uuid),
		})
		return nil
	}
	if err := storeReceivedFile(h.repo, uuid, deltaSrc, payload); err != nil {
		h.resp = append(h.resp, &xfer.ErrorCard{
			Message: fmt.Sprintf("storing %s: %v", uuid, err),
		})
		return nil
	}
	rid, ok := blob.Exists(h.repo.DB(), uuid)
	if h.buggify != nil && h.buggify.Check("handler.handleFile.missingAfterStore", 0.01) {
		ok = false
	}
	if !ok {
		h.resp = append(h.resp, &xfer.ErrorCard{
			Message: fmt.Sprintf("blob %s missing after store", uuid),
		})
		return nil
	}
	if h.nextIsPrivate {
		if err := content.MakePrivate(h.repo.DB(), int64(rid)); err != nil {
			return fmt.Errorf("handler: MakePrivate %s: %w", uuid, err)
		}
		h.nextIsPrivate = false
	} else {
		if err := content.MakePublic(h.repo.DB(), int64(rid)); err != nil {
			return fmt.Errorf("handler: MakePublic %s: %w", uuid, err)
		}
	}
	h.filesRecvd++
	return nil
}

func (h *handler) handleReqConfig(c *xfer.ReqConfigCard) error {
	if c == nil {
		panic("handler.handleReqConfig: c must not be nil")
	}
	var val string
	err := h.repo.DB().QueryRow(
		"SELECT value FROM config WHERE name = ?", c.Name,
	).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // config key not found — expected, not fatal.
	}
	if err != nil {
		return fmt.Errorf("handler: config query %q: %w", c.Name, err)
	}
	h.resp = append(h.resp, &xfer.ConfigCard{
		Name:    c.Name,
		Content: []byte(val),
	})
	return nil
}

func (h *handler) emitIGots() error {
	// Emit igot for all non-phantom blobs so the client can discover
	// everything the server has. Cluster generation is a client-side
	// optimization for push; the server always advertises all blobs.
	rows, err := h.repo.DB().Query(`
		SELECT uuid FROM blob WHERE size >= 0
		AND NOT EXISTS(SELECT 1 FROM shun WHERE uuid=blob.uuid)
		AND NOT EXISTS(SELECT 1 FROM private WHERE rid=blob.rid)`,
	)
	if err != nil {
		return fmt.Errorf("handler: listing blobs: %w", err)
	}
	defer rows.Close()

	var uuids []string
	for rows.Next() {
		var uuid string
		if err := rows.Scan(&uuid); err != nil {
			return err
		}
		// remoteHas is populated from client igot cards in handleIGot.
		// Skip if the client already has this blob as public (non-private).
		// If the client has it as private, we still emit the public igot so
		// the client can clear its private status (private→public transition).
		if e, ok := h.remoteHas[uuid]; ok && !e.isPrivate {
			continue
		}
		uuids = append(uuids, uuid)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// BUGGIFY: 10% chance truncate igot list to test multi-round convergence.
	if h.buggify != nil && h.buggify.Check("handler.emitIGots.truncate", 0.10) && len(uuids) > 1 {
		uuids = uuids[:len(uuids)/2]
	}

	for _, uuid := range uuids {
		h.resp = append(h.resp, &xfer.IGotCard{UUID: uuid})
	}

	if h.syncPrivate {
		if err := h.emitPrivateIGots(); err != nil {
			return err
		}
	}
	return nil
}

// emitPrivateIGots emits igot cards with IsPrivate=true for all blobs in
// the private table. Only called when the client sent pragma send-private
// and has the 'x' capability.
func (h *handler) emitPrivateIGots() error {
	rows, err := h.repo.DB().Query(
		"SELECT b.uuid FROM private p JOIN blob b ON p.rid=b.rid WHERE b.size >= 0",
	)
	if err != nil {
		return fmt.Errorf("handler: listing private blobs: %w", err)
	}
	defer rows.Close()

	var uuids []string
	for rows.Next() {
		var uuid string
		if err := rows.Scan(&uuid); err != nil {
			return err
		}
		// Skip if the client already has this blob as private.
		// If the client has it as public, we still emit the private igot so
		// the client can update its private status (public→private transition).
		if e, ok := h.remoteHas[uuid]; ok && e.isPrivate {
			continue
		}
		uuids = append(uuids, uuid)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// BUGGIFY: 10% chance truncate private igot list to test multi-round convergence.
	if h.buggify != nil && h.buggify.Check("handler.emitPrivateIGots.truncate", 0.10) && len(uuids) > 1 {
		uuids = uuids[:len(uuids)/2]
	}

	for _, uuid := range uuids {
		h.resp = append(h.resp, &xfer.IGotCard{UUID: uuid, IsPrivate: true})
	}
	return nil
}

// sendAllClusters emits igot cards for all cluster artifacts that are
// not still in unclustered (i.e., already fully clustered themselves).
func (h *handler) sendAllClusters() error {
	rows, err := h.repo.DB().Query(`
		SELECT b.uuid FROM tagxref tx
		JOIN blob b ON tx.rid = b.rid
		WHERE tx.tagid = 7
		  AND NOT EXISTS (SELECT 1 FROM unclustered WHERE rid = b.rid)
		  AND NOT EXISTS (SELECT 1 FROM phantom WHERE rid = b.rid)
		  AND NOT EXISTS (SELECT 1 FROM shun WHERE uuid = b.uuid)
		  AND NOT EXISTS (SELECT 1 FROM private WHERE rid = b.rid)
		  AND b.size >= 0
	`)
	if err != nil {
		return fmt.Errorf("handler: listing clusters: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var uuid string
		if err := rows.Scan(&uuid); err != nil {
			return err
		}
		// Clusters are always public (query excludes private table).
		if e, ok := h.remoteHas[uuid]; ok && !e.isPrivate {
			continue
		}
		h.resp = append(h.resp, &xfer.IGotCard{UUID: uuid})
	}
	return rows.Err()
}

func (h *handler) emitCloneBatch() error {
	// Capture the snapshot bound on the first round of a clone session.
	// Without an upper bound the batch query keeps picking up blobs that other
	// writers commit between rounds, the completion sentinel
	// CloneSeqNoCard{SeqNo:0} never fires, and the client loops to MaxRounds
	// (issue #17). The bound rides back to the client via CookieCard and
	// gets echoed on every subsequent round.
	if h.cloneSnapMax == 0 {
		var maxRid int
		if err := h.repo.DB().QueryRow("SELECT COALESCE(MAX(rid), 0) FROM blob WHERE size >= 0").Scan(&maxRid); err != nil {
			return fmt.Errorf("handler: capture clone snapshot: %w", err)
		}
		h.cloneSnapMax = maxRid
		h.resp = append(h.resp, &xfer.CookieCard{Value: fmt.Sprintf("%s%d", cloneCapPrefix, maxRid)})
	}

	budget := DefaultCloneBatchBytes
	// BUGGIFY: 10% chance reduce the budget to one byte to stress pagination.
	// The budget is consulted before each artifact and never suppresses the
	// first one, so this yields exactly one artifact per round.
	if h.buggify != nil && h.buggify.Check("handler.emitCloneBatch.smallBatch", 0.10) {
		budget = 1
	}
	truncate := h.buggify != nil && h.buggify.Check("clone.emitCloneBatch.truncate", 0.10)

	// cloneSeq is the rid of the next blob to send and is inclusive, matching
	// §8.2's "iterate from supplied RID through current maximum" and
	// canonical's `while( seqno<=max )` loop in page_xfer(). handleControlCard
	// is the sole writer and floors it at 1, but that is enforced far from
	// here with nothing compile-time behind it, so this stays a check rather
	// than an assumption.
	cursorAtEntry := h.cloneSeq
	if cursorAtEntry <= 0 {
		return fmt.Errorf("handler: clone cursor must be positive, got %d", cursorAtEntry)
	}
	rows, err := h.repo.DB().Query(
		"SELECT rid, uuid FROM blob WHERE rid >= ? AND rid <= ? AND size >= 0 ORDER BY rid",
		h.cloneSeq, h.cloneSnapMax,
	)
	if err != nil {
		return fmt.Errorf("handler: clone batch: %w", err)
	}
	defer rows.Close()

	count := 0
	// bytesSent accumulates the wire cost of the file cards emitted this
	// round, mirroring canonical's `while( mxSend > blob_size(pOut) )`: the
	// budget is tested before each artifact and the artifact that crosses it
	// is still sent whole. That ordering is what guarantees forward progress
	// when a single artifact is larger than the entire budget — a post-send
	// test would emit nothing and stall the cursor.
	bytesSent := 0
	var lastSentRID int
	// nextRID is the cursor reported back to the client: the rid of the first
	// blob this batch did not send, or 0 once the snapshot is exhausted.
	nextRID := 0
	for rows.Next() {
		var rid int
		var uuid string
		if err := rows.Scan(&rid, &uuid); err != nil {
			return err
		}

		isPriv := content.IsPrivate(h.repo.DB(), int64(rid))
		if isPriv && !h.syncPrivate {
			continue // skip private blob, don't count toward batch
		}

		if bytesSent >= budget {
			nextRID = rid
			break
		}

		data, err := h.cache.Expand(h.repo.DB(), libfossil.FslID(rid))
		if err != nil {
			return fmt.Errorf("handler: expanding rid %d: %w", rid, err)
		}
		if isPriv {
			h.resp = append(h.resp, &xfer.PrivateCard{})
		}
		h.resp = append(h.resp, &xfer.FileCard{UUID: uuid, Content: data})
		h.filesSent++
		lastSentRID = rid
		count++
		bytesSent += cloneCardOverheadBytes + len(data)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// BUGGIFY: truncate — remove last file card to simulate incomplete batch.
	// `count > 1` is a liveness constraint, not just a chaos-injection guard.
	// Dropping the only card in a batch would set nextRID to lastSentRID,
	// which equals the cursor we entered with, and the client would re-request
	// the same batch until MaxRounds. Under the old exclusive `rid > cursor`
	// semantics `count > 0` was safe here; it no longer is.
	if truncate && count > 1 {
		for i := len(h.resp) - 1; i >= 0; i-- {
			if _, ok := h.resp[i].(*xfer.FileCard); ok {
				h.resp = append(h.resp[:i], h.resp[i+1:]...)
				h.filesSent--
				count--
				// The dropped card held lastSentRID, so that rid becomes the
				// next one owed to the client rather than being skipped.
				nextRID = lastSentRID
				break
			}
		}
	}
	// The cursor must strictly advance or signal exhaustion. Catching a stall
	// here names the round that caused it; letting it through only surfaces
	// later as an opaque MaxRounds failure on the client.
	if nextRID != 0 && nextRID <= cursorAtEntry {
		return fmt.Errorf(
			"handler: clone cursor did not advance: next %d, cursor %d", nextRID, cursorAtEntry)
	}
	// SeqNo 0 signals completion so the client stops requesting.
	h.resp = append(h.resp, &xfer.CloneSeqNoCard{SeqNo: nextRID})
	return nil
}
