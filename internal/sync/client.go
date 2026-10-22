package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/shun"
	"github.com/danmestas/go-libfossil/internal/uv"
	"github.com/danmestas/go-libfossil/internal/xfer"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// buildRequest assembles one outbound xfer message for the given cycle.
func (s *session) buildRequest(cycle int) (*xfer.Message, error) {
	// BUGGIFY: shrink send budget to stress multi-round convergence.
	if s.opts.Buggify != nil && s.opts.Buggify.Check("sync.buildRequest.minBudget", 0.10) {
		s.maxSend = 1024
	}

	cards, err := s.headerCards(cycle)
	if err != nil {
		return nil, err
	}
	content, err := s.contentCards(cycle)
	if err != nil {
		return nil, err
	}
	cards = append(cards, content...)
	extra, err := s.extraCards(cycle)
	if err != nil {
		return nil, err
	}
	cards = append(cards, extra...)

	// Login card computed LAST, prepended to the front.
	// Nonce = SHA1 of all other cards encoded + random comment.
	//
	// The signature is salted with the project code, so a login built from
	// an empty one is rejected by every server. Callers routinely leave
	// SyncOpts.ProjectCode unset and let the repo's own config supply it,
	// exactly as the push and pull cards do.
	if s.opts.User != "" {
		loginCard, signed, err := s.buildLoginCard(cards, s.projectCode)
		if err != nil {
			return nil, fmt.Errorf("buildRequest login: %w", err)
		}
		cards = append([]xfer.Card{loginCard}, signed...)
	} else {
		// Every request ends with randomness (draft §3); a login card's
		// signature already covers the one buildLoginCard appends.
		cards = append(cards, randomComment(s.env.Rand))
	}

	msg := &xfer.Message{Cards: cards, LoginCookieOK: s.loginCookieOK}

	// This request's push card has now been committed. fossil emits the next
	// request's header before reading the reply that may stop the push, so a
	// "pull only" seen from here on drops the push card only one request later.
	if s.pushStopped {
		s.pushCardDropped = true
	}
	return msg, nil
}

// headerCards are the cards a request opens with, in fossil client_sync's
// order: client-version, then pull before push, then the cookie. pragma
// send-private goes first of all in the first request and after
// client-version in the rest, where client_sync puts it (§3.2).
func (s *session) headerCards(cycle int) ([]xfer.Card, error) {
	if cycle < 0 {
		panic("session.headerCards: negative cycle")
	}
	var cards []xfer.Card
	sendPrivate := &xfer.PragmaCard{Name: "send-private"}
	if s.opts.Private && cycle == 0 {
		cards = append(cards, sendPrivate)
	}
	cards = append(cards, versionPragma("client-version"), sendSubmanifests())
	if s.opts.Pull {
		sparse, err := localConePragmas(s.repo.DB())
		if err != nil {
			return nil, err
		}
		cards = append(cards, sparse...)
	}
	if s.opts.Private && cycle > 0 {
		cards = append(cards, sendPrivate)
	}

	// The project code is required on the wire (Fossil C rejects bare
	// push/pull). s.projectCode is the caller's code or, failing that, the
	// repo's own — every repo created by libfossil has a project-code.
	projCode := s.projectCode
	srvCode := s.serverCode()
	// Pull before push, as fossil's client_sync emits them (src/xfer.c): a
	// server reading the push card decides "not authorized to write" versus
	// "pull only" on whether it has already seen the pull card, so the order
	// is load-bearing for a read-only requester (§10.2).
	if s.opts.Pull {
		if projCode == "" {
			return nil, errNoProjectCode("pull")
		}
		cards = append(cards, &xfer.PullCard{ProjectCode: projCode, ServerCode: srvCode})
	}
	if s.pushCard() {
		if projCode == "" {
			return nil, errNoProjectCode("push")
		}
		cards = append(cards, &xfer.PushCard{ProjectCode: projCode, ServerCode: srvCode})
	}
	// §3.2: the initial request's req-links follows the operation cards.
	if s.sendsReqLinks(cycle) {
		cards = append(cards, &xfer.PragmaCard{Name: "req-links"})
	}
	if s.cookie != "" {
		cards = append(cards, &xfer.CookieCard{Value: s.cookie})
	}
	return cards, nil
}

// serverCode is the server-code a push or pull card carries. Fossil C reads
// a cached remote server-code from the local repo config ('server-code'
// written by a prior pull) when SyncOpts.ServerCode is not provided, and
// when none is cached sends its own placeholder — `zSCode =
// db_get("server-code", "x")` in xfer.c client_sync. The server ignores the
// value; only the project code that follows it is validated.
func (s *session) serverCode() string {
	if s.repo == nil {
		panic("session.serverCode: nil repo")
	}
	srvCode := s.opts.ServerCode
	if srvCode == "" {
		_ = s.repo.DB().QueryRow(
			"SELECT value FROM config WHERE name='server-code'",
		).Scan(&srvCode)
	}
	if srvCode == "" {
		srvCode = "x"
	}
	return srvCode
}

// contentCards announce what the client holds and move content: igot cards,
// cluster igots, file cards, gimme cards and private igots.
func (s *session) contentCards(cycle int) ([]xfer.Card, error) {
	if cycle < 0 {
		panic("session.contentCards: negative cycle")
	}
	// IGot cards: sendUnclustered every round.
	cards, err := s.sendUnclustered()
	if err != nil {
		return nil, fmt.Errorf("buildRequest igot: %w", err)
	}
	s.igotSentThisRound = len(cards)
	s.roundStats.IgotsSent = len(cards)

	// Send cluster igots on round 2+ when pushing and remote has requested
	// blobs. cycle is 0-indexed, so cycle>=1 means round 2+.
	if cycle >= 1 && s.pushing() && s.nGimmeRcvd > 0 {
		clusterCards, err := s.sendAllClusters()
		if err != nil {
			return nil, fmt.Errorf("buildRequest cluster igot: %w", err)
		}
		s.igotSentThisRound += len(clusterCards)
		s.roundStats.IgotsSent += len(clusterCards)
		cards = append(cards, clusterCards...)
	}

	// Request cluster catalog on round 2 when pulling. A horizon store whose
	// origin serves its horizon is announced what it keeps directly and asks
	// for none; against a horizon-ignorant origin it asks, since a new
	// check-in may be named only by a cluster (draft-fossil-horizon-00 §4.5).
	if cycle == 1 && s.opts.Pull && !s.servedHorizon() {
		cards = append(cards, &xfer.PragmaCard{Name: "req-clusters"})
	}

	// Refusals of SHA3 content the server is too old to accept, then file
	// cards from pendingSend, respecting the maxSend budget.
	for ; s.sha3Refusals > 0; s.sha3Refusals-- {
		cards = append(cards, sha3Refusal())
	}
	if s.smRefusals > 0 {
		cards = append(cards, submanifestRefusal())
		s.smRefusals = 0
	}
	// Private content a gimme asked for outside a private sync is not sent,
	// but the server is told it exists and is private (§8.6).
	for _, uuid := range s.privateIGots {
		cards = append(cards, &xfer.IGotCard{UUID: uuid, IsPrivate: true})
	}
	s.privateIGots = s.privateIGots[:0]
	fileCards, err := s.buildFileCards()
	if err != nil {
		return nil, fmt.Errorf("buildRequest file: %w", err)
	}
	cards = append(cards, fileCards...)

	// Gimme cards from phantoms (at most s.mxPhantomReq).
	cards = append(cards, s.buildGimmeCards()...)

	// Private: send igot cards for private blobs.
	if s.opts.Private {
		privCards, err := s.sendPrivate()
		if err != nil {
			return nil, fmt.Errorf("buildRequest sendPrivate: %w", err)
		}
		s.igotSentThisRound += len(privCards)
		s.roundStats.IgotsSent += len(privCards)
		cards = append(cards, privCards...)
	}
	return cards, nil
}

// extraCards are the check-in lock request, unversioned sync and table sync.
func (s *session) extraCards(cycle int) ([]xfer.Card, error) {
	if cycle < 0 {
		panic("session.extraCards: negative cycle")
	}
	var cards []xfer.Card
	// ci-lock: request check-in lock on first round only.
	if s.opts.CkinLock != nil && cycle == 0 {
		cards = append(cards, &xfer.PragmaCard{
			Name:   "ci-lock",
			Values: []string{s.opts.CkinLock.ParentUUID, s.opts.CkinLock.ClientID},
		})
	}
	if s.opts.UV {
		uvCards, err := s.uvRequestCards()
		if err != nil {
			return nil, err
		}
		cards = append(cards, uvCards...)
	}
	// Table sync cards (only between EdgeSync peers, not real Fossil servers)
	if s.opts.XTableSync {
		xTableCards, err := s.buildXTableCards()
		if err != nil {
			return nil, fmt.Errorf("buildRequest xtable: %w", err)
		}
		cards = append(cards, xTableCards...)
	}
	return cards, nil
}

// uvRequestCards are a request's unversioned sync cards: pragma uv-hash in
// the first request, uvgimme for the files asked for, and uvfile cards once
// the server has granted uv-push-ok.
func (s *session) uvRequestCards() ([]xfer.Card, error) {
	if !s.opts.UV {
		panic("session.uvRequestCards: unversioned sync is off")
	}
	var cards []xfer.Card
	if !s.uvHashSent {
		if err := uv.EnsureSchema(s.repo.DB()); err != nil {
			return nil, fmt.Errorf("buildRequest: uv.EnsureSchema: %w", err)
		}
		uvHash, err := uv.ContentHash(s.repo.DB())
		if err != nil {
			return nil, fmt.Errorf("buildRequest: uv.ContentHash: %w", err)
		}
		cards = append(cards, &xfer.PragmaCard{Name: "uv-hash", Values: []string{uvHash}})
		s.uvHashSent = true
	}
	for name := range s.uvGimmes {
		cards = append(cards, &xfer.UVGimmeCard{Name: name})
		s.nUvGimmeSent++
		s.result.UVGimmesSent++
		delete(s.uvGimmes, name)
	}
	if s.uvPushOK {
		uvCards, err := s.buildUVFileCards()
		if err != nil {
			return nil, fmt.Errorf("buildRequest uvfile: %w", err)
		}
		s.result.UVFilesSent += len(uvCards)
		cards = append(cards, uvCards...)
	}
	return cards, nil
}

// sendUnclustered queries the unclustered table and produces igot cards
// for artifacts the remote doesn't already have, excluding phantoms.
func (s *session) sendUnclustered() ([]xfer.Card, error) {
	rows, err := s.repo.DB().Query(`
		SELECT b.uuid FROM unclustered u JOIN blob b ON b.rid=u.rid
		WHERE b.size >= 0
		AND NOT EXISTS(SELECT 1 FROM phantom WHERE rid=u.rid)
		AND NOT EXISTS(SELECT 1 FROM shun WHERE uuid=b.uuid)
		AND NOT EXISTS(SELECT 1 FROM private WHERE rid=u.rid)`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cards []xfer.Card
	for rows.Next() {
		var uuid string
		if err := rows.Scan(&uuid); err != nil {
			return nil, err
		}
		if s.remoteHas[uuid] {
			continue
		}
		cards = append(cards, &xfer.IGotCard{UUID: uuid})
	}
	return cards, rows.Err()
}

// sendAllClusters produces igot cards for cluster artifacts that have already
// been clustered (not in unclustered) and are not phantoms. Sent on round 2+
// when pushing and the remote has requested blobs (nGimmeRcvd > 0).
func (s *session) sendAllClusters() ([]xfer.Card, error) {
	rows, err := s.repo.DB().Query(`
		SELECT b.uuid FROM tagxref tx JOIN blob b ON tx.rid=b.rid
		WHERE tx.tagid=7
		AND NOT EXISTS(SELECT 1 FROM unclustered WHERE rid=b.rid)
		AND NOT EXISTS(SELECT 1 FROM phantom WHERE rid=b.rid)
		AND NOT EXISTS(SELECT 1 FROM shun WHERE uuid=b.uuid)
		AND NOT EXISTS(SELECT 1 FROM private WHERE rid=b.rid)
		AND b.size >= 0`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cards []xfer.Card
	for rows.Next() {
		var uuid string
		if err := rows.Scan(&uuid); err != nil {
			return nil, err
		}
		if s.remoteHas[uuid] {
			continue
		}
		cards = append(cards, &xfer.IGotCard{UUID: uuid})
	}
	return cards, rows.Err()
}

// sendPrivate emits igot cards with IsPrivate=true for all blobs in the
// private table. This advertises private artifacts to the server so it can
// request them via gimme cards.
func (s *session) sendPrivate() ([]xfer.Card, error) {
	// BUGGIFY: 10% chance skip all private igots this round to test
	// multi-round private convergence.
	if s.opts.Buggify != nil && s.opts.Buggify.Check("sync.sendPrivate.skip", 0.10) {
		return nil, nil
	}

	rows, err := s.repo.DB().Query(
		"SELECT b.uuid FROM private p JOIN blob b ON p.rid=b.rid WHERE b.size >= 0",
	)
	if err != nil {
		return nil, fmt.Errorf("sendPrivate: %w", err)
	}
	defer rows.Close()
	var cards []xfer.Card
	for rows.Next() {
		var uuid string
		if err := rows.Scan(&uuid); err != nil {
			return nil, err
		}
		if s.remoteHas[uuid] {
			continue
		}
		cards = append(cards, &xfer.IGotCard{UUID: uuid, IsPrivate: true})
	}
	return cards, rows.Err()
}

// pushing reports whether this session still answers gimme cards: it was
// asked to push and the server has not said "pull only" (§10.2). It governs
// answering a gimme now, which fossil gates on the live SYNC_PUSH.
func (s *session) pushing() bool {
	return s.opts.Push && !s.pushStopped
}

// pushCard reports whether the next request carries a push card, which lags
// pushing by one request (see session.pushCardDropped).
func (s *session) pushCard() bool {
	return s.opts.Push && !s.pushCardDropped
}

// buildFileCards produces file cards from pendingSend and the unsent table,
// respecting the maxSend byte budget. It is gated on the configured push
// option only, not on pushStopped: a gimme answered before "pull only"
// queued its file, which still goes out, as fossil's send_file had already
// appended it to the reply before the push stopped.
func (s *session) buildFileCards() ([]xfer.Card, error) {
	if !s.opts.Push {
		return nil, nil
	}

	budget := s.maxSend
	var cards []xfer.Card

	// First: pendingSend
	for uuid := range s.pendingSend {
		if budget <= 0 {
			break
		}

		// Private filtering: check if blob is private.
		isPriv := false
		if rid, ok := blob.Exists(s.repo.DB(), uuid); ok {
			isPriv = content.IsPrivate(s.repo.DB(), int64(rid))
		}
		if isPriv && !s.opts.Private {
			delete(s.pendingSend, uuid)
			continue
		}
		card, size, ok, err := s.loadFileCard(uuid)
		if err != nil {
			return nil, err
		}
		if !ok {
			// A phantom, or a delta whose basis is one, is not sent, now or
			// later.
			delete(s.pendingSend, uuid)
			continue
		}
		if isPriv {
			cards = append(cards, &xfer.PrivateCard{})
		}
		cards = append(cards, card)
		budget -= size
		delete(s.pendingSend, uuid)
		s.result.FilesSent++
		s.roundStats.BytesSent += int64(size)
	}

	// Note: files from the unsent table are announced via igot cards (in buildIGotCards).
	// The server will gimme the ones it needs, which populates pendingSend for the next round.
	// We do NOT proactively send unsent files — Fossil's protocol expects igot first, gimme second.

	// BUGGIFY: drop the last file card to simulate partial send.
	if s.opts.Buggify != nil && s.opts.Buggify.Check("sync.buildFileCards.skip", 0.05) && len(cards) > 0 {
		cards = cards[:len(cards)-1]
	}

	return cards, nil
}

// loadFileCard loads a blob by UUID and returns a FileCard plus its payload size.
// It reports !ok, with no error, for content the repository cannot expand:
// a phantom, or a delta whose basis is one. A failure to read the repository
// is an error.
func (s *session) loadFileCard(uuid string) (*xfer.FileCard, int, bool, error) {
	rid, ok, err := content.CheckAvailableByUUID(s.repo.DB(), uuid)
	if err != nil || !ok {
		return nil, 0, false, err
	}
	data, err := s.cache.Expand(s.repo.DB(), rid)
	if err != nil {
		return nil, 0, false, err
	}
	return &xfer.FileCard{UUID: uuid, Content: data}, len(data), true, nil
}

// buildGimmeCards produces gimme cards from the phantoms set.
func (s *session) buildGimmeCards() []xfer.Card {
	if !s.opts.Pull {
		return nil
	}
	maxGimme := s.mxPhantomReq
	if maxGimme < MaxGimmeBase {
		panic("sync.buildGimmeCards: mxPhantomReq fell below MaxGimmeBase")
	}

	var cards []xfer.Card
	count := 0
	for uuid := range s.phantoms {
		if count >= maxGimme {
			break
		}
		// Best-effort private filtering: if the blob exists locally and is
		// private but Private mode is off, skip it. Phantoms (not yet in DB)
		// pass through — the server is the primary gate.
		if !s.opts.Private {
			rid, exists := blob.Exists(s.repo.DB(), uuid)
			if exists && content.IsPrivate(s.repo.DB(), int64(rid)) {
				continue
			}
		}
		cards = append(cards, &xfer.GimmeCard{UUID: uuid})
		s.roundStats.GimmesSent++
		count++
	}
	return cards
}

// buildUVFileCards produces uvfile cards from uvToSend.
func (s *session) buildUVFileCards() ([]xfer.Card, error) {
	// BUGGIFY: shrink UV send budget to stress multi-round UV convergence.
	if s.opts.Buggify != nil && s.opts.Buggify.Check("sync.buildUVFileCards.minBudget", 0.10) {
		return nil, nil // skip all UV sends this round
	}

	var cards []xfer.Card
	budget := s.maxSend

	for name, sendFullContent := range s.uvToSend {
		if budget <= 0 {
			break
		}

		content, mtime, fileHash, err := uv.Read(s.repo.DB(), name)
		if err != nil {
			return nil, fmt.Errorf("buildUVFileCards: read %q: %w", name, err)
		}

		// Tombstone: send deletion marker.
		if fileHash == "" {
			cards = append(cards, &xfer.UVFileCard{
				Name:  name,
				MTime: mtime,
				Hash:  "-",
				Size:  0,
				Flags: xfer.UVFlagDeletion,
			})
			delete(s.uvToSend, name)
			continue
		}

		// §7.6: a file named by SHA3 goes to a server older than SHA3
		// support as the refusal in its place, with or without content,
		// as fossil's send_unversioned_file sends it; a tombstone above
		// names no hash and still goes.
		if sha3Barred(fileHash, s.peerVersion) {
			cards = append(cards, sha3Refusal())
			delete(s.uvToSend, name)
			continue
		}

		if sendFullContent && content != nil {
			cards = append(cards, &xfer.UVFileCard{
				Name:    name,
				MTime:   mtime,
				Hash:    fileHash,
				Size:    len(content),
				Flags:   0,
				Content: content,
			})
			budget -= len(content)
		} else {
			cards = append(cards, &xfer.UVFileCard{
				Name:  name,
				MTime: mtime,
				Hash:  fileHash,
				Size:  len(content),
				Flags: xfer.UVFlagContentOmitted,
			})
		}
		delete(s.uvToSend, name)
	}
	return cards, nil
}

// buildLoginCard appends a random comment to cards and computes the login card
// over the result. It returns the login card and the card list it signed, which
// the caller must send verbatim behind that login card — the server recomputes
// the nonce from those bytes.
func (s *session) buildLoginCard(cards []xfer.Card, projectCode string) (*xfer.LoginCard, []xfer.Card, error) {
	signed := append(cards[:len(cards):len(cards)], randomComment(s.env.Rand))
	payload, err := encodeCards(signed)
	if err != nil {
		return nil, nil, err
	}
	if projectCode == "" {
		return nil, nil, errNoProjectCode("login")
	}
	// BUGGIFY: corrupt the nonce payload to trigger auth failures.
	if s.opts.Buggify != nil && s.opts.Buggify.Check("sync.buildLoginCard.badNonce", 0.02) {
		payload = append(payload, []byte("BUGGIFY")...)
	}
	return computeLogin(s.opts.User, s.opts.Password, projectCode, payload), signed, nil
}

// processResponse handles all cards in a server response. It returns true
// when the sync has converged (nothing more to do).
//
// The reply is one verification pass (§7.7) and runs in one write
// transaction, as fossil's client_sync runs each round between
// db_begin_transaction and db_end_transaction: a delta stored dangling is
// checked if its basis arrives later in this reply. A file or cfile whose
// content does not match its name, found before storage or late, rolls the
// whole reply back, prefix included, and ends the session (§10.1's fail-fast
// path). A card error otherwise ends only the card loop: the prefix before
// it, with the phantoms, visibility and crosslink cascade it owes, commits
// (§10.1), and the error is returned.
func (s *session) processResponse(ctx context.Context, msg *xfer.Message) (bool, error) {
	if msg == nil {
		panic("sync.processResponse: msg must not be nil")
	}
	var done bool
	recvd := s.result.FilesRecvd
	cardErr, err := runPass(s.repo, func(bound *repo.Repo) (cardErr, err error) {
		defer s.bindRepo(bound)()
		done, cardErr, err = s.processReply(ctx, msg)
		return cardErr, err
	})
	s.dephantomized = s.dephantomized[:0]
	if err != nil {
		// The reply rolled back: what it counted is not stored.
		s.result.FilesRecvd = recvd
		return false, err
	}
	if cardErr != nil {
		return false, cardErr
	}
	return done, nil
}

// bindRepo points the session at bound, the repository bound to the current
// pass's transaction, so every read and write the reply makes joins it. It
// returns the function that points the session back.
func (s *session) bindRepo(bound *repo.Repo) func() {
	if bound == nil {
		panic("sync.bindRepo: bound must not be nil")
	}
	orig := s.repo
	s.repo = bound
	return func() { s.repo = orig }
}

// processReply is processResponse inside its transaction. cardErr is the
// card error that ended the card loop; err is a failure of the reply's
// closing steps.
func (s *session) processReply(ctx context.Context,
	msg *xfer.Message) (done bool, cardErr, err error) {
	var t replyTally
	s.pass = newReceivePass()
	defer func() { s.pass = nil }()
	for _, card := range msg.Cards {
		stop, err := s.handleReplyCard(ctx, card, &t)
		if err != nil {
			cardErr = err
			break
		}
		if stop {
			break
		}
	}
	if failsRound(cardErr) {
		// §10.1: an integrity error is a fail-fast path. Returned as err, it
		// rolls back the reply, prefix included, and the session starts no
		// further round. fossil stores the content and aborts at commit,
		// which reaches the same repository.
		return false, nil, cardErr
	}
	var content *contentError
	if errors.As(cardErr, &content) {
		// §10.1: a handler-reported content error that leaves the stored
		// artifacts consistent warns, commits the prefix and follows ordinary
		// continuation, as fossil's client does after nErr++ and break.
		s.result.Errors = append(s.result.Errors, "error: "+content.Error())
		cardErr = nil
	}
	if err := s.closeReply(ctx, &t); err != nil {
		return false, cardErr, err
	}
	if cardErr != nil {
		return false, cardErr, nil
	}
	s.agePhantoms()
	done, err = s.converged(t)
	return done, nil, err
}

// closeReply records what the reply's cards buffered: the visibility igot
// cards announced, the crosslink cascade of the phantoms it filled, and the
// phantoms it learned of. It runs after a card error too, since the prefix
// before the error commits (#418).
func (s *session) closeReply(ctx context.Context, t *replyTally) error {
	if err := s.flushVisibilityUpdates(); err != nil {
		return fmt.Errorf("sync: record artifact visibility: %w", err)
	}
	// The phantoms this reply filled are crosslinked together, before the
	// phantoms that crosslinking creates are loaded below.
	manifest.AfterDephantomizeBatch(ctx, s.repo, s.dephantomized)
	s.dephantomized = s.dephantomized[:0]
	created, err := s.flushNewPhantoms()
	if err != nil {
		return fmt.Errorf("sync: record phantoms: %w", err)
	}
	t.newPhantoms = created
	// Phantoms the round's storage created (a delta's basis, a cluster's
	// members) join the session's, once per reply rather than per file.
	if err := s.loadDBPhantoms(); err != nil {
		return fmt.Errorf("sync: load phantoms: %w", err)
	}
	s.result.FilesRecvd += t.filesRecvd
	return nil
}

// unknownCommand answers a reply card fossil's client does not recognize,
// such as a gimme or igot whose HASH fails blob_is_hname: the line is
// reported as unknown command: [LINE] and the reply-card loop stops (§5.2).
// It reports stop for handleReplyCard to return.
func (s *session) unknownCommand(c xfer.Card) bool {
	line := strings.TrimRight(replyOnlyLine(c), "\n")
	s.result.Errors = append(s.result.Errors, "unknown command: ["+line+"]")
	return true
}

// validFileNames reports whether a file or cfile card names its artifact, and
// its delta basis when it has one, with artifact names. fossil's
// xfer_accept_file refuses any other (blob_is_hname) before storing.
func validFileNames(uuid, deltaSrc string) bool {
	return hash.IsValidHash(uuid) && (deltaSrc == "" || hash.IsValidHash(deltaSrc))
}

// malformedFile records fossil's framing error for a file or cfile card with
// a bad name and stops the reply loop (§5.2, SP-5.2-8): the prefix already
// processed stays effective, as it does after an unknown command.
func (s *session) malformedFile(compressed bool) bool {
	s.result.Errors = append(s.result.Errors, malformedFileLine(compressed))
	return true
}

// replyTally counts what one reply carried, for the convergence check.
type replyTally struct {
	filesRecvd   int // file and cfile cards received
	filesSent    int // files queued for the next request on the server's gimme
	newPhantoms  int // phantoms an igot created
	privateIGots int // igot HASH 1 answers queued for the server's gimme
}

// handleReplyCard acts on one reply card. It reports stop when the card ends
// the reply-card loop: the cards before it stand and those after it are not
// read.
//
// §5.2: a card the decoder could not read (an unrecognized or wrong-arity
// line, or a file/cfile/uvfile it could not frame) ends the loop. fossil's
// client sets xfer.err and breaks. The error is recorded, as fossil reports
// it: a reply cut short by a bad card must not look like a complete sync.
func (s *session) handleReplyCard(ctx context.Context, card xfer.Card,
	t *replyTally) (bool, error) {
	switch c := card.(type) {
	case *xfer.BadCommandCard:
		s.result.Errors = append(s.result.Errors,
			"unknown command: ["+strings.TrimRight(c.Line, "\n")+"]")
		return true, nil
	case *xfer.BadFileCard:
		s.result.Errors = append(s.result.Errors, "error: "+c.Message)
		return true, nil
	case *xfer.ErrorCard:
		// §10.1: an error card ends the reply-card loop. The message is
		// recorded first so it still surfaces. (A clone's first round
		// tolerates an error card; that path is cloneSession.serverError.)
		s.result.Errors = append(s.result.Errors, "error: "+c.Message)
		return true, nil
	case *xfer.PrivateCard:
		s.nextIsPrivate = true
	case *xfer.FileCard:
		if !validFileNames(c.UUID, c.DeltaSrc) {
			return s.malformedFile(false), nil
		}
		// Uncompressed cards never carry a wire-encoded frame to
		// preserve — storage always compresses payload itself.
		return false, s.receiveFile(ctx, c.UUID, c.DeltaSrc, c.Content, nil, t)
	case *xfer.CFileCard:
		if !validFileNames(c.UUID, c.DeltaSrc) {
			return s.malformedFile(true), nil
		}
		return false, s.receiveFile(ctx, c.UUID, c.DeltaSrc, c.Content, c.StoredBlob, t)
	case *xfer.IGotCard:
		if !hash.IsValidHash(c.UUID) {
			return s.unknownCommand(c), nil
		}
		return false, s.handleIGot(c)
	case *xfer.GimmeCard:
		if !hash.IsValidHash(c.UUID) {
			return s.unknownCommand(c), nil
		}
		// A session that is not pushing never sends files, so a gimme from
		// it could not be satisfied and would hold the pull open forever
		// (#277). A "pull only" message (§10.2) disables pushing mid-reply,
		// so a gimme after it is not answered either.
		if s.pushing() {
			s.queueGimme(c.UUID, t)
		}
	case *xfer.CookieCard:
		s.cookie = c.Value
	case *xfer.MessageCard:
		s.handleMessage(c)
	case *xfer.PragmaCard:
		if c.Name == "link" {
			return false, s.handleLink(c)
		}
		s.handleReplyPragma(c)
	case *xfer.UVIGotCard, *xfer.UVFileCard, *xfer.UVGimmeCard:
		return false, s.handleReplyUVCard(card)
	case *xfer.SchemaCard, *xfer.XIGotCard, *xfer.XGimmeCard, *xfer.XRowCard, *xfer.XDeleteCard:
		return false, s.processXTableCard(card)
	}
	return false, nil
}

// receiveFile stores a received file or cfile and counts it.
func (s *session) receiveFile(ctx context.Context, uuid, deltaSrc string,
	payload, storedBlob []byte, t *replyTally) error {
	stored, err := s.handleFileCard(ctx, uuid, deltaSrc, payload, storedBlob,
		s.takeVisibility())
	if err != nil || !stored {
		return err // a shunned file is dropped, and not counted as received
	}
	t.filesRecvd++
	s.roundStats.BytesReceived += int64(len(payload))
	delete(s.phantoms, uuid)
	return nil
}

// handleIGot records that the server holds an artifact: a held one has its
// visibility brought in line, a missing one becomes a phantom when pulling.
func (s *session) handleIGot(c *xfer.IGotCard) error {
	s.remoteHas[c.UUID] = true
	rid, exists := blob.Exists(s.repo.DB(), c.UUID)
	switch {
	case exists:
		// Buffered, not written here: a response announcing tens of
		// thousands of artifacts would otherwise cost one implicit
		// transaction per card (issue #200).
		s.visibilityUpdates = append(s.visibilityUpdates,
			visibilityUpdate{rid: rid, vis: visibility(c.IsPrivate)})
	case c.IsPrivate:
		if s.opts.Private && s.opts.Pull {
			return s.addPhantom(c.UUID, visibilityPrivate)
		}
	default:
		if s.opts.Pull {
			return s.addPhantom(c.UUID, visibilityPublic)
		}
	}
	return nil
}

// handleMessage records a message card. §10.2: "pull only ..." tells a
// pushing client to stop sending content, matching fossil's glob "pull only
// *". Every message is still recorded: a caller that asked to push must be
// able to see that the server refused it, not just a quietly shorter sync.
func (s *session) handleMessage(c *xfer.MessageCard) {
	if s.pushing() && strings.HasPrefix(c.Message, "pull only ") {
		s.pushStopped = true
	}
	s.result.Errors = append(s.result.Errors, "message: "+c.Message)
}

// handleReplyPragma acts on the pragmas a server sends a sync client.
func (s *session) handleReplyPragma(c *xfer.PragmaCard) {
	switch {
	case c.Name == "server-version" && len(c.Values) >= 1:
		s.peerVersion = peerVersion(c)
		s.peerDate = peerDate(c)
		// §6.5: once the server's release passes the gate, later requests
		// may carry their login in the x-f-l-c cookie.
		if loginCookieGate(c) {
			s.loginCookieOK = true
		}
	case c.Name == "uv-push-ok":
		s.uvPushOK = true
	case c.Name == "uv-pull-only":
		s.uvPullOnly = true
	case c.Name == coneServedPragma:
		// Sparse-cone §6.3: the peer applied this request's cone. Every
		// reply is filtered locally regardless; this only reports the mode.
		s.result.ConeServed++
	case c.Name == horizonServedPragma && servesContentHorizon(c):
		s.result.HorizonServed++
	case c.Name == sendSubmanifestsPragma:
		// draft-fossil-submanifests-00 §7.2: the server reads trees. Latched
		// for the session: a reply without the pragma does not unsay it.
		s.peerReadsTrees = true
	case c.Name == avoidSubmanifestsPragma:
		// draft-fossil-submanifests-00 §7.2: record the exchange-local
		// avoidance hint. Sync makes no commit; this does not bind a later one.
		s.result.AvoidSubmanifests = true
	case c.Name == "avoid-delta-manifests":
		// §9.5: a hint for the commit that asked for the lock, to write a
		// baseline manifest. It changes nothing about the transfer, and no
		// commit is pending here: go-libfossil commits without an autosync.
	case c.Name == "ci-lock-fail" && len(c.Values) >= 2:
		mtime, _ := strconv.ParseInt(c.Values[1], 10, 64)
		s.result.CkinLockFail = &CkinLockFail{
			HeldBy: deck.FossilDecode(c.Values[0]),
			Since:  time.Unix(mtime, 0),
		}
	}
}

// handleReplyUVCard acts on a uvigot, uvfile or uvgimme card when the session
// syncs unversioned files, and ignores it otherwise.
func (s *session) handleReplyUVCard(card xfer.Card) error {
	if !s.opts.UV {
		return nil
	}
	switch c := card.(type) {
	case *xfer.UVIGotCard:
		return s.handleUVIGotCard(c)
	case *xfer.UVFileCard:
		if err := s.handleUVFileCard(c); err != nil {
			return err
		}
		s.nUvFileRcvd++
		s.result.UVFilesRecvd++
	case *xfer.UVGimmeCard:
		if s.uvToSend == nil {
			s.uvToSend = make(map[string]bool)
		}
		s.uvToSend[c.Name] = true
	}
	return nil
}

// agePhantoms ages the phantoms still unresolved after a reply and evicts
// those asked for phantomEvictRounds rounds running. The file handlers
// already delete resolved phantoms, so anything still in the map was not
// delivered this round.
func (s *session) agePhantoms() {
	for uuid := range s.phantoms {
		s.phantomAge[uuid]++
		if s.phantomAge[uuid] >= phantomEvictRounds {
			delete(s.phantoms, uuid)
			delete(s.phantomAge, uuid)
		}
	}
	for uuid := range s.phantomAge {
		if !s.phantoms[uuid] {
			delete(s.phantomAge, uuid)
		}
	}
}

// converged reports whether the session is done after a reply, by §3.7's
// continuation rules and the client's own unversioned and table sync work.
func (s *session) converged(t replyTally) (bool, error) {
	// §3.7 (a): stored content or a new phantom continues the session only
	// while the phantom table is not empty. Remaining phantoms alone, or
	// content alone, do not.
	if t.filesRecvd > 0 || t.newPhantoms > 0 {
		var one int
		err := s.repo.DB().QueryRow("SELECT 1 FROM phantom LIMIT 1").Scan(&one)
		if err == nil {
			// fossil's client_sync assigns mxPhantomReq only on this path
			// (src/xfer.c): max(200, 2*files received). A round that
			// continues for any other reason leaves it as it was.
			s.mxPhantomReq = max(MaxGimmeBase, 2*t.filesRecvd)
			return false, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("sync: check phantoms: %w", err)
		}
	}
	// §3.7 (b): files are queued for the next request.
	if t.filesSent > 0 || len(s.pendingSend) > 0 {
		return false, nil
	}
	// §3.7 (c): the first reply was answered with an igot HASH 1, which the
	// server can only act on in another round. s.result.Rounds still counts
	// the rounds before this reply's, so 0 here is fossil's nCycle==1.
	if t.privateIGots > 0 && s.result.Rounds == 0 {
		return false, nil
	}

	// If we sent igot cards but the server didn't gimme anything,
	// clear the unsent table — those artifacts have been announced
	// and the server either has them or doesn't want them.
	if s.igotSentThisRound > 0 && t.filesSent == 0 {
		if _, err := s.repo.DB().Exec("DELETE FROM unsent"); err != nil {
			return false, fmt.Errorf("sync: delete unsent: %w", err)
		}
	}

	var unsentCount int
	err := s.repo.DB().QueryRow("SELECT count(*) FROM unsent").Scan(&unsentCount)
	if err != nil {
		return false, fmt.Errorf("checking unsent: %w", err)
	}
	if unsentCount > 0 {
		return false, nil
	}
	if s.uvPending() {
		return false, nil
	}
	return !s.tablesPending(), nil
}

// uvPending reports whether unversioned sync has work left, resetting the
// per-round counters it judges by. It runs before the round count is
// stored, so Rounds is the number of rounds before this one: fossil's
// post-increment nCycle<3 (§3.7(d)) is Rounds+1 < 3, that is Rounds < 2.
func (s *session) uvPending() bool {
	if s.opts.UV && s.nUvGimmeSent > 0 && (s.nUvFileRcvd > 0 || s.result.Rounds < 2) {
		s.nUvGimmeSent = 0
		s.nUvFileRcvd = 0
		return true
	}
	if s.opts.UV && len(s.uvToSend) > 0 {
		if s.uvPushOK {
			return true
		}
		// Server did not grant uv-push-ok — UV hashes match,
		// nothing to push. Clear stale uvToSend from prior rounds.
		s.uvToSend = nil
	}
	if s.opts.UV && len(s.uvGimmes) > 0 {
		return true
	}
	s.nUvGimmeSent = 0
	s.nUvFileRcvd = 0
	return false
}

// tablesPending reports whether x-table sync has rows left to ask for or send.
func (s *session) tablesPending() bool {
	if !s.opts.XTableSync {
		return false
	}
	for _, gimmes := range s.xTableGimmes {
		if len(gimmes) > 0 {
			return true
		}
	}
	for _, sends := range s.xTableToSend {
		if len(sends) > 0 {
			return true
		}
	}
	return false
}

// storeReceivedFile validates and stores a received file/cfile blob. It is
// used by the clone, sync-pull, and server-push receive paths alike — the
// single place that decides how a received card becomes a stored row, so
// the three callers can't drift out of sync with each other the way they
// previously did (issue #57).
//
// A delta is always stored as delta bytes, linked to its source via a
// create-phantom-if-missing lookup, regardless of whether that source is
// currently available: Fossil's steady state during a transfer is a delta
// arriving before its base (see content_put_ex, src/content.c:557-620),
// so receive time never has to decide whether the content can be expanded
// yet. Availability is a live, transitively-recomputed property —
// content.IsAvailable/AvailableByUUID walk the delta chain fresh on every
// call — so nothing here needs to notify anything when a base fills in.
//
// Because the raw bytes are trusted at receive time and not everyone who
// can construct a delta card also controls the base it claims, content.Expand
// verifies the expanded result hashes to its claimed UUID on every
// expansion: a delta's own internal checksum only proves the bytes are
// self-consistent with what the sender transmitted, not that they match
// the name they were received under.
//
// storedBlob, when non-nil, is payload already re-expressed in Fossil's
// on-disk blob format by the xfer decoder (see xfer.CFileCard.StoredBlob):
// bytes the sender itself would have stored, handed to us intact. Passing
// it through to storage instead of recompressing payload is what makes a
// clone byte-identical to its source rather than merely content-identical.
// Callers with no such bytes on hand (payload built locally, or a bare
// "file" card that never carried a compressed wire frame) pass nil, and
// storage falls back to compressing payload itself.
//
// pass is the verification pass the card belongs to (see receivePass). A
// late hash mismatch, found in a delta this pass stored earlier, aborts the
// pass, whose transaction (runPass) rolls back this card's store with the
// rest, so it owes no cascade.
func storeReceivedFile(ctx context.Context, r *repo.Repo, pass *receivePass, uuid, deltaSrc string,
	payload []byte, storedBlob []byte, vis visibility, linker *manifest.ReceiveLinker) error {
	dephantomizedRid, err := storeReceived(ctx, r, pass, uuid, deltaSrc, payload, storedBlob,
		vis, linker)
	// General sync and push paths preserve Fossil's after_dephantomize
	// cascade, in the pass's transaction. A clone uses its ReceiveLinker
	// inside the storage transaction instead, so it must not run a second.
	if linker == nil && dephantomizedRid > 0 {
		manifest.AfterDephantomize(ctx, r, dephantomizedRid)
	}
	return err
}

// storeReceived is storeReceivedFile without the after-dephantomize cascade:
// it returns the rid of a phantom it filled, for which the caller owes the
// cascade, with or without an error. The sync client runs those cascades once
// per reply, so a pull that fills many phantoms does not commit one crosslink
// transaction per artifact.
func storeReceived(ctx context.Context, r *repo.Repo, pass *receivePass, uuid, deltaSrc string,
	payload []byte, storedBlob []byte, vis visibility,
	linker *manifest.ReceiveLinker) (libfossil.FslID, error) {
	if r == nil {
		panic("storeReceivedFile: r must not be nil")
	}
	if pass == nil {
		panic("storeReceivedFile: pass must not be nil")
	}
	if uuid == "" {
		panic("storeReceivedFile: uuid must not be empty")
	}
	if payload == nil {
		panic("storeReceivedFile: payload must not be nil")
	}
	if !hash.IsValidHash(uuid) {
		return 0, fmt.Errorf("sync: invalid UUID format: %s", uuid)
	}
	if deltaSrc != "" {
		// deltaSrc becomes a blob.StorePhantom row under whatever string
		// arrives on the wire. The blob table's own CHECK constraint only
		// bounds uuid length, not hex-ness, so a non-hex value of valid
		// length (e.g. 40 'z' characters) passes it and would otherwise
		// create a permanent, unfillable phantom row that loadDBPhantoms
		// re-requests every sync round forever.
		if !hash.IsValidHash(deltaSrc) {
			return 0, fmt.Errorf("sync: invalid delta source UUID format: %s", deltaSrc)
		}
		// Section 7.7 (SP-7.7-4): a delta whose basis is held is applied and
		// checked against HASH before anything is stored, for the client,
		// the server and a clone alike. fossil's commit hook catches a cfile
		// delta, and a clone's, that xfer_accept_file does not pre-verify.
		// One whose basis is not held yet is stored dangling and checked by
		// the pass once the basis arrives.
		verdict, target, err := checkDelta(r.DB(), uuid, deltaSrc, payload)
		if err != nil {
			return 0, fmt.Errorf("received file %s: %w", uuid, err)
		}
		if verdict == deltaMismatch {
			return 0, &hashMismatchError{uuid: uuid}
		}
		return 0, storeDeltaContent(ctx, r, pass, uuid, deltaSrc, payload, storedBlob, vis,
			linker, target)
	}

	return storeResolvedContent(ctx, r, pass, uuid, payload, storedBlob, vis, linker)
}

// storeDeltaContent persists a received delta exactly as given — never
// expanded here; storeReceived has already verified it against its HASH
// when the basis is held (verified) — linked to its source via a
// create-phantom-if-missing lookup (mirrors Fossil's rid_from_uuid(&src,
// 1, ...) in src/xfer.c). The delta table's rid->srcid link is recorded
// unconditionally, matching content_put_ex's REPLACE INTO delta, whether
// or not srcRid is itself currently available.
//
// An unverified delta joins pass, to be checked when its basis arrives. A
// verified one -- target holds the content checkDelta rebuilt -- is
// available now, so the deltas pass holds against it are checked here, and a
// late mismatch among them aborts the pass.
func storeDeltaContent(ctx context.Context, r *repo.Repo, pass *receivePass, uuid,
	deltaSrc string, payload []byte, storedBlob []byte, vis visibility,
	linker *manifest.ReceiveLinker, target []byte) error {
	verified := target != nil
	var dangling, root libfossil.FslID // tracked once the store succeeds
	err := withLinkerTx(r, linker, func(tx *db.Tx) error {
		alreadyHeld, err := heldReal(tx, uuid)
		if err != nil {
			return err
		}
		_, held := blob.Exists(tx, deltaSrc)
		srcRid, err := blob.StorePhantom(tx, deltaSrc)
		if err != nil {
			return fmt.Errorf("storeDeltaContent: resolve base %s: %w", deltaSrc, err)
		}
		// A basis first seen as a private card's delta source is a private
		// phantom, as fossil's content_new makes it (§7.4): it must not be
		// announced or asked for as public content.
		if !held && vis == visibilityPrivate {
			if err := content.MakePrivate(tx, int64(srcRid)); err != nil {
				return fmt.Errorf("storeDeltaContent: private base %s: %w",
					deltaSrc, err)
			}
		}
		rid, err := blob.StoreDeltaRaw(tx, uuid, payload, srcRid, storedBlob)
		if err != nil {
			return fmt.Errorf("storeDeltaContent: %w", err)
		}
		if err := recordVisibility(tx, rid, vis); err != nil {
			return err
		}
		if linker != nil {
			if err := linker.LinkStored(ctx, tx, rid, uuid, nil); err != nil {
				return fmt.Errorf("storeDeltaContent: link %s: %w", uuid, err)
			}
		} else if verified {
			if err := noteReceived(tx, rid, target); err != nil {
				return fmt.Errorf("storeDeltaContent: %s: %w", uuid, err)
			}
		}
		switch {
		case alreadyHeld:
			return nil
		case verified:
			return pass.verifyFrom(tx, rid)
		}
		if root, err = pass.rootOf(tx, srcRid); err != nil {
			return fmt.Errorf("storeDeltaContent: %w", err)
		}
		dangling = rid
		return nil
	})
	if err == nil && dangling > 0 {
		pass.track(dangling, root, uuid)
	}
	return err
}

// noteReceived does for a received artifact, given its full content, what
// storing an S-bearing artifact owes the repository (manifest.NoteStoredArtifact);
// content that is no artifact owes nothing.
func noteReceived(tx *db.Tx, rid libfossil.FslID, full []byte) error {
	d, err := deck.Parse(full)
	if err != nil {
		return nil
	}
	return manifest.NoteStoredArtifact(tx, rid, d)
}

// withLinkerTx is r.WithTx for a write that may call linker.LinkStored: a
// failed transaction also drops the leaf rechecks it scheduled. A nil linker
// schedules nothing.
func withLinkerTx(r *repo.Repo, linker *manifest.ReceiveLinker,
	fn func(tx *db.Tx) error) error {
	if linker == nil {
		return r.WithTx(fn)
	}
	return linker.WithTx(r, fn)
}

// visibilityUpdate is one artifact whose private/public status an igot card
// announced, buffered until the round's single visibility transaction.
type visibilityUpdate struct {
	rid libfossil.FslID
	vis visibility
}

// visibility is whether a received artifact is private -- a private card
// preceded it on the wire -- or public.
type visibility bool

const (
	visibilityPublic  visibility = false
	visibilityPrivate visibility = true
)

// recordVisibility writes an artifact's private/public status. It takes a
// Querier rather than a *repo.Repo so it runs inside the transaction that
// stored the artifact: on the pool it would be a separate implicit
// transaction per artifact, each with its own fsync and its own contest for
// the write lock (issue #200), and the status would not be atomic with the
// content it describes.
func recordVisibility(q db.Querier, rid libfossil.FslID, vis visibility) error {
	if rid <= 0 {
		return nil
	}
	if vis == visibilityPrivate {
		if err := content.MakePrivate(q, int64(rid)); err != nil {
			return fmt.Errorf("record private rid=%d: %w", rid, err)
		}
		return nil
	}
	if err := content.MakePublic(q, int64(rid)); err != nil {
		return fmt.Errorf("record public rid=%d: %w", rid, err)
	}
	return nil
}

// errHashMismatch is a received artifact whose content is not its name.
var errHashMismatch = errors.New("wrong hash: content does not match its name")

// hashMatches reports whether data hashes to uuid under the algorithm its
// length names: 40 hex digits SHA1, 64 SHA3-256.
func hashMatches(uuid string, data []byte) bool {
	if len(uuid) > 40 {
		return hash.SHA3(data) == uuid
	}
	return hash.SHA1(data) == uuid
}

// storeResolvedContent verifies that fullContent hashes to uuid and stores
// it. "Verify, then store resolved content" is a single cohesive unit —
// full (non-delta) file cards always carry content that can be hashed
// immediately, so this stays eager rather than deferring to content.Expand's
// check the way storeDeltaContent's delta bytes must. Fills a phantom row
// in place if one already exists for uuid.
//
// Returns the rid of a phantom that was just filled, so a non-linker caller
// can run AfterDephantomize after this store, in the same pass; returns 0
// when no phantom was filled (blob already real, or newly inserted).
//
// Full content is available once stored, so the deltas pass holds against
// it are checked here too; a late mismatch among them aborts the pass.
func storeResolvedContent(ctx context.Context, r *repo.Repo, pass *receivePass, uuid string,
	fullContent []byte, storedBlob []byte, vis visibility,
	linker *manifest.ReceiveLinker) (libfossil.FslID, error) {
	if !hashMatches(uuid, fullContent) {
		return 0, &hashMismatchError{uuid: uuid}
	}

	var dephantomizedRid libfossil.FslID
	err := withLinkerTx(r, linker, func(tx *db.Tx) error {
		rid, filled, err := storeResolvedTx(ctx, tx, uuid, fullContent, storedBlob, vis,
			linker)
		if err != nil {
			return err
		}
		if filled {
			dephantomizedRid = rid
		}
		return pass.verifyFrom(tx, rid)
	})
	if err != nil {
		return 0, err
	}
	return dephantomizedRid, nil
}

// storeResolvedTx is storeResolvedContent's write, inside its transaction. It
// returns the rid holding uuid, and filled when that rid was a phantom this
// store filled.
func storeResolvedTx(ctx context.Context, tx *db.Tx, uuid string, fullContent []byte,
	storedBlob []byte, vis visibility,
	linker *manifest.ReceiveLinker) (rid libfossil.FslID, filled bool, err error) {
	// A received SHA3 name promotes an auto-policy repo, as in fossil.
	if err := hash.PromoteFor(tx, uuid); err != nil {
		return 0, false, fmt.Errorf("storeResolvedContent: hash policy: %w", err)
	}
	rid, filled, inserted, err := writeResolved(tx, uuid, fullContent, storedBlob)
	if err != nil {
		return 0, false, err
	}
	if err := recordVisibility(tx, rid, vis); err != nil {
		return 0, false, err
	}
	if linker != nil {
		if err := linker.LinkStored(ctx, tx, rid, uuid, fullContent); err != nil {
			return 0, false, fmt.Errorf("storeResolvedContent: link %s: %w", uuid, err)
		}
		return rid, filled, nil
	}
	if !inserted {
		return rid, filled, nil
	}
	// Crosslink cluster artifacts: parse the content and if it's a
	// cluster manifest, process its M-card UUIDs to create phantoms
	// for blobs we don't have yet. This is how the client discovers
	// individual blob UUIDs referenced by a cluster.
	if d, parseErr := deck.Parse(fullContent); parseErr == nil {
		if d.Type == deck.Cluster {
			if err := manifest.CrosslinkCluster(tx, rid, d); err != nil {
				return 0, false, fmt.Errorf("crosslink cluster %s: %w", uuid, err)
			}
		}
		// A tree arms the §7.2 gate as it is stored, not at the session's
		// closing sweep.
		if err := manifest.NoteStoredArtifact(tx, rid, d); err != nil {
			return 0, false, fmt.Errorf("received %s: %w", uuid, err)
		}
	}
	return rid, filled, nil
}

// writeResolved writes verified full content under uuid: a real row already
// there is left as it is, a phantom is filled in place, and otherwise a row
// is inserted. It reports which of the last two it did.
func writeResolved(tx *db.Tx, uuid string, fullContent []byte,
	storedBlob []byte) (rid libfossil.FslID, filled, inserted bool, err error) {
	existingRid, exists := blob.Exists(tx, uuid)
	if exists {
		var size int64
		tx.QueryRow("SELECT size FROM blob WHERE rid=?", existingRid).Scan(&size)
		if size != -1 {
			// Real blob already exists; only its visibility can have moved.
			return existingRid, false, false, nil
		}
	}
	compressed, err := blob.EncodeForStorage(fullContent, storedBlob)
	if err != nil {
		return 0, false, false, err
	}
	rcvid, err := db.ReceiptID(tx)
	if err != nil {
		return 0, false, false, err
	}
	if exists {
		// Fill phantom: update blob content, remove from phantom table.
		if _, err := tx.Exec("UPDATE blob SET size=?, content=?, rcvid=? WHERE rid=?",
			len(fullContent), compressed, rcvid, existingRid); err != nil {
			return 0, false, false, err
		}
		if _, err := tx.Exec("DELETE FROM phantom WHERE rid=?", existingRid); err != nil {
			return 0, false, false, fmt.Errorf("delete phantom rid=%d: %w", existingRid, err)
		}
		rid = existingRid
		filled = true
	} else {
		result, err := tx.Exec(
			"INSERT INTO blob(uuid, size, content, rcvid) VALUES(?, ?, ?, ?)",
			uuid, len(fullContent), compressed, rcvid)
		if err != nil {
			return 0, false, false, err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return 0, false, false, err
		}
		rid = libfossil.FslID(id)
	}
	if _, err := tx.Exec("INSERT OR IGNORE INTO unclustered(rid) VALUES(?)", rid); err != nil {
		return 0, false, false, fmt.Errorf("unclustered rid=%d: %w", rid, err)
	}
	return rid, filled, !filled, nil
}

// seedPhantoms asks, in the first round, for the phantoms the repository
// already holds, as fossil's client does (request_phantoms runs every round
// of a pull): an interrupted clone or sync leaves them for this one. They
// are asked for once: a phantom the server cannot supply must not hold the
// session open (draft §3.7, "Remaining phantoms alone MUST NOT continue"),
// so each starts one round short of eviction. One the server later
// announces with igot is a phantom of this session like any other.
func (s *session) seedPhantoms() error {
	if len(s.phantoms) != 0 {
		panic("session.seedPhantoms: phantoms already loaded")
	}
	if err := s.loadDBPhantoms(); err != nil {
		return err
	}
	for uuid := range s.phantoms {
		s.phantomAge[uuid] = phantomEvictRounds - 1
	}
	return nil
}

// addPhantom makes uuid, which an igot announced, a phantom unless it is
// shunned: as fossil's content_new makes no phantom of a shunned name, the
// client never asks for one (§3.3, §7.7). The phantom goes in the session's
// set and, at the end of the reply, in the phantom table, which persists it
// for a later session (Table 1).
func (s *session) addPhantom(uuid string, vis visibility) error {
	if uuid == "" {
		panic("session.addPhantom: empty uuid")
	}
	shunned, err := shun.IsShunned(s.repo.DB(), uuid)
	if err != nil {
		return err
	}
	if !shunned {
		s.phantoms[uuid] = true
		s.newPhantoms = append(s.newPhantoms, phantomUpdate{uuid: uuid, vis: vis})
	}
	return nil
}

// phantomUpdate is one phantom an igot announced, buffered until the reply's
// single phantom transaction.
type phantomUpdate struct {
	uuid string
	vis  visibility
}

// flushNewPhantoms writes the reply's igot phantoms to the phantom table in
// one transaction, as fossil's content_new does for each: a private one is
// marked private. A name stored since its igot is skipped. It returns how
// many phantoms it created, which §3.7 (a) counts as progress.
func (s *session) flushNewPhantoms() (int, error) {
	if len(s.newPhantoms) == 0 {
		return 0, nil
	}
	updates := s.newPhantoms
	s.newPhantoms = s.newPhantoms[:0]
	created := 0
	err := s.repo.WithTx(func(tx *db.Tx) error {
		created = 0
		for _, u := range updates {
			if _, held := blob.Exists(tx, u.uuid); held {
				continue
			}
			rid, err := blob.StorePhantom(tx, u.uuid)
			if err != nil {
				return err
			}
			if u.vis == visibilityPrivate {
				if err := content.MakePrivate(tx, int64(rid)); err != nil {
					return err
				}
			}
			created++
		}
		return nil
	})
	return created, err
}

// queueGimme queues the file a server's gimme asks for when the client can
// answer it, or, for SHA3 content the server is too old to accept, the
// refusal fossil's send_file appends in its place (§7.6); a refusal is not a
// file and does not continue the session. Like fossil's nGimmeRcvd, the count
// takes every gimme for an artifact the repository has a row for, a phantom
// or a private one included (xfer.c, rid_from_uuid before send_file).
func (s *session) queueGimme(uuid string, t *replyTally) {
	if uuid == "" {
		panic("session.queueGimme: empty uuid")
	}
	rid, held := blob.Exists(s.repo.DB(), uuid)
	if held {
		s.nGimmeRcvd++
	}
	// fossil's send_file checks privacy first, for any row: outside a
	// private sync, a peer dated 2020-04-13 or later is told the artifact
	// is private, and an older or undated one nothing (§8.6).
	if held && !s.opts.Private && content.IsPrivate(s.repo.DB(), int64(rid)) {
		if s.peerDate >= privateIGotDate {
			s.privateIGots = append(s.privateIGots, uuid)
			t.privateIGots++
		}
		return
	}
	if !s.canSend(uuid) {
		return
	}
	if sha3Barred(uuid, s.peerVersion) {
		s.sha3Refusals++
		return
	}
	if s.submanifestBarred(uuid, rid) {
		return
	}
	s.pendingSend[uuid] = true
	t.filesSent++
}

// submanifestBarred applies the draft-fossil-submanifests-00 §7.2 gate to a
// gimme: a tree or sub-manifest is withheld from a server that has sent no
// send-submanifests in this session. The withheld artifact is recorded in
// the result, so a push that left one behind does not read as complete, and
// the next request says why.
func (s *session) submanifestBarred(uuid string, rid libfossil.FslID) bool {
	barred, err := s.smGate.barred(s.repo.DB(), s.cache, rid, s.peerReadsTrees)
	if err != nil {
		s.result.Errors = append(s.result.Errors, err.Error())
		return true
	}
	if !barred {
		return false
	}
	s.smRefusals++
	s.result.Errors = append(s.result.Errors, fmt.Sprintf(
		"withheld %s: the server does not accept sub-manifests", uuid))
	return true
}

// canSend reports whether the client can answer a gimme for uuid: it holds
// the content, and the content is public or the session syncs private. A
// gimme it cannot answer is dropped, as fossil's send_file sends nothing for
// it, so it neither holds the session open nor costs a file card (§3.3).
func (s *session) canSend(uuid string) bool {
	rid, ok := content.AvailableByUUID(s.repo.DB(), uuid)
	if !ok {
		return false
	}
	return s.opts.Private || !content.IsPrivate(s.repo.DB(), int64(rid))
}

// phantomEvictRounds is how many consecutive rounds a phantom is asked for
// without arriving before the session stops asking.
const phantomEvictRounds = 3

// loadDBPhantoms promotes phantom-table entries into the session's phantom
// map. This is needed after crosslinking cluster artifacts which create
// phantom rows for blobs referenced in the cluster. A sparse store's elided
// phantoms are left out: synchronization never requests one spontaneously
// (draft-fossil-sparse-cone-00 §5.2).
func (s *session) loadDBPhantoms() error {
	if !s.opts.Pull {
		return nil
	}
	elided, err := elidedPhantoms(s.ctx, s.repo.DB())
	if err != nil {
		return err
	}
	rows, err := s.repo.DB().Query(
		"SELECT b.rid, b.uuid FROM phantom p JOIN blob b ON p.rid = b.rid WHERE b.size < 0" +
			" AND NOT EXISTS(SELECT 1 FROM shun WHERE uuid=b.uuid)",
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var rid libfossil.FslID
		var uuid string
		if err := rows.Scan(&rid, &uuid); err != nil {
			return err
		}
		if elided != nil && elided.Class(rid) == content.Elided {
			delete(s.phantoms, uuid)
			delete(s.phantomAge, uuid)
			continue
		}
		if !s.remoteHas[uuid] {
			s.phantoms[uuid] = true
		}
	}
	return rows.Err()
}

// elidedPhantoms classifies a sparse store's phantoms, or is nil for a dense
// store, which has nothing elided and so pays for no classification.
func elidedPhantoms(ctx context.Context, q db.Querier) (*content.Absences, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	storeCone, err := content.StoreCone(q)
	if err != nil || storeCone == nil {
		return nil, err
	}
	return content.LoadAbsencesContext(ctx, q)
}

// handleFileCard stores a received file (or delta-file) into the repo.
// If the stored artifact is a cluster manifest, crosslinking creates DB
// phantoms for referenced blobs; we promote those to session phantoms so
// gimme cards are emitted in the next round.
//
// storedBlob carries a cfile card's already wire-encoded bytes through to
// storage (see storeReceivedFile); it is nil for uncompressed "file" cards,
// which never carry a compressed frame to preserve.
//
// It reports whether the file was stored: a shunned artifact is dropped on
// receipt, with no error, as fossil's xfer_accept_file and
// xfer_accept_compressed_file drop it (§7.7).
func (s *session) handleFileCard(ctx context.Context, uuid, deltaSrc string, payload []byte,
	storedBlob []byte, vis visibility) (bool, error) {
	shunned, err := shun.IsShunned(s.repo.DB(), uuid)
	if err != nil || shunned {
		return false, err
	}
	// ctx is the sync loop's own deadline context, threaded down from
	// session.Sync through processResponse (issue #167). It reaches the
	// mid-round phantom-fill crosslink cascade inside storeReceivedFile, so a
	// long-running cascade on the general push/pull path is interruptible by the
	// caller's deadline -- the same guarantee #166 gave the clone and
	// server-push receive paths.
	if s.pass == nil {
		panic("sync.handleFileCard: pass must be set by processResponse")
	}
	if servedCluster(s.servedHorizon(), deltaSrc, payload) {
		return false, nil
	}
	if err := rememberSparseReceived(s.repo.DB(), s.sparseReceived, uuid); err != nil {
		return false, err
	}
	rid, err := storeReceived(ctx, s.repo, s.pass, uuid, deltaSrc, payload, storedBlob, vis,
		nil)
	if rid > 0 {
		s.dephantomized = append(s.dephantomized, rid)
	}
	if err != nil {
		return false, err
	}

	// BUGGIFY: simulate post-store failure to test retry/recovery logic.
	if s.opts.Buggify != nil && s.opts.Buggify.Check("sync.handleFileCard.reject", 0.03) {
		return false, fmt.Errorf("buggify: simulated storage failure for %s", uuid)
	}

	return true, nil
}

// takeVisibility consumes the pending private-card flag and reports the
// visibility the next file or cfile card is to be stored with. A private card
// applies to exactly one following card, so reading the flag clears it.
func (s *session) takeVisibility() visibility {
	if !s.nextIsPrivate {
		return visibilityPublic
	}
	s.nextIsPrivate = false
	// BUGGIFY: 3% chance the private status is dropped — the artifact lands
	// public and the next sync round should correct it.
	if s.opts.Buggify != nil && s.opts.Buggify.Check("sync.takeVisibility.skipMakePrivate", 0.03) {
		return visibilityPublic
	}
	return visibilityPrivate
}

// flushVisibilityUpdates writes the visibility changes announced by this
// round's igot cards, in one transaction. Buffering them is what keeps a round
// that hears about a large repository to a single write transaction instead of
// one per artifact.
func (s *session) flushVisibilityUpdates() error {
	if len(s.visibilityUpdates) == 0 {
		return nil
	}
	updates := s.visibilityUpdates
	s.visibilityUpdates = s.visibilityUpdates[:0]
	return s.repo.WithTx(func(tx *db.Tx) error {
		for _, u := range updates {
			if err := recordVisibility(tx, u.rid, u.vis); err != nil {
				return err
			}
		}
		return nil
	})
}

// handleUVIGotCard processes a uvigot card from the server.
func (s *session) handleUVIGotCard(c *xfer.UVIGotCard) error {
	if c == nil {
		panic("session.handleUVIGotCard: c must not be nil")
	}
	if err := uv.EnsureSchema(s.repo.DB()); err != nil {
		return fmt.Errorf("handleUVIGotCard: ensure schema: %w", err)
	}

	_, localMtime, localHash, err := uv.Read(s.repo.DB(), c.Name)
	if err != nil {
		return fmt.Errorf("handleUVIGotCard: read %q: %w", c.Name, err)
	}

	status := uv.Status(localMtime, localHash, c.MTime, c.Hash)

	switch {
	case status == 0 || status == 1:
		delete(s.uvToSend, c.Name)
		if c.Hash != "-" {
			s.uvGimmes[c.Name] = true
			if _, err := s.repo.DB().Exec("DELETE FROM unversioned WHERE name=?", c.Name); err != nil {
				return fmt.Errorf("handleUVIGotCard: delete %q: %w", c.Name, err)
			}
			if err := uv.InvalidateHash(s.repo.DB()); err != nil {
				return fmt.Errorf("handleUVIGotCard: invalidate hash: %w", err)
			}
		} else if status == 1 {
			if err := uv.Delete(s.repo.DB(), c.Name, c.MTime); err != nil {
				return fmt.Errorf("handleUVIGotCard: apply deletion %q: %w", c.Name, err)
			}
		}
	case status == 2:
		if _, err := s.repo.DB().Exec("UPDATE unversioned SET mtime=? WHERE name=?", c.MTime, c.Name); err != nil {
			return fmt.Errorf("handleUVIGotCard: update mtime %q: %w", c.Name, err)
		}
		if err := uv.InvalidateHash(s.repo.DB()); err != nil {
			return fmt.Errorf("handleUVIGotCard: invalidate hash: %w", err)
		}
	case status == 3:
		delete(s.uvToSend, c.Name)
	case status == 4:
		if s.uvPullOnly {
			delete(s.uvToSend, c.Name)
		} else {
			s.uvToSend[c.Name] = false
		}
	case status == 5:
		if s.uvPullOnly {
			delete(s.uvToSend, c.Name)
		} else {
			s.uvToSend[c.Name] = true
		}
	}
	return nil
}

// contentError is a handler-reported content error that leaves the stored
// artifacts consistent, such as a uvfile whose content is not its HASH.
// Section 10.1 has it end the card loop, warn and commit the prefix, and the
// session continue by Section 3.7. A received file or cfile that is not its
// HASH is the integrity error instead (hashMismatchError).
type contentError struct{ msg string }

func (e *contentError) Error() string { return e.msg }

// handleUVFileCard processes a uvfile card from the server.
func (s *session) handleUVFileCard(c *xfer.UVFileCard) error {
	if c == nil {
		panic("session.handleUVFileCard: c must not be nil")
	}
	if err := uv.EnsureSchema(s.repo.DB()); err != nil {
		return fmt.Errorf("handleUVFileCard: ensure schema: %w", err)
	}

	// BUGGIFY: reject a valid uvfile to test retry/re-request.
	if s.opts.Buggify != nil && s.opts.Buggify.Check("sync.handleUVFileCard.reject", 0.05) {
		return nil
	}

	// Only a card that carries content (sz>0, neither flag 1 nor 4) has its
	// HASH checked, as in fossil's xfer_accept_unversioned_file.
	if c.Size > 0 && c.Flags&xfer.UVFlagNoPayload == 0 {
		if c.Content == nil {
			panic("session.handleUVFileCard: flags indicate payload but Content is nil")
		}
		// BUGGIFY: flip a byte to test hash validation catches corruption.
		if s.opts.Buggify != nil && len(c.Content) > 0 && s.opts.Buggify.Check("sync.handleUVFileCard.corrupt", 0.02) {
			c.Content[0] ^= 0xFF
		}
		computed := hash.ContentHash(c.Content, c.Hash)
		if computed != c.Hash {
			return &contentError{msg: fmt.Sprintf("uvfile %s: hash mismatch", c.Name)}
		}
	}

	// Bound, so the row names this sync's receipt.
	err := s.repo.WithBoundTx(func(b *repo.Repo) error {
		return uv.Receive(b.DB(), c.Name, c.MTime, c.Hash, c.Content)
	})
	if err != nil {
		return fmt.Errorf("handleUVFileCard: %w", err)
	}
	return nil
}

// servedHorizon reports a horizon store whose origin has said it serves the
// store's horizon (horizon-served content) in a reply of this session.
func (s *session) servedHorizon() bool {
	return s.horizon && s.result.HorizonServed > 0
}
