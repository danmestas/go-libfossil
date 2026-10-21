package merge

func init() { Register(&ThreeWayText{}) }

// ThreeWayText is fossil's line-level three-way merge (merge3.c), byte for
// byte: the same alignment, the same clean result, and on conflict the same
// line-numbered markers and suggested resolution fossil writes.
type ThreeWayText struct{}

func (t *ThreeWayText) Name() string { return "three-way" }

// Merge merges the changes from base to remote into local. Content fossil
// cannot diff as text (a NUL byte, or a line over 32767 bytes) is not
// merged: as fossil does, the local copy is kept and the merge is not clean.
func (t *ThreeWayText) Merge(base, local, remote []byte) (*Result, error) {
	if base == nil {
		panic("merge.ThreeWayText.Merge: base must not be nil")
	}
	if local == nil {
		panic("merge.ThreeWayText.Merge: local must not be nil")
	}
	if remote == nil {
		panic("merge.ThreeWayText.Merge: remote must not be nil")
	}
	out, conflicts, n := mergeText(base, local, remote)
	if n < 0 {
		return &Result{
			Content:   local,
			Conflicts: []Conflict{{Local: local, Remote: remote, Base: base}},
		}, nil
	}
	return &Result{Content: out, Conflicts: conflicts, Clean: n == 0}, nil
}
