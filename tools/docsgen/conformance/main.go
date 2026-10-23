// Command conformance turns the Markdown report TestCoverage writes into the
// docs site's conformance data: per-draft counts for the landing page and
// every requirement row for the reference page. The requirement text and the
// draft each table pins come from the requirements tables themselves.
//
//	CONFORMANCE_REPORT=/tmp/report.md go test ./conformance -run '^TestCoverage$' -count=1
//	go run ./tools/docsgen/conformance -report /tmp/report.md -out docs/site/data/conformance.json
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// titles names each suite as the site shows it, in TestCoverage's order.
var titles = map[string]string{
	"artifact":    "Artifact format",
	"delta":       "Delta format",
	"repostate":   "Repository state",
	"sync":        "Sync protocol",
	"submanifest": "Sub-manifests",
	"sparsecone":  "Sparse cones",
	"horizon":     "Horizons",
}

type Data struct {
	Requirements int     `json:"requirements"`
	Covered      int     `json:"covered"`
	Partial      int     `json:"partial"`
	Suites       []Suite `json:"suites"`
}

type Suite struct {
	Suite        string `json:"suite"`
	Title        string `json:"title"`
	Draft        string `json:"draft"`
	Requirements int    `json:"requirements"`
	Covered      int    `json:"covered"`
	Partial      int    `json:"partial"`
	Deviation    int    `json:"deviation"`
	Failing      int    `json:"failing"`
	Gap          int    `json:"gap"`
	Cases        int    `json:"cases"`
	Rows         []Row  `json:"rows"`
}

type Row struct {
	ID      string `json:"id"`
	Section string `json:"section"`
	Status  string `json:"status"`
	Cases   int    `json:"cases"`
	Text    string `json:"text"`
	Notes   string `json:"notes"`
}

// requirement is what a requirements table says about one ID.
type requirement struct {
	text  string
	draft string
}

func main() {
	report := flag.String("report", "", "TestCoverage's CONFORMANCE_REPORT output")
	reqDir := flag.String("requirements", "conformance/requirements", "directory of requirements tables")
	out := flag.String("out", "docs/site/data/conformance.json", "file to write")
	flag.Parse()
	if err := run(*report, *reqDir, *out); err != nil {
		fmt.Fprintln(os.Stderr, "conformance:", err)
		os.Exit(1)
	}
}

func run(reportPath, reqDir, out string) error {
	if reportPath == "" {
		return fmt.Errorf("-report is required")
	}
	reqs, err := readTables(reqDir)
	if err != nil {
		return err
	}
	report, err := os.ReadFile(reportPath)
	if err != nil {
		return err
	}
	data, err := parseReport(report, reqs)
	if err != nil {
		return err
	}
	buf, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(out, append(buf, '\n'), 0o644)
}

// readTables maps every requirement ID to its text and the draft its table
// pins ("# draft\t<file>\t<sha256>").
func readTables(dir string) (map[string]requirement, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.tsv"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no requirements tables in %s", dir)
	}
	reqs := map[string]requirement{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		draft := ""
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Split(line, "\t")
			if len(fields) >= 2 && fields[0] == "# draft" {
				draft = strings.TrimSuffix(fields[1], ".txt")
				continue
			}
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if len(fields) < 3 {
				return nil, fmt.Errorf("%s: malformed row %q", path, line)
			}
			reqs[fields[0]] = requirement{text: fields[2], draft: draft}
		}
	}
	return reqs, nil
}

// parseReport reads the report's "## <suite>" sections and their
// "| id | section | status | cases | notes |" rows.
func parseReport(report []byte, reqs map[string]requirement) (*Data, error) {
	data := &Data{}
	var cur *Suite
	sc := bufio.NewScanner(bytes.NewReader(report))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if name, ok := strings.CutPrefix(line, "## "); ok {
			title, known := titles[name]
			if !known {
				return nil, fmt.Errorf("suite %q has no title in tools/docsgen/conformance", name)
			}
			data.Suites = append(data.Suites, Suite{Suite: name, Title: title})
			cur = &data.Suites[len(data.Suites)-1]
			continue
		}
		if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "| Requirement ") {
			continue
		}
		if cur == nil {
			return nil, fmt.Errorf("row before any suite: %q", line)
		}
		row, err := parseRow(line, reqs)
		if err != nil {
			return nil, err
		}
		if cur.Draft == "" {
			cur.Draft = reqs[row.ID].draft
		}
		cur.Rows = append(cur.Rows, row)
		cur.Requirements++
		cur.Cases += row.Cases
		switch row.Status {
		case "COVERED":
			cur.Covered++
		case "PARTIAL":
			cur.Partial++
		case "DEVIATION":
			cur.Deviation++
		case "FAILING":
			cur.Failing++
		case "GAP":
			cur.Gap++
		default:
			return nil, fmt.Errorf("%s: unknown status %q", row.ID, row.Status)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(data.Suites) != len(titles) {
		return nil, fmt.Errorf("report has %d suites, want %d", len(data.Suites), len(titles))
	}
	for _, s := range data.Suites {
		if s.Requirements == 0 {
			return nil, fmt.Errorf("suite %s has no requirements", s.Suite)
		}
		data.Requirements += s.Requirements
		data.Covered += s.Covered
		data.Partial += s.Partial
	}
	return data, nil
}

func parseRow(line string, reqs map[string]requirement) (Row, error) {
	// Notes are free text and may hold " | ", so only the first four
	// separators split.
	fields := strings.SplitN(strings.TrimSuffix(strings.TrimPrefix(line, "| "), " |"), " | ", 5)
	if len(fields) != 5 {
		return Row{}, fmt.Errorf("malformed report row %q", line)
	}
	cases, err := strconv.Atoi(fields[3])
	if err != nil {
		return Row{}, fmt.Errorf("row %s: cases %q: %w", fields[0], fields[3], err)
	}
	req, ok := reqs[fields[0]]
	if !ok {
		return Row{}, fmt.Errorf("row %s is in no requirements table", fields[0])
	}
	return Row{
		ID:      fields[0],
		Section: fields[1],
		Status:  fields[2],
		Cases:   cases,
		Text:    req.text,
		Notes:   strings.TrimSpace(fields[4]),
	}, nil
}
