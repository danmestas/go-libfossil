package checkout

import (
	"testing"

	"github.com/danmestas/go-libfossil/internal/vfile"
)

// fossil's vfile_check_signature, the permission half: the chnged value it
// stores given the content verdict and the file's recorded and current
// permission.
func TestNextPermState(t *testing.T) {
	cases := []struct {
		name          string
		chnged        int64
		orig, current filePerm
		want          int64
	}{
		{"unchanged", 0, permReg, permReg, 0},
		{"became exec", 0, permReg, permExe, 6},
		{"lost exec", 0, permExe, permReg, 8},
		{"became link", 0, permReg, permLnk, 7},
		{"exec became link", 0, permExe, permLnk, 7},
		{"link became file", 0, permLnk, permReg, 9},
		{"link became exec", 0, permLnk, permExe, 9},
		{"edited and became link", 1, permReg, permLnk, 7},
		{"edited link became file", 1, permLnk, permReg, 9},
		{"edited and became exec", 1, permReg, permExe, 1},
		{"edited and lost exec", 1, permExe, permReg, 1},
		{"merged and became exec", 2, permReg, permExe, 2},
		{"exec put back", 6, permReg, permReg, 0},
		{"unexec put back", 8, permExe, permExe, 0},
		{"exec, now unexec", 6, permExe, permReg, 8},
		{"link put back", 7, permLnk, permLnk, 0},
		{"unlink put back", 9, permLnk, permLnk, 0},
		{"link, still link", 7, permReg, permLnk, 7},
	}
	for _, tc := range cases {
		if got := nextPermState(tc.chnged, tc.orig, tc.current); got != tc.want {
			t.Errorf("%s: chnged %d, want %d", tc.name, got, tc.want)
		}
	}
}

// The mode changes classify as fossil's changes command labels them, after a
// rename (kept, as for an edit) and before an edit.
func TestClassifyModeChanges(t *testing.T) {
	want := map[int64]FileChange{
		6: ChangeExecutable, 7: ChangeSymlink, 8: ChangeUnexec, 9: ChangeUnlink,
	}
	for chnged, change := range want {
		r := vfile.Row{Pathname: "f", RID: 2, MRID: 2, Chnged: chnged}
		if got := vfile.Classify(r, false); got != change {
			t.Errorf("chnged %d: Classify = %v, want %v", chnged, got, change)
		}
		if got := vfile.Classify(r, true); got != ChangeMissing {
			t.Errorf("chnged %d, missing: Classify = %v, want missing", chnged, got)
		}
	}
}
