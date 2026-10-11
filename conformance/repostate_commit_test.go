package conformance

import (
	"path/filepath"
	"strings"

	"github.com/danmestas/go-libfossil/internal/branch"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
)

// The incremental leaf procedures of Section 7.3. A crosslink sweep
// recomputes the whole leaf table at its end, so it cannot show whether a
// single check is right; a local commit maintains leaves one check-in at a
// time, so these cases commit through manifest.Checkin and branch.Create.
// The reference is `fossil rebuild` on a copy of the result.

// commitStep is one commit: its label, its parents by label (primary
// first), and, when set, the branch it starts with branch.Create.
type commitStep struct {
	label   string
	parents []string
	branch  string
}

// committedLeaves commits steps in order and checks the leaf table.
func committedLeaves(id, req string, steps []commitStep, want ...string) rsAPICase {
	return rsAPICase{Case: Case{ID: id, Req: req}, run: func(e *rsEnv) error {
		path, labels, err := commitAll(e, steps)
		if err != nil {
			return err
		}
		expected := rsState{"leaf": rows("leaf", want...)}
		for i, row := range expected["leaf"] {
			expected["leaf"][i] = strings.TrimPrefix(row, "leaf: ")
		}
		if e.ref != nil {
			rebuilt, wrote, err := e.ref.rebuildCopy(fossilRepo{path: path})
			if err != nil || wrote {
				return expectf("fossil rebuild: %v, stored artifacts of its own: %v", err, wrote)
			}
			fossil, err := readState(rebuilt, labels, []string{"leaf"}, nil)
			if err != nil {
				return err
			}
			if d := diffState(expected, fossil, "want", "fossil rebuild"); d != "" {
				return expectf("fossil(1) disagrees with the case: %s", d)
			}
		}
		got, err := readState(path, labels, []string{"leaf"}, nil)
		if err != nil {
			return err
		}
		if d := diffState(expected, got, "want", "commits"); d != "" {
			return expectf("%s", d)
		}
		return nil
	}}
}

// commitAll makes each commit in a new repository, which it closes, and
// returns its path and the label of every check-in's hash.
func commitAll(e *rsEnv, steps []commitStep) (string, map[string]string, error) {
	path := filepath.Join(e.t.TempDir(), "commits.fossil")
	r, err := repo.Create(path, "u", simio.CryptoRand{}, "")
	if err != nil {
		return "", nil, err
	}
	defer r.Close()
	rids, labels := map[string]libfossil.FslID{}, map[string]string{}
	for i, step := range steps {
		rid, uuid, err := commitOne(r, step, rids, i+1)
		if err != nil {
			return "", nil, err
		}
		rids[step.label], labels[uuid] = rid, step.label
	}
	return path, labels, r.Close()
}

func commitOne(
	r *repo.Repo, step commitStep, rids map[string]libfossil.FslID, s int,
) (libfossil.FslID, string, error) {
	var parents []libfossil.FslID
	for _, label := range step.parents {
		parents = append(parents, rids[label])
	}
	if step.branch != "" {
		return branch.Create(r, branch.CreateOpts{
			Name: step.branch, Parent: parents[0], User: "u", Time: rsAt(s)})
	}
	opts := manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "f.txt", Content: []byte(step.label + "\n")}},
		Comment: step.label, User: "u", Time: rsAt(s),
	}
	if len(parents) > 0 {
		opts.Parent, opts.MergeParents = parents[0], parents[1:]
	}
	return manifest.Checkin(r, opts)
}

func commitLeafCases() []rsAPICase {
	return []rsAPICase{
		// c1 leaves the table when c2 lands on its branch.
		committedLeaves("repostate.7.3.incremental", "RS-7.3-2", []commitStep{
			{label: "c1"}, {label: "c2", parents: []string{"c1"}},
		}, "c2"),
		// b, a merge parent of m on its own branch, is rechecked too.
		committedLeaves("repostate.7.3.recheck-parents", "RS-7.3-3", []commitStep{
			{label: "c1"}, {label: "a", parents: []string{"c1"}},
			{label: "b", parents: []string{"c1"}}, {label: "m", parents: []string{"a", "b"}},
		}, "m"),
		// feat's root is checked only once its branch tag is in place, so
		// c1, whose only child is on feat, stays a leaf.
		committedLeaves("repostate.7.3.batch-applied", "RS-7.3-4", []commitStep{
			{label: "c1"}, {label: "feat", parents: []string{"c1"}, branch: "feat"},
		}, "c1", "feat"),
	}
}
