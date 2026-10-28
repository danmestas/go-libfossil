package sync

import (
	"encoding/binary"
	"hash/fnv"
	"strconv"
	"strings"

	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// maxCloneRuns bounds the delta chains a sparse walk's cookie carries from
// round to round, and so the cookie's length. A walk item sends its version
// with the chain of newer versions above it, so a run stands for one file's
// history forwarded ahead of the check-ins that reach it: about one run per
// in-cone file. A run dropped past the bound is only sent again.
const maxCloneRuns = 1024

// cloneRunRef is a run as a cookie names it: its head, the number of rids
// its delta chain covers from the head, and a checksum of those rids. The
// server rebuilds the run from its store and keeps it only when the checksum
// agrees, so a store re-deltified since never passes off a base the client
// was not sent.
type cloneRunRef struct {
	head, n int
	sum     uint64
}

// runSum is the checksum a run's reference carries over its rids, in order.
func runSum(rids []int) uint64 {
	h := fnv.New64a()
	var buf [8]byte
	for _, rid := range rids {
		binary.BigEndian.PutUint64(buf[:], uint64(rid))
		h.Write(buf[:])
	}
	return h.Sum64()
}

// parseCloneRuns reads a cookie's runs: "HEAD.N.SUM" joined by commas, SUM in
// base 36. A list that is malformed or longer than maxCloneRuns carries
// nothing, which only sends those bases again.
func parseCloneRuns(v string) []cloneRunRef {
	if v == "" || strings.Count(v, ",") >= maxCloneRuns {
		return nil
	}
	parts := strings.Split(v, ",")
	runs := make([]cloneRunRef, 0, len(parts))
	for _, part := range parts {
		fields := strings.Split(part, ".")
		if len(fields) != 3 {
			return nil
		}
		head, herr := strconv.Atoi(fields[0])
		n, nerr := strconv.Atoi(fields[1])
		sum, serr := strconv.ParseUint(fields[2], 36, 64)
		if herr != nil || nerr != nil || serr != nil || head <= 0 || n <= 0 || n > maxCloneChainWalk {
			return nil
		}
		runs = append(runs, cloneRunRef{head: head, n: n, sum: sum})
	}
	return runs
}

// formatCloneRuns is parseCloneRuns' inverse over runs' rids, keeping the
// first maxCloneRuns.
func formatCloneRuns(runs [][]int) string {
	runs = runs[:min(len(runs), maxCloneRuns)]
	parts := make([]string, len(runs))
	for i, run := range runs {
		if len(run) == 0 {
			panic("sync.formatCloneRuns: empty run")
		}
		parts[i] = strconv.Itoa(run[0]) + "." + strconv.Itoa(len(run)) + "." +
			strconv.FormatUint(runSum(run), 36)
	}
	return strings.Join(parts, ",")
}

// expandCloneRuns rebuilds the runs the request's cookie carried and counts
// their rids as sent: each was on the wire in an earlier round of this clone.
func (h *handler) expandCloneRuns(round *cloneBatch) error {
	if len(round.resume.runs) > maxCloneRuns {
		panic("sync.expandCloneRuns: cookie carried more runs than maxCloneRuns")
	}
	for _, ref := range round.resume.runs {
		run, err := h.cloneRun(ref)
		if err != nil {
			return err
		}
		if run == nil {
			continue
		}
		for _, rid := range run {
			round.sent[rid] = true
		}
		round.runs = append(round.runs, run)
	}
	return nil
}

// cloneRun follows ref's delta chain from its head, or returns nil when the
// store no longer holds that chain as the cookie's checksum names it.
func (h *handler) cloneRun(ref cloneRunRef) ([]int, error) {
	run := make([]int, 0, ref.n)
	for rid := ref.head; len(run) < ref.n; {
		if rid <= 0 || rid > h.cloneSnapMax {
			return nil, nil
		}
		run = append(run, rid)
		if len(run) == ref.n {
			break
		}
		src, err := content.DeltaSource(h.repo.DB(), libfossil.FslID(rid))
		if err != nil {
			return nil, err
		}
		rid = int(src)
	}
	if runSum(run) != ref.sum {
		return nil, nil
	}
	return run, nil
}

// noteRun records the sources a send forwarded ahead of top as a run. rids
// is the send's, source first, so reversed it follows the chain down from
// top's own source.
func (round *cloneBatch) noteRun(rids []int, top int) {
	run := make([]int, 0, len(rids))
	for i := len(rids) - 1; i >= 0; i-- {
		if rids[i] != top {
			run = append(run, rids[i])
		}
	}
	if len(run) > 0 {
		round.runs = append(round.runs, run)
	}
}

// carriedRuns is what the next round's cookie carries: each run less the
// leading rids this round's walk reached, which no later walk needs skipped.
// The walk reaches a file's versions oldest first, and so a run's from its
// head.
func (round *cloneBatch) carriedRuns() [][]int {
	out := make([][]int, 0, len(round.runs))
	for _, run := range round.runs {
		for len(run) > 0 && round.reached[run[0]] {
			run = run[1:]
		}
		if len(run) > 0 {
			out = append(out, run)
		}
	}
	return out
}
