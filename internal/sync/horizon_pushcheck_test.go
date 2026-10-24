package sync

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// pcOrigin is an origin with trunk root -> p.
type pcOrigin struct {
	t       *testing.T
	r       *repo.Repo
	when    time.Time
	p       string
	file    []byte
	fileUID string
	push    func(cards ...xfer.Card) *xfer.Message
	held    func(uuid string) bool
}

func newPCOrigin(t *testing.T) *pcOrigin {
	t.Helper()
	r := setupPushableTestRepo(t)
	o := &pcOrigin{t: t, r: r, when: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), file: []byte("c\n")}
	o.fileUID = hash.SHA1(o.file)
	root, _ := mbCommit(t, r, "root", 0, o.when)
	_, o.p = mbCommit(t, r, "p", root, o.when.Add(time.Hour))
	o.push = func(cards ...xfer.Card) *xfer.Message {
		t.Helper()
		req := &xfer.Message{Cards: append([]xfer.Card{&xfer.PushCard{ServerCode: "s", ProjectCode: testPC}}, cards...)}
		resp, err := HandleSync(context.Background(), r, req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	o.held = func(uuid string) bool {
		_, ok := blob.Exists(r.DB(), uuid)
		return ok
	}
	return o
}

// branchFrom is a check-in on p creating branch nb whose cancellation names
// cancelled as p's branch, as a clone whose seed says so writes it.
func (o *pcOrigin) branchFrom(cancelled string) (*xfer.FileCard, string) {
	o.t.Helper()
	d := &deck.Deck{Type: deck.Checkin, C: "nb", D: o.when.Add(2 * time.Hour), U: deck.User("u"), P: []string{o.p},
		F: []deck.FileCard{{Name: "c", UUID: o.fileUID}},
		T: []deck.TagCard{
			{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "nb"},
			{Type: deck.TagPropagating, Name: "sym-nb", UUID: "*"},
			{Type: deck.TagCancel, Name: "sym-" + cancelled, UUID: "*"},
		}}
	data, err := d.Marshal()
	if err != nil {
		o.t.Fatal(err)
	}
	uuid := hash.SHA1(data)
	return &xfer.FileCard{UUID: uuid, Content: data}, uuid
}

// correction is the control artifact cancelling sym-<on> on target.
func (o *pcOrigin) correction(target, on string) *xfer.FileCard {
	o.t.Helper()
	d := &deck.Deck{Type: deck.Control, D: o.when.Add(3 * time.Hour), U: deck.User("u"),
		T: []deck.TagCard{{Type: deck.TagCancel, Name: "sym-" + on, UUID: target}}}
	data, err := d.Marshal()
	if err != nil {
		o.t.Fatal(err)
	}
	return &xfer.FileCard{UUID: hash.SHA1(data), Content: data}
}

func (o *pcOrigin) blobCard() *xfer.FileCard { return &xfer.FileCard{UUID: o.fileUID, Content: o.file} }

func errorsOf(m *xfer.Message) []string {
	var out []string
	for _, c := range m.Cards {
		if e, ok := c.(*xfer.ErrorCard); ok {
			out = append(out, e.Message)
		}
	}
	return out
}

// draft-fossil-horizon-00 §5.7: a pushed check-in whose cancellation names a
// branch its parent is not on, by this origin's derivation, is refused with
// a diagnostic naming the stale seed and the parent's branch, and the whole
// push stores nothing.
func TestHorizonPushStaleCancelRefused(t *testing.T) {
	o := newPCOrigin(t)
	card, uuid := o.branchFrom("feat")
	errs := errorsOf(o.push(o.blobCard(), card))
	if len(errs) != 1 || !strings.HasPrefix(errs[0], "stale seed: ") ||
		!strings.Contains(errs[0], "'trunk'") || !strings.Contains(errs[0], "'feat'") {
		t.Fatalf("errors = %q, want one stale-seed refusal naming trunk and feat", errs)
	}
	if o.held(uuid) || o.held(o.fileUID) {
		t.Fatal("the refused push stored artifacts")
	}
}

// The check-in is admitted when it cancels the parent's real branch, when
// the same push carries the correcting control artifact, or when the store
// already holds it.
func TestHorizonPushStaleCancelAdmitted(t *testing.T) {
	o := newPCOrigin(t)
	card, uuid := o.branchFrom("trunk")
	if errs := errorsOf(o.push(o.blobCard(), card)); len(errs) != 0 || !o.held(uuid) {
		t.Fatalf("a correct cancellation: errors %q, held %v", errs, o.held(uuid))
	}

	o = newPCOrigin(t)
	card, uuid = o.branchFrom("feat")
	if errs := errorsOf(o.push(o.blobCard(), card, o.correction(uuid, "trunk"))); len(errs) != 0 || !o.held(uuid) {
		t.Fatalf("with the correction in the push: errors %q, held %v", errs, o.held(uuid))
	}

	o = newPCOrigin(t)
	card, uuid = o.branchFrom("feat")
	if errs := errorsOf(o.push(o.correction(uuid, "trunk"))); len(errs) != 0 {
		t.Fatalf("pushing the correction first: %q", errs)
	}
	if errs := errorsOf(o.push(o.blobCard(), card)); len(errs) != 0 || !o.held(uuid) {
		t.Fatalf("with the correction in the store: errors %q, held %v", errs, o.held(uuid))
	}
}
