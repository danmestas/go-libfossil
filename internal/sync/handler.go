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
// emitCloneBatch now retransmits stored content per §7.2 (issue #98) rather
// than the fully expanded artifacts it used to send, so bytesSent tracks the
// same quantity canonical's 5,000,000 max-download bounds: repository bytes,
// not the 80x-220x-larger expanded form. This constant was chosen before that
// fix, against the expanded-bytes regime documented in issue #88 where a
// smaller value pushed fossil- and sqlite-sized corpora past MaxRounds --
// that constraint is gone now that wire bytes and repository bytes are the
// same quantity. Retuning this value toward canonical's constant is issue
// #109's territory (server pagination/budget accounting), not this fix's; it
// is left at its prior value here to avoid colliding with that work in
// flight. 16,000,000 stays a safe, if generous, upper bound either way: it
// only widens how much stored content one round may carry.
//
// This bounds an ordinary round at the budget plus the artifact that crosses
// it, and emitCloneBatch caps that sum at xfer.MaxDecompressedBytes so the
// client can always decode a round (issue #109). The one exception is a single
// artifact larger than that bound: it is sent alone, in a round of its own, to
// make progress -- so a repository holding one 2 GB artifact still emits a 2 GB
// round, but such an artifact is unclonable regardless. Measured worst case is
// ~25 MB across the three repositories above, since the largest single
// artifacts are 9.4 MB (libfossil) and 17.0 MB (sqlite). Against the count
// bound's unbounded 44.9 MB that is a large reduction, and concurrent clones
// multiply it.
const DefaultCloneBatchBytes = 16_000_000

// A round this server emits must stay inside the bound the client applies when
// it decompresses one, or a clone between two libfossil peers fails on a
// message this same code produced (issue #104). emitCloneBatch enforces that at
// runtime: it flushes a round rather than let an artifact carry it past
// xfer.MaxDecompressedBytes, and sends an over-bound artifact alone (issue
// #109). This compile-time guard makes that enforcement well-conditioned: with
// the bound at least twice the budget, filler accumulates up to one budget and
// any artifact up to a second budget still fits in the same round, so ordinary
// artifacts ride along and only genuinely large ones (above bound minus budget)
// take a round of their own. Raising DefaultCloneBatchBytes past half of
// xfer.MaxDecompressedBytes underflows this unsigned expression and fails the
// build rather than the clone.
const _ = uint(xfer.MaxDecompressedBytes - 2*DefaultCloneBatchBytes)

// cloneCardOverheadBytes over-approximates the non-payload bytes of an
// encoded cfile card -- keyword, hash, delta-source hash, two decimal
// lengths, and newline come to somewhat more than the plain "file" card's
// ~58 -- so that a repository of zero-length artifacts still paginates
// instead of emitting the whole blob table into a single response. The slack
// also absorbs the PrivateCard that may precede a card, which is not charged
// separately. This is charged against data before encodeCFile's zlib
// compression runs, so it is already conservative in the same direction as
// the framing overhead it approximates. Over-charging is the safe direction
// and cannot be defeated: a corpus of nothing but zero-length artifacts caps
// at ~167,000 cards per round, about 9.7 MB of actual output, still inside
// the budget.
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

func (h *handler) process(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
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

	// Process data cards and emit response blobs.
	if err := h.processDataCards(req.Cards); err != nil {
		return nil, err
	}

	// Emit PushCard with project-code/server-code so the clone client can
	// identify the repo. Only in clone mode — sync clients already have
	// codes, and real Fossil treats server-sent "push" as unknown during sync.
	//
	// This must trail the clone batch's clone_seqno card, matching canonical's
	// order (fossil-scm xfer.c emits the batch, then clone_seqno at :1571, then
	// push at :1577). A real fossil client re-issues `clone 3 SEQNO` every round
	// it sees a `push` card while its clone cursor is still positive (xfer.c:2706),
	// and it learns the cursor hit zero only from the clone_seqno card. Emitting
	// push ahead of clone_seqno makes the client queue one more clone request
	// before it sees the terminal clone_seqno 0, so the server serves the whole
	// repository a second time — the 2.06x end-to-end blow-up of issue #138.
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

	// Crosslink any newly-received manifests into the relational tables
	// (event/leaf/plink/mlink/tagxref). Without this the receiving repo
	// stores blobs durably but exposes nothing on the timeline, fork
	// detection sees stale leaf state, and the clone protocol cannot
	// traverse manifest parents (mlink is empty) — symptoms documented
	// in agent-infra trial 2026-04-25 finding #3.
	//
	// Only walk the crosslink scanner when we accepted files this round;
	// pure pull/igot rounds add nothing relational to update.
	//
	// Context-aware (#120): this sweep walks the whole repository in one call,
	// so on a large repo it is where a request whose client has already gone
	// away would otherwise keep working uninterrupted.
	if h.filesRecvd > 0 {
		if _, err := manifest.CrosslinkContext(ctx, h.repo); err != nil {
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
		return h.handleFile(c.UUID, c.DeltaSrc, c.Content, nil)
	case *xfer.CFileCard:
		return h.handleFile(c.UUID, c.DeltaSrc, c.Content, c.StoredBlob)
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

func (h *handler) handleFile(uuid, deltaSrc string, payload []byte, storedBlob []byte) error {
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
	if err := storeReceivedFile(h.repo, uuid, deltaSrc, payload, storedBlob); err != nil {
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
		"SELECT rid, uuid, size FROM blob WHERE rid >= ? AND rid <= ? AND size >= 0 ORDER BY rid",
		h.cloneSeq, h.cloneSnapMax,
	)
	if err != nil {
		return fmt.Errorf("handler: clone batch: %w", err)
	}
	defer rows.Close()

	count := 0
	// sent records every rid emitted this round, including delta sources a
	// dependent's chain pulls in ahead of their ascending position (issue
	// #141). It caps re-emission at once per round the way canonical fossil's
	// per-request "sent" bag does; across rounds the ascending cursor resumes,
	// and a source pulled in early in a prior round may be re-sent, which the
	// receiver dedupes by hash.
	sent := map[int]bool{}
	// sentTop counts top-level artifacts sent this round. Each carries a whole
	// delta chain, so it is distinct from count (the card total) and is the
	// quantity the truncate liveness guard below reasons about.
	sentTop := 0
	// bytesSent accumulates the wire cost of the cards emitted this round,
	// mirroring canonical's `while( mxSend > blob_size(pOut) )`: the budget is
	// tested before each artifact and the artifact (with its chain) that
	// crosses it is still sent whole. That ordering is what guarantees forward
	// progress when a single artifact is larger than the entire budget — a
	// post-send test would emit nothing and stall the cursor.
	bytesSent := 0
	var lastTopRID int
	// nextRID is the cursor reported back to the client: the rid of the first
	// blob this batch did not send, or 0 once the snapshot is exhausted.
	nextRID := 0
	for rows.Next() {
		var rid int
		var uuid string
		var fullSize int
		if err := rows.Scan(&rid, &uuid, &fullSize); err != nil {
			return err
		}

		// A higher-rid delta source that a dependent's chain already pulled in
		// this round is on the wire; skip it before it can consume budget or
		// advance the cursor a second time.
		if sent[rid] {
			continue
		}

		isPriv := content.IsPrivate(h.repo.DB(), int64(rid))
		if isPriv && !h.syncPrivate {
			continue // skip private blob, don't count toward batch
		}

		if bytesSent >= budget {
			nextRID = rid
			break
		}

		// §7.2/#141: emit the artifact source-first. A row stored as a delta
		// goes out as a delta card preceded by the source it names, reclaiming
		// the delta-transmission bandwidth a plain (expanded) cfile gives up.
		// content.Deltify deltifies the OLDER artifact against the NEWER one,
		// so a delta's source almost always has a *greater* rid than the delta
		// itself and would not yet have been sent under this loop's ascending
		// order; buildCloneArtifact walks the chain and emits each source ahead
		// of its dependent so no delta ever forward-references a card that has
		// not arrived. The delta rides an uncompressed "file" card (not a
		// "cfile"), matching canonical fossil's send_delta_native.
		//
		// This is a bandwidth win, verified content-identical for
		// libfossil<->libfossil clones by the self-round-trip tests. It does
		// NOT by itself make a real fossil client's clone usable: full content
		// still rides a compressed cfile, which go-libfossil emits as bare zlib
		// while fossil expects [4-byte size][zlib] framing, so a real fossil
		// client still decodes full content to garbage and rebuilds to zero
		// check-ins. That is a separate, pre-existing bug tracked as #152; see
		// TestCloneRealFossilWithDeltaChain, which skips against it.
		cards, rids, cost, err := h.buildCloneArtifact(rid, uuid, fullSize, sent)
		if err != nil {
			return err
		}
		// Respect the client's decode bound: the round the client decompresses
		// must stay within xfer.MaxDecompressedBytes. If this artifact's chain
		// would push the round past that bound and the round already carries
		// content, flush now so the chain leads a round of its own. Without this
		// the budget check above only bounds the accumulated *filler*, and a
		// large artifact riding behind sub-budget filler carried the round to
		// budget-plus-artifact, past what the client could decode (issue #109).
		//
		// The first artifact of a round (count == 0) is always sent, so a chain
		// larger than the bound still makes forward progress -- it is simply
		// unclonable, a property of the artifact alone and not of what precedes
		// it in the round.
		if count > 0 && bytesSent+cost > xfer.MaxDecompressedBytes {
			nextRID = rid
			break
		}

		h.resp = append(h.resp, cards...)
		for _, r := range rids {
			sent[r] = true
		}
		h.filesSent += len(rids)
		lastTopRID = rid
		count += len(rids)
		sentTop++
		bytesSent += cost
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// BUGGIFY: truncate — remove the last cfile card to simulate an incomplete
	// batch. `sentTop > 1` is a liveness constraint, not just a chaos-injection
	// guard. The removed card is the deepest dependent of the last artifact's
	// chain (emitted last in source-first order, so nothing else on the wire
	// depends on it), and nextRID is set to that artifact's top-level rid. With
	// more than one top-level artifact sent this round that rid strictly
	// exceeds the cursor we entered with, so the client re-requests from it
	// rather than looping on the same batch until MaxRounds.
	if truncate && sentTop > 1 {
		for i := len(h.resp) - 1; i >= 0; i-- {
			if _, ok := h.resp[i].(*xfer.CFileCard); ok {
				h.resp = append(h.resp[:i], h.resp[i+1:]...)
				h.filesSent--
				// The dropped card held lastTopRID, so that rid becomes the
				// next one owed to the client rather than being skipped.
				nextRID = lastTopRID
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

// maxCloneChainWalk bounds the delta-chain walk in walkCloneChain. A real
// chain is only as deep as an artifact's revision count and always terminates
// at a full-content tip; this cap exists solely so a corrupt or adversarial
// delta graph cannot spin the walk forever. content.Deltify forbids cycles,
// but the send path does not trust its store to be uncorrupted here. On
// reaching the cap the deepest artifact is emitted as full content, which is
// always safe (it references no source card).
const maxCloneChainWalk = 4096

// cloneArtifact is one artifact the clone will emit. An empty srcUUID means
// emit full content as a compressed cfile card; a set srcUUID means emit a
// delta against that source, which buildCloneArtifact guarantees precedes this
// artifact on the wire.
type cloneArtifact struct {
	rid     int
	uuid    string
	usize   int
	priv    bool
	srcUUID string
}

// cloneBlobRow holds the columns walkCloneChain needs to decide whether a
// delta source can be forwarded to the receiver this clone.
type cloneBlobRow struct {
	uuid string
	size int
	priv bool
}

// loadCloneBlob reads the blob row for rid. ok is false when no such row
// exists (a source named by a dangling delta link, which is treated as
// unforwardable rather than an error).
func (h *handler) loadCloneBlob(rid int) (cloneBlobRow, bool, error) {
	var row cloneBlobRow
	err := h.repo.DB().QueryRow("SELECT uuid, size FROM blob WHERE rid = ?", rid).
		Scan(&row.uuid, &row.size)
	if errors.Is(err, sql.ErrNoRows) {
		return cloneBlobRow{}, false, nil
	}
	if err != nil {
		return cloneBlobRow{}, false, fmt.Errorf("handler: clone blob info rid %d: %w", rid, err)
	}
	row.priv = content.IsPrivate(h.repo.DB(), int64(rid))
	return row, true, nil
}

// walkCloneChain follows rid's delta chain toward its full-content tip and
// returns the artifacts to emit, ordered DEPENDENT-FIRST (rid itself first).
// The walk stops at the first source that is already available to the receiver
// (emitted earlier this round) or that cannot be forwarded this clone —
// outside the snapshot bound, a phantom, or a private source under a public
// clone — and tags the artifact below such a source for full-content emission
// so it never depends on a card that will not arrive.
func (h *handler) walkCloneChain(rid int, uuid string, usize int, sent map[int]bool) ([]cloneArtifact, error) {
	chain := make([]cloneArtifact, 0, 8)
	curRID, curUUID, curUSize := rid, uuid, usize
	curPriv := content.IsPrivate(h.repo.DB(), int64(curRID))
	for step := 0; step < maxCloneChainWalk; step++ {
		src, err := content.DeltaSource(h.repo.DB(), libfossil.FslID(curRID))
		if err != nil {
			return nil, fmt.Errorf("handler: delta source for rid %d: %w", curRID, err)
		}
		m := cloneArtifact{rid: curRID, uuid: curUUID, usize: curUSize, priv: curPriv}
		if src == 0 {
			return append(chain, m), nil // full-content tip
		}
		info, ok, err := h.loadCloneBlob(int(src))
		if err != nil {
			return nil, err
		}
		forwardable := ok && info.size >= 0 && int(src) <= h.cloneSnapMax &&
			(!info.priv || h.syncPrivate)
		if !forwardable {
			return append(chain, m), nil // source unsendable: emit curRID full
		}
		m.srcUUID = info.uuid
		chain = append(chain, m)
		if sent[int(src)] {
			return chain, nil // source already on the wire this round
		}
		curRID, curUUID, curUSize, curPriv = int(src), info.uuid, info.size, info.priv
	}
	// Walk cap hit: force the deepest artifact to full content so its delta
	// does not reference a source we never emitted.
	chain[len(chain)-1].srcUUID = ""
	return chain, nil
}

// buildCloneArtifact assembles the wire cards that deliver artifact rid to the
// receiver, ordered source-first: the chain tip (or the lowest full-content
// anchor) leads, each delta follows the source it names, and rid itself is
// last. It returns the cards, the artifact rids they cover (for the caller's
// sent bookkeeping), and the accumulated wire cost. It does not consult the
// byte budget — the caller decides whether the whole sequence fits the round.
func (h *handler) buildCloneArtifact(
	rid int, uuid string, usize int, sent map[int]bool,
) ([]xfer.Card, []int, int, error) {
	chain, err := h.walkCloneChain(rid, uuid, usize, sent)
	if err != nil {
		return nil, nil, 0, err
	}
	if len(chain) == 0 {
		panic("handler.buildCloneArtifact: walkCloneChain returned no artifacts")
	}
	cards := make([]xfer.Card, 0, 2*len(chain))
	rids := make([]int, 0, len(chain))
	cost := 0
	// chain is dependent-first; emit it reversed so every source precedes the
	// delta that names it.
	for i := len(chain) - 1; i >= 0; i-- {
		m := chain[i]
		var data []byte
		if m.srcUUID == "" {
			data, err = content.Expand(h.repo.DB(), libfossil.FslID(m.rid))
			if err != nil {
				return nil, nil, 0, fmt.Errorf("handler: expanding rid %d: %w", m.rid, err)
			}
		} else {
			data, err = blob.Load(h.repo.DB(), libfossil.FslID(m.rid))
			if err != nil {
				return nil, nil, 0, fmt.Errorf("handler: loading delta rid %d: %w", m.rid, err)
			}
		}
		if m.priv {
			cards = append(cards, &xfer.PrivateCard{})
		}
		// Full content rides a compressed cfile (the #98/#113 wire-size win). A
		// delta rides an uncompressed "file" card, matching canonical fossil's
		// send_delta_native: a real fossil client stores a cfile's payload
		// verbatim and cannot decompress it without fossil's on-disk
		// [4-byte size][zlib] framing, which the wire cfile omits, whereas an
		// uncompressed file card is re-framed by the receiver's own compressor.
		if m.srcUUID == "" {
			cards = append(cards, &xfer.CFileCard{UUID: m.uuid, USize: m.usize, Content: data})
		} else {
			cards = append(cards, &xfer.FileCard{UUID: m.uuid, DeltaSrc: m.srcUUID, Content: data})
		}
		rids = append(rids, m.rid)
		cost += cloneCardOverheadBytes + len(data)
	}
	return cards, rids, cost, nil
}
