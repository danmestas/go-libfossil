package sync

import (
	"context"
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

	var cards []xfer.Card

	// 1. Pragma: client-version (every round)
	cards = append(cards, &xfer.PragmaCard{
		Name:   "client-version",
		Values: []string{"22800", "20260315", "120000"},
	})

	// 2. Push/Pull cards.
	//
	// The project code is required on the wire (Fossil C rejects bare push/pull).
	// s.projectCode is the caller's code or, failing that, the repo's own —
	// every repo created by libfossil has a project-code (see db/schema.go).
	projCode := s.projectCode
	// Fossil C also reads a cached remote server-code from the local repo
	// config ('server-code' written by a prior pull). Fall back to that when
	// SyncOpts.ServerCode is not provided. Both push and pull carry it, and
	// when none is cached Fossil C sends its own placeholder — `zSCode =
	// db_get("server-code", "x")` in xfer.c client_sync. The server ignores
	// the value; only the project code that follows it is validated.
	srvCode := s.opts.ServerCode
	if srvCode == "" {
		_ = s.repo.DB().QueryRow(
			"SELECT value FROM config WHERE name='server-code'",
		).Scan(&srvCode)
	}
	if srvCode == "" {
		srvCode = "x"
	}
	// Pull before push, as fossil's client_sync emits them (src/xfer.c): a
	// server reading the push card decides "not authorized to write" versus
	// "pull only" on whether it has already seen the pull card, so the order
	// is load-bearing for a read-only requester (§10.2).
	if s.opts.Pull {
		if projCode == "" {
			return nil, errNoProjectCode("pull")
		}
		cards = append(cards, &xfer.PullCard{
			ProjectCode: projCode,
			ServerCode:  srvCode,
		})
	}
	if s.pushCard() {
		if projCode == "" {
			return nil, errNoProjectCode("push")
		}
		cards = append(cards, &xfer.PushCard{
			ProjectCode: projCode,
			ServerCode:  srvCode,
		})
	}

	// 3. Cookie if cached
	if s.cookie != "" {
		cards = append(cards, &xfer.CookieCard{Value: s.cookie})
	}

	// 4. IGot cards: sendUnclustered every round.
	igotCards, err := s.sendUnclustered()
	if err != nil {
		return nil, fmt.Errorf("buildRequest igot: %w", err)
	}
	s.igotSentThisRound = len(igotCards)
	s.roundStats.IgotsSent = len(igotCards)
	cards = append(cards, igotCards...)

	// 4b. Send cluster igots on round 2+ when pushing and remote has requested blobs.
	// cycle is 0-indexed, so cycle>=1 means round 2+.
	if cycle >= 1 && s.pushing() && s.nGimmeRcvd > 0 {
		clusterCards, err := s.sendAllClusters()
		if err != nil {
			return nil, fmt.Errorf("buildRequest cluster igot: %w", err)
		}
		s.igotSentThisRound += len(clusterCards)
		s.roundStats.IgotsSent += len(clusterCards)
		cards = append(cards, clusterCards...)
	}

	// 4c. Request cluster catalog on round 2 when pulling.
	// cycle is 0-indexed, so cycle==1 means round 2.
	if cycle == 1 && s.opts.Pull {
		cards = append(cards, &xfer.PragmaCard{Name: "req-clusters"})
	}

	// 5. File cards from pendingSend + unsent table, respecting maxSend budget
	fileCards, err := s.buildFileCards()
	if err != nil {
		return nil, fmt.Errorf("buildRequest file: %w", err)
	}
	cards = append(cards, fileCards...)

	// 6. Gimme cards from phantoms (max = max(MaxGimmeBase, filesRecvdLastRound*2))
	gimmeCards := s.buildGimmeCards()
	cards = append(cards, gimmeCards...)

	// Private: send pragma every round (handler is stateless per-round).
	if s.opts.Private {
		cards = append(cards, &xfer.PragmaCard{Name: "send-private"})
	}

	// Private: send igot cards for private blobs
	if s.opts.Private {
		privCards, err := s.sendPrivate()
		if err != nil {
			return nil, fmt.Errorf("buildRequest sendPrivate: %w", err)
		}
		s.igotSentThisRound += len(privCards)
		s.roundStats.IgotsSent += len(privCards)
		cards = append(cards, privCards...)
	}

	// ci-lock: request check-in lock on first round only.
	if s.opts.CkinLock != nil && cycle == 0 {
		cards = append(cards, &xfer.PragmaCard{
			Name:   "ci-lock",
			Values: []string{s.opts.CkinLock.ParentUUID, s.opts.CkinLock.ClientID},
		})
	}

	// UV: pragma uv-hash on first round
	if s.opts.UV && !s.uvHashSent {
		if err := uv.EnsureSchema(s.repo.DB()); err != nil {
			return nil, fmt.Errorf("buildRequest: uv.EnsureSchema: %w", err)
		}
		uvHash, err := uv.ContentHash(s.repo.DB())
		if err != nil {
			return nil, fmt.Errorf("buildRequest: uv.ContentHash: %w", err)
		}
		cards = append(cards, &xfer.PragmaCard{
			Name:   "uv-hash",
			Values: []string{uvHash},
		})
		s.uvHashSent = true
	}

	// UV: gimme cards for requested UV files
	if s.opts.UV {
		for name := range s.uvGimmes {
			cards = append(cards, &xfer.UVGimmeCard{Name: name})
			s.nUvGimmeSent++
			s.result.UVGimmesSent++
			delete(s.uvGimmes, name)
		}
	}

	// UV: send uvfile cards from uvToSend (only after uvPushOK)
	if s.opts.UV && s.uvPushOK {
		uvCards, err := s.buildUVFileCards()
		if err != nil {
			return nil, fmt.Errorf("buildRequest uvfile: %w", err)
		}
		s.result.UVFilesSent += len(uvCards)
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

	// 7. Login card computed LAST, prepended to the front.
	// Nonce = SHA1 of all other cards encoded + random comment.
	//
	// projCode, not s.opts.ProjectCode: the signature is salted with the
	// project code, so a login built from an empty one is rejected by every
	// server. Callers routinely leave SyncOpts.ProjectCode unset and let the
	// repo's own config supply it, exactly as the push and pull cards above do.
	if s.opts.User != "" {
		loginCard, signed, err := s.buildLoginCard(cards, projCode)
		if err != nil {
			return nil, fmt.Errorf("buildRequest login: %w", err)
		}
		cards = append([]xfer.Card{loginCard}, signed...)
	} else {
		// Every request ends with randomness (draft §3); a login card's
		// signature already covers the one buildLoginCard appends.
		cards = append(cards, randomComment(s.env.Rand))
	}

	// This request's push card has now been committed. fossil emits the next
	// request's header before reading the reply that may stop the push, so a
	// "pull only" seen from here on drops the push card only one request later.
	if s.pushStopped {
		s.pushCardDropped = true
	}

	return &xfer.Message{Cards: cards}, nil
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
		rid, ridOK := blob.Exists(s.repo.DB(), uuid)
		if ridOK {
			isPriv := content.IsPrivate(s.repo.DB(), int64(rid))
			if isPriv && !s.opts.Private {
				delete(s.pendingSend, uuid)
				continue
			}
			if isPriv {
				cards = append(cards, &xfer.PrivateCard{})
			}
		}

		card, size, err := s.loadFileCard(uuid)
		if err != nil {
			// Skip files we can't load (phantoms, etc.)
			continue
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
func (s *session) loadFileCard(uuid string) (*xfer.FileCard, int, error) {
	rid, ok := content.AvailableByUUID(s.repo.DB(), uuid)
	if !ok {
		return nil, 0, fmt.Errorf("blob %s not found", uuid)
	}
	data, err := s.cache.Expand(s.repo.DB(), rid)
	if err != nil {
		return nil, 0, err
	}
	return &xfer.FileCard{UUID: uuid, Content: data}, len(data), nil
}

// buildGimmeCards produces gimme cards from the phantoms set.
func (s *session) buildGimmeCards() []xfer.Card {
	if !s.opts.Pull {
		return nil
	}
	maxGimme := MaxGimmeBase
	if alt := s.filesRecvdLastRound * 2; alt > maxGimme {
		maxGimme = alt
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

// processResponse handles all cards in a server response.
// It returns true when the sync has converged (nothing more to do).
func (s *session) processResponse(ctx context.Context, msg *xfer.Message) (bool, error) {
	if msg == nil {
		panic("sync.processResponse: msg must not be nil")
	}
	var t replyTally
	for _, card := range msg.Cards {
		stop, err := s.handleReplyCard(ctx, card, &t)
		if err != nil {
			return false, err
		}
		if stop {
			break
		}
	}

	if err := s.flushVisibilityUpdates(); err != nil {
		return false, fmt.Errorf("sync: record artifact visibility: %w", err)
	}

	s.result.FilesRecvd += t.filesRecvd
	s.filesRecvdLastRound = t.filesRecvd
	s.agePhantoms()
	return s.converged(t)
}

// replyTally counts what one reply carried, for the convergence check.
type replyTally struct {
	filesRecvd int // file and cfile cards received
	filesSent  int // files the server asked us to send
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
		// Uncompressed cards never carry a wire-encoded frame to
		// preserve — storage always compresses payload itself.
		return false, s.receiveFile(ctx, c.UUID, c.DeltaSrc, c.Content, nil, t)
	case *xfer.CFileCard:
		return false, s.receiveFile(ctx, c.UUID, c.DeltaSrc, c.Content, c.StoredBlob, t)
	case *xfer.IGotCard:
		return false, s.handleIGot(c)
	case *xfer.GimmeCard:
		// A session that is not pushing never sends files, so a gimme from
		// it could not be satisfied and would hold the pull open forever
		// (#277). A "pull only" message (§10.2) disables pushing mid-reply,
		// so a gimme after it is not answered either.
		if s.pushing() {
			s.pendingSend[c.UUID] = true
			s.nGimmeRcvd++
			t.filesSent++
		}
	case *xfer.CookieCard:
		s.cookie = c.Value
	case *xfer.MessageCard:
		s.handleMessage(c)
	case *xfer.PragmaCard:
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
			return s.addPhantom(c.UUID)
		}
	default:
		if s.opts.Pull {
			return s.addPhantom(c.UUID)
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
	case c.Name == "uv-push-ok":
		s.uvPushOK = true
	case c.Name == "uv-pull-only":
		s.uvPullOnly = true
	case c.Name == "ci-lock-fail" && len(c.Values) >= 2:
		mtime, _ := strconv.ParseInt(c.Values[1], 10, 64)
		s.result.CkinLockFail = &CkinLockFail{
			HeldBy: c.Values[0],
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

// converged reports whether the session is done after a reply: no files
// moved this round, nothing is owed either way, and no unversioned or table
// sync work is pending.
func (s *session) converged(t replyTally) (bool, error) {
	if t.filesRecvd > 0 || t.filesSent > 0 {
		return false, nil
	}
	if len(s.phantoms) > 0 || len(s.pendingSend) > 0 {
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
// per-round counters it judges by.
func (s *session) uvPending() bool {
	if s.opts.UV && s.nUvGimmeSent > 0 && (s.nUvFileRcvd > 0 || s.result.Rounds < 3) {
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
func storeReceivedFile(ctx context.Context, r *repo.Repo, uuid, deltaSrc string, payload []byte, storedBlob []byte, vis visibility, linker *manifest.ReceiveLinker) error {
	if r == nil {
		panic("storeReceivedFile: r must not be nil")
	}
	if uuid == "" {
		panic("storeReceivedFile: uuid must not be empty")
	}
	if payload == nil {
		panic("storeReceivedFile: payload must not be nil")
	}
	if !hash.IsValidHash(uuid) {
		return fmt.Errorf("sync: invalid UUID format: %s", uuid)
	}
	if deltaSrc != "" {
		// deltaSrc becomes a blob.StorePhantom row under whatever string
		// arrives on the wire. The blob table's own CHECK constraint only
		// bounds uuid length, not hex-ness, so a non-hex value of valid
		// length (e.g. 40 'z' characters) passes it and would otherwise
		// create a permanent, unfillable phantom row that loadDBPhantoms
		// re-requests every sync round forever.
		if !hash.IsValidHash(deltaSrc) {
			return fmt.Errorf("sync: invalid delta source UUID format: %s", deltaSrc)
		}
		return storeDeltaContent(ctx, r, uuid, deltaSrc, payload, storedBlob, vis, linker)
	}

	dephantomizedRid, err := storeResolvedContent(ctx, r, uuid, payload, storedBlob, vis, linker)
	if err != nil {
		return err
	}
	// General sync and push paths preserve Fossil's post-commit
	// after_dephantomize cascade. A clone uses its ReceiveLinker inside the
	// storage transaction instead, so it must not run a second cascade.
	if linker == nil && dephantomizedRid > 0 {
		manifest.AfterDephantomize(ctx, r, dephantomizedRid)
	}
	return nil
}

// storeDeltaContent persists a received delta exactly as given — never
// expanded, never verified at receive time — linked to its source via a
// create-phantom-if-missing lookup (mirrors Fossil's rid_from_uuid(&src,
// 1, ...) in src/xfer.c). The delta table's rid->srcid link is recorded
// unconditionally, matching content_put_ex's REPLACE INTO delta, whether
// or not srcRid is itself currently available.
func storeDeltaContent(ctx context.Context, r *repo.Repo, uuid, deltaSrc string, payload []byte, storedBlob []byte, vis visibility, linker *manifest.ReceiveLinker) error {
	return r.WithTx(func(tx *db.Tx) error {
		srcRid, err := blob.StorePhantom(tx, deltaSrc)
		if err != nil {
			return fmt.Errorf("storeDeltaContent: resolve base %s: %w", deltaSrc, err)
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
		}
		return nil
	})
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

// storeResolvedContent verifies that fullContent hashes to uuid and stores
// it. "Verify, then store resolved content" is a single cohesive unit —
// full (non-delta) file cards always carry content that can be hashed
// immediately, so this stays eager rather than deferring to content.Expand's
// check the way storeDeltaContent's delta bytes must. Fills a phantom row
// in place if one already exists for uuid.
//
// Returns the rid of a phantom that was just filled, so a non-linker caller
// can run AfterDephantomize once this transaction has committed; returns 0
// when no phantom was filled (blob already real, or newly inserted).
func storeResolvedContent(ctx context.Context, r *repo.Repo, uuid string, fullContent []byte, storedBlob []byte, vis visibility, linker *manifest.ReceiveLinker) (libfossil.FslID, error) {
	var computedUUID string
	if len(uuid) > 40 {
		computedUUID = hash.SHA3(fullContent)
	} else {
		computedUUID = hash.SHA1(fullContent)
	}
	if computedUUID != uuid {
		return 0, fmt.Errorf("UUID mismatch for received file: expected %s, got %s", uuid, computedUUID)
	}

	var dephantomizedRid libfossil.FslID
	err := r.WithTx(func(tx *db.Tx) error {
		recordAndLink := func(rid libfossil.FslID) error {
			if err := recordVisibility(tx, rid, vis); err != nil {
				return err
			}
			if linker != nil {
				if err := linker.LinkStored(ctx, tx, rid, uuid, fullContent); err != nil {
					return fmt.Errorf("storeResolvedContent: link %s: %w", uuid, err)
				}
			}
			return nil
		}

		// A received SHA3 name promotes an auto-policy repo, as in fossil.
		if err := hash.PromoteFor(tx, uuid); err != nil {
			return fmt.Errorf("storeResolvedContent: hash policy: %w", err)
		}
		existingRid, exists := blob.Exists(tx, uuid)
		if exists {
			var size int64
			tx.QueryRow("SELECT size FROM blob WHERE rid=?", existingRid).Scan(&size)
			if size != -1 {
				// Real blob already exists; only its visibility can have moved.
				return recordAndLink(existingRid)
			}
			// Fill phantom: update blob content, remove from phantom table.
			compressed, err := blob.EncodeForStorage(fullContent, storedBlob)
			if err != nil {
				return err
			}
			if _, err := tx.Exec("UPDATE blob SET size=?, content=?, rcvid=1 WHERE rid=?",
				len(fullContent), compressed, existingRid); err != nil {
				return err
			}
			if _, err := tx.Exec("DELETE FROM phantom WHERE rid=?", existingRid); err != nil {
				return fmt.Errorf("delete phantom rid=%d: %w", existingRid, err)
			}
			if _, err := tx.Exec("INSERT OR IGNORE INTO unclustered(rid) VALUES(?)", existingRid); err != nil {
				return fmt.Errorf("unclustered rid=%d: %w", existingRid, err)
			}
			dephantomizedRid = libfossil.FslID(existingRid)
			return recordAndLink(existingRid)
		}
		compressed, err := blob.EncodeForStorage(fullContent, storedBlob)
		if err != nil {
			return err
		}
		result, err := tx.Exec(
			"INSERT INTO blob(uuid, size, content, rcvid) VALUES(?, ?, ?, 1)",
			uuid, len(fullContent), compressed,
		)
		if err != nil {
			return err
		}
		rid, err := result.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT OR IGNORE INTO unclustered(rid) VALUES(?)", rid); err != nil {
			return err
		}
		if err := recordAndLink(libfossil.FslID(rid)); err != nil {
			return err
		}
		if linker != nil {
			return nil
		}

		// Crosslink cluster artifacts: parse the content and if it's a
		// cluster manifest, process its M-card UUIDs to create phantoms
		// for blobs we don't have yet. This is how the client discovers
		// individual blob UUIDs referenced by a cluster.
		if d, parseErr := deck.Parse(fullContent); parseErr == nil && d.Type == deck.Cluster {
			if err := manifest.CrosslinkCluster(tx, libfossil.FslID(rid), d); err != nil {
				return fmt.Errorf("crosslink cluster %s: %w", uuid, err)
			}
		}

		return nil
	})
	if err != nil {
		return 0, err
	}
	return dephantomizedRid, nil
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

// addPhantom makes uuid a phantom of this session unless it is shunned: as
// fossil's content_new makes no phantom of a shunned name, the client never
// asks for one (§3.3, §7.7).
func (s *session) addPhantom(uuid string) error {
	shunned, err := shun.IsShunned(s.repo.DB(), uuid)
	if err != nil {
		return err
	}
	if !shunned {
		s.phantoms[uuid] = true
	}
	return nil
}

// phantomEvictRounds is how many consecutive rounds a phantom is asked for
// without arriving before the session stops asking.
const phantomEvictRounds = 3

// loadDBPhantoms promotes phantom-table entries into the session's phantom
// map. This is needed after crosslinking cluster artifacts which create
// phantom rows for blobs referenced in the cluster.
func (s *session) loadDBPhantoms() error {
	if !s.opts.Pull {
		return nil
	}
	rows, err := s.repo.DB().Query(
		"SELECT b.uuid FROM phantom p JOIN blob b ON p.rid = b.rid WHERE b.size < 0" +
			" AND NOT EXISTS(SELECT 1 FROM shun WHERE uuid=b.uuid)",
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var uuid string
		if err := rows.Scan(&uuid); err != nil {
			return err
		}
		if !s.remoteHas[uuid] {
			s.phantoms[uuid] = true
		}
	}
	return rows.Err()
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
	if err := storeReceivedFile(ctx, s.repo, uuid, deltaSrc, payload, storedBlob, vis, nil); err != nil {
		return false, err
	}

	// Promote any new DB phantoms into the session phantom map so gimme
	// cards are emitted. This covers phantoms created by crosslinking
	// cluster artifacts in storeReceivedFile.
	if err := s.loadDBPhantoms(); err != nil {
		return false, fmt.Errorf("handleFileCard: loadDBPhantoms: %w", err)
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

	// Validate hash if content present.
	if c.Flags&xfer.UVFlagNoPayload == 0 {
		if c.Content == nil {
			panic("session.handleUVFileCard: flags indicate payload but Content is nil")
		}
		// BUGGIFY: flip a byte to test hash validation catches corruption.
		if s.opts.Buggify != nil && len(c.Content) > 0 && s.opts.Buggify.Check("sync.handleUVFileCard.corrupt", 0.02) {
			c.Content[0] ^= 0xFF
		}
		computed := hash.ContentHash(c.Content, c.Hash)
		if computed != c.Hash {
			return fmt.Errorf("uvfile %s: hash mismatch", c.Name)
		}
	}

	// Double-check status.
	_, localMtime, localHash, err := uv.Read(s.repo.DB(), c.Name)
	if err != nil {
		return fmt.Errorf("handleUVFileCard: read %q: %w", c.Name, err)
	}
	status := uv.Status(localMtime, localHash, c.MTime, c.Hash)
	if status >= 2 {
		return nil
	}

	if c.Hash == "-" {
		return uv.Delete(s.repo.DB(), c.Name, c.MTime)
	}
	if c.Content != nil {
		return uv.Write(s.repo.DB(), c.Name, c.Content, c.MTime)
	}
	return nil
}
