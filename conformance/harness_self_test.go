package conformance

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The harness decides what a run means, so its own rules are tested here:
// how a marked case is reported, how tables are read and audited, and how
// a requirement's status is derived from its cases.

func TestJudge(t *testing.T) {
	problem := errors.New("rejected")
	tests := []struct {
		name string
		c    Case
		err  error
		want outcome
	}{
		{"conforms", Case{ID: "x.1"}, nil, outcomePass},
		{"fails", Case{ID: "x.1"}, problem, outcomeFail},
		{"known gap still failing", Case{ID: "x.1", Gap: "#1"}, problem, outcomeSkip},
		{"known gap now conforming", Case{ID: "x.1", Gap: "#1"}, nil, outcomeFail},
		{"deviation still present", Case{ID: "x.1", Deviation: "why"}, problem, outcomeSkip},
		{"deviation gone", Case{ID: "x.1", Deviation: "why"}, nil, outcomeFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, message := judge(tt.c, tt.err)
			if got != tt.want {
				t.Fatalf("judge = %d (%q), want %d", got, message, tt.want)
			}
			if got != outcomePass && !strings.Contains(message, tt.c.ID) {
				t.Fatalf("message %q does not name the case", message)
			}
		})
	}
}

func TestJudgeRefusesDoubleMark(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a case marked both Gap and Deviation was accepted")
		}
	}()
	judge(Case{ID: "x.1", Gap: "#1", Deviation: "why"}, nil)
}

func TestParseTable(t *testing.T) {
	data := "# a comment\n# draft\td.txt\tabc\n# erratum\te.md\tdef\n\n" +
		"R-1\t3.1\tNames are hashes.\n" +
		"R-2\t3.2\tLengths are fixed.\tonly SHA1 checked\n"
	tbl, err := parseTable([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl.sources) != 2 || tbl.sources[1].Path != "erratum/e.md" {
		t.Fatalf("sources = %+v", tbl.sources)
	}
	if len(tbl.requirements) != 2 || tbl.requirements[1].Scope != "only SHA1 checked" {
		t.Fatalf("requirements = %+v", tbl.requirements)
	}
	for _, bad := range []string{
		"R-1\t3.1\tNo pin.\n",
		"# draft\td.txt\tabc\nR-1\t3.1\n",
		"# draft\td.txt\nR-1\t3.1\tx\n",
		"# draft\td.txt\tabc\nR-1\t\tx\n",
	} {
		if _, err := parseTable([]byte(bad)); err == nil {
			t.Errorf("parseTable accepted %q", bad)
		}
	}
}

func TestVerifySources(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "d.txt"), []byte("draft\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// SHA-256 of "draft\n".
	good := table{sources: []source{{Path: "d.txt",
		SHA256: "7eb2ca55b87a4d45d66a63f76db11f9b4aa9106472a62b5865060f9fd8eadaaa"}}}
	if problems := verifySources(good, dir); len(problems) != 0 {
		t.Fatalf("a correct pin gave %v", problems)
	}
	stale := table{sources: []source{{Path: "d.txt", SHA256: "00"}}}
	problems := verifySources(stale, dir)
	if len(problems) != 1 || !strings.Contains(problems[0].Error(), "move the pin") {
		t.Fatalf("a stale pin gave %v", problems)
	}
	missing := table{sources: []source{{Path: "absent.txt", SHA256: "00"}}}
	if problems := verifySources(missing, dir); len(problems) != 1 {
		t.Fatalf("a missing draft gave %v", problems)
	}
}

func TestAudit(t *testing.T) {
	tbl := table{requirements: []requirement{{ID: "R-1"}, {ID: "R-1"}}}
	s := suite{name: "x", cases: []Case{
		{ID: "x.a", Req: "R-1"},
		{ID: "x.a", Req: "R-1"},
		{ID: "y.b", Req: "R-1"},
		{ID: "x.c", Req: "R-9"},
		{ID: "x.d", Req: "R-1", Gap: "270"},
		{ID: "x.e", Req: "R-1", Gap: "#0"},
	}}
	problems := audit(s, tbl)
	for _, want := range []string{"R-1 listed twice", "x.a listed twice", `y.b does not start`,
		`unknown requirement "R-9"`, "must be an issue reference",
		"Gap #0 is a placeholder"} {
		found := false
		for _, p := range problems {
			found = found || strings.Contains(p.Error(), want)
		}
		if !found {
			t.Errorf("audit missed %q in %v", want, problems)
		}
	}
}

func TestAssess(t *testing.T) {
	row := requirement{ID: "R-1"}
	scoped := requirement{ID: "R-1", Scope: "SHA1 only"}
	tests := []struct {
		name  string
		row   requirement
		cases []Case
		want  status
	}{
		{"no cases", row, nil, statusGap},
		{"all conform", row, []Case{{ID: "a"}}, statusCovered},
		{"scoped", scoped, []Case{{ID: "a"}}, statusPartial},
		{"deviation", scoped, []Case{{ID: "a", Deviation: "why"}}, statusDeviation},
		{"failing outranks deviation", row,
			[]Case{{ID: "a", Deviation: "why"}, {ID: "b", Gap: "#1"}}, statusFailing},
	}
	for _, tt := range tests {
		if got, _ := assess(tt.row, tt.cases); got != tt.want {
			t.Errorf("%s: assess = %s, want %s", tt.name, got, tt.want)
		}
	}
}

func TestWithZ(t *testing.T) {
	// The Z card fossil computed for this body (fossil test-parse-manifest
	// reports "expected 74f3096fa00a0995e85bb466be787372" for it).
	got := string(withZ("C x\nD 2026-01-02T03:04:05\n"))
	if want := "C x\nD 2026-01-02T03:04:05\nZ 74f3096fa00a0995e85bb466be787372\n"; got != want {
		t.Fatalf("withZ = %q, want %q", got, want)
	}
}

func TestFossilReference(t *testing.T) {
	ref := newFossilReference(t)
	if ref == nil {
		t.Skip("fossil not found")
	}
	accepted, reason, err := ref.parseArtifact(withZ("C x\nD 2026-01-02T03:04:05\n"))
	if err != nil || !accepted {
		t.Fatalf("fossil rejected a valid control artifact: %v %q", err, reason)
	}
	accepted, reason, err = ref.parseArtifact(withZ("D 2026-01-02T03:04:05\nC x\n"))
	if err != nil || accepted || reason == "" {
		t.Fatalf("fossil verdict on misordered cards: accepted=%v reason=%q err=%v",
			accepted, reason, err)
	}
}
