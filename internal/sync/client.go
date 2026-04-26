package sync

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/internal/content"
	"github.com/danmestas/libfossil/db"
	"github.com/danmestas/libfossil/internal/deck"
	"github.com/danmestas/libfossil/internal/delta"
	"github.com/danmestas/libfossil/internal/hash"
	"github.com/danmestas/libfossil/internal/manifest"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/internal/uv"
	"github.com/danmestas/libfossil/internal/xfer"

	libfossil "github.com/danmestas/libfossil/internal/fsltype"
)

// ErrDeltaSourceMissing is returned by storeReceivedFile when the delta source
// blob is not present in the repository. Clone uses this to create phantoms.
var ErrDeltaSourceMissing = errors.New("delta source not found")

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
	// Prefer the caller-supplied code; fall back to the local repo's stored
	// project-code so callers that don't populate SyncOpts.ProjectCode still work.
	// Every repo created by libfossil has a project-code (see db/schema.go).
	projCode := s.opts.ProjectCode
	if projCode == "" {
		_ = s.repo.DB().QueryRow(
			"SELECT value FROM config WHERE name='project-code'",
		).Scan(&projCode)
	}
	// Fossil C also reads a cached remote server-code from the local repo
	// config ('server-code' written by a prior pull). Fall back to that when
	// SyncOpts.ServerCode is not provided.
	srvCode := s.opts.ServerCode
	if srvCode == "" {
		_ = s.repo.DB().QueryRow(
			"SELECT value FROM config WHERE name='server-code'",
		).Scan(&srvCode)
	}
	if s.opts.Push {
		if projCode == "" {
			panic("sync.buildRequest: ProjectCode is required for push but not found in SyncOpts or repo config")
		}
		cards = append(cards, &xfer.PushCard{
			ProjectCode: projCode,
			ServerCode:  srvCode, // optional; omitted on first push to a remote
		})
	}
	if s.opts.Pull {
		if projCode == "" {
			panic("sync.buildRequest: ProjectCode is required for pull but not found in SyncOpts or repo config")
		}
		// Fossil C's pull parser requires both project-code and server-code.
		// Use "0" as the server-code placeholder when none is cached — this is
		// what Fossil C itself sends on the first pull to an unknown remote
		// (xfer.c: zSCode defaults to "0" when not found in the config).
		if srvCode == "" {
			srvCode = "0"
		}
		cards = append(cards, &xfer.PullCard{
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
	if cycle >= 1 && s.opts.Push && s.nGimmeRcvd > 0 {
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
	if s.opts.User != "" {
		loginCard, err := s.buildLoginCard(cards)
		if err != nil {
			return nil, fmt.Errorf("buildRequest login: %w", err)
		}
		cards = append([]xfer.Card{loginCard}, cards...)
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

// buildFileCards produces file cards from pendingSend and the unsent table,
// respecting the maxSend byte budget.
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
	rid, ok := blob.Exists(s.repo.DB(), uuid)
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

// buildLoginCard encodes the non-login cards, appends a random comment,
// then computes the login card and returns it.
func (s *session) buildLoginCard(cards []xfer.Card) (*xfer.LoginCard, error) {
	var buf bytes.Buffer
	for _, c := range cards {
		if err := xfer.EncodeCard(&buf, c); err != nil {
			return nil, err
		}
	}
	payload := appendRandomComment(buf.Bytes(), s.env.Rand)
	// BUGGIFY: corrupt the nonce payload to trigger auth failures.
	if s.opts.Buggify != nil && s.opts.Buggify.Check("sync.buildLoginCard.badNonce", 0.02) {
		payload = append(payload, []byte("BUGGIFY")...)
	}
	return computeLogin(s.opts.User, s.opts.Password, s.opts.ProjectCode, payload), nil
}

// processResponse handles all cards in a server response.
// It returns true when the sync has converged (nothing more to do).
func (s *session) processResponse(msg *xfer.Message) (bool, error) {
	if msg == nil {
		panic("sync.processResponse: msg must not be nil")
	}
	filesRecvd := 0
	filesSent := 0 // files the server asked us to send this round

	for _, card := range msg.Cards {
		switch c := card.(type) {
		case *xfer.PrivateCard:
			s.nextIsPrivate = true

		case *xfer.FileCard:
			if err := s.handleFileCard(c.UUID, c.DeltaSrc, c.Content); err != nil {
				return false, err
			}
			if err := s.applyPrivateStatus(c.UUID); err != nil {
				return false, err
			}
			filesRecvd++
			s.roundStats.BytesReceived += int64(len(c.Content))
			delete(s.phantoms, c.UUID)

		case *xfer.CFileCard:
			if err := s.handleFileCard(c.UUID, c.DeltaSrc, c.Content); err != nil {
				return false, err
			}
			if err := s.applyPrivateStatus(c.UUID); err != nil {
				return false, err
			}
			filesRecvd++
			s.roundStats.BytesReceived += int64(len(c.Content))
			delete(s.phantoms, c.UUID)

		case *xfer.IGotCard:
			s.remoteHas[c.UUID] = true
			rid, exists := blob.Exists(s.repo.DB(), c.UUID)
			if c.IsPrivate {
				if exists {
					if err := content.MakePrivate(s.repo.DB(), int64(rid)); err != nil {
						return false, fmt.Errorf("sync: MakePrivate %s: %w", c.UUID, err)
					}
				} else if s.opts.Private && s.opts.Pull {
					s.phantoms[c.UUID] = true
				}
			} else {
				if exists {
					if err := content.MakePublic(s.repo.DB(), int64(rid)); err != nil {
						return false, fmt.Errorf("sync: MakePublic %s: %w", c.UUID, err)
					}
				} else if s.opts.Pull {
					s.phantoms[c.UUID] = true
				}
			}

		case *xfer.GimmeCard:
			s.pendingSend[c.UUID] = true
			s.nGimmeRcvd++
			filesSent++

		case *xfer.CookieCard:
			s.cookie = c.Value

		case *xfer.ErrorCard:
			s.result.Errors = append(s.result.Errors, "error: "+c.Message)

		case *xfer.MessageCard:
			s.result.Errors = append(s.result.Errors, "message: "+c.Message)

		case *xfer.PragmaCard:
			if c.Name == "uv-push-ok" {
				s.uvPushOK = true
			} else if c.Name == "uv-pull-only" {
				s.uvPullOnly = true
			} else if c.Name == "ci-lock-fail" && len(c.Values) >= 2 {
				mtime, _ := strconv.ParseInt(c.Values[1], 10, 64)
				s.result.CkinLockFail = &CkinLockFail{
					HeldBy: c.Values[0],
					Since:  time.Unix(mtime, 0),
				}
			}

		case *xfer.UVIGotCard:
			if s.opts.UV {
				if err := s.handleUVIGotCard(c); err != nil {
					return false, err
				}
			}

		case *xfer.UVFileCard:
			if s.opts.UV {
				if err := s.handleUVFileCard(c); err != nil {
					return false, err
				}
				s.nUvFileRcvd++
				s.result.UVFilesRecvd++
			}

		case *xfer.UVGimmeCard:
			if s.opts.UV {
				if s.uvToSend == nil {
					s.uvToSend = make(map[string]bool)
				}
				s.uvToSend[c.Name] = true
			}

		case *xfer.SchemaCard, *xfer.XIGotCard, *xfer.XGimmeCard, *xfer.XRowCard, *xfer.XDeleteCard:
			if err := s.processXTableCard(card); err != nil {
				return false, err
			}
		}
	}

	s.result.FilesRecvd += filesRecvd
	s.filesRecvdLastRound = filesRecvd

	// Age unresolved phantoms; evict after 3 consecutive rounds without delivery.
	// FileCard/CFileCard handlers already delete resolved phantoms from s.phantoms,
	// so anything still in the map was not delivered this round.
	for uuid := range s.phantoms {
		s.phantomAge[uuid]++
		if s.phantomAge[uuid] >= 3 {
			delete(s.phantoms, uuid)
			delete(s.phantomAge, uuid)
		}
	}
	for uuid := range s.phantomAge {
		if !s.phantoms[uuid] {
			delete(s.phantomAge, uuid)
		}
	}

	// Convergence: done if no files received, no files sent this round,
	// phantoms empty, pendingSend empty, and unsent table empty.
	if filesRecvd > 0 || filesSent > 0 {
		return false, nil
	}
	if len(s.phantoms) > 0 || len(s.pendingSend) > 0 {
		return false, nil
	}

	// If we sent igot cards but the server didn't gimme anything,
	// clear the unsent table — those artifacts have been announced
	// and the server either has them or doesn't want them.
	if s.igotSentThisRound > 0 && filesSent == 0 {
		if _, err := s.repo.DB().Exec("DELETE FROM unsent"); err != nil {
			return false, fmt.Errorf("sync: delete unsent: %w", err)
		}
	}

	// Check unsent table
	var unsentCount int
	err := s.repo.DB().QueryRow("SELECT count(*) FROM unsent").Scan(&unsentCount)
	if err != nil {
		return false, fmt.Errorf("checking unsent: %w", err)
	}
	if unsentCount > 0 {
		return false, nil
	}

	// UV convergence
	if s.opts.UV && s.nUvGimmeSent > 0 && (s.nUvFileRcvd > 0 || s.result.Rounds < 3) {
		s.nUvGimmeSent = 0
		s.nUvFileRcvd = 0
		return false, nil
	}
	if s.opts.UV && len(s.uvToSend) > 0 {
		if !s.uvPushOK {
			// Server did not grant uv-push-ok — UV hashes match,
			// nothing to push. Clear stale uvToSend from prior rounds.
			s.uvToSend = nil
		} else {
			return false, nil
		}
	}
	if s.opts.UV && s.uvGimmes != nil && len(s.uvGimmes) > 0 {
		return false, nil
	}
	s.nUvGimmeSent = 0
	s.nUvFileRcvd = 0

	// Table sync convergence (only when x-table sync is enabled)
	if s.opts.XTableSync {
		for _, gimmes := range s.xTableGimmes {
			if len(gimmes) > 0 {
				return false, nil
			}
		}
		for _, sends := range s.xTableToSend {
			if len(sends) > 0 {
				return false, nil
			}
		}
	}

	return true, nil
}

// storeReceivedFile validates, resolves deltas, verifies hashes, and stores
// a received file (or delta-file) into the repo. It is used by both Sync and Clone.
// When the delta source is missing, it returns ErrDeltaSourceMissing so callers
// can handle the case (e.g., Clone creates a phantom).
// resolveFileContent resolves the full content of a received file card.
// For non-delta files, returns the payload directly. For deltas, expands
// the base and applies the delta. Returns ErrDeltaSourceMissing if the
// delta source is not in the repo.
func resolveFileContent(r *repo.Repo, uuid, deltaSrc string, payload []byte, cache *content.Cache) ([]byte, error) {
	if deltaSrc == "" {
		return payload, nil
	}
	srcRid, ok := blob.Exists(r.DB(), deltaSrc)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrDeltaSourceMissing, deltaSrc)
	}
	baseContent, err := cache.Expand(r.DB(), srcRid)
	if err != nil {
		return nil, fmt.Errorf("expanding delta source %s: %w", deltaSrc, err)
	}
	applied, err := delta.Apply(baseContent, payload)
	if err != nil {
		return nil, fmt.Errorf("applying delta for %s: %w", uuid, err)
	}
	return applied, nil
}

// storeReceivedFile validates and stores a received file/cfile blob.
// Returns ErrDeltaSourceMissing if the delta source is not found.
func storeReceivedFile(r *repo.Repo, uuid, deltaSrc string, payload []byte, cache *content.Cache) error {
	if r == nil { panic("storeReceivedFile: r must not be nil") }
	if uuid == "" { panic("storeReceivedFile: uuid must not be empty") }
	if payload == nil { panic("storeReceivedFile: payload must not be nil") }
	if !hash.IsValidHash(uuid) {
		return fmt.Errorf("sync: invalid UUID format: %s", uuid)
	}

	fullContent, err := resolveFileContent(r, uuid, deltaSrc, payload, cache)
	if err != nil {
		return err
	}

	// Verify hash matches UUID.
	var computedUUID string
	if len(uuid) > 40 {
		computedUUID = hash.SHA3(fullContent)
	} else {
		computedUUID = hash.SHA1(fullContent)
	}
	if computedUUID != uuid {
		return fmt.Errorf("UUID mismatch for received file: expected %s, got %s", uuid, computedUUID)
	}

	return r.WithTx(func(tx *db.Tx) error {
		existingRid, exists := blob.Exists(tx, uuid)
		if exists {
			var size int64
			tx.QueryRow("SELECT size FROM blob WHERE rid=?", existingRid).Scan(&size)
			if size != -1 {
				return nil // real blob already exists
			}
			// Fill phantom: update blob content, remove from phantom table.
			compressed, err := blob.Compress(fullContent)
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
			return nil
		}
		compressed, err := blob.Compress(fullContent)
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
}

// loadDBPhantoms promotes phantom-table entries into the session's phantom
// map. This is needed after crosslinking cluster artifacts which create
// phantom rows for blobs referenced in the cluster.
func (s *session) loadDBPhantoms() error {
	if !s.opts.Pull {
		return nil
	}
	rows, err := s.repo.DB().Query(
		"SELECT b.uuid FROM phantom p JOIN blob b ON p.rid = b.rid WHERE b.size < 0",
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
func (s *session) handleFileCard(uuid, deltaSrc string, payload []byte) error {
	if err := storeReceivedFile(s.repo, uuid, deltaSrc, payload, s.cache); err != nil {
		return err
	}

	// Promote any new DB phantoms into the session phantom map so gimme
	// cards are emitted. This covers phantoms created by crosslinking
	// cluster artifacts in storeReceivedFile.
	if err := s.loadDBPhantoms(); err != nil {
		return fmt.Errorf("handleFileCard: loadDBPhantoms: %w", err)
	}

	// BUGGIFY: simulate post-store failure to test retry/recovery logic.
	if s.opts.Buggify != nil && s.opts.Buggify.Check("sync.handleFileCard.reject", 0.03) {
		return fmt.Errorf("buggify: simulated storage failure for %s", uuid)
	}

	return nil
}

// applyPrivateStatus marks a just-stored blob as private or public based on
// whether a PrivateCard preceded it. Extracted from FileCard/CFileCard handlers
// to eliminate duplication.
func (s *session) applyPrivateStatus(uuid string) error {
	if s.nextIsPrivate {
		// BUGGIFY: 3% chance skip MakePrivate — leave blob as public;
		// the next sync round should correct the status.
		if s.opts.Buggify != nil && s.opts.Buggify.Check("sync.applyPrivateStatus.skipMakePrivate", 0.03) {
			s.nextIsPrivate = false
			return nil
		}
		rid, _ := blob.Exists(s.repo.DB(), uuid)
		if err := content.MakePrivate(s.repo.DB(), int64(rid)); err != nil {
			return fmt.Errorf("sync: MakePrivate %s: %w", uuid, err)
		}
		s.nextIsPrivate = false
	} else {
		rid, exists := blob.Exists(s.repo.DB(), uuid)
		if exists {
			if err := content.MakePublic(s.repo.DB(), int64(rid)); err != nil {
				return fmt.Errorf("sync: MakePublic %s: %w", uuid, err)
			}
		}
	}
	return nil
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
