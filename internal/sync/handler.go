package sync

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/auth"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/seedcheck"
	"github.com/danmestas/go-libfossil/internal/shun"
	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/simio"

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
	fields, _, ok := cloneCapFields(v)
	if !ok {
		return 0, false
	}
	return fields[0], true
}

// maxCloneCookieBytes bounds a clone-cap cookie the server reads: well past
// the longest it writes (maxCloneRuns runs), and refused before it is split,
// since a request carries a cookie of any length.
const maxCloneCookieBytes = 64 << 10

// cloneCapFields splits a clone-cap cookie: the snapshot bound, then from a
// sparse walk the row the next round starts at and, while that row is a
// check-in part way through its walk, the last rid of the walk already sent
// ("lf-clone-cap:MAX", "lf-clone-cap:MAX:ROW", "lf-clone-cap:MAX:ROW:LAST").
// A sparse walk's cookie may end in "/" and the delta bases it carried
// (cloneCarriedRIDs), returned unparsed.
func cloneCapFields(v string) ([]int, string, bool) {
	if len(v) > maxCloneCookieBytes || !strings.HasPrefix(v, cloneCapPrefix) {
		return nil, "", false
	}
	head, carried, _ := strings.Cut(v[len(cloneCapPrefix):], "/")
	parts := strings.SplitN(head, ":", 4)
	if len(parts) > 3 {
		return nil, "", false
	}
	fields := make([]int, len(parts))
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return nil, "", false
		}
		fields[i] = n
	}
	return fields, carried, true
}

// HandleFunc is the server-side sync handler signature.
// Transport listeners call this with decoded requests and write back the response.
type HandleFunc func(ctx context.Context, r *repo.Repo, req *xfer.Message) (*xfer.Message, error)

// HandleOpts configures optional behavior for HandleSync.
type HandleOpts struct {
	// PartialMirror permits clone requests beyond this store's cone, as the
	// SettingPartialMirror repository setting does for every caller.
	PartialMirror bool
	Buggify       BuggifyChecker // nil in production.
	Observer      Observer       // nil defaults to no-op.
	ContentCache  *content.Cache // nil = no caching; rid-keyed, so not across a rollback
	Clock         simio.Clock    // time for the reply's timestamp comment and receipt; nil = real time
	// RemoteAddr is the address the request came from, recorded with what it
	// pushes (fossil's REMOTE_ADDR). "" takes the one WithRemoteAddr put in
	// the context, if any.
	RemoteAddr string
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

	h := &handler{repo: r, buggify: opts.Buggify, cache: opts.ContentCache, clock: opts.Clock, partialMirror: opts.PartialMirror, ctx: ctx,
		memo: originMemo(r.DB())}
	if h.clock == nil {
		h.clock = simio.RealClock{}
	}
	gate, err := newSubmanifestGate(r.DB())
	if err != nil {
		return nil, fmt.Errorf("sync.HandleSync: %w", err)
	}
	h.smGate = gate
	rc, err := serverReceipt(ctx, r, opts)
	if err != nil {
		return nil, fmt.Errorf("sync.HandleSync: %w", err)
	}
	h.repo, h.receipt = r.WithReceipt(rc), rc
	resp, err := h.process(ctx, req)
	if err == nil && resp == nil {
		panic("sync.HandleSync: resp must not be nil on success")
	}
	if err == nil {
		h.publishView()
	}

	obs.HandleCompleted(ctx, HandleEnd{
		CardsProcessed:   len(req.Cards),
		FilesSent:        h.filesSent,
		FilesReceived:    h.filesRecvd,
		SparseClassified: h.keep.classified() + h.structuralRead,
		Err:              err,
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
	ctx             context.Context
	cone            *cone.Cone
	keep            *sparseKeep
	partialMirror   bool
	historyURL      string // named in a horizon reply's horizon-served (§6.2.5)
	repo            *repo.Repo
	buggify         BuggifyChecker
	resp            []xfer.Card
	pushOK          bool                      // client sent a valid push card
	pullOK          bool                      // client sent a valid pull card
	cloneMode       bool                      // client sent a clone card
	fatal           bool                      // a card rule ended the request; resp holds only its error
	aborted         bool                      // a late hash mismatch rolls the request back (§7.7)
	broke           bool                      // card loop broken; the post-loop still runs
	errsReset       int                       // counted errors a reset discarded (§5.3 errors N)
	projectCode     string                    // the repository's project-code
	serverCode      string                    // the repository's server-code
	uncompressed    bool                      // reply goes out uncompressed (clone v3)
	cloneDone       bool                      // clone card has no usable cursor: send nothing
	peerVersion     int                       // client's release, from client-version
	peerDate        int                       // client's check-in date, from client-version
	cloneSeq        int                       // rid of the next blob to send, from the client's clone card
	cloneSnapMax    int                       // upper rid bound for this clone session (from cookie); 0 = capture fresh in emitCloneBatch
	cloneResume     cloneResume               // a sparse walk's progress at the cursor row (from cookie)
	walkClone       bool                      // a clone without pull: keep-set discovery walks the round (§6.2)
	uvCatalogSent   bool                      // true after sending UV catalog
	pendingPhantoms []pendingPhantom          // igot names awaiting flushPhantoms
	filesSent       int                       // files sent in response (for observer)
	smGate          submanifestGate           // §7.2 transfer gate, read once per request
	peerReadsTrees  bool                      // the request carries send-submanifests (§7.2)
	capabilitySent  bool                      // the reply's §7.2 pragma run is emitted
	smRefused       bool                      // this reply already carries the §7.2 refusal
	filesRecvd      int                       // files received from client (for observer)
	syncPrivate     bool                      // true if pragma send-private was accepted
	nextIsPrivate   bool                      // true if a private card precedes the next file/cfile
	syncedTables    map[string]*SyncedTable   // cached table definitions
	xrowsSent       int                       // table sync rows sent
	xrowsRecvd      int                       // table sync rows received
	cache           *content.Cache            // nil = passthrough to content.Expand
	clock           simio.Clock               // time for the reply's timestamp comment
	remoteHas       map[string]remoteHasEntry // UUIDs the client announced via igot (mirrors Fossil's onremote table)
	remoteLacks     map[string]bool           // UUIDs the client asked for (fossil's unk)
	logins          int                       // login cards read so far; only one is allowed
	pass            *receivePass              // this request's verification pass (§7.7)
	cursor          int64                     // the request's horizon cursor (horizon §6.2.6); 0 bounds nothing
	structural      *structuralServe          // a structural request this origin serves (horizon §6.2); nil otherwise
	seedOffered     string                    // the seed a structural request names as held (horizon §6.2.3)
	servedCursor    *xfer.PragmaCard          // the reply's horizon-cursor, valued once the inventory is out
	mergeBaseQuery  MergeBaseQuery            // the request's horizon-mergebase query (horizon §6.2.4); zero when none
	received        []string                  // the artifacts this request stored, in card order
	danglingHere    []storedDelta             // deltas this request stored unreadable (horizon §5.2.3)
	withheld        map[libfossil.FslID]bool  // pushed artifacts pending or refused as seeds (seedcheck), read once
	receipt         *db.Receipt               // what this request stores is recorded under
	memo            *content.Memo             // the store's classification memo (horizon §6.2); nil classifies afresh
	view            *content.MemoView         // what the inventory classified over, published once the request commits
	structuralRead  int                       // what placing a structural horizon read through the memo
	resolved        *xfer.PragmaCard          // echoed to a request naming its horizon (horizon §6.2.7)
	nameRefusal     string                    // why the request's horizon-name is refused; "" when it is not

	// Auth state
	user   string // verified username ("nobody" if no login card)
	caps   string // capability string from user table
	authed bool   // whether login card was verified
	// cloneRead is Read derived from Clone before any login, as page_xfer
	// derives it from nobody's capabilities ahead of its card loop.
	cloneRead bool

	// sources memoizes forwardableSource for the request, whose snapshot
	// does not change under it: a round's chain walks and sentEarlier's
	// searches ask about the same rows again and again.
	sources map[int]forwardable
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
	h.cloneRead = auth.CanClone(h.caps)
}

// canRead reports the requester's Read permission for this request, the one
// owner of that decision for every read gate the handler applies: the
// unversioned catalog, uvigot and uvgimme answers, xtable-hash and
// read-gated reqconfig.
//
// Clone implies Read for the capabilities a request starts with, nobody's,
// as fossil's page_xfer sets it (`if( g.perm.Clone ) g.perm.Read = 1;`,
// xfer.c:1301) before its card loop and §7.3 states. A Clone
// that a login card grants does not become Read: check_login only adds the
// user's capabilities, after that point.
//
// Pull is deliberately not one of these gates. fossil trunk's derived Read
// also lets a Clone-only nobody pull; here a pull still needs o
// (auth.CanPull), by the owner's decision.
func (h *handler) canRead() bool {
	return auth.CanRead(h.caps) || h.cloneRead
}

// handleLoginCard verifies a login card and adds the user's capabilities to
// nobody's, as fossil's check_login does through login_set_capabilities: a
// login only grants. It reports the refusal's error text, "" when the login
// is accepted. fossil does not go on serving the request as nobody after a
// refusal, and nor does this: the caller ends card processing there.
func (h *handler) handleLoginCard(c *xfer.LoginCard) string {
	// A login's shared secret is salted with the project code; a repository
	// without one can verify no login, and fossil's page_xfer stops there
	// too ("missing project code"). Refuse it rather than ask VerifyLogin,
	// which takes an empty project code as a programmer error.
	if h.projectCode == "" {
		return "login failed"
	}
	u, err := auth.VerifyLogin(h.repo.DB(), h.projectCode, c)
	if err != nil {
		return "login failed"
	}
	h.user = u.Login
	h.caps += u.Cap
	h.authed = true
	// check_login credits what the request stores to the user and records
	// the card's nonce; a nobody or anonymous login changes neither.
	if u.UID != 0 {
		h.receipt.UID = int64(u.UID)
		h.receipt.Nonce = c.Nonce
	}
	return ""
}

// handleLogin answers a login card at its own position in the request, as
// page_xfer's check_login runs where the card is read: the capabilities it
// grants apply to the cards after it, never to those before. Only one is
// allowed (§6.1, src/xfer.c "error multiple\slogin\scards"). A refusal is
// page_xfer's reset, error and break, so the cards before it keep their
// effect and the end-of-request steps still run (§6.4).
func (h *handler) handleLogin(c *xfer.LoginCard) {
	if c == nil {
		panic("handler.handleLogin: c must not be nil")
	}
	h.logins++
	if h.logins > 1 {
		h.breakLoop("multiple login cards")
		return
	}
	if msg := h.handleLoginCard(c); msg != "" {
		h.breakLoop(msg)
	}
}

// fail ends the request with a single error card, discarding the reply built
// so far, as fossil's cgi_reset_content before an error does.
func (h *handler) fail(msg string) {
	h.resetContent()
	h.resp = []xfer.Card{&xfer.ErrorCard{Message: msg}}
	h.fatal = true
}

// resetContent discards the reply built so far, as fossil's cgi_reset_content
// does before a card-level error. Only the reply is cleared; any blob already
// stored this round stays unless the request rolls back (§7.7 late mismatch,
// §8.1 fatal), as fossil keeps its mutations too (§10.1). The errors
// it discards are still counted in the closing comment (§5.3).
func (h *handler) resetContent() {
	h.errsReset += countedErrors(h.resp)
	h.resp = nil
}

// badCommandPrefix begins the error that answers an unknown or malformed card.
const badCommandPrefix = "bad command: "

// badCommand answers a line no card rule accepts. §5.2, src/xfer.c page_xfer's
// final "else": the reply so far is reset, an "error bad\scommand:\s<LINE>"
// card is emitted, and parsing goes on -- so a later card still takes effect.
// line keeps its terminating LF, which the error quotes verbatim.
func (h *handler) badCommand(line string) {
	h.resetContent()
	h.resp = append(h.resp, &xfer.ErrorCard{Message: badCommandPrefix + line})
}

// receivedLine returns the line a card arrived as, LF kept, so a bad command
// quotes it the way fossil quotes blob_str(&xfer.line). Cards the decoder did
// not stamp with their raw line fall back to replyOnlyLine.
func receivedLine(c xfer.Card) string {
	switch c := c.(type) {
	case *xfer.CloneSeqNoCard:
		if c.Raw != "" {
			return c.Raw
		}
	case *xfer.ErrorCard:
		if c.Raw != "" {
			return c.Raw
		}
	case *xfer.MessageCard:
		if c.Raw != "" {
			return c.Raw
		}
	case *xfer.UnknownCard:
		if c.Raw != "" {
			return c.Raw
		}
	}
	return replyOnlyLine(c)
}

// replyOnlyLine re-encodes a card fossil would not have recognized (an error
// or message the server received, which page_xfer has no branch for, or an
// igot or gimme whose HASH is no artifact name) into the wire line it arrived
// as. The LF EncodeCard writes is the one the error text carries.
func replyOnlyLine(c xfer.Card) string {
	var buf bytes.Buffer
	if err := xfer.EncodeCard(&buf, c); err != nil {
		// These cards always encode; this is a programmer error.
		panic("handler.replyOnlyLine: " + err.Error())
	}
	return buf.String()
}

// breakLoop answers a card with fossil's cgi_reset_content, error and break:
// the reply so far is reset to one error card and no later request card is
// processed, but the post-loop work and the timestamp comment still follow,
// as they do after page_xfer's card loop.
func (h *handler) breakLoop(msg string) {
	h.resetContent()
	h.resp = append(h.resp, &xfer.ErrorCard{Message: msg})
	h.broke = true
}

func (h *handler) process(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	// Initialize auth state from nobody user.
	h.initAuth()

	// A login's shared secret and every push and pull card are checked
	// against the project code. A repository without one leaves it empty,
	// which no artifact hash matches.
	if err := h.loadCodes(); err != nil {
		return nil, err
	}

	// Load synced tables.
	if err := h.loadSyncedTables(); err != nil {
		return nil, err
	}

	// The clone snapshot cookie is read ahead of the card loop, since the
	// clone batch goes out at the clone card and fossil's client sends its
	// cookie after that card.
	h.cloneSnapMax = cloneCapFromCards(req.Cards)
	h.cloneResume = cloneResumeFromCards(req.Cards)
	h.walkClone = detectOperation(req) == "clone" && !hasPullCard(req.Cards)
	// §7.2: a server decides from the request it is answering, so the
	// capability is read ahead of the card loop like the cookie above. A
	// request with no client-version gets its reply's pragma run here, at
	// the head; otherwise it follows server-version (handlePragma).
	h.peerReadsTrees = sendsSubmanifests(req.Cards)
	// Horizon §6.2.7: a named horizon is served as the hash it resolves to,
	// once the requester is known to read or clone.
	cards := req.Cards
	if h.nameAuthority(req.Cards) {
		var err error
		if cards, h.resolved, h.nameRefusal, err = resolveRequestName(h.repo.DB(), req.Cards); err != nil {
			return nil, err
		}
	}
	var coneErr error
	h.cone, coneErr = requestCone(cards)
	if coneErr != nil {
		h.fail(coneErr.Error())
		return h.reply(), nil
	}
	if h.cone != nil && h.nameRefusal == "" {
		if err := h.prepareStructural(ctx, cards); err != nil {
			return nil, err
		}
		var err error
		if h.historyURL, err = historyURL(h.repo.DB()); err != nil {
			return nil, err
		}
	}
	if !hasClientVersion(req.Cards) {
		h.emitCapabilityPragmas()
	}
	// §10.1: the request's cards run in one write transaction, as page_xfer
	// runs them after db_begin_write, and so does the closing crosslink. A
	// request none of whose cards can write runs in a read transaction
	// instead: it holds the write lock from no other request, and a clone
	// round on a large repository is served for longer than a waiting
	// request's busy timeout.
	begin := h.repo.WithBoundTx
	if readOnlyRequest(req.Cards) {
		begin = h.repo.WithBoundReadTx
	}
	if _, err := runPassIn(begin, func(bound *repo.Repo) (error, error) {
		defer h.bindRepo(bound)()
		return nil, h.processInTx(ctx, req)
	}); err != nil {
		return nil, err
	}
	// Outside the transaction, now the requester is known to read: a
	// structural reply's seed is signed and its cards filled (horizon §5.2.4).
	if err := h.finishSeedSigning(ctx); err != nil {
		return nil, err
	}
	if h.fatal || h.aborted {
		h.filesRecvd = 0 // rolled back with the request
	}
	if h.fatal {
		return h.reply(), nil
	}
	h.resp = append(h.resp, h.timestampComment())
	return h.reply(), nil
}

// readOnlyRequest reports whether no card of a request can make the handler
// write: it neither pushes (push, file, cfile, private, an igot under push)
// nor sends configuration, unversioned content or table rows, nor a pragma
// whose answer writes: uv-hash and xtable-hash (which an unversioned or table
// sync sends in place of a push card) and the check-in lock pair. Without a
// push card an igot only records what the client holds. A read transaction
// refuses a write the list misses, under the ncruces driver at least.
func readOnlyRequest(cards []xfer.Card) bool {
	for _, card := range cards {
		switch c := card.(type) {
		case *xfer.LoginCard, *xfer.CloneCard, *xfer.PullCard, *xfer.CookieCard,
			*xfer.GimmeCard, *xfer.IGotCard, *xfer.ReqConfigCard,
			*xfer.BadFileCard, *xfer.BadCommandCard, *xfer.ErrorCard,
			*xfer.MessageCard, *xfer.CloneSeqNoCard:
		case *xfer.PragmaCard:
			switch c.Name {
			case "uv-hash", "xtable-hash", "ci-lock", "ci-unlock":
				return false
			}
		default:
			return false
		}
	}
	return true
}

// processInTx is process's work inside the request transaction. A §8.1
// fatal or a late hash mismatch (§7.7) rolls the whole request back: it
// returns errPassAborted once the reply holds its error. After a late
// mismatch the reply is the wrong-hash card plus the timestamp comment, as
// sync.7.7.server-late-basis-wrong-hash requires; fossil 2.28/2.29 differs:
// its commit hook fails inside page_xfer and it aborts the request with an
// HTTP error page instead of sending that card.
func (h *handler) processInTx(ctx context.Context, req *xfer.Message) error {
	if err := h.processCards(ctx, req.Cards); err != nil {
		return err
	}
	h.refuseName()
	if h.fatal || h.aborted {
		return errPassAborted
	}
	// Horizon §5.2.3: a seed this request carried, made readable by a basis
	// later in it, refuses the request whole, its reply the refusal alone.
	seed, err := h.ownSeedReadable()
	if err != nil {
		return err
	}
	if seed {
		h.fail(seedRefusedText)
		return errPassAborted
	}
	// The end-of-request steps run after a broken loop too (§6.4).
	if err := h.emitInventory(ctx); err != nil {
		return err
	}
	if err := h.crosslinkReceived(ctx); err != nil {
		return err
	}
	// Horizon §5.7: a push whose cancellation contradicts this origin's
	// derivation is refused whole, so it stores nothing.
	refusal, err := h.staleCancel()
	if err != nil {
		return err
	}
	if refusal != "" {
		h.fail(refusal)
		return errPassAborted
	}
	return nil
}

// bindRepo points the handler at bound, the repository bound to the request
// transaction, and returns the function that points it back.
func (h *handler) bindRepo(bound *repo.Repo) func() {
	if bound == nil {
		panic("handler.bindRepo: bound must not be nil")
	}
	orig := h.repo
	h.repo = bound
	return func() { h.repo = orig }
}

// processCards handles the request's cards in request order, as page_xfer's
// card loop reads them, until one ends the request (h.fatal) or breaks the
// loop. Each card sees only the state the cards before it established: a
// file card before its push card is refused, and a card before a login card
// has nobody's capabilities (§5.2, §6.4, §6.6).
func (h *handler) processCards(ctx context.Context, cards []xfer.Card) error {
	for _, card := range cards {
		// Phantoms from a run of igot cards are written in one transaction,
		// as fossil writes them inside page_xfer's single write
		// transaction; any other card sees them already stored.
		if _, isIGot := card.(*xfer.IGotCard); !isIGot {
			if err := h.flushPhantoms(); err != nil {
				return err
			}
		}
		if err := h.processCard(ctx, card); err != nil {
			return err
		}
		if h.fatal || h.broke {
			break
		}
	}
	if h.fatal {
		// A fatal rolls the request back (§8.1): nothing pending is kept.
		h.pendingPhantoms = nil
		return nil
	}
	return h.flushPhantoms()
}

// processCard handles one request card.
func (h *handler) processCard(ctx context.Context, card xfer.Card) error {
	if card == nil {
		panic("handler.processCard: card must not be nil")
	}
	switch c := card.(type) {
	case *xfer.LoginCard:
		h.handleLogin(c)
	case *xfer.CloneCard:
		return h.handleClone(c)
	case *xfer.PragmaCard:
		return h.handlePragma(c)
	case *xfer.PushCard, *xfer.PullCard, *xfer.CookieCard, *xfer.SchemaCard:
		h.handleControlCard(card)
	case *xfer.BadFileCard:
		// A file/cfile/uvfile line the decoder could not frame: page_xfer
		// resets its reply, emits the handler's error and breaks (§5.2).
		h.breakLoop(c.Message)
	case *xfer.BadCommandCard:
		h.badCommand(c.Line)
	case *xfer.ErrorCard, *xfer.MessageCard, *xfer.CloneSeqNoCard:
		// error, message and clone_seqno are reply-only: a client that sends
		// one gets a bad command and later cards are processed, as fossil's
		// page_xfer has no branch for them (§5.2, SP-5.2-6).
		h.badCommand(receivedLine(c))
	case *xfer.UnknownCard:
		// A clone_seqno whose NEXT is not decimal decodes to UnknownCard but
		// is still the defined reply-only operator (§5.2); fossil has no
		// server branch for any clone_seqno line.
		if c.Command == "clone_seqno" {
			h.badCommand(receivedLine(c))
			return nil
		}
		return h.handleDataCard(ctx, card)
	default:
		return h.handleDataCard(ctx, card)
	}
	return nil
}

// crosslinkReceived crosslinks any newly-received manifests into the
// relational tables (event/leaf/plink/mlink/tagxref). Without this the
// receiving repo stores blobs durably but exposes nothing on the timeline,
// fork detection sees stale leaf state, and the clone protocol cannot
// traverse manifest parents (mlink is empty).
//
// It walks the scanner only when we accepted files this round; pure
// pull/igot rounds add nothing relational to update. It is context-aware
// (#120): the sweep walks the whole repository in one call, so on a large
// repo it is where a request whose client has already gone away would
// otherwise keep working uninterrupted.
func (h *handler) crosslinkReceived(ctx context.Context) error {
	if h.filesRecvd == 0 {
		return nil
	}
	if _, err := manifest.CrosslinkContext(ctx, h.repo); err != nil {
		return fmt.Errorf("HandleSync: crosslink: %w", err)
	}
	return nil
}

// reply is the message the handler answers with. A horizon-cursor no
// inventory valued is dropped: only a reply to a pull hands one out.
func (h *handler) reply() *xfer.Message {
	cards := h.resp
	if h.servedCursor != nil && len(h.servedCursor.Values) == 0 {
		cards = slices.DeleteFunc(slices.Clone(cards), func(c xfer.Card) bool {
			return c == xfer.Card(h.servedCursor)
		})
	}
	return &xfer.Message{Cards: cards, Uncompressed: h.uncompressed}
}

// loadCodes reads the repository's project and server codes. A login's
// shared secret and every push and pull card are checked against the project
// code; a clone's push card carries both. A repository without one leaves it
// empty, which no artifact hash matches.
func (h *handler) loadCodes() error {
	for _, c := range []struct {
		name string
		dst  *string
	}{{"project-code", &h.projectCode}, {"server-code", &h.serverCode}} {
		err := h.repo.DB().QueryRow(
			"SELECT value FROM config WHERE name=?", c.name).Scan(c.dst)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("HandleSync: %s: %w", c.name, err)
		}
	}
	return nil
}

// emitClonePush emits the push card with project-code and server-code so
// the clone client can identify the repo. Only in clone mode — sync clients
// already have codes, and real Fossil treats server-sent "push" as unknown
// during sync.
//
// This must trail the clone batch's clone_seqno card, matching canonical's
// order (fossil-scm xfer.c emits the batch, then clone_seqno at :1571, then
// push at :1577). A real fossil client re-issues `clone 3 SEQNO` every round
// it sees a `push` card while its clone cursor is still positive (xfer.c:2706),
// and it learns the cursor hit zero only from the clone_seqno card. Emitting
// push ahead of clone_seqno makes the client queue one more clone request
// before it sees the terminal clone_seqno 0, so the server serves the whole
// repository a second time — the 2.06x end-to-end blow-up of issue #138.
func (h *handler) emitClonePush() error {
	if !h.cloneMode {
		panic("handler.emitClonePush: not a clone")
	}
	if h.projectCode != "" {
		h.resp = append(h.resp, &xfer.PushCard{
			ProjectCode: h.projectCode,
			ServerCode:  h.serverCode,
		})
	}
	return nil
}

// timestampComment is the comment that ends a completed reply, as fossil's
// page_xfer writes it: the server's UTC time and the request's error count
// (§5.3). A fail-fast reply goes without it.
func (h *handler) timestampComment() *xfer.CommentCard {
	if h.clock == nil {
		panic("handler.timestampComment: nil clock")
	}
	if h.errsReset < 0 {
		panic("handler.timestampComment: negative reset error count")
	}
	errs := h.errsReset + countedErrors(h.resp)
	now := h.clock.Now().UTC().Format("2006-01-02T15:04:05")
	return &xfer.CommentCard{Text: fmt.Sprintf("timestamp %s errors %d", now, errs)}
}

// privateRefusal is the error a requester without the private capability
// gets for pragma send-private or a private card.
const privateRefusal = "not authorized to sync private content"

// countedErrors counts the error cards that §5.3's errors N counts. N is the
// errors raised, as page_xfer's nErr is, and three of them fossil never adds
// to nErr: the unknown-operator error, the private-content refusal and the
// SHA3 version refusal (§5.3). The
// count is made from card text because those three texts are fixed by the
// draft and no other error carries them.
func countedErrors(cards []xfer.Card) int {
	n := 0
	for _, c := range cards {
		e, ok := c.(*xfer.ErrorCard)
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(e.Message, badCommandPrefix),
			e.Message == privateRefusal,
			e.Message == sha3Refusal().Message:
			continue
		}
		n++
	}
	return n
}

// emitInventory sends what follows the card loop: the phantom gimmes of a
// push and the igot inventory of a pull. A clone's batch and the cluster
// igots of req-clusters go out in place instead, at their cards.
func (h *handler) emitInventory(ctx context.Context) error {
	// An accepted push asks for the server's phantoms, ahead of the pull's
	// inventory as page_xfer's request_phantoms runs ahead of
	// send_unclustered (§6.4, §8.5).
	if h.pushOK {
		if err := h.requestPhantoms(); err != nil {
			return err
		}
	}
	// If pull was requested, emit igot for all non-phantom blobs. Horizon
	// §6.2.4: a merge-base answer goes ahead of them, where zit puts it.
	if h.pullOK {
		if err := h.answerMergeBase(ctx); err != nil {
			return err
		}
		if err := h.emitIGots(); err != nil {
			return err
		}
		if err := h.emitXIGots(); err != nil {
			return err
		}
	}
	return nil
}

// handleClone answers a clone card in place, as page_xfer's clone branch
// writes its reply where the card is read (xfer.c:1532-1578): the
// unversioned catalog when uv-sync is on (§8.4), the batch and clone_seqno,
// then the push card naming the repository (§8.2). Sent there, they are
// discarded by any later reset, a bad command or a refused push, as fossil's
// cgi_reset_content discards them (§10.1).
func (h *handler) handleClone(c *xfer.CloneCard) error {
	h.handleCloneCard(c)
	if !h.cloneMode || h.fatal || h.refuseName() {
		return nil
	}
	mirror, err := h.servesPartialMirror()
	if err != nil {
		return err
	}
	if !mirror {
		have, err := content.StoreCone(h.repo.DB())
		if err != nil {
			return err
		}
		covered, err := h.coversRequest(have)
		if err != nil {
			return err
		}
		if !covered {
			h.refuseClone("Sparse cone does not cover requested clone; configure a partial mirror to allow it.")
			return nil
		}
	}
	if err := h.ensureSparseKeep(); err != nil {
		return err
	}
	if err := h.sendCloneUVCatalog(); err != nil {
		return err
	}
	if h.cloneDone {
		h.resp = append(h.resp, &xfer.CloneSeqNoCard{SeqNo: 0})
	} else if err := h.emitCloneBatch(); err != nil {
		return err
	}
	return h.emitClonePush()
}

// cloneCapFromCards is the clone snapshot bound the request's last clone-cap
// cookie names, or 0 when it carries none.
func cloneCapFromCards(cards []xfer.Card) int {
	snap := 0
	for _, card := range cards {
		if c, ok := card.(*xfer.CookieCard); ok {
			if rid, ok := parseCloneCap(c.Value); ok {
				snap = rid
			}
		}
	}
	return snap
}

func (h *handler) handleControlCard(card xfer.Card) {
	switch c := card.(type) {
	case *xfer.PushCard:
		h.handlePushCard(c)
	case *xfer.PullCard:
		h.handlePullCard(c)
	case *xfer.CookieCard:
		// Clone clients echo the cap cookie on every round; process reads
		// it ahead of the card loop (cloneCapFromCards), since the batch
		// it bounds goes out at the clone card. Sync sessions use cookies
		// opaquely.
	case *xfer.SchemaCard:
		h.handleSchemaCard(c)
	}
}

// handlePushCard accepts a push from a requester with Write, after the
// project check.
func (h *handler) handlePushCard(c *xfer.PushCard) {
	if h.wrongProject(c.ProjectCode) {
		return
	}
	if auth.CanPush(h.caps) {
		h.pushOK = true
		return
	}
	if h.pullOK {
		// A requester who may read but not write is told to pull only,
		// not refused, as fossil's page_xfer answers a push card with
		// "message pull only - not authorized to push" once a pull card
		// has set isPull. The pull still proceeds (§10.2).
		h.resp = append(h.resp, &xfer.MessageCard{
			Message: "pull only - not authorized to push",
		})
		return
	}
	// Otherwise the reply so far is reset and the push refused, but card
	// processing goes on (§6.6): page_xfer has no break here.
	h.resetContent()
	h.resp = append(h.resp, &xfer.ErrorCard{Message: "not authorized to write"})
}

// handlePullCard accepts a pull from a requester with Read, after the project
// check.
func (h *handler) handlePullCard(c *xfer.PullCard) {
	if h.wrongProject(c.ProjectCode) {
		return
	}
	if auth.CanPull(h.caps) {
		h.pullOK = true
		return
	}
	// page_xfer resets, refuses and breaks (§6.6, §10.1).
	h.breakLoop("not authorized to read")
}

// handleCloneCard starts a clone for a requester with Clone, at the rid the
// card's SEQNO names.
func (h *handler) handleCloneCard(c *xfer.CloneCard) {
	if !auth.CanClone(h.caps) {
		h.refuseClone("not authorized to clone")
		return
	}
	// §4.1, §8.1: page_xfer sends a version 3 clone's reply uncompressed,
	// and sets that before the sequence-number fatal below.
	h.uncompressed = c.Version >= 3
	// §8.1: an otherwise authorized three-token clone with VERSION >= 2
	// and a digit-only SEQNO of zero or less clears accumulated output,
	// rolls back, emits this error, and parses no later request card.
	// processInTx rolls the request transaction back on h.fatal, so
	// nothing the cards before this one stored survives.
	if c.SeqNoIsDecimal && c.Version >= 2 && c.SeqNo <= 0 {
		h.fail("invalid clone sequence number")
		return
	}
	h.cloneMode = true
	// The pagination cursor rides on the clone card and nowhere else
	// (§8.1; canonical xfer.c:1553 reads token 2 of `clone VERSION
	// SEQNO`). §8.1 withholds the fatal above from a negative SEQNO, which
	// fails digit-only recognition; page_xfer then has no cursor at all, so
	// no content is sent and the reply says the clone is done. A bare
	// `clone` starts from the first rid.
	if c.Version >= 2 && c.SeqNo < 0 {
		h.cloneDone = true
		return
	}
	h.cloneSeq = c.SeqNo
	if h.cloneSeq <= 0 {
		h.cloneSeq = 1
	}
}

// refuseClone answers a clone it will not serve as page_xfer answers one from
// a requester without Clone (§8.2, §10.1): the reply so far is reset, the
// push card naming the repository goes ahead of the error, and no later card
// is processed. A sparse store that does not cover the request refuses the
// same way (sparse-cone §5.3). fossil's clone client needs the push card: a
// first round without one reads as an empty, finished clone, and it exits 0
// with no project code, where with the codes it reads the error as fatal.
func (h *handler) refuseClone(msg string) {
	h.resetContent()
	serverCode := h.serverCode
	if serverCode == "" {
		serverCode = "x" // db_get("server-code", "x")
	}
	h.resp = append(h.resp,
		&xfer.PushCard{ServerCode: serverCode, ProjectCode: h.projectCode},
		&xfer.ErrorCard{Message: msg})
	h.broke = true
}

// wrongProject refuses a push or pull card naming another project, ahead of
// any capability check, as page_xfer does with "error wrong\sproject" and a
// break (§8.5). The servercode beside it is informational and not checked.
// Cards are handled in request order, so a file card ahead of it has been
// stored, as fossil stores it.
func (h *handler) wrongProject(code string) bool {
	if code == h.projectCode {
		return false
	}
	h.breakLoop("wrong project")
	return true
}

// handlePragma acts on the pragmas the server understands and ignores the
// rest, client-version included.
//
// Pragmas are handled in card order, so a pull card later in the request is
// not yet known here. The catalogs are therefore gated on
// the Read capability itself, as fossil's page_xfer gates uv-hash on
// g.perm.Read, rather than on an accepted pull.
func (h *handler) handlePragma(c *xfer.PragmaCard) error {
	if c == nil {
		panic("handler.handlePragma: c must not be nil")
	}
	switch {
	case c.Name == "client-version" && len(c.Values) >= 1:
		// §9.1: the client's release gates SHA3 content (§7.6). A
		// client-version carrying DATE and TIME is answered in place, as
		// page_xfer answers it, so the answer precedes any content.
		h.peerVersion = peerVersion(c)
		h.peerDate = peerDate(c)
		if len(c.Values) >= 3 {
			h.resp = append(h.resp, versionPragma("server-version"))
		}
		h.emitCapabilityPragmas()
	case c.Name == "uv-hash" && len(c.Values) >= 1:
		if err := h.handlePragmaUVHash(c.Values[0]); err != nil {
			h.resp = append(h.resp, &xfer.ErrorCard{
				Message: fmt.Sprintf("uv-hash: %v", err),
			})
		}
	case c.Name == "xtable-hash" && len(c.Values) >= 2:
		// The inventory serves both directions of table sync: xgimme
		// pulls rows from it, and a pushing client queues the rows it
		// holds newer (client_tablesync.go handleXIGotResponse). So it
		// needs Read and a direction card, not xgimme's pull alone. A
		// Clone-only requester has Read by derivation (#312) but sends no
		// direction card, so it gets no inventory (#351). Table sync is a
		// go-libfossil extension with no fossil counterpart.
		if h.canRead() && (h.pullOK || h.pushOK) {
			h.handlePragmaXTableHash(c.Values[0], c.Values[1])
		}
	case c.Name == "req-clusters" && !h.horizonScoped():
		// Answered in place, as page_xfer answers it, so a later reset
		// discards the igots (§10.1). Like fossil, any requester gets them:
		// they name public artifacts and carry no content, which a gimme
		// still needs Read to fetch. A horizon-scoped request is sent no
		// cluster (draft-fossil-horizon-00 §6.2), so it gets nothing.
		return h.sendAllClusters()
	case c.Name == "ci-lock" && len(c.Values) == 2 && hash.IsValidHash(c.Values[0]):
		// fossil takes the pragma only with exactly HASH and CLIENT-ID, HASH
		// a hash (xfer.c:1781); anything else is no lock request. As in
		// fossil, any requester may take a lock, with no Write test (§9.4).
		h.handleCkinLock(c.Values[0], c.Values[1])
	case c.Name == "ci-unlock" && len(c.Values) == 1 && hash.IsValidHash(c.Values[0]):
		// fossil drops CLIENT-ID's check-in locks for any requester, with no
		// Write or ownership test (§9.4).
		releaseCkinLocks(h.repo.DB(), c.Values[0])
	case c.Name == horizonCursorPragma:
		if n, ok := parseCursor(c); ok {
			h.cursor = n
		}
	case c.Name == horizonMergeBasePragma:
		if q, ok := parseMergeBaseQuery(c); ok {
			h.mergeBaseQuery = q
		}
	case c.Name == "send-private":
		if auth.CanSyncPrivate(h.caps) {
			h.syncPrivate = true
		} else {
			h.resp = append(h.resp, &xfer.ErrorCard{Message: privateRefusal})
		}
	}
	return nil
}

// emitCapabilityPragmas appends the reply's §7.2 pragma run, once per
// reply: send-submanifests, then avoid-submanifests while this repository's
// enable-submanifests is off.
func (h *handler) emitCapabilityPragmas() {
	if h.capabilitySent {
		return
	}
	h.capabilitySent = true
	h.resp = append(h.resp, capabilityPragmas(enableSubmanifests(h.repo.DB()))...)
	// Sparse-cone §6.3: this server applies a request's whole cone or
	// refuses it (requestCone, handleClone), so a cone request's reply is
	// acknowledged here, and a horizon request's says the level it is served
	// at (horizon §6.2.5). A refusal resets the reply and discards both.
	if h.cone != nil && len(h.cone.Include()) > 0 {
		h.resp = append(h.resp, &xfer.PragmaCard{Name: coneServedPragma})
	}
	if h.horizonScoped() {
		h.echoResolved()
		h.emitHorizonServed()
	}
}

// horizonScoped reports a request that carried a horizon pragma.
func (h *handler) horizonScoped() bool {
	if h.cone == nil {
		return false
	}
	_, ok := h.cone.Horizon()
	return ok
}

// handleCkinLock answers pragma ci-lock HASH CLIENT-ID. The lock row holds
// the requester's login, null for nobody as fossil's g.zLogin is, and
// ci-lock-fail carries the holder's login fossilized (%F in xfer.c). For a
// lock with no login fossil emits an empty LOGIN, which the -00 grammar
// (1*fossilized) does not allow and which fossil's own client drops as a
// short card; this server names the holder nobody instead.
//
// No capability gates the pragma, so the config table is bounded instead by
// the sweep processCkinLock runs before each write: a lock whose HASH is not
// a leaf is deleted, so at most one lock per leaf plus the latest lock on a
// non-leaf survive, whatever the requester sends.
//
// Whether or not a lock was written, the reply then carries pragma
// avoid-delta-manifests while forbid-delta-manifests is on, after any
// ci-lock-fail (§9.4), as fossil's handler ends (xfer.c:1831).
func (h *handler) handleCkinLock(parentUUID, clientID string) {
	login := h.user
	if login == "nobody" {
		login = ""
	}
	fail := processCkinLock(h.repo.DB(), parentUUID, clientID, login,
		lockTimeout(h.repo.DB()))
	if fail != nil {
		holder := fail.HeldBy
		if holder == "" {
			holder = "nobody"
		}
		h.resp = append(h.resp, &xfer.PragmaCard{
			Name: "ci-lock-fail",
			Values: []string{deck.FossilEncode(holder),
				fmt.Sprintf("%d", fail.Since.Unix())},
		})
	}
	if forbidDeltaManifests(h.repo.DB()) {
		h.resp = append(h.resp, &xfer.PragmaCard{Name: "avoid-delta-manifests"})
	}
}

// submanifestBarred applies the §7.2 gate to an answer to a gimme: a tree or
// sub-manifest is withheld from a client whose request did not carry
// send-submanifests, and the reply says so once, explicitly, rather than
// omitting it in silence.
func (h *handler) submanifestBarred(rid libfossil.FslID) (bool, error) {
	barred, err := h.smGate.barred(h.repo.DB(), h.cache, rid, h.peerReadsTrees)
	if err != nil || !barred {
		return false, err
	}
	if !h.smRefused {
		h.resp = append(h.resp, submanifestRefusal())
		h.smRefused = true
	}
	return true, nil
}

func (h *handler) handleDataCard(ctx context.Context, card xfer.Card) error {
	switch c := card.(type) {
	case *xfer.IGotCard:
		return h.handleIGot(c)
	case *xfer.GimmeCard:
		return h.handleGimme(c)
	case *xfer.FileCard:
		return h.handleFile(ctx, c.UUID, c.DeltaSrc, c.Content, nil, false)
	case *xfer.CFileCard:
		return h.handleFile(ctx, c.UUID, c.DeltaSrc, c.Content, c.StoredBlob, true)
	case *xfer.PrivateCard:
		if !auth.CanSyncPrivate(h.caps) {
			h.resp = append(h.resp, &xfer.ErrorCard{Message: privateRefusal})
			h.nextIsPrivate = false
		} else {
			h.nextIsPrivate = true
		}
		return nil
	case *xfer.ReqConfigCard:
		return h.handleReqConfig(c)
	case *xfer.ConfigCard:
		h.handleConfigPush()
		return nil
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
	// page_xfer reads igot only with an artifact name (blob_is_hname); any
	// other line falls through to its bad command (§5.2). IsValidHash takes
	// lowercase hex only where validate16 also takes A-F: an uppercase name
	// would break the blob table's CHECK constraint, so it is refused here.
	if !hash.IsValidHash(c.UUID) {
		h.badCommand(replyOnlyLine(c))
		return nil
	}
	// Only a pushing client can deliver what it announces. Fossil's
	// page_xfer wraps igot handling in `if( isPush )`; asking a pull-only
	// client for artifacts it will never send holds its pull open until
	// MaxRounds (#277).
	if !h.pushOK {
		// Record that the client has this blob so emitIGots can skip it.
		// Mirrors Fossil's remote_has() → onremote table (xfer.c:1471),
		// which fossil fills only under push; it is kept for a pull too, so
		// a pull-only client is not told about what it just announced.
		if _, exists := blob.Exists(h.repo.DB(), c.UUID); exists {
			h.recordRemoteHas(c)
		}
		return nil
	}
	// A private igot from a requester who may not push private content
	// makes no phantom, but reclassifies what the server holds (§7.5).
	if c.IsPrivate && !auth.CanSyncPrivate(h.caps) {
		return h.reclassifyPrivate(c)
	}
	if _, exists := blob.Exists(h.repo.DB(), c.UUID); exists {
		// page_xfer's rid_from_uuid finds the artifact or phantom and
		// content_make_private or content_make_public reclassifies it as
		// the igot says (§7.5); flushPhantoms applies that in its batch.
		h.pendingPhantoms = append(h.pendingPhantoms,
			pendingPhantom{uuid: c.UUID, private: c.IsPrivate})
		h.recordRemoteHas(c)
		return nil
	}
	// Fossil's igot handling goes through rid_from_uuid → content_new, which
	// returns 0 for a shunned name: no phantom is made, so no gimme follows
	// (src/content.c, #349).
	shunned, err := shun.IsShunned(h.repo.DB(), c.UUID)
	if err != nil {
		return fmt.Errorf("handler: shun check for igot %s: %w", c.UUID, err)
	}
	if shunned {
		return nil
	}
	h.pendingPhantoms = append(h.pendingPhantoms,
		pendingPhantom{uuid: c.UUID, private: c.IsPrivate})
	// page_xfer calls remote_has on the new phantom too, so a file card
	// later in the request that fills it is not announced back.
	h.recordRemoteHas(c)
	return nil
}

// reclassifyPrivate answers igot HASH 1 from a requester without the
// private capability, as page_xfer's rid_from_uuid(&uuid, 0, 1) branch does:
// an artifact or phantom the server holds is made private and recorded as
// the client's, and an absent HASH gets no phantom (§7.5). The pending
// phantoms of the igots before it are stored first, so one of them counts
// as held, as it does in fossil, which stores each as it reads it.
func (h *handler) reclassifyPrivate(c *xfer.IGotCard) error {
	if !c.IsPrivate {
		panic("handler.reclassifyPrivate: igot is not private")
	}
	if err := h.flushPhantoms(); err != nil {
		return err
	}
	rid, exists := blob.Exists(h.repo.DB(), c.UUID)
	if !exists {
		return nil
	}
	if err := content.MakePrivate(h.repo.DB(), int64(rid)); err != nil {
		return fmt.Errorf("handler: private igot %s: %w", c.UUID, err)
	}
	h.recordRemoteHas(c)
	return nil
}

// recordRemoteHas notes that the client holds what an igot announced.
func (h *handler) recordRemoteHas(c *xfer.IGotCard) {
	if c == nil {
		panic("handler.recordRemoteHas: c must not be nil")
	}
	if h.remoteHas == nil {
		h.remoteHas = make(map[string]remoteHasEntry)
	}
	h.remoteHas[c.UUID] = remoteHasEntry{isPrivate: c.IsPrivate}
}

// pendingPhantom is an artifact a pushing client announced that
// flushPhantoms has yet to record.
type pendingPhantom struct {
	uuid    string
	private bool
}

// flushPhantoms records the pending igot names as phantoms in one
// transaction, as page_xfer's rid_from_uuid(..., 1, isPriv) does: push
// creates server phantoms from client igot (§3.3, §8.5). Each name, new or
// already held, is then made private or public as its igot said, as
// content_make_private and content_make_public follow (§7.5). The gimme for
// the phantoms is sent by requestPhantoms after the card loop, so a file card
// later in the same request fills one without being asked for. One
// transaction for the run avoids a WAL commit per announced artifact.
func (h *handler) flushPhantoms() error {
	if len(h.pendingPhantoms) == 0 {
		return nil
	}
	pending := h.pendingPhantoms
	h.pendingPhantoms = nil
	return h.repo.WithTx(func(tx *db.Tx) error {
		for _, p := range pending {
			rid, err := blob.StorePhantom(tx, p.uuid)
			if err != nil {
				return fmt.Errorf("handler: phantom for igot %s: %w", p.uuid, err)
			}
			if err := recordVisibility(tx, rid, visibility(p.private)); err != nil {
				return fmt.Errorf("handler: igot %s: %w", p.uuid, err)
			}
		}
		return nil
	})
}

// maxPhantomGimme bounds the gimme cards one reply sends for the server's
// phantoms: page_xfer's request_phantoms(&xfer, 500).
const maxPhantomGimme = 500

// requestPhantoms asks a pushing client for the server's phantoms, as
// page_xfer's request_phantoms does after the card loop: every phantom but
// those the client asked for this round (it lacks them), shunned names,
// private ones outside a private sync (§3.3), and a sparse store's elided
// ones, at most maxPhantomGimme.
func (h *handler) requestPhantoms() error {
	if !h.pushOK {
		panic("handler.requestPhantoms: push not accepted")
	}
	query := "SELECT rid, uuid FROM phantom CROSS JOIN blob USING(rid)" +
		" WHERE NOT EXISTS(SELECT 1 FROM shun WHERE uuid=blob.uuid)"
	if !h.syncPrivate {
		query += " AND NOT EXISTS(SELECT 1 FROM private WHERE rid=blob.rid)"
	}
	// draft-fossil-sparse-cone-00 §5.2: an elided phantom is never asked for.
	elided, err := elidedPhantoms(h.ctx, h.repo.DB())
	if err != nil {
		return fmt.Errorf("handler: classifying phantoms: %w", err)
	}
	rows, err := h.repo.DB().Query(query + " ORDER BY rid")
	if err != nil {
		return fmt.Errorf("handler: listing phantoms: %w", err)
	}
	defer rows.Close()
	sent := 0
	for sent < maxPhantomGimme && rows.Next() {
		var rid libfossil.FslID
		var uuid string
		if err := rows.Scan(&rid, &uuid); err != nil {
			return fmt.Errorf("handler: listing phantoms: %w", err)
		}
		// A structural store asks for a name the pusher announced though
		// nothing it holds names it yet (horizon §5.1): a new check-in.
		_, heard := h.remoteHas[uuid]
		announced := heard && elided != nil && elided.Unnamed(rid)
		if h.remoteLacks[uuid] || (elided != nil && elided.Class(rid) == content.Elided && !announced) {
			continue
		}
		h.resp = append(h.resp, &xfer.GimmeCard{UUID: uuid})
		sent++
	}
	return rows.Err()
}

func (h *handler) handleGimme(c *xfer.GimmeCard) error {
	if c == nil {
		panic("handler.handleGimme: c must not be nil")
	}
	// page_xfer reads gimme only with an artifact name (blob_is_hname); any
	// other line falls through to its bad command (§5.2).
	if !hash.IsValidHash(c.UUID) {
		h.badCommand(replyOnlyLine(c))
		return nil
	}
	// The client lacks what it asks for, so the server does not ask for it
	// back (fossil's remote_unk, ahead of the isPull check).
	if h.remoteLacks == nil {
		h.remoteLacks = make(map[string]bool)
	}
	h.remoteLacks[c.UUID] = true
	// Fossil's page_xfer answers gimme only `if( isPull )`: a pull card the
	// requester's Read permission accepted, or a clone, which sets isPull
	// too. Answering any other gimme hands content to whoever names a hash.
	if !h.pullOK && !h.cloneMode {
		return nil
	}
	// BUGGIFY: 5% chance skip sending a file to test client retry.
	if h.buggify != nil && h.buggify.Check("handler.handleGimme.skip", 0.05) {
		return nil
	}
	rid, ok := content.AvailableByUUID(h.repo.DB(), c.UUID)
	if !ok {
		return nil // blob not found — not fatal, skip.
	}
	// §6.4: a gimme names its artifact, so a cone request is answered
	// densely; the requester never names what its own cone elides. A
	// horizon request with path members is not answered a node the origin
	// withholds (horizon §6.2); one without path members is.
	if err := h.ensureSparseKeep(); err != nil {
		return err
	}
	if h.keep.withholds(rid) {
		return nil
	}
	isPriv := content.IsPrivate(h.repo.DB(), int64(rid))
	if isPriv && !h.syncPrivate {
		// §8.6: outside a private sync, a peer dated 2020-04-13 or later is
		// told the artifact exists and is private; an older or undated one
		// is told nothing, as fossil's send_file decides on remoteDate.
		if h.peerDate >= privateIGotDate {
			h.resp = append(h.resp, &xfer.IGotCard{UUID: c.UUID, IsPrivate: true})
		}
		return nil
	}
	// §7.6: SHA3 content is withheld from a client older than SHA3
	// support, or one that named no release; the refusal is appended, so
	// what the reply already holds stands (§10.1).
	if sha3Barred(c.UUID, h.peerVersion) {
		h.resp = append(h.resp, sha3Refusal())
		return nil
	}
	// Preserve send_file's private notice/SHA3 refusal ordering, then withhold
	// shunned payloads before any submanifest or content expansion.
	shunned, err := shun.IsShunned(h.repo.DB(), c.UUID)
	if err != nil || shunned {
		return err
	}
	if withheld, err := h.withheldSeed(rid); err != nil || withheld {
		return err
	}
	if barred, err := h.submanifestBarred(rid); err != nil || barred {
		return err
	}
	data, err := h.cache.Expand(h.repo.DB(), rid)
	if err != nil {
		h.resp = append(h.resp, &xfer.ErrorCard{
			Message: fmt.Sprintf("expand %s: %v", c.UUID, err),
		})
		return nil
	}
	// Horizon §5.1: a structural request keeps no manifest or node below
	// its horizon, so one asked for is not sent, as zit skips it.
	if h.structural.withholds(c.UUID, data) {
		return nil
	}
	// Horizon §4.5, §6.2: a horizon-scoped request is sent no cluster,
	// even one it names, so a clone never learns what only a cluster names.
	if h.horizonScoped() && isCluster(data) {
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

// handleFile stores a pushed file or cfile card (compressed). A validation
// failure is xfer_accept_file's error path: page_xfer resets the reply,
// emits the error and parses no later card (§5.2), so each failure here
// ends in breakLoop.
func (h *handler) handleFile(ctx context.Context, uuid, deltaSrc string, payload []byte,
	storedBlob []byte, compressed bool) error {
	if uuid == "" {
		panic("handler.handleFile: uuid must not be empty")
	}
	// §6.6: without an accepted push, the first file or cfile card resets
	// the reply, is refused and ends card processing, as page_xfer's
	// "error not authorized to write" and break do. The end-of-request
	// steps of a pull accepted earlier still run.
	if !h.pushOK {
		h.breakLoop("not authorized to write")
		return nil
	}
	// xfer_accept_file and xfer_accept_compressed_file test HASH and
	// DELTASRC with blob_is_hname before anything else.
	if !hash.IsValidHash(uuid) || (deltaSrc != "" && !hash.IsValidHash(deltaSrc)) {
		h.breakLoop(malformedFileLine(compressed))
		return nil
	}
	// A shunned artifact is dropped on receipt, with no error, as fossil's
	// xfer_accept_file drops it (§7.7); it is not received.
	shunned, err := shun.IsShunned(h.repo.DB(), uuid)
	if err != nil {
		return err
	}
	if shunned {
		h.nextIsPrivate = false
		return nil
	}
	// Horizon §5.2.3: besides a seed's shape (isSeed), an origin
	// refuses a file bearing the name the request's horizon-seed offered.
	if h.seedOffered != "" && strings.EqualFold(uuid, h.seedOffered) {
		h.fail(seedRefusedText)
		return nil
	}
	// BUGGIFY: 3% chance reject a valid file to test client re-push.
	if h.buggify != nil && h.buggify.Check("handler.handleFile.reject", 0.03) {
		h.resp = append(h.resp, &xfer.ErrorCard{
			Message: fmt.Sprintf("buggify: rejected file %s", uuid),
		})
		return nil
	}
	vis := visibility(h.nextIsPrivate)
	h.nextIsPrivate = false
	// storeFile verifies a delta against HASH before storing it (Section
	// 7.7, SP-7.7-4), file and cfile alike, so this handler never stores an
	// artifact under a name it does not have. fossil verifies a file here and
	// a cfile in its commit hook, which rolls back the request's earlier
	// changes; this refuses the card with the same reset and error instead,
	// so those changes stay applied. A delta found wrong only when its basis
	// arrives is the commit hook's case: the request rolls back.
	msg, late := h.storeFile(ctx, uuid, deltaSrc, payload, storedBlob, vis)
	if msg != "" {
		h.breakLoop(msg)
		h.aborted = late
		return nil
	}
	if h.fatal {
		return nil // storeFile refused a seed: the request is refused whole
	}
	return h.recordStored(uuid, deltaSrc, payload)
}

// recordStored notes an artifact handleFile stored, for crosslinking and
// for the seed check (horizon §5.2.3). An artifact is judged when it first
// becomes readable: full text and a delta on a readable basis now; a delta
// on an unreadable basis is recorded pending in seedcheck, and judged by
// ownSeedReadable or at the crosslink step of whatever later fills it.
func (h *handler) recordStored(uuid, deltaSrc string, payload []byte) error {
	if payload == nil {
		panic("handler.recordStored: payload must not be nil")
	}
	h.filesRecvd++
	h.received = append(h.received, uuid)
	if deltaSrc == "" {
		if seedcheck.IsSeedBytes(payload) {
			h.fail(seedRefusedText)
		}
		return nil
	}
	q := h.repo.DB()
	rid, ok := blob.Exists(q, uuid)
	if !ok {
		panic("handler.recordStored: a stored artifact must exist")
	}
	if !content.IsAvailable(q, rid) {
		// storeDeltaContent recorded it pending; this request judges it if a
		// basis later in the request makes it readable (ownSeedReadable).
		h.danglingHere = append(h.danglingHere, storedDelta{uuid: uuid, rid: rid})
		return nil
	}
	if h.pass == nil {
		panic("handler.recordStored: a stored delta implies a receive pass")
	}
	// storeFile verified these bytes against uuid; expanding them again is
	// tolerated, since the pass's basis cache makes it one delta apply.
	data, err := h.pass.basis.ExpandUnchecked(q, rid)
	if err != nil {
		return fmt.Errorf("handler: seed check: %w", err)
	}
	if seedcheck.IsSeedBytes(data) {
		h.fail(seedRefusedText)
	}
	return nil
}

// storedDelta is a delta a request stored, by name and rid.
type storedDelta struct {
	uuid string
	rid  libfossil.FslID
}

// withheldSeed reports whether rid is a pushed artifact not yet judged, or
// judged a seed, which no reply sends (seedcheck). The set is read once per
// request: a row this request adds is a delta it stored unreadable, which
// nothing can send, and a row it clears only withholds more than needed. An
// inventory reads the set itself (inventoryWithheld).
func (h *handler) withheldSeed(rid libfossil.FslID) (bool, error) {
	if h.withheld == nil {
		set, err := seedcheck.Withheld(h.repo.DB())
		if err != nil {
			return false, err
		}
		h.withheld = set
	}
	return h.withheld[rid], nil
}

// storeFile stores a validated file or cfile card's content and reports the
// handler error text for a failure, "" when the artifact is stored, and late
// when the failure is a late hash mismatch that aborts the request. A hash
// mismatch is fossil's "wrong hash on received artifact"; any other failure
// is its content_put_ex failure, whose g.zErrMsg page_xfer sends.
func (h *handler) storeFile(ctx context.Context, uuid, deltaSrc string, payload []byte,
	storedBlob []byte, vis visibility) (msg string, late bool) {
	if h.pass == nil {
		h.pass = newReceivePass() // one pass per request
	}
	err := storeReceivedFile(ctx, h.repo, h.pass, uuid, deltaSrc, payload, storedBlob, vis,
		nil)
	// A late mismatch names the delta at fault, which this request stored
	// earlier, not the card that made its bytes available.
	var mismatch *hashMismatchError
	if errors.As(err, &mismatch) {
		return wrongHashText(mismatch.uuid), mismatch.late
	}
	if errors.Is(err, errSeed) {
		h.fail(seedRefusedText)
		return "", false
	}
	if err != nil {
		return fmt.Sprintf("storing %s: %v", uuid, err), false
	}
	_, ok := blob.Exists(h.repo.DB(), uuid)
	if h.buggify != nil && h.buggify.Check("handler.handleFile.missingAfterStore", 0.01) {
		ok = false
	}
	if !ok {
		return fmt.Sprintf("blob %s missing after store", uuid), false
	}
	return "", false
}

// wrongHashText is xfer_accept_file's error for content that is not HASH.
func wrongHashText(uuid string) string {
	return "wrong hash on received artifact: " + uuid
}

// malformedFileLine is the framing error of a file or cfile handler.
func malformedFileLine(compressed bool) string {
	if compressed {
		return "malformed cfile line"
	}
	return "malformed file line"
}

// handleReqConfig answers reqconfig NAME with the configuration groups NAME
// selects and the requester may read (config_groups.go). Anything else — a
// bare name, an unknown group, a group the requester lacks the capability
// for — gets no cards and no error, as fossil answers it, so the rest of the
// exchange goes on.
func (h *handler) handleReqConfig(c *xfer.ReqConfigCard) error {
	if c == nil {
		panic("handler.handleReqConfig: c must not be nil")
	}
	if c.Name == configScalarMarker && h.canRead() {
		// zit's request for the sub-manifest settings (§6.3), which fossil
		// matches to no group and answers with nothing.
		cards, err := submanifestConfigCards(h.repo.DB())
		if err != nil {
			return fmt.Errorf("handler: reqconfig %q: %w", c.Name, err)
		}
		h.resp = append(h.resp, cards...)
		return nil
	}
	mask := configRequestMask(c.Name, h.caps, h.canRead(), h.authed)
	if mask == 0 {
		return nil
	}
	cards, err := configGroupCards(h.repo.DB(), mask)
	if err != nil {
		return fmt.Errorf("handler: reqconfig %q: %w", c.Name, err)
	}
	h.resp = append(h.resp, cards...)
	return nil
}

func (h *handler) emitIGots() error {
	if err := h.ensureSparseKeep(); err != nil {
		return err
	}
	position, err := originPosition(h.repo.DB())
	if err != nil {
		return err
	}
	floor, err := h.announceFloor(position)
	if err != nil {
		return err
	}
	// Read after the request's stores, so it names every row they added.
	withheld, err := seedcheck.Withheld(h.repo.DB())
	if err != nil {
		return err
	}
	// Sparse inventories advertise only the request's keep-set; announcing an
	// unseen outside leaf's blobs would create phantoms with no F provenance.
	rows, err := h.repo.DB().Query(`
		SELECT rid,uuid FROM blob WHERE size >= 0
		AND NOT EXISTS(SELECT 1 FROM shun WHERE uuid=blob.uuid)
		AND NOT EXISTS(SELECT 1 FROM private WHERE rid=blob.rid)`,
	)
	if err != nil {
		return fmt.Errorf("handler: listing blobs: %w", err)
	}
	defer rows.Close()

	var uuids []string
	var rids []libfossil.FslID
	for rows.Next() {
		var rid libfossil.FslID
		var uuid string
		if err := rows.Scan(&rid, &uuid); err != nil {
			return err
		}
		// Horizon §6.2.6: what lies at or below the cursor was announced or
		// sent already, in any reply, classified or not.
		if int64(rid) <= floor || !h.keep.keeps(rid) {
			continue
		}
		if withheld[rid] {
			continue
		}
		// remoteHas is populated from client igot cards in handleIGot.
		// Skip if the client already has this blob as public (non-private).
		// If the client has it as private, we still emit the public igot so
		// the client can clear its private status (private→public transition).
		if e, ok := h.remoteHas[uuid]; ok && !e.isPrivate {
			continue
		}
		uuids = append(uuids, uuid)
		rids = append(rids, rid)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// BUGGIFY: 10% chance truncate igot list to test multi-round convergence.
	if h.buggify != nil && h.buggify.Check("handler.emitIGots.truncate", 0.10) && len(uuids) > 1 {
		// The cursor stops short of the first name left unannounced.
		for _, rid := range rids[len(uuids)/2:] {
			position = min(position, int64(rid)-1)
		}
		uuids = uuids[:len(uuids)/2]
	}

	for _, uuid := range uuids {
		h.resp = append(h.resp, &xfer.IGotCard{UUID: uuid})
	}

	if h.syncPrivate {
		unsent, err := h.emitPrivateIGots(floor)
		if err != nil {
			return err
		}
		if unsent > 0 {
			position = min(position, int64(unsent)-1)
		}
	}
	if h.servedCursor != nil {
		h.servedCursor.Values = cursorCard(max(position, 0)).Values
	}
	return nil
}

// announceFloor is the rid at or below which this reply announces nothing:
// the request's cursor, or 0 when it carried none, one beyond position this
// origin cannot have handed out, or one from before the origin's last
// reclassification, which is answered as if it carried none (horizon
// §6.2.6). That reply's cursor moves the client past it.
func (h *handler) announceFloor(position int64) (int64, error) {
	if h.cursor == 0 || h.cursor > position {
		return 0, nil
	}
	at, err := rekeptAt(h.repo.DB())
	if err != nil {
		return 0, err
	}
	if h.cursor < at {
		return 0, nil
	}
	voids, err := h.keep.voids(h.cursor)
	if err != nil || voids {
		return 0, err
	}
	return h.cursor, nil
}

// emitPrivateIGots emits igot cards with IsPrivate=true for all blobs in
// the private table. Only called when the client sent pragma send-private
// and has the 'x' capability.
//
// Shunned private artifacts are still announced: fossil's send_private
// (src/xfer.c) selects from the private table with no shun filter, unlike
// the public igot paths. #349 asked for the filter, but matching fossil
// wins; the receiving side drops a shunned file on arrival either way.
//
// Nothing at or below floor is announced (horizon §6.2.6). It returns the
// lowest rid a truncation left unannounced, 0 when none.
func (h *handler) emitPrivateIGots(floor int64) (libfossil.FslID, error) {
	if err := h.ensureSparseKeep(); err != nil {
		return 0, err
	}
	withheld, err := seedcheck.Withheld(h.repo.DB())
	if err != nil {
		return 0, err
	}
	rows, err := h.repo.DB().Query(
		"SELECT b.rid,b.uuid FROM private p JOIN blob b ON p.rid=b.rid WHERE b.size >= 0",
	)
	if err != nil {
		return 0, fmt.Errorf("handler: listing private blobs: %w", err)
	}
	defer rows.Close()

	var uuids []string
	var rids []libfossil.FslID
	for rows.Next() {
		var rid libfossil.FslID
		var uuid string
		if err := rows.Scan(&rid, &uuid); err != nil {
			return 0, err
		}
		if int64(rid) <= floor || !h.keep.keeps(rid) {
			continue
		}
		if withheld[rid] {
			continue
		}
		// Skip if the client already has this blob as private.
		// If the client has it as public, we still emit the private igot so
		// the client can update its private status (public→private transition).
		if e, ok := h.remoteHas[uuid]; ok && e.isPrivate {
			continue
		}
		uuids = append(uuids, uuid)
		rids = append(rids, rid)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	// BUGGIFY: 10% chance truncate private igot list to test multi-round convergence.
	var unsent libfossil.FslID
	if h.buggify != nil && h.buggify.Check("handler.emitPrivateIGots.truncate", 0.10) && len(uuids) > 1 {
		for _, rid := range rids[len(uuids)/2:] {
			if unsent == 0 || rid < unsent {
				unsent = rid
			}
		}
		uuids = uuids[:len(uuids)/2]
	}

	for _, uuid := range uuids {
		h.resp = append(h.resp, &xfer.IGotCard{UUID: uuid, IsPrivate: true})
	}
	return unsent, nil
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

	bracketed := false
	for rows.Next() {
		var uuid string
		if err := rows.Scan(&uuid); err != nil {
			return err
		}
		// Clusters are always public (query excludes private table).
		if e, ok := h.remoteHas[uuid]; ok && !e.isPrivate {
			continue
		}
		if !bracketed {
			h.resp = append(h.resp, &xfer.CommentCard{Text: "sending-clusters"})
			bracketed = true
		}
		h.resp = append(h.resp, &xfer.IGotCard{UUID: uuid})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// fossil's send_all_clusters brackets the cluster igots with comments,
	// and sends neither when there is no cluster to announce (§5.3).
	if bracketed {
		h.resp = append(h.resp, &xfer.CommentCard{Text: "end-of-clusters"})
	}
	return nil
}

// handleConfigPush answers a pushed config card. fossil's page_xfer refuses
// one from a requester without Admin with a reset, this error and a break
// (§9.3). It applies an Admin's through configure_receive, which this server
// does not implement, so it refuses that too, naming why, rather than drop
// the configuration while the push appears to succeed.
func (h *handler) handleConfigPush() {
	if !auth.IsAdmin(h.caps) {
		h.breakLoop("not authorized to push configuration")
		return
	}
	h.breakLoop("configuration push is not supported by this server")
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
		// A sparse walk sends its one cookie with the round's progress.
		if h.keep == nil || h.keep.walk == nil {
			h.resp = append(h.resp, &xfer.CookieCard{Value: fmt.Sprintf("%s%d", cloneCapPrefix, maxRid)})
		}
	}
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
	round := h.newCloneBatch()
	if err := h.expandCloneRuns(round); err != nil {
		return err
	}
	if err := h.walkCloneRows(round); err != nil {
		return err
	}
	if round.truncate && round.sentTop > 1 {
		h.truncateCloneBatch(round)
	}
	// The cursor must strictly advance or signal exhaustion. Catching a stall
	// here names the round that caused it; letting it through only surfaces
	// later as an opaque MaxRounds failure on the client. A check-in whose
	// walk outgrew the round stays the cursor, its progress in the cookie.
	stalled := round.nextRID != 0 && round.nextRID <= cursorAtEntry
	if stalled && !(round.pending.set && round.nextRID == cursorAtEntry && round.count > 0) {
		return fmt.Errorf(
			"handler: clone cursor did not advance: next %d, cursor %d", round.nextRID, cursorAtEntry)
	}
	h.emitCloneResume(round)
	// SeqNo 0 signals completion so the client stops requesting.
	h.resp = append(h.resp, &xfer.CloneSeqNoCard{SeqNo: round.nextRID})
	return h.valueCloneCursor(round)
}

// valueCloneCursor values the reply's horizon-cursor with the position this
// round served the clone sequence through (horizon §6.2.6): every rid below
// the sequence it hands on, or the origin's whole position once it hands on
// none, bounded by the snapshot the clone opened.
func (h *handler) valueCloneCursor(round *cloneBatch) error {
	if h.servedCursor == nil {
		return nil
	}
	position, err := originPosition(h.repo.DB())
	if err != nil {
		return err
	}
	position = min(position, int64(h.cloneSnapMax))
	if round.nextRID != 0 {
		position = min(position, int64(round.nextRID)-1)
	}
	h.servedCursor.Values = cursorCard(max(position, 0)).Values
	return nil
}

// cloneBatch is one clone reply's batch state.
type cloneBatch struct {
	budget   int
	truncate bool
	// sent records every rid emitted this round, including delta sources a
	// dependent's chain pulls in ahead of their ascending position (issue
	// #141). It caps re-emission at once per round the way canonical fossil's
	// per-request "sent" bag does; across rounds the ascending cursor resumes,
	// and a source pulled in early in a prior round may be re-sent, which the
	// receiver dedupes by hash. A sparse walk adds the chains its cookie
	// carried (runs).
	sent map[int]bool
	// runs are a sparse walk's delta chains sent ahead of the rows or walks
	// that reach them, carried round to round in its cookie: the walk
	// restarts at its cursor each round and would otherwise send each such
	// base again there (issue #465). reached holds the bases this round's
	// walk found sent already, which leave their runs.
	runs    [][]int
	reached map[int]bool
	// count is the card total; sentTop counts top-level artifacts sent this
	// round. Each carries a whole delta chain, so it is the quantity the
	// truncate liveness guard reasons about.
	count, sentTop int
	// bytesSent accumulates the wire cost of the cards emitted this round,
	// mirroring canonical's `while( mxSend > blob_size(pOut) )`: the budget is
	// tested before each artifact and the artifact (with its chain) that
	// crosses it is still sent whole. That ordering is what guarantees forward
	// progress when a single artifact is larger than the entire budget — a
	// post-send test would emit nothing and stall the cursor.
	bytesSent  int
	lastTopRID int
	// nextRID is the cursor reported back to the client: the rid of the first
	// blob this batch did not send, or 0 once the snapshot is exhausted.
	nextRID int
	// entry is the cursor the request named.
	entry int
	// resume is the walk progress the request's cookie carried; pending is
	// the progress this reply hands on (§6.2).
	resume, pending cloneResume
}

func (h *handler) newCloneBatch() *cloneBatch {
	r := &cloneBatch{budget: DefaultCloneBatchBytes, sent: map[int]bool{}, reached: map[int]bool{}, entry: h.cloneSeq}
	// BUGGIFY: 10% chance reduce the budget to one byte to stress pagination.
	// The budget is consulted before each artifact and never suppresses the
	// first one, so this yields exactly one artifact per round.
	if h.buggify != nil && h.buggify.Check("handler.emitCloneBatch.smallBatch", 0.10) {
		r.budget = 1
	}
	// A sparse walk sends rows out of rid order, so dropping the round's last
	// card cannot name the cursor that owes it; truncation stays dense-only.
	r.truncate = h.buggify != nil && h.buggify.Check("clone.emitCloneBatch.truncate", 0.10) &&
		(h.keep == nil || h.keep.walk == nil)
	if h.cloneResume.aware && h.cloneResume.row == h.cloneSeq {
		r.resume = h.cloneResume
	}
	return r
}

func (h *handler) walkCloneRows(round *cloneBatch) error {
	rows, err := h.repo.DB().Query(
		`SELECT rid, uuid, size, substr(content, 1, 4) FROM blob
		  WHERE rid >= ? AND rid <= ? AND size >= 0 ORDER BY rid`,
		h.cloneSeq, h.cloneSnapMax,
	)
	if err != nil {
		return fmt.Errorf("handler: clone batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var rid int
		var uuid string
		var fullSize int
		var prefix []byte
		if err := rows.Scan(&rid, &uuid, &fullSize, &prefix); err != nil {
			return err
		}
		more, err := h.cloneRow(round, cloneCandidate{
			rid: rid, uuid: uuid, fullSize: fullSize, storedSize: storedSize(prefix, fullSize),
		})
		if err != nil {
			return err
		}
		if !more {
			break
		}
	}
	return rows.Err()
}

// excludedCost is what a round is charged for a row it walks past (§6.2,
// SC-6.2-1): what a dense round charges to send it as stored. Charging the
// expanded size instead made a sparse round of fossil-scm.org's repository
// cover a hundredth of a dense round's rows, and the clone outlasted
// MaxRounds.
func excludedCost(c cloneCandidate) int {
	return cloneCardOverheadBytes + c.storedSize
}

// cloneCandidate is one row a clone round may send. storedSize is the size
// of its stored bytes once inflated: its delta when it is stored as one.
type cloneCandidate struct {
	rid        int
	uuid       string
	fullSize   int
	storedSize int
}

// storedSize reads a blob's inflated stored size from the four-byte
// big-endian prefix fossil's blob_compress writes ahead of the zlib stream,
// so a row can be charged without inflating it. A content too short to
// carry the prefix is charged its full size.
func storedSize(prefix []byte, fullSize int) int {
	if len(prefix) < 4 {
		return max(fullSize, 0)
	}
	return int(binary.BigEndian.Uint32(prefix))
}

// cloneRow offers one candidate row to the round. It reports false once the
// round is full, having set the cursor the client resumes from.
func (h *handler) cloneRow(round *cloneBatch, c cloneCandidate) (bool, error) {
	rid, uuid, fullSize := c.rid, c.uuid, c.fullSize
	// A higher-rid delta source that a dependent's chain already pulled in
	// this round is on the wire; skip it before it can consume budget or
	// advance the cursor a second time. A sparse walk still owes the walk of
	// a check-in that went out as another manifest's delta source.
	walk := h.keep != nil && h.keep.walk != nil
	if round.sent[rid] && !walk {
		return true, nil
	}
	if !walk {
		earlier, err := h.sentEarlier(rid)
		if err != nil || earlier {
			return err == nil, err
		}
	}
	shunned, err := shun.IsShunned(h.repo.DB(), uuid)
	if err != nil || shunned {
		return err == nil, err
	}
	if withheld, err := h.withheldSeed(libfossil.FslID(rid)); err != nil || withheld {
		return err == nil, err
	}
	isPriv := content.IsPrivate(h.repo.DB(), int64(rid))
	if isPriv && !h.syncPrivate {
		return true, nil // skip private blob, don't count toward batch
	}
	if round.bytesSent >= round.budget {
		round.nextRID = rid
		return false, nil
	}
	if walk {
		return h.cloneSparseRow(round, c)
	}
	// §6.2: a cone-excluded row is charged as a dense round would charge
	// it, at the size of its stored bytes (excludedCost).
	if !h.keep.keeps(libfossil.FslID(rid)) {
		round.bytesSent += excludedCost(c)
		return true, nil
	}
	sent, err := h.cloneSend(round, rid, uuid, fullSize)
	if err != nil || sent {
		return sent, err
	}
	round.nextRID = rid
	return false, nil
}

// errUnexpandableChain reports a clone chain that ends at a phantom.
var errUnexpandableChain = errors.New("clone chain ends at a phantom")

// cloneSend emits rid with its delta chain unless the chain would carry the
// round past the client's decode bound; it reports whether rid was sent.
func (h *handler) cloneSend(round *cloneBatch, rid int, uuid string, fullSize int) (bool, error) {
	// §7.2/#141: emit the artifact source-first. A row stored as a delta
	// goes out as a delta card preceded by the source it names, reclaiming
	// the delta-transmission bandwidth a plain (expanded) cfile gives up.
	// content.Deltify deltifies the OLDER artifact against the NEWER one,
	// so a delta's source almost always has a *greater* rid than the delta
	// itself and would not yet have been sent under this loop's ascending
	// order; buildCloneArtifact walks the chain and emits each source ahead
	// of its dependent so no delta ever forward-references a card that has
	// not arrived. The delta rides a cfile that names its source, as in
	// canonical's send_compressed_file.
	cards, rids, cost, err := h.buildCloneArtifact(rid, uuid, fullSize, round.sent)
	if errors.Is(err, errUnexpandableChain) {
		return true, nil // handled as a cone-excluded row is: sent as nothing
	}
	if err != nil {
		return false, err
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
	if round.count > 0 && round.bytesSent+cost > xfer.MaxDecompressedBytes {
		return false, nil
	}
	h.resp = append(h.resp, cards...)
	for _, r := range rids {
		round.sent[r] = true
	}
	if h.keep != nil && h.keep.walk != nil {
		round.noteRun(rids, rid)
	}
	h.filesSent += len(rids)
	round.lastTopRID = rid
	round.count += len(rids)
	round.sentTop++
	round.bytesSent += cost
	return true, nil
}

// truncateCloneBatch is BUGGIFY's truncate: it removes the last cfile card
// to simulate an incomplete batch. The caller's `sentTop > 1` is a liveness
// constraint, not just a chaos-injection guard. The removed card is the
// deepest dependent of the last artifact's chain (emitted last in
// source-first order, so nothing else on the wire depends on it), and the
// cursor is set to that artifact's top-level rid. With more than one
// top-level artifact sent this round that rid strictly exceeds the cursor we
// entered with, so the client re-requests from it rather than looping on the
// same batch until MaxRounds.
func (h *handler) truncateCloneBatch(round *cloneBatch) {
	for i := len(h.resp) - 1; i >= 0; i-- {
		if _, ok := h.resp[i].(*xfer.CFileCard); ok {
			h.resp = append(h.resp[:i], h.resp[i+1:]...)
			h.filesSent--
			// The dropped card held lastTopRID, so that rid becomes the
			// next one owed to the client rather than being skipped.
			round.nextRID = round.lastTopRID
			return
		}
	}
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
	rid      int
	uuid     string
	usize    int
	priv     bool
	srcUUID  string
	expanded bool // stored as a delta but sent as full content
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
	shunned, err := shun.IsShunned(h.repo.DB(), row.uuid)
	if err != nil {
		return cloneBlobRow{}, false, err
	}
	if shunned {
		return cloneBlobRow{}, false, nil
	}
	if withheld, err := h.withheldSeed(libfossil.FslID(rid)); err != nil || withheld {
		return cloneBlobRow{}, false, err
	}
	row.priv = content.IsPrivate(h.repo.DB(), int64(rid))
	return row, true, nil
}

// storedContent returns rid's content as the blob table stores it: the
// on-disk blob format, compressed, of the full artifact or of its delta.
func (h *handler) storedContent(rid int) ([]byte, error) {
	var stored []byte
	err := h.repo.DB().QueryRow("SELECT content FROM blob WHERE rid = ?", rid).Scan(&stored)
	if err != nil {
		return nil, fmt.Errorf("handler: stored content of rid %d: %w", rid, err)
	}
	return stored, nil
}

// walkCloneChain follows rid's delta chain toward its full-content tip and
// returns the artifacts to emit, ordered DEPENDENT-FIRST (rid itself first).
// The walk stops at the first source that is already available to the receiver
// (emitted earlier this round) or that cannot be forwarded this clone —
// outside the snapshot bound, a phantom, a private source under a public
// clone, or a tree the §7.2 gate withholds from this client — and tags the
// artifact below such a source for full-content emission so it never depends
// on a card that will not arrive.
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
		info, forwardable, err := h.forwardableSource(int(src))
		if err != nil {
			return nil, err
		}
		if !forwardable {
			if info.size < 0 && info.uuid != "" {
				// The chain ends at a phantom, so no artifact on it can be
				// expanded and the clone gets none of them. fossil's
				// send_compressed_file would send the delta naming the
				// phantom, but a fossil client then rejects the whole clone
				// for its unresolved deltas. A later pull fetches the chain
				// once its source arrives.
				return nil, errUnexpandableChain
			}
			m.expanded = true
			return append(chain, m), nil // source unsendable: emit curRID full
		}
		m.srcUUID = info.uuid
		chain = append(chain, m)
		if sent[int(src)] {
			return chain, nil // source already on the wire this round
		}
		earlier, err := h.sentEarlier(int(src))
		if err != nil {
			return nil, err
		}
		if earlier {
			return chain, nil // the client holds the source from an earlier round
		}
		curRID, curUUID, curUSize, curPriv = int(src), info.uuid, info.size, info.priv
	}
	// Walk cap hit: force the deepest artifact to full content so its delta
	// does not reference a source we never emitted.
	chain[len(chain)-1].srcUUID = ""
	chain[len(chain)-1].expanded = true
	return chain, nil
}

// forwardableSource reports whether a chain may name src as a delta source
// this clone sends: a held row inside the snapshot bound that the store cone
// keeps as a source, public unless the clone takes private content, and no
// tree the §7.2 gate withholds from this client.
func (h *handler) forwardableSource(src int) (cloneBlobRow, bool, error) {
	if f, ok := h.sources[src]; ok {
		return f.info, f.ok, nil
	}
	info, ok, err := h.decideSource(src)
	if err != nil {
		return cloneBlobRow{}, false, err
	}
	if h.sources == nil {
		h.sources = make(map[int]forwardable)
	}
	h.sources[src] = forwardable{info: info, ok: ok}
	return info, ok, nil
}

// forwardable is a memoized forwardableSource verdict.
type forwardable struct {
	info cloneBlobRow
	ok   bool
}

// decideSource is forwardableSource without the memo.
func (h *handler) decideSource(src int) (cloneBlobRow, bool, error) {
	info, ok, err := h.loadCloneBlob(src)
	if err != nil || !ok {
		return cloneBlobRow{}, false, err
	}
	if info.size < 0 || src > h.cloneSnapMax || !h.keep.sourceKept(src) ||
		(info.priv && !h.syncPrivate) {
		return info, false, nil
	}
	// Checked before sent: a withheld source is never on the wire, whatever
	// an earlier chain recorded.
	barred, err := h.smGate.barred(h.repo.DB(), h.cache, libfossil.FslID(src), h.peerReadsTrees)
	if err != nil {
		return cloneBlobRow{}, false, err
	}
	return info, !barred, nil
}

// maxSentEarlierRows bounds the rows sentEarlier examines below one row.
const maxSentEarlierRows = 256

// sentEarlier reports whether rid went out in an earlier round of this clone.
// Rounds take rows in rid order and send each with its delta chain, source
// first, through every source the clone forwards; so rid went out when a row
// below the cursor that its round sent reaches rid through forwardable
// sources. fossil deltifies each version of a file against the next, so the
// versions' rows come in rid order, each a delta on a later one: the oldest
// row's round sends the whole history, and without this each later version's
// row sent the rest of it again in its own round. A dense clone of the Fossil
// repository carried its histories about five times over, each copy verified
// again by the client.
//
// The search walks down the delta table from rid, through rows that are
// forwardable sources themselves, examining at most maxSentEarlierRows rows,
// far short of maxCloneChainWalk. Not finding such a row sends rid again,
// which is only redundant. A row below the cursor that was a phantom or
// private when its round passed it, and has since been filled or published,
// was not sent but is taken as sent; rows below the cursor are not sent again
// in any case.
func (h *handler) sentEarlier(rid int) (bool, error) {
	if h.cloneSeq <= 1 {
		return false, nil
	}
	frontier := []int{rid}
	examined := 0
	for len(frontier) > 0 {
		var next []int
		for _, parent := range frontier {
			kids, err := childRows(h.repo.DB(), libfossil.FslID(parent), nil)
			if err != nil {
				return false, err
			}
			// rid went out only as a source; asked once it is one.
			if parent == rid && len(kids) > 0 {
				if _, ok, err := h.forwardableSource(rid); err != nil || !ok {
					return false, err
				}
			}
			for _, kid := range kids {
				if examined++; examined > maxSentEarlierRows {
					return false, nil
				}
				sent, err := h.sentAsRow(int(kid))
				if err != nil || sent {
					return sent, err
				}
				_, ok, err := h.forwardableSource(int(kid))
				if err != nil {
					return false, err
				}
				if ok {
					next = append(next, int(kid))
				}
			}
		}
		frontier = next
	}
	return false, nil
}

// sentAsRow reports whether rid is a row an earlier round of this clone sent
// as itself: below the cursor, held, and not one cloneRow or
// buildCloneArtifact withholds (shunned, private to a public clone, outside
// the store cone, a refused SHA3 name or a withheld tree).
func (h *handler) sentAsRow(rid int) (bool, error) {
	if rid >= h.cloneSeq || rid > h.cloneSnapMax || !h.keep.keeps(libfossil.FslID(rid)) {
		return false, nil
	}
	info, ok, err := h.loadCloneBlob(rid)
	if err != nil || !ok || info.size < 0 || (info.priv && !h.syncPrivate) ||
		sha3Barred(info.uuid, h.peerVersion) {
		return false, err
	}
	barred, err := h.smGate.barred(h.repo.DB(), h.cache, libfossil.FslID(rid), h.peerReadsTrees)
	return err == nil && !barred, err
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
		rids = append(rids, m.rid)
		// §7.6, Appendix A.2: the refusal takes a SHA3 artifact's place.
		// Not sent, so not recorded as sent, as a withheld tree below.
		if sha3Barred(m.uuid, h.peerVersion) {
			cards = append(cards, sha3Refusal())
			rids = rids[:len(rids)-1]
			continue
		}
		// draft-fossil-submanifests-00 §7.2: so does a tree's, for a client
		// that did not send send-submanifests. walkCloneChain never names a
		// withheld tree as a source, so a dependent stored as a delta of one
		// goes out whole.
		barred, err := h.smGate.barred(h.repo.DB(), h.cache, libfossil.FslID(m.rid), h.peerReadsTrees)
		if err != nil {
			return nil, nil, 0, err
		}
		if barred {
			if !h.smRefused {
				cards = append(cards, submanifestRefusal())
				h.smRefused = true
			}
			// Not sent, so not recorded as sent: a later chain must never
			// take it as a source already on the wire.
			rids = rids[:len(rids)-1]
			continue
		}
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
		// Every clone artifact rides a cfile, a delta naming its source, as
		// canonical's send_compressed_file does for clone version 3 and as the
		// draft requires (§7.1, §8.2). USIZE is the full artifact's size even
		// for a delta, as fossil sends blob.size.
		// An artifact sent as it is stored goes out as its stored bytes,
		// as send_compressed_file sends the blob table's content (§7.2).
		var stored []byte
		if !m.expanded {
			if stored, err = h.storedContent(m.rid); err != nil {
				return nil, nil, 0, err
			}
		}
		cards = append(cards, &xfer.CFileCard{
			UUID: m.uuid, DeltaSrc: m.srcUUID, USize: m.usize, Content: data,
			StoredBlob: stored,
		})
		cost += cloneCardOverheadBytes + len(data)
	}
	return cards, rids, cost, nil
}
