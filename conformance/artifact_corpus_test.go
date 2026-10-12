package conformance

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// FuzzArtifactRoundTrip checks that whatever deck.Parse accepts, Marshal
// writes back as an artifact that parses to the same deck. Equal bytes are
// not required: accepted input need not be in the form Marshal writes.
func FuzzArtifactRoundTrip(f *testing.F) {
	for _, b := range artifactBases {
		f.Add(withZ(strings.Join(b.cards, "")))
	}
	for _, c := range artifactCases() {
		f.Add(c.input)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if data == nil {
			data = []byte{}
		}
		first, err := deck.Parse(data)
		if err != nil {
			return
		}
		encoded, err := first.Marshal()
		if err != nil {
			t.Fatalf("Marshal of an accepted artifact: %v", err)
		}
		second, err := deck.Parse(encoded)
		if err != nil {
			t.Fatalf("deck.Parse rejects what Marshal wrote, %q: %v", encoded, err)
		}
		for _, loss := range knownLosses {
			loss.forgive(first)
			loss.forgive(second)
		}
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("round trip changed the deck:\n%+v\n%+v", first, second)
		}
	})
}

// knownLosses are the changes a round trip is already known to make, each
// tracked by a gap case, so the fuzz test looks for new ones.
// TestArtifactGapKeys fails once a case's gap is removed, so the matching
// loss is removed with it.
var knownLosses = []struct {
	caseID  string
	forgive func(*deck.Deck)
}{}

// corpusBudget bounds TestArtifactCorpus as a whole.
const corpusBudget = 20 * time.Minute

// fossilParsed lists the rids fossil(1) crosslinked as structural
// artifacts, read from the tables crosslinking fills. A table a repository
// lacks is skipped.
var fossilParsed = []string{
	"SELECT objid FROM event",
	"SELECT srcid FROM tagxref WHERE srcid > 0",
	"SELECT rid FROM tagxref WHERE tagid = (SELECT tagid FROM tag WHERE tagname = 'cluster')",
	"SELECT attachid FROM attachment",
	"SELECT tkt_rid FROM ticketchng",
	"SELECT fpid FROM forumpost",
}

// TestArtifactCorpus parses, with deck.Parse, every artifact fossil(1)
// parsed in real repositories. It runs only when CONFORMANCE_REPOS names
// them, colon-separated. Each repository is copied first, so the original
// is never written.
func TestArtifactCorpus(t *testing.T) {
	paths := os.Getenv("CONFORMANCE_REPOS")
	if paths == "" {
		t.Skip("set CONFORMANCE_REPOS to colon-separated .fossil paths to run")
	}
	deadline := time.Now().Add(corpusBudget)
	if d, ok := t.Deadline(); ok && d.Before(deadline) {
		deadline = d.Add(-30 * time.Second)
	}
	for _, path := range strings.Split(paths, ":") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			parseCorpus(t, copyRepo(t, path), deadline)
		})
	}
}

func copyRepo(t *testing.T, path string) string {
	t.Helper()
	source, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	copied := filepath.Join(t.TempDir(), filepath.Base(path))
	target, err := os.Create(copied)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(target, source); err != nil {
		t.Fatal(err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	return copied
}

// parseCorpus parses each artifact fossil parsed and reports every
// rejection, grouped by cause.
func parseCorpus(t *testing.T, path string, deadline time.Time) {
	r, err := repo.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	rids := corpusRids(t, r)
	cache := content.NewCache(64 << 20)
	causes := map[string][]libfossil.FslID{}
	parsed := 0
	for _, rid := range rids {
		if time.Now().After(deadline) {
			t.Errorf("out of time after %d of %d artifacts", parsed, len(rids))
			break
		}
		data, err := cache.Expand(r.DB(), rid)
		if err != nil {
			cause := "expand: " + generalize(err.Error())
			causes[cause] = append(causes[cause], rid)
			continue
		}
		parsed++
		if _, err := deck.Parse(data); err != nil {
			cause := generalize(err.Error())
			causes[cause] = append(causes[cause], rid)
		}
	}
	reportCauses(t, len(rids), parsed, causes)
}

func corpusRids(t *testing.T, r *repo.Repo) []libfossil.FslID {
	seen := map[libfossil.FslID]bool{}
	for _, query := range fossilParsed {
		rows, err := r.DB().Query(query)
		if err != nil {
			if strings.Contains(err.Error(), "no such table") {
				continue
			}
			t.Fatal(err)
		}
		for rows.Next() {
			var rid int64
			if err := rows.Scan(&rid); err != nil {
				t.Fatal(err)
			}
			seen[libfossil.FslID(rid)] = true
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	rids := make([]libfossil.FslID, 0, len(seen))
	for rid := range seen {
		rids = append(rids, rid)
	}
	sort.Slice(rids, func(i, j int) bool { return rids[i] < rids[j] })
	return rids
}

// generalize strips the artifact-specific parts of an error, so rejections
// with the same cause group together.
var specifics = regexp.MustCompile(`"[^"]*"|'[^']*'|[0-9a-f]{8,}|\d+`)

func generalize(message string) string {
	return specifics.ReplaceAllString(message, "_")
}

func reportCauses(t *testing.T, total, parsed int, causes map[string][]libfossil.FslID) {
	rejected := 0
	var lines []string
	for cause, rids := range causes {
		rejected += len(rids)
		examples := rids
		if len(examples) > 5 {
			examples = examples[:5]
		}
		lines = append(lines, fmt.Sprintf("%s: %d, e.g. rids %v", cause, len(rids), examples))
	}
	sort.Strings(lines)
	t.Logf("%d artifacts fossil parsed, %d expanded, %d not accepted", total, parsed, rejected)
	for _, line := range lines {
		t.Errorf("rejected: %s", line)
	}
}
