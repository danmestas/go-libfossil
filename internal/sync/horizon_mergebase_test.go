package sync

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/merge"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// mbCommit checks in one file on parent (0 for a root) and returns its rid
// and hash.
func mbCommit(t *testing.T, r *repo.Repo, name string, parent libfossil.FslID, when time.Time) (libfossil.FslID, string) {
	t.Helper()
	rid, uuid, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{{Name: name, Content: []byte(name + "\n")}}, Comment: name, User: "u",
		Parent: parent, Time: when,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rid, uuid
}

// mbFork is root -> a and root -> b, plus an unrelated root, at the origin.
type mbFork struct {
	r                   *repo.Repo
	root, a, b, unrelat string
	rootRID, aRID       libfossil.FslID
}

func newMBFork(t *testing.T) *mbFork {
	t.Helper()
	f := &mbFork{r: setupPushableTestRepo(t)}
	when := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	f.rootRID, f.root = mbCommit(t, f.r, "root", 0, when)
	f.aRID, f.a = mbCommit(t, f.r, "a", f.rootRID, when.Add(time.Hour))
	_, f.b = mbCommit(t, f.r, "b", f.rootRID, when.Add(2*time.Hour))
	_, f.unrelat = mbCommit(t, f.r, "unrelated", 0, when.Add(3*time.Hour))
	return f
}

func mbAsk(t *testing.T, r *repo.Repo, pull bool, hashes ...string) []string {
	t.Helper()
	cards := []xfer.Card{&xfer.PragmaCard{Name: horizonMergeBasePragma, Values: hashes}}
	if pull {
		cards = append(cards, &xfer.PullCard{ProjectCode: testPC, ServerCode: "s"})
	}
	resp, err := HandleSync(context.Background(), r, &xfer.Message{Cards: cards})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range pragmaLines(t, resp.Cards) {
		if strings.HasPrefix(line, "pragma "+horizonMergeBasePragma) {
			out = append(out, line)
		}
	}
	return out
}

// draft-fossil-horizon-00 §6.2.4: an origin answers a merge-base query in
// its pull reply with the base, or - when the two share no ancestor, spelled
// as zit spells it.
func TestHorizonMergeBaseAnswered(t *testing.T) {
	f := newMBFork(t)
	if got, want := mbAsk(t, f.r, true, f.a, f.b), "pragma horizon-mergebase "+f.a+" "+f.b+" "+f.root; !slices.Equal(got, []string{want}) {
		t.Fatalf("answer = %q, want %q", got, want)
	}
	if got, want := mbAsk(t, f.r, true, f.a, f.unrelat), "pragma horizon-mergebase "+f.a+" "+f.unrelat+" -"; !slices.Equal(got, []string{want}) {
		t.Fatalf("answer = %q, want %q", got, want)
	}
}

// A query the origin cannot answer -- a check-in it does not hold, a
// malformed hash, or no pull to answer in -- draws no answer, which the
// client reads as unanswered.
func TestHorizonMergeBaseUnanswered(t *testing.T) {
	f := newMBFork(t)
	missing := strings.Repeat("ab", 20)
	for _, tc := range []struct {
		hashes []string
		pull   bool
	}{
		{[]string{f.a, missing}, true},
		{[]string{f.a, "abc"}, true},
		{[]string{f.a, f.b}, false},
		{[]string{f.a}, true},
		// §6.2.4: an origin lacking any named check-in answers nothing, and
		// one asked more secondaries than an answer card can repeat answers
		// none.
		{[]string{f.a, f.b, missing}, true},
		{[]string{f.a, f.b, f.root, f.unrelat}, true},
	} {
		if got := mbAsk(t, f.r, tc.pull, tc.hashes...); len(got) != 0 {
			t.Fatalf("query %q pull=%v answered %q", tc.hashes, tc.pull, got)
		}
	}
}

// §8: the ancestry walk behind the answer carries the visited-node bound of
// the origin's other graph walks; past it the query goes unanswered rather
// than walking on.
func TestHorizonMergeBaseBounded(t *testing.T) {
	f := newMBFork(t)
	tip := f.aRID
	when := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	for i := range 8 {
		tip, _ = mbCommit(t, f.r, "chain", tip, when.Add(time.Duration(i)*time.Minute))
	}
	tipUUID := mbUUID(t, f.r, tip)
	if _, ok, err := originMergeBase(context.Background(), f.r, MergeBaseQuery{Primary: tipUUID, Secondaries: []string{f.b}}, 4); ok || !errors.Is(err, merge.ErrBound) {
		t.Fatalf("a walk past the bound answered=%v err=%v, want ErrBound", ok, err)
	}
	base, ok, err := originMergeBase(context.Background(), f.r, MergeBaseQuery{Primary: tipUUID, Secondaries: []string{f.b}}, 64)
	if err != nil || !ok || base != f.root {
		t.Fatalf("within the bound: %q %v %v, want %s", base, ok, err, f.root)
	}
}

func mbUUID(t *testing.T, r *repo.Repo, rid libfossil.FslID) string {
	t.Helper()
	var uuid string
	if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
		t.Fatal(err)
	}
	return uuid
}

// The client asks once, in the first request after the cursor, as zit asks,
// and reports the answer.
func TestHorizonMergeBaseClient(t *testing.T) {
	f := newMBFork(t)
	client := setupSyncTestRepo(t)
	var sent []*xfer.Message
	transport := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		sent = append(sent, req)
		resp, err := HandleSync(context.Background(), f.r, req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}}
	res, err := Sync(context.Background(), client, transport, SyncOpts{Pull: true, ProjectCode: testPC,
		MergeBaseOf: MergeBaseQuery{Primary: f.a, Secondaries: []string{f.b}}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.MergeBaseAnswered || res.MergeBase != f.root {
		t.Fatalf("result answered=%v base=%q, want %s", res.MergeBaseAnswered, res.MergeBase, f.root)
	}
	asked := 0
	for i, req := range sent {
		for _, line := range pragmaLines(t, req.Cards) {
			if line == "pragma horizon-mergebase "+f.a+" "+f.b {
				asked++
				if i != 0 {
					t.Fatalf("asked in request %d, want the first only", i)
				}
			}
		}
	}
	if asked != 1 {
		t.Fatalf("asked %d times, want once", asked)
	}
	res, err = Sync(context.Background(), client, transport, SyncOpts{Pull: true, ProjectCode: testPC,
		MergeBaseOf: MergeBaseQuery{Primary: f.a, Secondaries: []string{f.unrelat}}})
	if err != nil || !res.MergeBaseAnswered || res.MergeBase != "" {
		t.Fatalf("no common ancestor: %v answered=%v base=%q", err, res.MergeBaseAnswered, res.MergeBase)
	}
}

// mbPending is root -> x -> v and root -> y -> p, y -> m: a merge of p into
// a checkout of v with m already merged in, where Fossil's pivot over the
// set is y and the pivot of p and v alone is root.
type mbPending struct {
	r                *repo.Repo
	root, y, v, p, m string
}

func newMBPending(t *testing.T) *mbPending {
	t.Helper()
	f := &mbPending{r: setupPushableTestRepo(t)}
	when := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	rootRID, root := mbCommit(t, f.r, "root", 0, when)
	xRID, _ := mbCommit(t, f.r, "x", rootRID, when.Add(time.Hour))
	yRID, y := mbCommit(t, f.r, "y", rootRID, when.Add(2*time.Hour))
	_, f.v = mbCommit(t, f.r, "v", xRID, when.Add(3*time.Hour))
	_, f.p = mbCommit(t, f.r, "p", yRID, when.Add(4*time.Hour))
	_, f.m = mbCommit(t, f.r, "m", yRID, when.Add(5*time.Hour))
	f.root, f.y = root, y
	return f
}

// draft-fossil-horizon-00 §6.2.4: the list form. The query is the primary
// and one or two secondaries; the answer repeats them in the order asked and
// appends Fossil's pivot over the whole set.
func TestHorizonMergeBaseListForm(t *testing.T) {
	f := newMBPending(t)
	if got, want := mbAsk(t, f.r, true, f.p, f.v), "pragma horizon-mergebase "+f.p+" "+f.v+" "+f.root; !slices.Equal(got, []string{want}) {
		t.Fatalf("one secondary: answer = %q, want %q", got, want)
	}
	if got, want := mbAsk(t, f.r, true, f.p, f.v, f.m), "pragma horizon-mergebase "+f.p+" "+f.v+" "+f.m+" "+f.y; !slices.Equal(got, []string{want}) {
		t.Fatalf("two secondaries: answer = %q, want %q", got, want)
	}
}

// The client asks the list form and takes only an answer that repeats its
// query in order: one that does not answers some other question.
func TestHorizonMergeBaseClientListForm(t *testing.T) {
	f := newMBPending(t)
	client := setupSyncTestRepo(t)
	var asked []string
	reorder := false
	transport := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		for _, line := range pragmaLines(t, req.Cards) {
			if strings.HasPrefix(line, "pragma "+horizonMergeBasePragma) {
				asked = append(asked, line)
			}
		}
		resp, err := HandleSync(context.Background(), f.r, req)
		if err != nil {
			t.Fatal(err)
		}
		if reorder {
			for _, c := range resp.Cards {
				if p, ok := c.(*xfer.PragmaCard); ok && p.Name == horizonMergeBasePragma {
					p.Values[1], p.Values[2] = p.Values[2], p.Values[1]
				}
			}
		}
		return resp
	}}
	q := MergeBaseQuery{Primary: f.p, Secondaries: []string{f.v, f.m}}
	res, err := Sync(context.Background(), client, transport, SyncOpts{Pull: true, ProjectCode: testPC, MergeBaseOf: q})
	if err != nil {
		t.Fatal(err)
	}
	if want := "pragma horizon-mergebase " + f.p + " " + f.v + " " + f.m; !slices.Equal(asked, []string{want}) {
		t.Fatalf("asked %q, want %q once", asked, want)
	}
	if !res.MergeBaseAnswered || res.MergeBase != f.y {
		t.Fatalf("answered=%v base=%q, want %s", res.MergeBaseAnswered, res.MergeBase, f.y)
	}
	reorder = true
	res, err = Sync(context.Background(), client, transport, SyncOpts{Pull: true, ProjectCode: testPC, MergeBaseOf: q})
	if err != nil {
		t.Fatal(err)
	}
	if res.MergeBaseAnswered {
		t.Fatalf("an answer repeating the query out of order was taken: base %q", res.MergeBase)
	}
}
