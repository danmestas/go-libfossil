package conformance

import (
	"fmt"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/verify"
)

func TestReparentWhitespace(t *testing.T) {
	base := rsBuild(chain())
	h1, h2 := base.hashes["c1"], base.hashes["c2"]
	for _, tc := range []struct {
		name   string
		value  string
		want   []string
		prior  bool
		cancel bool
		root   bool
	}{
		{name: "single", value: h1, want: []string{"c1"}},
		{name: "uppercase", value: strings.ToUpper(h1), want: []string{"c1"}},
		{name: "multiple", value: h1 + " " + h2, want: []string{"c1", "c2"}},
		{name: "leading space", value: " " + h1, want: []string{"c2"}},
		{name: "repeated spaces", value: h1 + "  " + h2, want: []string{"c2"}},
		{name: "trailing space", value: h1 + " ", want: []string{"c1"}},
		{name: "repeated trailing spaces", value: h1 + "  ", want: []string{"c2"}},
		{name: "tab", value: h1 + "\t" + h2, want: []string{"c1", "c2"}},
		{name: "newline", value: h1 + "\n" + h2, want: []string{"c1", "c2"}},
		{name: "carriage return", value: h1 + "\r" + h2, want: []string{"c1", "c2"}},
		{name: "vertical tab", value: h1 + "\v" + h2, want: []string{"c1", "c2"}},
		{name: "form feed", value: h1 + "\f" + h2, want: []string{"c1", "c2"}},
		{name: "leading tab", value: "\t" + h1, want: []string{"c2"}},
		{name: "mixed separators", value: h1 + "\r\n" + h2, want: []string{"c2"}},
		{name: "nonbreaking space", value: h1 + " " + h2, want: []string{"c2"}},
		{name: "nul terminates", value: h1 + "\x00" + h2, want: []string{"c1"}},
		{name: "empty", want: []string{"c2"}},
		{name: "root stays root", root: true},
		{name: "malformed retains prior", value: " " + h2, prior: true, want: []string{"c1"}},
		{name: "cancel retains prior", prior: true, cancel: true, want: []string{"c1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := "c3"
			if tc.root {
				target = "c1"
			}
			arts := chain()
			if tc.prior {
				arts = with(arts, control("prior", 8, "+parent {"+target+"} "+h1))
			}
			prefix := "+parent"
			if tc.cancel {
				prefix = "-parent"
			}
			arts = with(arts, control("t", 9, prefix+" {"+target+"} "+fossilize(tc.value)))
			script := rsBuild(arts)
			want := rows("plink", "c1>c2 1 @2 -", "c3>c4 1 @4 -")
			if tc.root {
				want = chainPlink
			} else {
				for i, parent := range tc.want {
					primary := 0
					if i == 0 {
						primary = 1
					}
					want = append(want, fmt.Sprintf("plink: %s>c3 %d @3 -", parent, primary))
				}
			}
			c := rsCase{tables: []string{"plink"}, want: want}
			assert := func(path string, stage string) {
				t.Helper()
				state, err := readState(path, script.labels, c.tables, nil)
				if err != nil {
					t.Fatal(err)
				}
				if diff := diffState(c.expected(), state, "want", stage); diff != "" {
					t.Error(diff)
				}
				r, err := repo.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				report, err := verify.Verify(r)
				if err != nil {
					t.Fatal(err)
				}
				if !report.OK() {
					t.Errorf("%s Verify: %v", stage, report.Issues)
				}
			}
			if ref := newFossilReference(t); ref != nil {
				f, err := ref.loadScript(script, nil)
				if err != nil {
					t.Fatal(err)
				}
				assert(f.path, "fossil")
			}
			path, err := goLoad(t, script, loadEach, nil)
			if err != nil {
				t.Fatal(err)
			}
			assert(path, "go")
		})
	}
}
