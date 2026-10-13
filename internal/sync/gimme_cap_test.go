package sync

import (
	"fmt"
	"testing"
)

// addDBPhantom makes a phantom row so converged's "phantoms remain" check
// sees one.
func addDBPhantom(t *testing.T, s *session) {
	t.Helper()
	db := s.repo.DB()
	res, err := db.Exec("INSERT INTO blob(rcvid,size,uuid,content) VALUES(0,-1,?,NULL)",
		"dddddddddddddddddddddddddddddddddddddddd")
	if err != nil {
		t.Fatalf("phantom blob: %v", err)
	}
	rid, _ := res.LastInsertId()
	if _, err := db.Exec("INSERT INTO phantom VALUES(?)", rid); err != nil {
		t.Fatalf("phantom row: %v", err)
	}
}

func fillPhantoms(s *session, n int) {
	for i := 0; i < n; i++ {
		s.phantoms[fmt.Sprintf("%040x", i+1)] = true
	}
}

// TestGimmeCapFollowsFossilMxPhantomReq pins fossil's per-round phantom
// request limit (src/xfer.c mxPhantomReq): 200 at first, then
// max(200, 2*files received) after a round that continues for its phantoms,
// and left as it was by a round that does not (#350).
func TestGimmeCapFollowsFossilMxPhantomReq(t *testing.T) {
	s, _ := newTestSession(t, SyncOpts{Pull: true})
	fillPhantoms(s, 1000)
	if got := len(s.buildGimmeCards()); got != 200 {
		t.Fatalf("first round: %d gimmes, want 200", got)
	}

	addDBPhantom(t, s)
	if _, err := s.converged(replyTally{filesRecvd: 150}); err != nil {
		t.Fatal(err)
	}
	if got := len(s.buildGimmeCards()); got != 300 {
		t.Fatalf("after 150 files: %d gimmes, want 300", got)
	}

	// A round that continues for another reason leaves the limit alone,
	// as fossil only assigns mxPhantomReq in its phantom branch.
	if _, err := s.converged(replyTally{filesSent: 1}); err != nil {
		t.Fatal(err)
	}
	if got := len(s.buildGimmeCards()); got != 300 {
		t.Fatalf("after a no-file round: %d gimmes, want 300", got)
	}

	// Few files never lower it below the base.
	if _, err := s.converged(replyTally{filesRecvd: 10}); err != nil {
		t.Fatal(err)
	}
	if got := len(s.buildGimmeCards()); got != 200 {
		t.Fatalf("after 10 files: %d gimmes, want 200", got)
	}
}
