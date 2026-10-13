package conformance

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/verify"
)

// TestRepoStateCorpus derives the tables of real repositories three ways
// and compares them, table by table and column by column: `fossil rebuild`
// on one copy is the reference; verify.Rebuild runs on a second copy, and a
// manifest.Crosslink sweep over emptied tables on a third. It runs only
// when CONFORMANCE_REPOS names the repositories, colon-separated, and
// fossil(1) is present. The originals are never written.
func TestRepoStateCorpus(t *testing.T) {
	paths := os.Getenv("CONFORMANCE_REPOS")
	if paths == "" {
		t.Skip("set CONFORMANCE_REPOS to colon-separated .fossil paths to run")
	}
	ref := newFossilReference(t)
	if ref == nil {
		t.Skip("fossil(1) not found")
	}
	for _, path := range strings.Split(paths, ":") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			reference, wrote, err := ref.rebuildCopy(fossilRepo{path: copyRepo(t, path)})
			if err != nil {
				t.Fatal(err)
			}
			if wrote {
				t.Errorf("fossil rebuild stored artifacts of its own; the reference is not " +
					"derived from the repository's artifacts alone")
			}
			want := corpusState(t, reference)
			for _, way := range []corpusWay{{"verify.Rebuild", goRebuild}, {"Crosslink", goSweep}} {
				got := corpusState(t, way.derive(t, copyRepo(t, path)))
				for _, line := range corpusDiff(want, got) {
					t.Errorf("%s: %s", way.name, line)
				}
			}
		})
	}
}

// corpusWay is one way go-libfossil derives a repository's tables.
type corpusWay struct {
	name   string
	derive func(t *testing.T, path string) string
}

func goRebuild(t *testing.T, path string) string {
	r, err := repo.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := verify.Rebuild(r); err != nil {
		t.Fatal(err)
	}
	return path
}

// goSweep empties the tables a crosslink fills, as they are in a clone
// that has received every artifact, then crosslinks once.
func goSweep(t *testing.T, path string) string {
	r, err := repo.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, table := range []string{"event", "plink", "leaf", "mlink", "tagxref", "forumpost",
		"attachment", "backlink", "cherrypick", "orphan"} {
		if _, err := r.DB().Exec("DELETE FROM " + table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manifest.Crosslink(r); err != nil {
		t.Fatal(err)
	}
	return path
}

// corpusKeys is how many leading columns identify a row of each table; the
// rest are compared column by column.
var corpusKeys = map[string]int{
	"event": 1, "leaf": 1, "mlink": 2, "phantom": 1, "plink": 1, "tagxref": 2,
}

// corpusColumns names the compared columns, keys first, as rsQueries reads
// them.
var corpusColumns = map[string][]string{
	"event":   {"objid", "type", "mtime"},
	"leaf":    {"rid"},
	"mlink":   {"mid", "fid", "pid", "pmid", "name", "isaux"},
	"phantom": {"uuid"},
	"plink":   {"pid>cid", "isprim", "mtime", "baseid"},
	"tagxref": {"tag", "rid", "tagtype", "srcid", "origid", "value", "mtime"},
}

// corpusState reads every table, keyed: rows[table][key] = the other
// columns. mlink's key is mid and filename.
func corpusState(t *testing.T, path string) map[string]map[string][]string {
	conn, err := db.OpenSQL(path, db.OpenConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	state := map[string]map[string][]string{}
	for _, table := range rsAllTables {
		rows, err := queryColumns(conn, rsQueries[table], nil)
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		keyed := map[string][]string{}
		for _, columns := range rows {
			if table == "mlink" {
				columns = append([]string{columns[0], columns[4]},
					columns[1], columns[2], columns[3], columns[5])
			}
			n := corpusKeys[table]
			keyed[strings.Join(columns[:n], " ")] = columns[n:]
		}
		state[table] = keyed
	}
	return state
}

// corpusDiff summarizes how got differs from want: rows missing and extra
// per table, with the commonest first column, and differing values per
// column.
func corpusDiff(want, got map[string]map[string][]string) []string {
	var lines []string
	for _, table := range rsAllTables {
		missing, extra := map[string]int{}, map[string]int{}
		differ := map[string]int{}
		for key, values := range want[table] {
			other, ok := got[table][key]
			if !ok {
				missing[corpusGroup(table, key, values)]++
				continue
			}
			for i := range values {
				if values[i] != other[i] {
					differ[corpusColumns[table][corpusKeys[table]+i]]++
				}
			}
		}
		for key, values := range got[table] {
			if _, ok := want[table][key]; !ok {
				extra[corpusGroup(table, key, values)]++
			}
		}
		lines = append(lines, corpusLines(table, missing, extra, differ)...)
	}
	return lines
}

func corpusLines(table string, missing, extra, differ map[string]int) []string {
	var lines []string
	if n := total(missing); n > 0 {
		lines = append(lines, fmt.Sprintf("%s: %d rows missing (%s)", table, n, breakdown(missing)))
	}
	if n := total(extra); n > 0 {
		lines = append(lines, fmt.Sprintf("%s: %d rows extra (%s)", table, n, breakdown(extra)))
	}
	for _, column := range sortedKeys(differ) {
		lines = append(lines, fmt.Sprintf("%s.%s: %d rows differ", table, column, differ[column]))
	}
	return lines
}

// corpusGroup is what a summary counts a row under: a tag row's tag name,
// with the per-ticket, per-page and per-technote names folded, an event's
// type, and nothing for tables keyed by hashes alone.
func corpusGroup(table, key string, values []string) string {
	switch table {
	case "tagxref":
		name := strings.Fields(key)[0]
		for _, prefix := range []string{"tkt-", "wiki-", "event-"} {
			if strings.HasPrefix(name, prefix) {
				return prefix + "*"
			}
		}
		return name
	case "event":
		return "type " + values[0]
	default:
		return "rows"
	}
}

// breakdown lists the counts per group.
func breakdown(counts map[string]int) string {
	var parts []string
	for _, name := range sortedKeys(counts) {
		parts = append(parts, fmt.Sprintf("%s %d", name, counts[name]))
	}
	return strings.Join(parts, ", ")
}

func total(counts map[string]int) int {
	n := 0
	for _, c := range counts {
		n += c
	}
	return n
}

func sortedKeys(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
