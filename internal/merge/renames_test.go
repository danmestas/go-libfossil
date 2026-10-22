package merge

import (
	"reflect"
	"testing"

	"github.com/danmestas/go-libfossil/internal/path"
)

func TestMatchFiles(t *testing.T) {
	ch := func(pairs ...string) []path.NameChange {
		var out []path.NameChange
		for i := 0; i < len(pairs); i += 2 {
			out = append(out, path.NameChange{From: pairs[i], To: pairs[i+1]})
		}
		return out
	}
	local := func(names ...string) []LocalFile {
		var out []LocalFile
		for _, n := range names {
			out = append(out, LocalFile{Name: n})
		}
		return out
	}
	cases := []struct {
		name string
		in   MatchInput
		want []FileMatch
	}{{
		name: "renamed in M",
		in: MatchInput{Local: local("f"), Pivot: []string{"f"}, Merged: []string{"r"},
			ToMerged: ch("f", "r")},
		want: []FileMatch{{Local: 0, Pivot: "f", Merged: "r"}},
	}, {
		name: "renamed in V",
		in: MatchInput{Local: local("r"), Pivot: []string{"f"}, Merged: []string{"f"},
			ToLocal: ch("f", "r")},
		want: []FileMatch{{Local: 0, Pivot: "f", Merged: "f"}},
	}, {
		name: "renamed apart",
		in: MatchInput{Local: local("r"), Pivot: []string{"f"}, Merged: []string{"s"},
			ToLocal: ch("f", "r"), ToMerged: ch("f", "s")},
		want: []FileMatch{{Local: 0, Pivot: "f", Merged: "s"}},
	}, {
		name: "swapped in M",
		in: MatchInput{Local: local("a", "b", "f"), Pivot: []string{"a", "b", "f"},
			Merged: []string{"a", "b", "f"}, ToMerged: ch("a", "f", "f", "a")},
		want: []FileMatch{
			{Local: 0, Pivot: "a", Merged: "f"},
			{Local: 1, Pivot: "b", Merged: "b"},
			{Local: 2, Pivot: "f", Merged: "a"},
		},
	}, {
		name: "added and deleted",
		in: MatchInput{Local: []LocalFile{{Name: "f"}, {Name: "n", Added: true}},
			Pivot: []string{"f", "g"}, Merged: []string{"g", "m"}},
		want: []FileMatch{
			{Local: 0, Pivot: "f", Merged: "f"},
			{Local: 1, Pivot: "n", Merged: "n"},
			{Local: -1, Pivot: "g", Merged: "g"},
			{Local: -1, Merged: "m"},
		},
	}, {
		name: "renamed locally, not committed",
		in: MatchInput{Local: []LocalFile{{Name: "r", Orig: "f"}}, Pivot: []string{"f"},
			Merged: []string{"f"}},
		want: []FileMatch{{Local: 0, Pivot: "f", Merged: "f"}},
	}, {
		// The name pivot is not the content pivot: P's name for the file
		// is filled in from N's.
		name: "name pivot fill-in",
		in: MatchInput{Local: local("c"), Pivot: []string{"a"}, Merged: []string{"a"},
			ToLocal: ch("a", "c"), NamePivotDiffers: true},
		want: []FileMatch{{Local: 0, Pivot: "a", Merged: "a"}},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MatchFiles(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("MatchFiles = %+v, want %+v", got, tc.want)
			}
		})
	}
}
