package sync

import (
	"context"
	"runtime"
	gosync "sync"
	"weak"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// originMemos holds the classification memo of each store this process
// serves (draft-fossil-horizon-00 §6.2), keyed by the store's pool DB and
// dropped when that DB is collected.
var originMemos = struct {
	gosync.Mutex
	m map[weak.Pointer[db.DB]]*content.Memo
}{m: map[weak.Pointer[db.DB]]*content.Memo{}}

// originMemo returns d's memo, nil when d is bound to a transaction it does
// not own: a memo is published only after the request that read the store
// commits, which such a caller decides.
func originMemo(d *db.DB) *content.Memo {
	if d == nil {
		panic("sync.originMemo: nil DB")
	}
	if d.Bound() {
		return nil
	}
	key := weak.Make(d)
	originMemos.Lock()
	defer originMemos.Unlock()
	if m := originMemos.m[key]; m != nil {
		return m
	}
	m := content.NewMemo()
	originMemos.m[key] = m
	runtime.AddCleanup(d, func(key weak.Pointer[db.DB]) {
		originMemos.Lock()
		defer originMemos.Unlock()
		delete(originMemos.m, key)
	}, key)
	return m
}

// classifier is how an origin classifies a request's cone: over the store
// read afresh, or over its memo.
type classifier interface {
	references(ctx context.Context, q db.Querier, c *cone.Cone) (*content.ConeReferences, error)
	below(ctx context.Context, q db.Querier, refs *content.ConeReferences, c *cone.Cone) (func(libfossil.FslID) bool, error)
	reach(ctx context.Context, q db.Querier, h cone.Horizon) ([]libfossil.FslID, map[string]bool, bool, error)
	// slots is the walk of the trees a horizon keeps for c's path members:
	// every held tree, or with reach those a structural horizon reaches.
	slots(ctx context.Context, refs *content.ConeReferences, c *cone.Cone, reach map[string]bool) (*content.SlotKeep, error)
	// read is how many artifacts classifying read from the store.
	read(refs *content.ConeReferences) int
}

type freshClassifier struct{}

func (freshClassifier) references(ctx context.Context, q db.Querier, c *cone.Cone) (*content.ConeReferences, error) {
	return content.LoadConeReferences(ctx, q, c)
}

func (freshClassifier) below(
	ctx context.Context, q db.Querier, refs *content.ConeReferences, c *cone.Cone,
) (func(libfossil.FslID) bool, error) {
	return refs.HorizonBelow(ctx, q, c)
}

func (freshClassifier) reach(ctx context.Context, q db.Querier, h cone.Horizon) ([]libfossil.FslID, map[string]bool, bool, error) {
	return content.HorizonReach(ctx, q, h)
}

func (freshClassifier) slots(
	ctx context.Context, refs *content.ConeReferences, c *cone.Cone, reach map[string]bool,
) (*content.SlotKeep, error) {
	return refs.SlotKeep(ctx, c, reach)
}

func (freshClassifier) read(refs *content.ConeReferences) int { return refs.Artifacts() }

type memoClassifier struct{ v *content.MemoView }

func (m memoClassifier) references(ctx context.Context, _ db.Querier, c *cone.Cone) (*content.ConeReferences, error) {
	return m.v.References(ctx, c)
}

func (m memoClassifier) below(
	ctx context.Context, q db.Querier, refs *content.ConeReferences, c *cone.Cone,
) (func(libfossil.FslID) bool, error) {
	return m.v.HorizonBelow(ctx, q, refs, c)
}

func (m memoClassifier) reach(ctx context.Context, q db.Querier, h cone.Horizon) ([]libfossil.FslID, map[string]bool, bool, error) {
	return m.v.HorizonReach(ctx, q, h)
}

func (m memoClassifier) slots(
	ctx context.Context, refs *content.ConeReferences, c *cone.Cone, reach map[string]bool,
) (*content.SlotKeep, error) {
	return m.v.SlotKeep(ctx, refs, c, reach)
}

func (m memoClassifier) read(*content.ConeReferences) int { return m.v.Read() }

// classifier returns how this request classifies over q: through the
// store's memo when it has one, else afresh. The memo's view is the
// handler's to publish.
func (h *handler) classifier(ctx context.Context, q db.Querier) (classifier, error) {
	if h.memo == nil {
		return freshClassifier{}, nil
	}
	v, err := h.memo.View(ctx, q)
	if err != nil {
		return nil, err
	}
	h.view = v
	return memoClassifier{v}, nil
}

// publishView hands the memo what this request's classification computed,
// once the request is known to have committed and to have stored no
// artifact: the inventory classifies before the request's received files
// are crosslinked, so a view from such a request describes a store that
// never committed.
func (h *handler) publishView() {
	if h.view == nil || h.fatal || h.aborted || h.filesRecvd > 0 {
		return
	}
	h.view.Publish()
}
