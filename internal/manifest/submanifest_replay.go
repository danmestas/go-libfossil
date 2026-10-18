package manifest

// zit's refinement of the automatic selection of draft-fossil-submanifests-00
// §6.3. The draft's objective is advisory -- the implementation SHOULD pick
// the least C(k) -- and zit, on enabling, lets the repository's own history
// outrank that snapshot: it replays the recent check-ins through the tree
// builder at the snapshot's k and its two neighbours and pins the value whose
// steady-state commits materialize the fewest bytes. Two writers sharing a
// repository should pin alike, so this follows zit's replay step for step:
// the window, the candidates, the byte accounting and the tie-break.

import (
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
)

// replayWindow is how many of the most recent check-ins the replay reads,
// zit's window.
const replayWindow = 200

// replayArm is one candidate k's tally over the window.
type replayArm struct {
	k       int
	allFlat bool
	// steady is the materialization bytes of every replayed commit but the
	// first: a flat commit's manifest, or a tree's root and each sub-manifest
	// on its first appearance in the window (later ones are shared).
	steady int64
	seen   map[string]bool
}

// replayCommit is one replayed check-in: its manifest and its file set.
type replayCommit struct {
	data  []byte
	d     *deck.Deck
	files []deck.FileCard
}

// replayK is zit's replay over the last replayWindow check-ins at snapshot,
// snapshot-1 and snapshot+1 within 1..12. It reports false when fewer than
// two check-ins replay or every candidate stays flat throughout; the caller
// then keeps the snapshot's value.
func replayK(q db.Querier, snapshot int) (int, bool, error) {
	if !validPartitionK(snapshot) {
		panic("manifest.replayK: snapshot must be from 1 to 12")
	}
	commits, name, err := replayCommits(q)
	if err != nil || len(commits) < 2 {
		return 0, false, err
	}
	var arms []*replayArm
	for k := snapshot - 1; k <= snapshot+1; k++ {
		if validPartitionK(k) {
			arms = append(arms, &replayArm{k: k, allFlat: true, seen: map[string]bool{}})
		}
	}
	for i, c := range commits {
		for _, arm := range arms {
			bytes, err := arm.replay(c, name)
			if err != nil {
				return 0, false, err
			}
			if i > 0 {
				arm.steady += bytes
			}
		}
	}
	var best *replayArm
	for _, arm := range arms {
		if !arm.allFlat && (best == nil || arm.steady < best.steady) {
			best = arm
		}
	}
	if best == nil {
		return 0, false, nil
	}
	return best.k, true, nil
}

// replay charges one commit to the arm and returns its bytes.
func (a *replayArm) replay(c replayCommit, name func([]byte) (string, error)) (int64, error) {
	tree, err := BuildTree(c.files, a.k, name)
	if err != nil {
		return 0, err
	}
	if len(tree.S) == 0 {
		return int64(len(c.data)), nil
	}
	a.allFlat = false
	root := *c.d
	root.F, root.S, root.B = nil, tree.S, ""
	rootBytes, err := root.Marshal()
	if err != nil {
		return 0, fmt.Errorf("replay root: %w", err)
	}
	total := int64(len(rootBytes))
	for _, sub := range tree.Subs {
		uuid, err := name(sub)
		if err != nil {
			return 0, err
		}
		if a.seen[uuid] {
			continue
		}
		a.seen[uuid] = true
		total += int64(len(sub))
	}
	return total, nil
}

// replayCommits loads the window's check-ins, oldest first, skipping one
// whose file set cannot be enumerated, and the naming the newest one's name
// implies, as zit names by its tip.
func replayCommits(q db.Querier) ([]replayCommit, func([]byte) (string, error), error) {
	rows, err := q.Query(`SELECT objid FROM (SELECT objid FROM event WHERE type='ci'
		ORDER BY objid DESC LIMIT ?) ORDER BY objid`, replayWindow)
	if err != nil {
		return nil, nil, fmt.Errorf("replay window: %w", err)
	}
	var rids []libfossil.FslID
	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			rows.Close()
			return nil, nil, err
		}
		rids = append(rids, libfossil.FslID(rid))
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(rids) == 0 {
		return nil, nil, err
	}
	var tip string
	if err := q.QueryRow("SELECT uuid FROM blob WHERE rid=?", rids[len(rids)-1]).Scan(&tip); err != nil {
		return nil, nil, fmt.Errorf("replay tip: %w", err)
	}
	alg := hash.AlgSHA3
	if len(tip) == 40 {
		alg = hash.AlgSHA1
	}
	name := func(data []byte) (string, error) { return alg.Hash(data), nil }
	var out []replayCommit
	for _, rid := range rids {
		if c, ok := loadReplayCommit(q, rid); ok {
			out = append(out, c)
		}
	}
	return out, name, nil
}

// loadReplayCommit reads one check-in for the replay, reporting false when
// it cannot be read or enumerated.
func loadReplayCommit(q db.Querier, rid libfossil.FslID) (replayCommit, bool) {
	data, err := content.Expand(q, rid)
	if err != nil {
		return replayCommit{}, false
	}
	d, err := deck.Parse(data)
	if err != nil || d.Type != deck.Checkin {
		return replayCommit{}, false
	}
	files, err := checkinFileCards(q, nil, d)
	if err != nil {
		return replayCommit{}, false
	}
	for i, f := range files {
		if f.UUID == "" || (i > 0 && deck.Compare(files[i-1].Name, f.Name) >= 0) {
			return replayCommit{}, false
		}
	}
	return replayCommit{data: data, d: d, files: files}, true
}
