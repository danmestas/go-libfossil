package conformance

import (
	"context"
	"errors"
	"testing"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/merge"
	"github.com/danmestas/go-libfossil/internal/sync"
)

// The merge-base query and deepen (draft-fossil-horizon-00 Appendix A step
// 6: §5.5, §5.7, §5.8, §5.9, §6.2.4) over hzStruct: go-libfossil's handler
// is the origin, its structural clone the client.

func horizonDeepenCases() []Case {
	return []Case{horizonMergeBaseQuery, horizonMergeBaseBound}
}

var (
	horizonMergeBaseQuery = Case{ID: "horizon.6.2.4.merge-base", Req: "HZ-6.2-7"}
	horizonMergeBaseBound = Case{ID: "horizon.8.merge-base-bound", Req: "HZ-8-1"}
)

func TestHorizonDeepen(t *testing.T) {
	check(t, horizonMergeBaseQuery, func() error { return hzMergeBaseQuery(t) })
	check(t, horizonMergeBaseBound, func() error { return hzMergeBaseBound(t) })
}

// hzMergeBaseQuery: a structural clone asks its origin for the merge base of
// two check-ins, one below its horizon, and the origin answers from its own
// graph: the base, or - for check-ins sharing no ancestor.
func hzMergeBaseQuery(t *testing.T) error {
	x := newHZStruct(t)
	r, _, err := x.clone(t, x.peer())
	if err != nil {
		return err
	}
	for _, q := range [][3]string{{x.c4, x.feat1, x.c1}, {x.feat2, x.c4, x.c3}} {
		res, err := sync.Sync(context.Background(), r, x.peer(), sync.SyncOpts{Pull: true, MergeBaseOf: [2]string{q[0], q[1]}})
		if err != nil || !res.MergeBaseAnswered || res.MergeBase != q[2] {
			return expectf("base of %.10s and %.10s: %v answered=%v %q, want %.10s",
				q[0], q[1], err, res.MergeBaseAnswered, res.MergeBase, q[2])
		}
	}
	root := hzCommit(t, x.s.f, "unrelated", []libfossil.FslID{0}, nil)
	res, err := sync.Sync(context.Background(), r, x.peer(), sync.SyncOpts{Pull: true,
		MergeBaseOf: [2]string{x.c4, hzUUID(t, x.s.f.r, root)}})
	if err != nil || !res.MergeBaseAnswered || res.MergeBase != "" {
		return expectf("unrelated: %v answered=%v %q, want -", err, res.MergeBaseAnswered, res.MergeBase)
	}
	return nil
}

// hzMergeBaseBound: the walk behind an answer stops at its visited-node
// bound, as the origin's other graph walks do (§8).
func hzMergeBaseBound(t *testing.T) error {
	x := newHZStruct(t)
	if _, err := merge.FindCommonAncestorWithin(context.Background(), x.s.f.r, x.s.c4, x.s.feat1, 2); !errors.Is(err, merge.ErrBound) {
		return expectf("a walk past its bound: %v, want merge.ErrBound", err)
	}
	base, err := merge.FindCommonAncestorWithin(context.Background(), x.s.f.r, x.s.c4, x.s.feat1, 64)
	if err != nil || base != x.s.c1 {
		return expectf("within the bound: %d %v, want c1", base, err)
	}
	return nil
}
