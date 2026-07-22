package sync

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/danmestas/libfossil/internal/content"
	"github.com/danmestas/libfossil/internal/manifest"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/simio"
	"github.com/danmestas/libfossil/internal/xfer"
)

// phantomStallSampleSize caps how many missing UUIDs PhantomStallError
// carries, so a repository with thousands of phantoms doesn't produce an
// unreadable error message.
const phantomStallSampleSize = 10

// PhantomStallError reports a clone that stopped making progress while
// phantom blobs — content referenced but never received — were still
// outstanding. Terminating a stalled clone is correct; reporting it as
// success is not, so this is always returned instead of nil in that case.
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
	repo        *repo.Repo
	env         *simio.Env
	opts        CloneOpts
	result      CloneResult
	phantoms    map[string]bool
	seqno       int
	projectCode string
	serverCode  string
	cookie      string // server-issued snapshot bound; echoed every round so the
	// server can scope its blob query to the rid range that existed when the
	// clone session opened. See cloneCapPrefix in handler.go.
	obs Observer
}

// Clone performs a full repository clone from a remote Fossil server.
// It creates a new repository at path, runs the clone protocol until
// convergence, and returns the opened repo and a result summary.
// On error, the partially-created repo file is removed.
func Clone(ctx context.Context, path string, t Transport, opts CloneOpts) (r *repo.Repo, result *CloneResult, err error) {
	if path == "" {
		panic("sync.Clone: path must not be empty")
	}
	if t == nil {
		panic("sync.Clone: t must not be nil")
	}

	env := opts.Env
	if env == nil {
		env = simio.RealEnv()
	}
	storage := env.Storage
	if storage == nil {
		storage = simio.OSStorage{}
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

	// Clear project-code — the server will provide its own.
	if _, execErr := r.DB().Exec("DELETE FROM config WHERE name='project-code'"); execErr != nil {
		err = fmt.Errorf("sync.Clone: clear project-code: %w", execErr)
		return
	}

	cs := &cloneSession{
		repo:     r,
		env:      env,
		opts:     opts,
		seqno:    1,
		phantoms: make(map[string]bool),
	}
	cs.obs = resolveObserver(opts.Observer)

	cloneResult, cloneErr := cs.run(ctx, t)
	if cloneErr != nil {
		// cloneResult is still meaningful on a stall or round-limit error —
		// callers inspecting the returned result (e.g. BlobsRecvd) alongside
		// a *PhantomStallError shouldn't see it silently dropped to nil.
		result = cloneResult
		err = cloneErr
		return
	}

	// Crosslink: parse received manifests into event/plink/leaf/mlink tables.
	linked, xlinkErr := manifest.Crosslink(r)
	if xlinkErr != nil {
		result = cloneResult
		err = fmt.Errorf("sync.Clone: crosslink: %w", xlinkErr)
		return
	}
	cloneResult.ArtifactsLinked = linked

	return r, cloneResult, nil
}

// run executes the clone loop.
func (cs *cloneSession) run(ctx context.Context, t Transport) (*CloneResult, error) {
	ctx = cs.obs.Started(ctx, SessionStart{
		Operation: "clone",
		Pull:      true,
	})

	prevPhantomCount := -1

	for cycle := 0; ; cycle++ {
		select {
		case <-ctx.Done():
			cs.obs.Completed(ctx, sessionEndFromClone(&cs.result), ctx.Err())
			return &cs.result, ctx.Err()
		default:
		}
		if cycle >= MaxRounds {
			err := fmt.Errorf("sync.Clone: exceeded %d rounds", MaxRounds)
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

		resp, err := t.Exchange(ctx, req)
		if err != nil {
			cs.obs.RoundCompleted(roundCtx, cycle, RoundStats{})
			cs.obs.Completed(ctx, sessionEndFromClone(&cs.result), err)
			return &cs.result, fmt.Errorf("sync.Clone: exchange round %d: %w", cycle, err)
		}

		recvdBefore := cs.result.BlobsRecvd

		done, err := cs.processResponse(resp)
		if err != nil {
			cs.obs.RoundCompleted(roundCtx, cycle, RoundStats{FilesReceived: cs.result.BlobsRecvd - recvdBefore})
			cs.obs.Completed(ctx, sessionEndFromClone(&cs.result), err)
			return &cs.result, fmt.Errorf("sync.Clone: process round %d: %w", cycle, err)
		}

		cs.result.Rounds = cycle + 1
		cs.obs.RoundCompleted(roundCtx, cycle, RoundStats{FilesReceived: cs.result.BlobsRecvd - recvdBefore})

		stop, stopErr := cs.checkStop(cycle, done, &prevPhantomCount)
		if stopErr != nil {
			cs.obs.Completed(ctx, sessionEndFromClone(&cs.result), stopErr)
			return &cs.result, stopErr
		}
		if stop {
			break
		}
	}

	cs.result.ProjectCode = cs.projectCode
	cs.result.ServerCode = cs.serverCode
	cs.obs.Completed(ctx, sessionEndFromClone(&cs.result), nil)
	return &cs.result, nil
}

// checkStop decides whether the clone loop should stop after processing a
// round. Convergence needs at least two rounds (cycle >= 1), and only once a
// round delivers no new file content (roundDone). *prevPhantomCount tracks
// the phantom count observed at the end of the prior round and is updated
// in place.
//
// A non-nil err means the loop is stopping because it stalled with phantoms
// still outstanding — the phantom count held steady or grew across a round
// that delivered nothing new. That is a failed clone, not a successful one,
// even though continuing would only spin forever (see PhantomStallError).
func (cs *cloneSession) checkStop(cycle int, roundDone bool, prevPhantomCount *int) (stop bool, err error) {
	if prevPhantomCount == nil {
		panic("cloneSession.checkStop: prevPhantomCount must not be nil")
	}

	if cycle < 1 {
		return false, nil
	}
	if !roundDone {
		*prevPhantomCount = len(cs.phantoms)
		return false, nil
	}

	phantomCount := len(cs.phantoms)
	if cs.seqno <= 0 && phantomCount == 0 {
		return true, nil
	}
	if cs.seqno <= 0 && phantomCount > 0 && phantomCount >= *prevPhantomCount {
		return true, newPhantomStallError(cs.phantoms)
	}
	*prevPhantomCount = phantomCount
	return false, nil
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

	// Pragma: client-version (every round)
	cards = append(cards, &xfer.PragmaCard{
		Name:   "client-version",
		Values: []string{"22800", "20260315", "120000"},
	})

	// Cookie carries the server-captured clone snapshot bound. Echoing it back
	// every round lets the stateless handler enforce a stable upper rid bound
	// on its batch query, so a hub that is being written to during a clone
	// session can't keep extending the queue and prevent convergence.
	if cs.cookie != "" {
		cards = append(cards, &xfer.CookieCard{Value: cs.cookie})
	}

	// Clone card — only when seqno > 0 (sequential delivery in progress).
	// When seqno reaches 0, the server has sent all blobs and the client
	// switches to gimme-based phantom resolution (matching Fossil xfer.c:2706).
	if cs.seqno > 0 {
		version := cs.opts.Version
		if version <= 0 {
			version = 3
		}
		// The clone card carries the pagination cursor itself — `clone
		// VERSION SEQNO`, canonical xfer.c:1553. A clone_seqno card must
		// never go out from here: it is server-to-client only, canonical's
		// page_xfer() has no parser for it, and sending one lands in the
		// server's unknown-card branch as `bad command: clone_seqno N`
		// (issue #74).
		cards = append(cards, &xfer.CloneCard{
			Version:  version,
			SeqNo:    cs.seqno,
			SeqNoIsDecimal: true,
		})
	} else {
		// Pull mode for phantom resolution after sequential delivery completes.
		if cs.projectCode != "" && cs.serverCode != "" {
			cards = append(cards, &xfer.PullCard{
				ServerCode:  cs.serverCode,
				ProjectCode: cs.projectCode,
			})
		}
	}

	// Gimme cards for phantoms — only when seqno <= 1 (main transfer done or finishing).
	if cs.seqno <= 1 {
		gimmes := make([]string, 0, len(cs.phantoms))
		for uuid := range cs.phantoms {
			gimmes = append(gimmes, uuid)
		}
		// BUGGIFY: 5% chance drop the last gimme card.
		if len(gimmes) > 1 && cs.opts.Buggify != nil && cs.opts.Buggify.Check("clone.buildRequest.dropGimme", 0.05) {
			gimmes = gimmes[:len(gimmes)-1]
		}
		for _, uuid := range gimmes {
			cards = append(cards, &xfer.GimmeCard{UUID: uuid})
		}
	}

	// Login card: skip round 0. On round 1+, only if User is set AND projectCode received.
	if cycle > 0 && cs.opts.User != "" && cs.projectCode != "" {
		loginCard, err := cs.buildLoginCard(cards)
		if err != nil {
			return nil, fmt.Errorf("clone buildLoginCard: %w", err)
		}
		cards = append([]xfer.Card{loginCard}, cards...)
	}

	return &xfer.Message{Cards: cards}, nil
}

// buildLoginCard encodes the non-login cards, appends a random comment,
// then computes the login card.
func (cs *cloneSession) buildLoginCard(cards []xfer.Card) (*xfer.LoginCard, error) {
	var buf bytes.Buffer
	for _, c := range cards {
		if err := xfer.EncodeCard(&buf, c); err != nil {
			return nil, err
		}
	}
	payload := appendRandomComment(buf.Bytes(), cs.env.Rand)
	login := computeLogin(cs.opts.User, cs.opts.Password, cs.projectCode, payload)
	// BUGGIFY: 5% chance corrupt login nonce to test auth failure recovery.
	if cs.opts.Buggify != nil && cs.opts.Buggify.Check("clone.buildRequest.badLogin", 0.05) {
		login.Nonce = "corrupted-nonce"
	}
	return login, nil
}

// processResponse handles all cards in a server response for a clone round.
// Returns true when the round produced no new file content.
func (cs *cloneSession) processResponse(msg *xfer.Message) (bool, error) {
	if msg == nil {
		panic("sync.Clone.processResponse: msg must not be nil")
	}

	filesRecvd := 0

	for _, card := range msg.Cards {
		switch c := card.(type) {
		case *xfer.PushCard:
			// Server sends push card with project-code and server-code.
			if c.ProjectCode != "" && cs.projectCode == "" {
				cs.projectCode = c.ProjectCode
				if _, err := cs.repo.DB().Exec(
					"REPLACE INTO config(name, value) VALUES('project-code', ?)",
					c.ProjectCode,
				); err != nil {
					return false, fmt.Errorf("sync.Clone: store project-code: %w", err)
				}
			}
			if c.ServerCode != "" && cs.serverCode == "" {
				cs.serverCode = c.ServerCode
				if _, err := cs.repo.DB().Exec(
					"REPLACE INTO config(name, value) VALUES('server-code', ?)",
					c.ServerCode,
				); err != nil {
					return false, fmt.Errorf("sync.Clone: store server-code: %w", err)
				}
			}

		case *xfer.FileCard:
			content := c.Content
			// BUGGIFY: 2% chance corrupt file content to test hash verification.
			// Relies on blob.Store verify-before-commit to catch corruption.
			if cs.opts.Buggify != nil && cs.opts.Buggify.Check("clone.processResponse.corruptHash", 0.02) {
				corrupted := make([]byte, len(content))
				copy(corrupted, content)
				if len(corrupted) > 0 {
					corrupted[0] ^= 0xff
				}
				content = corrupted
			}
			// BUGGIFY: 5% chance skip storing a received file, creating a phantom.
			if cs.opts.Buggify != nil && cs.opts.Buggify.Check("clone.processResponse.dropFile", 0.05) {
				filesRecvd++
				continue
			}
			if err := cs.handleFile(c.UUID, c.DeltaSrc, content); err != nil {
				return false, err
			}
			filesRecvd++

		case *xfer.CFileCard:
			content := c.Content
			// BUGGIFY: 2% chance corrupt file content to test hash verification.
			// Relies on blob.Store verify-before-commit to catch corruption.
			if cs.opts.Buggify != nil && cs.opts.Buggify.Check("clone.processResponse.corruptHash", 0.02) {
				corrupted := make([]byte, len(content))
				copy(corrupted, content)
				if len(corrupted) > 0 {
					corrupted[0] ^= 0xff
				}
				content = corrupted
			}
			// BUGGIFY: 5% chance skip storing a received file, creating a phantom.
			if cs.opts.Buggify != nil && cs.opts.Buggify.Check("clone.processResponse.dropFile", 0.05) {
				filesRecvd++
				continue
			}
			if err := cs.handleFile(c.UUID, c.DeltaSrc, content); err != nil {
				return false, err
			}
			filesRecvd++

		case *xfer.CloneSeqNoCard:
			// BUGGIFY: 5% chance ignore completion signal, forcing extra round.
			if c.SeqNo == 0 && cs.opts.Buggify != nil && cs.opts.Buggify.Check("clone.processResponse.dropSeqNo", 0.05) {
				continue
			}
			// The server owns this cursor; the client only reads it. A
			// non-decimal NEXT never reaches here — the decoder withholds it
			// per §8.2 so the recorded sequence stays unchanged — and §8.2
			// bars the client from validating that a positive value advances.
			cs.seqno = c.SeqNo

		case *xfer.ErrorCard:
			return false, fmt.Errorf("sync.Clone: server error: %s", c.Message)

		case *xfer.CookieCard:
			cs.cookie = c.Value

		case *xfer.MessageCard:
			cs.result.Messages = append(cs.result.Messages, c.Message)
		}
	}

	cs.result.BlobsRecvd += filesRecvd
	return filesRecvd == 0, nil
}

// handleFile stores a received file. storeReceivedFile now persists a
// delta whose base hasn't arrived yet rather than discarding it (see
// storeDeltaAgainstPhantomBase), so uuid itself is never re-requested here
// once stored — only a still-missing base might need another round.
func (cs *cloneSession) handleFile(uuid, deltaSrc string, payload []byte) error {
	if err := storeReceivedFile(cs.repo, uuid, deltaSrc, payload); err != nil {
		return fmt.Errorf("sync.Clone: handleFile %s: %w", uuid, err)
	}

	delete(cs.phantoms, uuid)
	if deltaSrc != "" {
		// Availability, not existence: a phantom row for deltaSrc still
		// needs requesting, and so does a delta whose own base is a
		// phantom.
		if _, available := content.AvailableByUUID(cs.repo.DB(), deltaSrc); !available {
			cs.phantoms[deltaSrc] = true
		}
	}
	return nil
}
