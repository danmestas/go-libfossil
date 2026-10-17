package conformance

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// suite is one draft's requirements table and the cases that exercise it.
type suite struct {
	name  string // case IDs start with name + "."
	table string // path of the requirements table, relative to this package
	cases []Case
}

// suites lists every suite the coverage test audits. Each draft's test file
// adds its suite here.
func suites() []suite {
	return []suite{
		{name: "artifact", table: "requirements/artifact.tsv", cases: artifactSuiteCases()},
		{name: "delta", table: "requirements/delta.tsv", cases: deltaSuiteCases()},
		{name: "repostate", table: "requirements/repostate.tsv", cases: repostateSuiteCases()},
		{name: "sync", table: "requirements/sync.tsv", cases: syncSuiteCases()},
		{name: "submanifest", table: "requirements/submanifest.tsv", cases: submanifestSuiteCases()},
	}
}

// requirement is one row of a requirements table.
type requirement struct {
	ID      string
	Section string
	Text    string
	// Scope, when set, says what the cases leave out; such a requirement is
	// at most PARTIAL.
	Scope string
}

// source is a document a table was written against, pinned by its SHA-256.
type source struct {
	Path   string // relative to rfcs/
	SHA256 string
}

type table struct {
	sources      []source
	requirements []requirement
}

// Table format: tab-separated. Lines starting with "#" are comments, except
// "# draft\t<file>\t<sha256>" and "# erratum\t<file>\t<sha256>", which pin
// the sources. Every other line is "<id>\t<section>\t<requirement>[\t<scope>]".
func parseTable(data []byte) (table, error) {
	var parsed table
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if strings.HasPrefix(line, "#") {
			kind := strings.TrimSpace(strings.TrimPrefix(fields[0], "#"))
			if kind != "draft" && kind != "erratum" {
				continue
			}
			if len(fields) != 3 {
				return table{}, fmt.Errorf("line %d: %s pin needs a file and a SHA-256", lineNumber, kind)
			}
			path := fields[1]
			if kind == "erratum" {
				path = "erratum/" + path
			}
			parsed.sources = append(parsed.sources, source{Path: path, SHA256: fields[2]})
			continue
		}
		if len(fields) != 3 && len(fields) != 4 {
			return table{}, fmt.Errorf("line %d: want 3 or 4 tab-separated fields, got %d",
				lineNumber, len(fields))
		}
		row := requirement{ID: fields[0], Section: fields[1], Text: fields[2]}
		if len(fields) == 4 {
			row.Scope = fields[3]
		}
		if row.ID == "" || row.Section == "" || row.Text == "" {
			return table{}, fmt.Errorf("line %d: empty ID, section or requirement", lineNumber)
		}
		parsed.requirements = append(parsed.requirements, row)
	}
	if err := scanner.Err(); err != nil {
		return table{}, err
	}
	if len(parsed.sources) == 0 {
		return table{}, fmt.Errorf("no draft pinned")
	}
	return parsed, nil
}

// verifySources checks every pinned document still has the hash the table
// was written against. A refreshed draft must be re-read before its table's
// pin is moved.
func verifySources(tbl table, rfcDir string) []error {
	var problems []error
	for _, pinned := range tbl.sources {
		data, err := os.ReadFile(filepath.Join(rfcDir, pinned.Path))
		if err != nil {
			problems = append(problems, err)
			continue
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != pinned.SHA256 {
			problems = append(problems, fmt.Errorf(
				"%s: SHA-256 is %s, table pins %s; re-read the changes, then move the pin",
				pinned.Path, got, pinned.SHA256))
		}
	}
	return problems
}

// audit checks a suite's cases against its table: IDs unique and prefixed by
// the suite name, and every case naming a requirement in the table.
func audit(s suite, tbl table) []error {
	var problems []error
	known := map[string]bool{}
	for _, row := range tbl.requirements {
		if known[row.ID] {
			problems = append(problems, fmt.Errorf("requirement %s listed twice", row.ID))
		}
		known[row.ID] = true
	}
	seen := map[string]bool{}
	for _, c := range s.cases {
		if !strings.HasPrefix(c.ID, s.name+".") {
			problems = append(problems, fmt.Errorf("case %s does not start with %q", c.ID, s.name+"."))
		}
		if seen[c.ID] {
			problems = append(problems, fmt.Errorf("case %s listed twice", c.ID))
		}
		seen[c.ID] = true
		if !known[c.Req] {
			problems = append(problems, fmt.Errorf("case %s names unknown requirement %q", c.ID, c.Req))
		}
		if c.Gap != "" && !strings.HasPrefix(c.Gap, "#") {
			problems = append(problems, fmt.Errorf(
				"case %s: Gap %q must be an issue reference like #270", c.ID, c.Gap))
		}
		// "#0" stands in for an issue not yet filed; it must not reach CI.
		if c.Gap == "#0" {
			problems = append(problems, fmt.Errorf("case %s: Gap #0 is a placeholder; file the issue", c.ID))
		}
	}
	return problems
}

type status string

const (
	statusCovered   status = "COVERED"
	statusPartial   status = "PARTIAL"
	statusFailing   status = "FAILING"
	statusDeviation status = "DEVIATION"
	statusGap       status = "GAP"
)

// assess derives one requirement's status from its cases. A known failure
// outranks a deliberate deviation, which outranks partial coverage.
func assess(row requirement, cases []Case) (status, []string) {
	var failing, deviating []string
	for _, c := range cases {
		if c.Gap != "" {
			failing = append(failing, c.ID+" "+c.Gap)
		}
		if c.Deviation != "" {
			deviating = append(deviating, c.ID+": "+c.Deviation)
		}
	}
	switch {
	case len(cases) == 0:
		return statusGap, nil
	case len(failing) > 0:
		return statusFailing, failing
	case len(deviating) > 0:
		return statusDeviation, deviating
	case row.Scope != "":
		return statusPartial, []string{row.Scope}
	default:
		return statusCovered, nil
	}
}

func TestCoverage(t *testing.T) {
	var report strings.Builder
	for _, s := range suites() {
		data, err := os.ReadFile(s.table)
		if err != nil {
			t.Fatal(err)
		}
		tbl, err := parseTable(data)
		if err != nil {
			t.Fatalf("%s: %v", s.table, err)
		}
		for _, problem := range append(verifySources(tbl, "rfcs"), audit(s, tbl)...) {
			t.Errorf("%s: %v", s.name, problem)
		}
		writeSuiteReport(t, &report, s, tbl)
	}
	if path := os.Getenv("CONFORMANCE_REPORT"); path != "" {
		if err := os.WriteFile(path, []byte(report.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// writeSuiteReport logs one line of counts per suite and appends the
// requirement-by-requirement table to the Markdown report.
func writeSuiteReport(t *testing.T, report *strings.Builder, s suite, tbl table) {
	t.Helper()
	byReq := map[string][]Case{}
	for _, c := range s.cases {
		byReq[c.Req] = append(byReq[c.Req], c)
	}
	counts := map[status]int{}
	fmt.Fprintf(report, "## %s\n\n| Requirement | Section | Status | Cases | Notes |\n", s.name)
	report.WriteString("|---|---|---|---|---|\n")
	for _, row := range tbl.requirements {
		got, notes := assess(row, byReq[row.ID])
		counts[got]++
		fmt.Fprintf(report, "| %s | %s | %s | %d | %s |\n",
			row.ID, row.Section, got, len(byReq[row.ID]), strings.Join(notes, "; "))
	}
	report.WriteString("\n")
	order := []status{statusCovered, statusPartial, statusDeviation, statusFailing, statusGap}
	var parts []string
	for _, st := range order {
		parts = append(parts, fmt.Sprintf("%s %d", st, counts[st]))
	}
	t.Logf("%s: %d requirements, %d cases: %s",
		s.name, len(tbl.requirements), len(s.cases), strings.Join(parts, ", "))
}
