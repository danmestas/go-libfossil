package sync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/internal/manifest"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/simio"
	"github.com/danmestas/libfossil/internal/xfer"
)

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
		err = cloneErr
		return
	}

	// Crosslink: parse received manifests into event/plink/leaf/mlink tables.
	linked, xlinkErr := manifest.Crosslink(r)
	if xlinkErr != nil {
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

		// Convergence: need at least 2 rounds.
		// Continue while seqno > 0 or phantoms remain with progress.
		if cycle >= 1 && done {
			if cs.seqno <= 0 && len(cs.phantoms) == 0 {
				break
			}
			// If phantoms remain but no progress, stop.
			phantomCount := len(cs.phantoms)
			if cs.seqno <= 0 && phantomCount > 0 && phantomCount >= prevPhantomCount {
				break
			}
			prevPhantomCount = phantomCount
			if cs.seqno <= 0 {
				// Only phantoms remain, keep going if making progress.
				continue
			}
		}
		if cycle >= 1 {
			prevPhantomCount = len(cs.phantoms)
		}
	}

	cs.result.ProjectCode = cs.projectCode
	cs.result.ServerCode = cs.serverCode
	cs.obs.Completed(ctx, sessionEndFromClone(&cs.result), nil)
	return &cs.result, nil
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
		cards = append(cards, &xfer.CloneCard{
			Version: version,
			SeqNo:   cs.seqno,
		})
		// CloneSeqNoCard carries the actual pagination cursor server-side
		// (see TestHandleClonePagination). The CloneCard alone only signals
		// clone mode; without this card the server's cloneSeq stays 0 every
		// round and the same blob prefix is re-emitted forever — fine when
		// a repo fits in one batch, fatal under writing-hub or smallBatch
		// conditions. Skip on round 0 so the server treats it as a fresh
		// session (any starting cursor would skip rid <= cs.seqno).
		if cycle > 0 {
			cards = append(cards, &xfer.CloneSeqNoCard{SeqNo: cs.seqno})
		}
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

// handleFile stores a received file, creating a phantom on delta source miss.
func (cs *cloneSession) handleFile(uuid, deltaSrc string, payload []byte) error {
	err := storeReceivedFile(cs.repo, uuid, deltaSrc, payload, nil)
	if err == nil {
		delete(cs.phantoms, uuid)
		return nil
	}

	if errors.Is(err, ErrDeltaSourceMissing) {
		// Delta source not yet received — store phantom for the target,
		// and track the delta source as a phantom to request later.
		if _, phantomErr := blob.StorePhantom(cs.repo.DB(), uuid); phantomErr != nil {
			return fmt.Errorf("sync.Clone: store phantom for %s: %w", uuid, phantomErr)
		}
		cs.phantoms[uuid] = true
		if deltaSrc != "" {
			if _, exists := blob.Exists(cs.repo.DB(), deltaSrc); !exists {
				cs.phantoms[deltaSrc] = true
			}
		}
		return nil
	}

	return fmt.Errorf("sync.Clone: handleFile %s: %w", uuid, err)
}
