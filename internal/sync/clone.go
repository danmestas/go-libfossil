package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/clearsign"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/seedcheck"
	"github.com/danmestas/go-libfossil/internal/shun"
	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/simio"
)

// phantomStallSampleSize caps how many missing UUIDs PhantomStallError
// carries, so a repository with thousands of phantoms doesn't produce an
// unreadable error message.
const phantomStallSampleSize = 10

// processResponseCancelCheckStride is how many cards processResponse handles
// between context-cancellation checks. One clone batch is processed card by
// card with no network wait in between, so without an inner check a cancelled
// context would not be observed until the next round begins; the check is
// batched to avoid a channel read per card.
const processResponseCancelCheckStride = 256

// cloneAvailableDeltaSourceLimit bounds the positive-only availability memo
// used while processing received delta cards. Once full, correctness falls
// back to querying the repository for subsequent sources.
const cloneAvailableDeltaSourceLimit = 4096

// PhantomStallError reports a clone that stopped making progress while
// phantom blobs — content referenced but never received — were still
// outstanding. It is returned only when CloneOpts.PullPhantoms is set and
// phantoms remain after the follow-up pull; a default Clone ends at
// clone_seqno 0 (sync-protocol-00 Section 8.2) and does not look for them.
// Count is programmatically reachable so a caller can act on it without
// parsing the message.
type PhantomStallError struct {
	Count  int      // phantom UUIDs still outstanding when the clone stopped
	Sample []string // up to phantomStallSampleSize lexicographically-smallest of those UUIDs, for diagnosis
}

func (e *PhantomStallError) Error() string {
	return fmt.Sprintf("sync.Clone: stalled with %d phantom artifact(s) outstanding, e.g. %s",
		e.Count, strings.Join(e.Sample, ", "))
}

// newPhantomStallError builds a PhantomStallError from the session's current
// phantom set. Called only once that set is known to be non-empty.
func newPhantomStallError(phantoms map[string]bool) *PhantomStallError {
	if len(phantoms) == 0 {
		panic("sync.newPhantomStallError: phantoms must not be empty")
	}
	all := make([]string, 0, len(phantoms))
	for uuid := range phantoms {
		all = append(all, uuid)
	}
	sort.Strings(all)
	if len(all) > phantomStallSampleSize {
		all = all[:phantomStallSampleSize]
	}
	return &PhantomStallError{Count: len(phantoms), Sample: all}
}

// cloneSession holds the mutable state of a running clone.
type cloneSession struct {
	sparseReceived        map[string]bool
	repo                  *repo.Repo
	env                   *simio.Env
	opts                  CloneOpts
	result                CloneResult
	linker                *manifest.ReceiveLinker
	phantoms              map[string]bool
	availableDeltaSources map[string]struct{}
	seqno                 int
	projectCode           string
	serverCode            string
	roundRecvd            int      // file/cfile cards the last reply delivered
	unjudged              unjudged // deltas stored unreadable, counted once judged
	roundFrom             int      // the clone sequence the last request named
	roundPush             bool     // the last reply carried a push card
	coned                 bool     // requests carry the store cone (§6.1)
	structural            structuralClient
	structuralDecided     bool        // a reply carrying the clone sequence has decided the depth (horizon §6.2.5)
	named                 *namedClone // a horizon asked for by name (horizon §6.2.7); nil otherwise
	peerCursor            int64       // the last cursor a reply handed out (horizon §6.2.6)
	// smConfig stages the sub-manifest settings one reply's config cards
	// carry, applied together once the reply ends (§6.3).
	smConfig  stagedSubmanifestConfig
	cleanup   bool   // the next request is an operation-less cleanup
	refused   bool   // a no-progress refusal permits only the required cleanup
	serverErr string // a refusal, error after round 0, or unreadable card fails the clone
	cookie    string // server-issued snapshot bound; echoed every round so the
	// server can scope its blob query to the rid range that existed when the
	// clone session opened. See cloneCapPrefix in handler.go.
	obs Observer
	// pass is the current round's verification pass (§7.7).
	pass *receivePass
	// basis is every round's pass's basis cache. A chain sent in one round
	// is sent again, source first, with a later row of it in a later round,
	// so a cache per round rebuilt each such chain from its root again.
	basis *content.Cache
}

// Clone performs a full repository clone from a remote Fossil server.
// It creates a new repository at path, runs the clone protocol until
// convergence, and returns the opened repo and a result summary.
// On error, the partially-created repo file is removed. Artifacts the clone
// rounds leave missing stay phantoms unless opts.PullPhantoms is set.
func Clone(ctx context.Context, path string, t Transport, opts CloneOpts) (r *repo.Repo, result *CloneResult, err error) {
	if path == "" {
		panic("sync.Clone: path must not be empty")
	}
	if t == nil {
		panic("sync.Clone: t must not be nil")
	}

	env, storage := cloneEnv(opts)
	named, err := newNamedClone(opts)
	if err != nil {
		return nil, nil, err
	}

	// Path must not already exist.
	if _, statErr := storage.Stat(path); statErr == nil {
		return nil, nil, fmt.Errorf("sync.Clone: file already exists: %s", path)
	}

	user := opts.User
	if user == "" {
		user = "setup"
	}

	// Create the repository.
	r, err = repo.CreateWithEnv(path, user, env, "")
	if err != nil {
		return nil, nil, fmt.Errorf("sync.Clone: create repo: %w", err)
	}

	// Cleanup on error: close repo and remove the file.
	defer func() {
		if err != nil {
			r.Close()
			if rmErr := storage.Remove(path); rmErr != nil {
				fmt.Fprintf(os.Stderr, "sync.Clone: cleanup failed: %v\n", rmErr)
			}
			r = nil
		}
	}()

	if err = clearInheritedConfig(r); err != nil {
		return
	}
	storeCone := opts.StoreCone
	if named != nil {
		storeCone = named.pathCone() // horizon §6.1: C is not known until resolved
	}
	if err = content.SetStoreCone(r.DB(), storeCone); err != nil {
		return
	}
	if err = keepSeedKey(r, opts.SeedKey); err != nil {
		return
	}
	structural, structuralErr := newStructuralClient(r.DB())
	if structuralErr != nil {
		err = fmt.Errorf("sync.Clone: %w", structuralErr)
		return
	}
	structural.active = structural.active || named != nil && named.structural()

	linker, linkerErr := manifest.NewReceiveLinker(r)
	if linkerErr != nil {
		err = fmt.Errorf("sync.Clone: create receive linker: %w", linkerErr)
		return
	}

	// The clone is one receipt, credited to its admin user. The Repo it
	// returns does not carry it: what the caller stores later is its own.
	recv, recvErr := withClientReceipt(r, user, env.Clock)
	if recvErr != nil {
		err = fmt.Errorf("sync.Clone: %w", recvErr)
		return
	}

	cs := &cloneSession{
		repo:                  recv,
		env:                   env,
		opts:                  opts,
		linker:                linker,
		phantoms:              make(map[string]bool),
		availableDeltaSources: make(map[string]struct{}),
		basis:                 content.NewUncheckedCache(receiveBasisBytes),
		seqno:                 1,
		structural:            structural,
		named:                 named,
	}
	if opts.StoreCone != nil || named != nil {
		cs.sparseReceived = map[string]bool{}
	}
	cs.obs = resolveObserver(opts.Observer)

	result, err = cs.complete(ctx, t)
	if err != nil {
		return
	}
	return r, result, nil
}

// cloneEnv is the environment a clone runs in and the storage its file
// lives in.
func cloneEnv(opts CloneOpts) (*simio.Env, simio.Storage) {
	env := opts.Env
	if env == nil {
		env = simio.RealEnv()
	}
	storage := env.Storage
	if storage == nil {
		storage = simio.OSStorage{}
	}
	return env, storage
}

// complete runs the clone session, links what it received and, when the
// caller opted in, fetches any artifact it left missing. The result is
// meaningful on error too: callers inspecting it (e.g. BlobsRecvd)
// alongside a *PhantomStallError shouldn't see it dropped to nil.
func (cs *cloneSession) complete(ctx context.Context, t Transport) (*CloneResult, error) {
	cloneResult, err := cs.run(ctx, t)
	if err != nil {
		return cloneResult, err
	}

	// The receive linker has already handled artifacts as their verified
	// content arrived. Finalize retries deferred candidates and runs the
	// bounded residual sweep with that same cache.
	_, finalizeErr := cs.linker.Finalize(ctx)
	cloneResult.ArtifactsLinked = cs.linker.Stats().Linked
	if finalizeErr != nil {
		return cloneResult, fmt.Errorf(
			"sync.Clone: finalize receive-linker crosslink: %w", finalizeErr)
	}
	// A delta its source sent without the base is kept, but not announced
	// or served while it cannot be expanded (horizon §5.2.3).
	if err := cs.repo.WithTx(func(tx *db.Tx) error { return seedcheck.WithholdUnreadable(tx) }); err != nil {
		return cloneResult, fmt.Errorf("sync.Clone: %w", err)
	}
	if err := cs.settleNamed(); err != nil {
		return cloneResult, err
	}
	if cs.sparseReceived != nil {
		if err := cs.repo.WithTx(func(tx *db.Tx) error {
			var err error
			cs.result.Elided, err = filterSparseReceived(ctx, tx, cs.sparseReceived, nil)
			return err
		}); err != nil {
			return cloneResult, err
		}
	}
	// Horizon §5.2.3: the seed applies to what the linker's finalize linked.
	fell, err := cs.structural.apply(cs.repo)
	if err != nil {
		return cloneResult, fmt.Errorf("sync.Clone: horizon seed: %w", err)
	}
	cs.result.HorizonFallback = cs.result.HorizonFallback || fell
	remedy, err := fallbackRemedy(cs.repo.DB(), cs.result.HorizonFallback, cs.result.HorizonHistoryURL)
	if err != nil {
		return cloneResult, fmt.Errorf("sync.Clone: %w", err)
	}
	if remedy != "" {
		cs.result.Messages = append(cs.result.Messages, remedy)
	}
	fillErr := cs.fillPhantoms(ctx, t)
	// Horizon §5.2.3: a delta stored unreadable is counted once the rounds,
	// the linker and any phantom pull have judged it, and only if it was not
	// a seed.
	kept, err := cs.unjudged.settle(cs.repo.DB())
	if err != nil {
		return cloneResult, fmt.Errorf("sync.Clone: %w", err)
	}
	cs.result.BlobsRecvd += kept
	if fillErr != nil {
		return cloneResult, fillErr
	}
	return cloneResult, cs.recordCursor()
}

// recordCursor keeps what a converged horizon clone leaves its origin
// knowing (horizon §6.2.6): the cursor of the last reply and the count of
// this store's artifacts, all of which came from the origin, so the first
// request that sends announces only what is appended after. zit records the
// same after its clone (cmd/sync.zig).
func (cs *cloneSession) recordCursor() error {
	if cs.opts.Origin == "" {
		return nil
	}
	c, err := content.StoreCone(cs.repo.DB())
	if err != nil {
		return err
	}
	if c == nil {
		return nil
	}
	if _, ok := c.Horizon(); !ok {
		return nil
	}
	end, err := storePosition(cs.repo.DB())
	if err != nil {
		return err
	}
	return storeCursor(cs.repo.DB(), cs.opts.Origin, c.Digest(), cursorRecord{peer: cs.peerCursor, local: end})
}

// clearInheritedConfig drops the config rows a fresh repository is seeded
// with that a clone must take from its source instead.
func clearInheritedConfig(r *repo.Repo) error {
	if r == nil {
		panic("sync.clearInheritedConfig: nil repo")
	}
	// Clear project-code — the server will provide its own.
	if _, execErr := r.DB().Exec("DELETE FROM config WHERE name='project-code'"); execErr != nil {
		return fmt.Errorf("sync.Clone: clear project-code: %w", execErr)
	}

	// Clear hash-policy for the same reason: a clone adopts its source's
	// policy, not the sha3 a fresh repo is seeded with, so a clone of a SHA1
	// repo must not start naming artifacts SHA3. The protocol carries no
	// config, so dropping the row is what conveys it — an absent row reads
	// as "auto", which derives the algorithm from the artifacts that arrive.
	if _, execErr := r.DB().Exec("DELETE FROM config WHERE name='hash-policy'"); execErr != nil {
		return fmt.Errorf("sync.Clone: clear hash-policy: %w", execErr)
	}

	return nil
}

// keepSeedKey records the origin's key as the clone's horizon-seed-key
// setting before any round, so the first seed is verified against it and
// every later pull and rebuild reads it from there. An empty key keeps none.
func keepSeedKey(r *repo.Repo, key string) error {
	if key == "" {
		return nil
	}
	k, err := clearsign.ParseKey(key)
	if err != nil {
		return fmt.Errorf("sync.Clone: seed key: %w", err)
	}
	if err := r.SetConfig(clearsign.SettingSeedKey, k.String()); err != nil {
		return fmt.Errorf("sync.Clone: %w", err)
	}
	return nil
}

// fillPhantoms fetches artifacts the clone left missing (phantoms) with an
// ordinary pull session, when CloneOpts.PullPhantoms asks for it. The clone
// session cannot ask for them: clone_seqno 0 bars pull from it, and a gimme
// without pull fetches nothing (draft §8.2, SP-8.2-5). That bar is why the
// pull is opt-in: by default the clone ends at clone_seqno 0 with its
// phantoms in place, as fossil(1)'s does. A clone of a consistent
// repository leaves none, and then no pull is made. Any still missing after
// the pull end the clone with a *PhantomStallError, as a caller that asked
// for a complete clone must not get an incomplete one.
func (cs *cloneSession) fillPhantoms(ctx context.Context, t Transport) error {
	if !cs.opts.PullPhantoms {
		return nil
	}
	missing, err := phantomUUIDs(ctx, cs.repo)
	if err != nil {
		return err
	}
	if len(missing) == 0 {
		return nil
	}
	res, err := Sync(ctx, cs.repo, t, SyncOpts{
		Pull:        true,
		ProjectCode: cs.projectCode,
		ServerCode:  cs.serverCode,
		User:        cs.opts.User,
		Password:    cs.opts.Password,
		Env:         cs.env,
		Buggify:     cs.opts.Buggify,
		Observer:    cs.opts.Observer,
	})
	if res != nil {
		cs.result.HorizonFallback = cs.result.HorizonFallback || res.HorizonFallback
		// The phantom pull's own fallback remedy, if it fell back.
		for _, e := range res.Errors {
			if strings.HasPrefix(e, fallbackPrefix) {
				cs.result.Messages = append(cs.result.Messages, e)
			}
		}
	}
	if res != nil {
		cs.result.Rounds += res.Rounds
		cs.result.BlobsRecvd += res.FilesRecvd
	}
	if err != nil {
		return fmt.Errorf("sync.Clone: pull missing artifacts: %w", err)
	}
	if missing, err = phantomUUIDs(ctx, cs.repo); err != nil {
		return err
	}
	if len(missing) > 0 {
		return newPhantomStallError(missing)
	}
	return nil
}

// phantomUUIDs returns the artifacts the repository knows of but lacks.
func phantomUUIDs(ctx context.Context, r *repo.Repo) (map[string]bool, error) {
	a, err := elidedPhantoms(ctx, r.DB())
	if err != nil {
		return nil, err
	}
	// A refused seed's phantom is never asked for (horizon §5.2.3), so it is
	// not missing either.
	refused, err := seedcheck.RefusedUUIDs(r.DB())
	if err != nil {
		return nil, err
	}
	rows, err := r.DB().Query(
		"SELECT b.rid,b.uuid FROM phantom p JOIN blob b ON p.rid = b.rid WHERE b.size < 0")
	if err != nil {
		return nil, fmt.Errorf("sync.Clone: query phantoms: %w", err)
	}
	missing := make(map[string]bool)
	for rows.Next() {
		var rid libfossil.FslID
		var uuid string
		if err := rows.Scan(&rid, &uuid); err != nil {
			return nil, errors.Join(fmt.Errorf("sync.Clone: scan phantom: %w", err), rows.Close())
		}
		if refused[uuid] || a != nil && a.Class(rid) == content.Elided {
			continue
		}
		missing[uuid] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("sync.Clone: read phantoms: %w", err)
	}
	return missing, nil
}

// run executes the clone loop.
func (cs *cloneSession) run(ctx context.Context, t Transport) (*CloneResult, error) {
	ctx = cs.obs.Started(ctx, SessionStart{
		Operation: "clone",
		Pull:      true,
	})

	for cycle := 0; ; cycle++ {
		select {
		case <-ctx.Done():
			cs.obs.Completed(ctx, sessionEndFromClone(&cs.result), ctx.Err())
			return &cs.result, ctx.Err()
		default:
		}
		if cycle >= MaxRounds {
			err := cs.roundCapError()
			cs.obs.Completed(ctx, sessionEndFromClone(&cs.result), err)
			return &cs.result, err
		}

		roundCtx := cs.obs.RoundStarted(ctx, cycle)

		req, err := cs.buildRequest(cycle)
		if err != nil {
			cs.obs.RoundCompleted(roundCtx, cycle, RoundStats{})
			cs.obs.Completed(ctx, sessionEndFromClone(&cs.result), err)
			return &cs.result, fmt.Errorf("sync.Clone: buildRequest round %d: %w", cycle, err)
		}

		resp, err := exchangeWithRetry(ctx, t, req, cs.env.Clock, cs.obs)
		if err != nil {
			cs.obs.RoundCompleted(roundCtx, cycle, RoundStats{})
			cs.obs.Completed(ctx, sessionEndFromClone(&cs.result), err)
			return &cs.result, fmt.Errorf("sync.Clone: exchange round %d: %w", cycle, err)
		}
		notePeer(cs.repo, t)

		recvdBefore := cs.result.BlobsRecvd

		err = cs.processResponse(ctx, cycle, resp)
		if err != nil {
			cs.obs.RoundCompleted(roundCtx, cycle, RoundStats{FilesReceived: cs.result.BlobsRecvd - recvdBefore})
			cs.obs.Completed(ctx, sessionEndFromClone(&cs.result), err)
			return &cs.result, fmt.Errorf("sync.Clone: process round %d: %w", cycle, err)
		}

		cs.result.Rounds = cycle + 1
		cs.obs.RoundCompleted(roundCtx, cycle, RoundStats{FilesReceived: cs.result.BlobsRecvd - recvdBefore})

		next, nextErr := cs.continueAfter(cycle)
		if cs.serverErr != "" && (nextErr != nil || !next) {
			nextErr = fmt.Errorf("sync.Clone: server error: %s", cs.serverErr)
		}
		if nextErr != nil {
			cs.obs.Completed(ctx, sessionEndFromClone(&cs.result), nextErr)
			return &cs.result, nextErr
		}
		if !next {
			break
		}
	}

	if cs.projectCode == "" {
		err := fmt.Errorf("sync.Clone: server sent no project code")
		cs.obs.Completed(ctx, sessionEndFromClone(&cs.result), err)
		return &cs.result, err
	}
	cs.result.ProjectCode = cs.projectCode
	cs.result.ServerCode = cs.serverCode
	cs.obs.Completed(ctx, sessionEndFromClone(&cs.result), nil)
	return &cs.result, nil
}

// roundCapError is the error a clone that ran MaxRounds rounds fails with:
// the server's own error when it sent one, since a server that errs every
// round is the cause, not the round cap.
func (cs *cloneSession) roundCapError() error {
	if cs.serverErr != "" {
		return fmt.Errorf("sync.Clone: server error: %s", cs.serverErr)
	}
	return fmt.Errorf("sync.Clone: exceeded %d rounds", MaxRounds)
}

// continueAfter decides, after a reply is processed, whether the clone
// session sends another request and of which kind: draft §3.7, which for a
// clone continues when the round was the first, or stored content (limbs (a)
// and (e); this client sends no private or unversioned cards in a clone, so
// (b) through (d) never hold). The next request reissues `clone 3 NEXT` only
// when the reply carried a push card and the recorded sequence is positive;
// otherwise it is the operation-less cleanup of §8.2, so nothing follows
// clone_seqno 0 but at most one cleanup.
//
// A session that stops while the sequence is still positive did not get the
// whole repository; the draft simply stops, but Clone reports it.
func (cs *cloneSession) continueAfter(cycle int) (bool, error) {
	if cycle < 0 {
		panic("cloneSession.continueAfter: negative cycle")
	}
	// §6.2: a cone-aware server charges the excluded rows it walks past as a
	// dense batch would, so a round spent inside a long excluded run may carry
	// nothing while its cursor still advances.
	advanced := cs.coned && cs.seqno > cs.roundFrom
	if cycle > 0 && cs.roundRecvd == 0 && !advanced {
		if cs.seqno > 0 {
			return false, fmt.Errorf(
				"sync.Clone: server stopped sending at clone sequence %d", cs.seqno)
		}
		return false, nil
	}
	cs.cleanup = !cs.roundPush || cs.seqno <= 0
	return true, nil
}

// sessionEndFromClone builds a SessionEnd from a CloneResult.
func sessionEndFromClone(r *CloneResult) SessionEnd {
	return SessionEnd{
		Operation:   "clone",
		Rounds:      r.Rounds,
		FilesRecvd:  r.BlobsRecvd,
		ProjectCode: r.ProjectCode,
		Errors:      r.Messages,
	}
}

// buildRequest assembles one outbound xfer message for a clone round.
func (cs *cloneSession) buildRequest(cycle int) (*xfer.Message, error) {
	var cards []xfer.Card

	// A clone sends no pragma send-private (CloneOpts has no private mode).
	// Were one added, fossil puts it before client-version in cycle 0
	// (src/xfer.c client_sync), as headerCards does for a sync (#351).
	// Pragma: client-version, then send-submanifests (every round, §7.2)
	cards = append(cards, versionPragma("client-version"), sendSubmanifests())
	sparse, err := localConePragmas(cs.repo.DB())
	if err != nil {
		return nil, err
	}
	cards = append(cards, sparse...)
	named := cs.named.pragmas(cs.cleanup)
	cards = append(cards, named...)
	cs.coned = len(sparse)+len(named) > 0

	// Cookie carries the server-captured clone snapshot bound. Echoing it back
	// every round lets the stateless handler enforce a stable upper rid bound
	// on its batch query, so a hub that is being written to during a clone
	// session can't keep extending the queue and prevent convergence.
	if cs.cookie != "" {
		cards = append(cards, &xfer.CookieCard{Value: cs.cookie})
	}

	// The clone card carries the pagination cursor itself — `clone VERSION
	// SEQNO`, canonical xfer.c:1553. A clone_seqno card must never go out
	// from here: it is server-to-client only, canonical's page_xfer() has no
	// parser for it, and sending one lands in the server's unknown-card
	// branch as `bad command: clone_seqno N` (issue #74). A cleanup request
	// carries no operation card at all (draft §8.2).
	if !cs.cleanup {
		if cs.seqno <= 0 {
			panic("cloneSession.buildRequest: clone reissue after clone_seqno 0")
		}
		version := cs.opts.Version
		if version <= 0 {
			version = 3
		}
		cards = append(cards, &xfer.CloneCard{
			Version:        version,
			SeqNo:          cs.seqno,
			SeqNoIsDecimal: true,
		})
	}

	// The cleanup round is where a clone asks for the sub-manifest settings
	// (draft-fossil-submanifests-00 §6.3), as zit's clone does: once, after
	// the content, with zit's scalar marker. fossil has no group of that
	// name and answers it with nothing, so against fossil the request costs
	// one card and changes nothing.
	if cs.cleanup {
		cards = append(cards, &xfer.ReqConfigCard{Name: configScalarMarker})
	}

	// Login card: skip round 0. On round 1+, only if User is set AND projectCode received.
	if cycle > 0 && cs.opts.User != "" && cs.projectCode != "" {
		loginCard, signed, err := cs.buildLoginCard(cards)
		if err != nil {
			return nil, fmt.Errorf("clone buildLoginCard: %w", err)
		}
		cards = append([]xfer.Card{loginCard}, signed...)
	} else {
		// Every request ends with randomness (draft §3); a login card's
		// signature already covers the one buildLoginCard appends.
		cards = append(cards, randomComment(cs.env.Rand))
	}

	return &xfer.Message{Cards: cards}, nil
}

// buildLoginCard appends a random comment to cards and computes the login card
// over the result. It returns the login card and the card list it signed, which
// the caller must send verbatim behind that login card — the server recomputes
// the nonce from those bytes.
func (cs *cloneSession) buildLoginCard(cards []xfer.Card) (*xfer.LoginCard, []xfer.Card, error) {
	signed := append(cards[:len(cards):len(cards)], randomComment(cs.env.Rand))
	payload, err := encodeCards(signed)
	if err != nil {
		return nil, nil, err
	}
	login := computeLogin(cs.opts.User, cs.opts.Password, cs.projectCode, payload)
	// BUGGIFY: 5% chance corrupt login nonce to test auth failure recovery.
	if cs.opts.Buggify != nil && cs.opts.Buggify.Check("clone.buildRequest.badLogin", 0.05) {
		login.Nonce = "corrupted-nonce"
	}
	return login, signed, nil
}

// processResponse handles all cards in a server response for a clone round.
// cycle is that round's 0-based index; round 0 is the one sent before the
// project code — and so the login card — is known.
//
// The round is one verification pass (§7.7) and runs in one write
// transaction, as fossil's client_sync runs each round: a file or cfile
// whose content does not match its name, found before storage or late, rolls
// the round back, prefix included (§10.1's fail-fast path). Any other card
// error commits the prefix before it and is returned.
func (cs *cloneSession) processResponse(ctx context.Context, cycle int, msg *xfer.Message) error {
	if msg == nil {
		panic("sync.Clone.processResponse: msg must not be nil")
	}
	filesRecvd := 0
	noted := cs.unjudged.mark()
	cs.roundFrom = cs.seqno
	defer func() { cs.roundRecvd = filesRecvd }()
	if cs.refused {
		// The mandatory cleanup exchange cannot accept more clone content.
		cs.result.Rounds = cycle + 1
		return fmt.Errorf("sync.Clone: %s", cs.serverErr)
	}
	cardErr, err := runPass(cs.repo, func(bound *repo.Repo) (error, error) {
		defer cs.bindRepo(bound)()
		filesRecvd = 0
		cs.smConfig = stagedSubmanifestConfig{}
		cs.structural.beginReply()
		cardErr := cs.processCards(ctx, cycle, msg, &filesRecvd)
		if failsRound(cardErr) {
			return nil, cardErr // §10.1: the round, prefix included, rolls back
		}
		cs.applySubmanifestConfig()
		cs.named.endReply(msg)
		if err := cs.decideStructural(msg); err != nil {
			return nil, err
		}
		return cardErr, nil
	})
	if err != nil {
		// The round rolled back, and with it the rows its pass cached.
		cs.basis.Clear()
		cs.unjudged.rollback(noted)
		return err
	}
	// A delta not yet readable is counted once judged (unjudged.settle).
	cs.result.BlobsRecvd += filesRecvd - (cs.unjudged.mark() - noted)
	// An authentication challenge includes push codes, and a cosmetic error
	// may accompany content or a clone cursor. An error alone is a refusal,
	// not a successful empty clone (sparse-cone §5.3).
	if filesRecvd == 0 && !cs.roundPush {
		progress := false
		var refusal string
		for _, card := range msg.Cards {
			switch c := card.(type) {
			case *xfer.CloneSeqNoCard:
				progress = true
			case *xfer.ErrorCard:
				refusal = c.Message
			}
		}
		if refusal != "" && !progress {
			cs.serverErr = refusal
			cs.refused = true
			if cycle > 0 {
				return fmt.Errorf("sync.Clone: %s", refusal)
			}
			// Round zero still owes Fossil's operation-less cleanup (§8.3).
			// Retain the refusal so that exchange can never make cloning succeed.
		}
	}
	return cardErr
}

// processCards is processResponse's card loop, inside the round's
// transaction. It counts the files it stores in filesRecvd.
func (cs *cloneSession) processCards(ctx context.Context, cycle int, msg *xfer.Message,
	filesRecvd *int) error {
	cs.roundPush = false
	cs.pass = newReceivePassOver(cs.basis) // each round is one verification pass (§7.7)
	defer func() { cs.pass = nil }()
	for i, card := range msg.Cards {
		if i%processResponseCancelCheckStride == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		stop, err := cs.processCard(ctx, cycle, card, filesRecvd)
		if err != nil || stop {
			return err
		}
	}
	return nil
}

// processCard acts on one card of a clone reply. It reports stop when the
// card ends the reply-card loop.
func (cs *cloneSession) processCard(ctx context.Context, cycle int, card xfer.Card,
	filesRecvd *int) (bool, error) {
	switch c := card.(type) {
	// §5.2: a reply card the decoder could not read ends the reply-card
	// loop, on every round, the first included.
	case *xfer.BadCommandCard:
		cs.badCard("unknown command: [" + strings.TrimRight(c.Line, "\n") + "]")
		return true, nil
	case *xfer.BadFileCard:
		cs.badCard(c.Message)
		return true, nil
	case *xfer.PushCard:
		cs.roundPush = true
		return false, cs.recordCodes(c)
	case *xfer.FileCard:
		cs.named.noteContent()
		if taken, err := cs.structural.takeSeed(cs.repo, c.UUID, c.DeltaSrc, c.Content); taken || err != nil {
			return false, err
		}
		stored, err := cs.receiveContent(ctx, c.UUID, c.DeltaSrc, c.Content, nil)
		if err != nil {
			return false, err
		}
		if stored {
			*filesRecvd++
		}
	case *xfer.CFileCard:
		cs.named.noteContent()
		stored, err := cs.receiveCFile(ctx, c)
		if err != nil {
			return false, err
		}
		if stored {
			*filesRecvd++
		}
	case *xfer.CloneSeqNoCard:
		cs.recordSeqNo(c)
	case *xfer.ErrorCard:
		return cs.serverError(cycle, c), nil
	case *xfer.CookieCard:
		cs.cookie = c.Value
	case *xfer.MessageCard:
		cs.result.Messages = append(cs.result.Messages, c.Message)
	case *xfer.PragmaCard:
		if c.Name == horizonResolvedPragma {
			return false, cs.takeResolved(c)
		}
		if handled, err := cs.structural.pragma(cs.repo.DB(), c); handled || err != nil {
			return false, err
		}
		if c.Name == coneServedPragma {
			cs.result.ConeServed++ // sparse-cone §6.3, reported only
		}
		if n, ok := parseCursor(c); ok && c.Name == horizonCursorPragma {
			cs.peerCursor = n
		}
		if d, ok := cs.structural.level(c); ok && c.Name == horizonServedPragma {
			cs.result.HorizonServed++ // horizon §6.2.5
			if d == cone.DepthStructural {
				cs.result.StructuralServed++
			}
			if u := servedHistoryURL(c); u != "" {
				cs.result.HorizonHistoryURL = u
			}
		}
	case *xfer.ConfigCard:
		if c.Name == configScalarMarker {
			cs.smConfig.stage(c.Content)
		}
	}
	return false, nil
}

// applySubmanifestConfig writes the sub-manifest settings the reply carried,
// all or none (§6.3). A rejected set is reported, not fatal: the clone is
// whole without it, and enable-submanifests simply stays off.
func (cs *cloneSession) applySubmanifestConfig() {
	if err := cs.smConfig.apply(cs.repo.DB()); err != nil {
		cs.result.Messages = append(cs.result.Messages, err.Error())
	}
}

// receiveCFile stores a cfile and reports whether it did. fossil drops a
// shunned cfile even in a clone, though not a shunned file card (xfer.c:162,
// !cloneFlag), so a shunned one is skipped and not counted.
func (cs *cloneSession) receiveCFile(ctx context.Context, c *xfer.CFileCard) (bool, error) {
	if taken, err := cs.structural.takeSeed(cs.repo, c.UUID, c.DeltaSrc, c.Content); taken || err != nil {
		return false, err
	}
	shunned, err := shun.IsShunned(cs.repo.DB(), c.UUID)
	if err != nil || shunned {
		return false, err
	}
	return cs.receiveContent(ctx, c.UUID, c.DeltaSrc, c.Content, c.StoredBlob)
}

// recordSeqNo records the clone sequence a clone_seqno card carries. The
// server owns this cursor; the client only reads it. A non-decimal NEXT never
// reaches here — the decoder withholds it per §8.2 so the recorded sequence
// stays unchanged — and §8.2 bars the client from validating that a positive
// value advances.
func (cs *cloneSession) recordSeqNo(c *xfer.CloneSeqNoCard) {
	// BUGGIFY: 5% chance ignore completion signal, forcing extra round.
	if c.SeqNo == 0 && cs.opts.Buggify != nil &&
		cs.opts.Buggify.Check("clone.processResponse.dropSeqNo", 0.05) {
		return
	}
	cs.seqno = c.SeqNo
}

// serverError handles an error card and reports whether it ends the
// reply-card loop. A clone's first round carries no login card: the
// signature is salted with the project code, and the project code is exactly
// what this round exists to learn. A server that requires authentication
// therefore answers round 0 with its push card — which supplies that code —
// followed by "not authorized to clone". Treating that as fatal ends the
// clone before the credentials are ever offered, so round 0 records the
// message and carries on; round 1 repeats the request with a login card.
// Canonical does the same: `(syncFlags & SYNC_CLONE)==0 || nCycle>0` guards
// the abort in xfer.c client_sync.
//
// After round 0 an error ends the reply-card loop (§10.1): the cards after it
// are not read, and the session continues by §3.7 on what was stored before
// it. Clone still fails once the session ends, naming the error, so a clone
// cut short by the server is never reported complete.
func (cs *cloneSession) serverError(cycle int, c *xfer.ErrorCard) bool {
	if c == nil {
		panic("cloneSession.serverError: nil card")
	}
	cs.result.Messages = append(cs.result.Messages, c.Message)
	if cycle == 0 {
		return false
	}
	if cs.serverErr == "" {
		cs.serverErr = c.Message
	}
	return true
}

// badCard records a reply card the decoder could not read. As fossil's
// client sets xfer.err and breaks, the cards before it stand and the session
// goes on by §3.7, so a clone's mandatory second round is still made. Clone
// fails naming the card once the session ends, as it does after a server
// error (§10.1). Unlike an error card, the first round does not excuse it:
// fossil's unknown-command branch has no clone exception.
func (cs *cloneSession) badCard(msg string) {
	if msg == "" {
		panic("cloneSession.badCard: empty message")
	}
	cs.result.Messages = append(cs.result.Messages, msg)
	if cs.serverErr == "" {
		cs.serverErr = msg
	}
}

// recordCodes stores the project and server codes a push card carries, the
// first time each arrives.
func (cs *cloneSession) recordCodes(c *xfer.PushCard) error {
	if c == nil {
		panic("cloneSession.recordCodes: nil card")
	}
	if c.ProjectCode != "" && cs.projectCode == "" {
		cs.projectCode = c.ProjectCode
		if _, err := cs.repo.DB().Exec(
			"REPLACE INTO config(name, value) VALUES('project-code', ?)",
			c.ProjectCode,
		); err != nil {
			return fmt.Errorf("sync.Clone: store project-code: %w", err)
		}
	}
	if c.ServerCode != "" && cs.serverCode == "" {
		cs.serverCode = c.ServerCode
		if _, err := cs.repo.DB().Exec(
			"REPLACE INTO config(name, value) VALUES('server-code', ?)",
			c.ServerCode,
		); err != nil {
			return fmt.Errorf("sync.Clone: store server-code: %w", err)
		}
	}
	return nil
}

// receiveContent stores the content a file or cfile card delivered.
// storedBlob is the cfile's payload in fossil's on-disk format, or nil.
func (cs *cloneSession) receiveContent(
	ctx context.Context, uuid, deltaSrc string, content, storedBlob []byte,
) (bool, error) {
	// BUGGIFY: 2% chance corrupt file content to test hash verification.
	// Relies on blob.Store verify-before-commit to catch corruption.
	// storedBlob must be dropped alongside content: it decodes to the
	// *original* bytes, so keeping it would let verbatim storage quietly mask
	// the corruption this path exists to exercise, instead of the corrupted
	// content being what gets hash-checked and stored.
	if cs.opts.Buggify != nil && cs.opts.Buggify.Check("clone.processResponse.corruptHash", 0.02) {
		corrupted := make([]byte, len(content))
		copy(corrupted, content)
		if len(corrupted) > 0 {
			corrupted[0] ^= 0xff
		}
		content = corrupted
		storedBlob = nil
	}
	// BUGGIFY: 5% chance skip storing a received file, creating a phantom.
	if cs.opts.Buggify != nil && cs.opts.Buggify.Check("clone.processResponse.dropFile", 0.05) {
		return true, nil
	}
	if servedCluster(cs.servedHorizon(), deltaSrc, content) {
		return true, nil
	}
	// Horizon F.2: a seed outside the announcement is dropped, not counted
	// and not asked for again; its hash is refused (seedcheck).
	refused, err := seedcheck.IsRefusedUUID(cs.repo.DB(), uuid)
	if err != nil {
		return false, err
	}
	if refused {
		delete(cs.phantoms, uuid)
		return false, nil
	}
	// A delta on a dropped seed is left a phantom, for a pull to fetch in
	// full (onRefusedBasis).
	if again, _, err := onRefusedBasis(cs.repo.DB(), uuid, deltaSrc, visibilityPublic); again || err != nil {
		return false, err
	}
	err = cs.handleFile(ctx, uuid, deltaSrc, content, storedBlob)
	if errors.Is(err, errSeed) {
		delete(cs.phantoms, uuid)
		return false, seedcheck.Refuse(cs.repo.DB(), uuid)
	}
	if err != nil {
		return false, err
	}
	cs.unjudged.note(cs.repo.DB(), cs.pass, uuid, deltaSrc)
	return true, nil
}

// handleFile stores a received file. storeReceivedFile now persists a
// delta whose base hasn't arrived yet rather than discarding it (see
// storeDeltaAgainstPhantomBase), so uuid itself is never re-requested here
// once stored — only a still-missing base might need another round.
//
// storedBlob, when non-nil, is payload already re-expressed in Fossil's
// on-disk blob format (see xfer.CFileCard.StoredBlob) and is persisted
// verbatim rather than recompressed — this is what makes a clone
// byte-identical to its source's blob table, not merely content-identical.
func (cs *cloneSession) handleFile(ctx context.Context, uuid, deltaSrc string, payload []byte, storedBlob []byte) error {
	if cs.pass == nil {
		panic("sync.Clone: handleFile: pass must be set by processResponse")
	}
	if err := rememberSparseReceived(cs.repo.DB(), cs.sparseReceived, uuid); err != nil {
		return err
	}
	if err := storeReceivedFile(ctx, cs.repo, cs.pass, uuid, deltaSrc, payload, storedBlob,
		visibilityPublic, cs.linker); err != nil {
		return fmt.Errorf("sync.Clone: handleFile %s: %w", uuid, err)
	}

	delete(cs.phantoms, uuid)
	if deltaSrc != "" {
		if _, cached := cs.availableDeltaSources[deltaSrc]; !cached {
			// Availability, not existence: a phantom row for deltaSrc still
			// needs requesting, and so does a delta whose own base is a
			// phantom. Only a confirmed available source is memoized: an
			// unavailable source can arrive later in this same clone.
			if !cs.pass.available(cs.repo.DB(), deltaSrc) {
				// request_phantoms never asks for a shunned name.
				shunned, err := shun.IsShunned(cs.repo.DB(), deltaSrc)
				if err != nil {
					return fmt.Errorf("sync.Clone: basis of %s: %w", uuid, err)
				}
				if !shunned {
					cs.phantoms[deltaSrc] = true
				}
			} else if len(cs.availableDeltaSources) < cloneAvailableDeltaSourceLimit {
				cs.availableDeltaSources[deltaSrc] = struct{}{}
			}
		}
	}
	return nil
}

// decideStructural settles a structural clone's depth at the first reply
// that carries the clone sequence, the first that answers the clone rather
// than an authentication challenge (horizon §6.2.5), and applies the held
// seed to what each later reply linked.
func (cs *cloneSession) decideStructural(msg *xfer.Message) error {
	if !cs.structuralDecided && slices.ContainsFunc(msg.Cards, func(c xfer.Card) bool {
		_, ok := c.(*xfer.CloneSeqNoCard)
		return ok
	}) {
		cs.structuralDecided = true
		fell, err := cs.structural.endFirstReply(cs.repo)
		if err != nil {
			return err
		}
		cs.result.HorizonFallback = cs.result.HorizonFallback || fell
	}
	if !cs.structuralDecided {
		return nil
	}
	fell, err := cs.structural.endReply(cs.repo)
	cs.result.HorizonFallback = cs.result.HorizonFallback || fell
	return err
}

// bindRepo points the clone session at bound, the repository bound to the
// current round's transaction, and returns the function that points it back.
func (cs *cloneSession) bindRepo(bound *repo.Repo) func() {
	if bound == nil {
		panic("sync.Clone.bindRepo: bound must not be nil")
	}
	orig := cs.repo
	cs.repo = bound
	return func() { cs.repo = orig }
}

// servedHorizon reports a horizon clone whose origin has said it serves the
// clone's horizon (horizon-served content) in a reply of this session.
func (cs *cloneSession) servedHorizon() bool {
	if cs.result.HorizonServed == 0 {
		return false
	}
	_, ok := horizonOf(cs.opts.StoreCone)
	return ok || cs.named != nil
}
