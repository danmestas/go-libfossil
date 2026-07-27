package libfossil_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/testutil"
)

// corpusParityEnv opts a run in to the corpus parity gate. The gate builds a
// repository with canonical fossil, serves it, and clones it twice -- once with
// the fossil binary and once with this library -- so it costs a server process
// and a few seconds even though the corpus is deliberately tiny. That is more
// than the default suite should pay per edit, so it is a pre-release and
// nightly check rather than a per-edit one.
const corpusParityEnv = "LIBFOSSIL_CORPUS_PARITY"

// parityTables are the derived tables compared as totals. These are the tables
// a clone is responsible for populating from the blobs it received; a
// divergence in any of them means the two stores disagree about what the same
// artifacts mean.
var parityTables = []string{"blob", "event", "mlink", "plink", "tagxref", "leaf"}

// corpusUser is the single user every artifact in the corpus is attributed to.
// Fossil refuses to write most artifact types without a resolvable user, and
// the ambient USER environment variable is not dependable under `go test`.
const corpusUser = "alice"

// TestCloneCorpusParity is the corpus parity gate for issue #188.
//
// The existing real-fossil parity test (internal/manifest) compares our
// derivation against fossil's own for fixtures constructed in-test. That
// assertion is only as broad as the shapes someone thought to build, which is
// how issue #184 -- a silent 100% loss of the ticket-change artifact shape
// (D,J,K,U,Z) -- survived a passing suite. It was found by an external
// benchmark cloning the real Fossil repository.
//
// This gate closes the class rather than the instance. It clones a real
// repository two ways and compares what each derived:
//
//	corpus --(fossil server)--> fossil clone   (reference)
//	                       \--> libfossil.Clone (ours)
//
// The comparison is per-`event`-type, not just totals, and that distinction is
// the point. On the real Fossil repository the totals moved 23,396 vs 27,099 --
// a 14% shortfall that reads like rounding. The per-type breakdown showed
// ticket events at 96 vs 3,808, which is unmissable. Deltas are reported in
// both directions because a surplus is as much a defect signal as a shortfall:
// our tagxref ran +17,119 over fossil's on the real repository, which means
// something writes tags without completing the work the tag claims is done.
//
// The gate verifies its own corpus (see requireCorpusShapes). A corpus that
// silently stopped containing ticket artifacts would pass this comparison
// while reproducing exactly the blind spot the gate exists to remove, so
// "the corpus contains the shapes that matter" is asserted, not assumed.
func TestCloneCorpusParity(t *testing.T) {
	if os.Getenv(corpusParityEnv) != "1" {
		t.Skipf("corpus parity gate is opt-in; set %s=1 to run it", corpusParityEnv)
	}
	bin := testutil.RequireFossilBin(t)

	dir := t.TempDir()
	source := buildParityCorpus(t, bin, dir)
	requireCorpusShapes(t, bin, source)

	addr := serveRepo(t, bin, source)

	reference := filepath.Join(dir, "reference.fossil")
	corpusFossil(t, bin, dir, "", "clone", "http://"+addr+"/", reference)

	ours := filepath.Join(dir, "ours.fossil")
	repo, _, err := libfossil.Clone(context.Background(), ours,
		libfossil.NewHTTPTransport("http://"+addr+"/"), libfossil.CloneOpts{})
	if err != nil {
		t.Fatalf("libfossil clone failed: %v", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("close clone: %v", err)
	}

	compareEventTypes(t, bin, reference, ours)
	compareTableTotals(t, bin, reference, ours)
}

// compareEventTypes asserts the per-type event breakdown matches. This is the
// assertion that would have caught #184: a whole artifact type going missing
// barely moves the total but takes its own type's count to zero.
func compareEventTypes(t *testing.T, bin, reference, ours string) {
	t.Helper()
	want := fossilCounts(t, bin, reference, "SELECT type, count(*) FROM event GROUP BY type")
	got := fossilCounts(t, bin, ours, "SELECT type, count(*) FROM event GROUP BY type")

	for _, typ := range unionKeys(want, got) {
		if got[typ] != want[typ] {
			t.Errorf("event type %q: got %d, want %d (%+d)",
				typ, got[typ], want[typ], got[typ]-want[typ])
		}
	}
}

// compareTableTotals asserts the derived-table totals match. A shortfall means
// we dropped work; a surplus means we wrote rows fossil did not, which on the
// real repository was the signal that tags were being written by an operation
// that never finished.
func compareTableTotals(t *testing.T, bin, reference, ours string) {
	t.Helper()
	for _, table := range parityTables {
		want := fossilCount(t, bin, reference, "SELECT count(*) FROM "+table)
		got := fossilCount(t, bin, ours, "SELECT count(*) FROM "+table)
		if got != want {
			t.Errorf("table %q: got %d rows, want %d (%+d)", table, got, want, got-want)
		}
	}
}

// buildParityCorpus constructs the corpus with the canonical binary and returns
// the repository path.
//
// The corpus is synthetic rather than a downloaded repository, for three
// reasons. It is fast and small -- seconds and well under a megabyte, against
// roughly ten minutes and a gigabyte for the real Fossil repository (#187) --
// which is what makes a nightly gate practical. It needs no network, so the
// gate cannot go red because a remote server moved. And its contents are
// controlled, so the shapes under test are a property of this function rather
// than of whatever happens to be in someone else's repository today.
//
// Every artifact type the fossil CLI can produce is represented: check-ins
// (including a branch and a merge, so mlink and plink carry more than a
// straight line), ticket creations and ticket updates, a wiki page, a technote,
// an attachment, and a tag control artifact. Forum posts are the one shape
// absent -- fossil exposes no CLI to create them -- so `f` events are outside
// this gate's coverage.
func buildParityCorpus(t *testing.T, bin, dir string) string {
	t.Helper()
	repo := filepath.Join(dir, "corpus.fossil")
	corpusFossil(t, bin, dir, "", "init", repo, "-A", corpusUser)

	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	corpusFossil(t, bin, work, "", "open", repo)

	// Check-ins on trunk, then a branch and a merge, so the manifest-derived
	// tables (mlink, plink, leaf) describe a graph rather than a line.
	writeFile(t, filepath.Join(work, "a.txt"), "first\n")
	corpusFossil(t, bin, work, "", "add", "a.txt")
	commit(t, bin, work, "initial commit")

	writeFile(t, filepath.Join(work, "a.txt"), "first\nsecond\n")
	writeFile(t, filepath.Join(work, "b.txt"), "b\n")
	corpusFossil(t, bin, work, "", "add", "b.txt")
	commit(t, bin, work, "second commit")

	corpusFossil(t, bin, work, "", "branch", "new", "sidebranch", "trunk")
	corpusFossil(t, bin, work, "", "update", "sidebranch")
	writeFile(t, filepath.Join(work, "c.txt"), "c\n")
	corpusFossil(t, bin, work, "", "add", "c.txt")
	commit(t, bin, work, "commit on sidebranch")

	corpusFossil(t, bin, work, "", "update", "trunk")
	corpusFossil(t, bin, work, "", "merge", "sidebranch")
	commit(t, bin, work, "merge sidebranch to trunk")

	// Ticket artifacts -- the D,J,K,U,Z shape #184 loses entirely. Both the
	// creation and the update of a ticket produce one.
	for i := 1; i <= 5; i++ {
		corpusFossil(t, bin, work, "", "ticket", "add",
			"title", fmt.Sprintf("defect %d", i),
			"status", "Open",
			"comment", fmt.Sprintf("description of defect %d", i),
			"-R", repo)
	}
	for _, uuid := range fossilRows(t, bin, repo, "SELECT tkt_uuid FROM ticket") {
		corpusFossil(t, bin, work, "", "ticket", "set", uuid,
			"status", "Closed",
			"comment", "resolved",
			"-R", repo)
	}

	// Wiki, technote, attachment and tag control artifacts.
	corpusFossil(t, bin, work, "wiki body\n", "wiki", "create", "PageOne", "-R", repo)
	corpusFossil(t, bin, work, "technote body\n", "wiki", "create", "TechNote", "--technote", "now", "-R", repo)

	writeFile(t, filepath.Join(work, "attach.txt"), "attached bytes\n")
	corpusFossil(t, bin, work, "", "attachment", "add", "PageOne", "attach.txt")

	corpusFossil(t, bin, work, "", "tag", "add", "release-1.0", "current")

	return repo
}

// requireCorpusShapes asserts the corpus actually contains the artifact shapes
// the gate claims to cover, by classifying every blob in it by its card set.
//
// Without this the gate is self-defeating: if the corpus builder stopped
// producing ticket artifacts, the two clones would agree on zero ticket events
// and the gate would pass while covering nothing. The minimums are floors, not
// exact counts, so enriching the corpus later does not require editing them.
func requireCorpusShapes(t *testing.T, bin, repo string) {
	t.Helper()
	inventory := artifactShapes(t, bin, repo)
	required := map[string]int{
		"ticket":     10, // 5 creations + 5 updates -- the D,J,K,U,Z shape
		"checkin":    4,
		"wiki":       1,
		"technote":   1,
		"attachment": 1,
		"control":    1,
	}
	for _, shape := range sortedKeys(required) {
		if inventory[shape] < required[shape] {
			t.Fatalf("corpus contains %d %s artifacts, need at least %d; "+
				"the gate would pass without exercising this shape (full inventory: %v)",
				inventory[shape], shape, required[shape], inventory)
		}
	}
	t.Logf("corpus artifact inventory: %v", inventory)
}

// artifactShapes classifies every blob in repo by the cards its artifact
// carries, returning shape name -> count. Classification order matters: a
// ticket change is identified by its K card, which no other shape carries,
// while a check-in is only a check-in once the more specific shapes are ruled
// out. See the Fossil "File Format" document for the card sets.
func artifactShapes(t *testing.T, bin, repo string) map[string]int {
	t.Helper()
	shapes := map[string]int{}
	for _, uuid := range fossilRows(t, bin, repo, "SELECT uuid FROM blob WHERE size > 0") {
		cards := cardSet(t, bin, repo, uuid)
		switch {
		case cards["K"]:
			shapes["ticket"]++
		case cards["A"]:
			shapes["attachment"]++
		case cards["E"]:
			shapes["technote"]++
		case cards["W"]:
			shapes["wiki"]++
		case cards["T"] && !cards["C"]:
			shapes["control"]++
		case cards["C"]:
			shapes["checkin"]++
		default:
			// Plain file content carries no cards at all.
			shapes["content"]++
		}
	}
	return shapes
}

// cardSet returns the set of card letters present in an artifact. Cards are
// one uppercase letter followed by a space at the start of a line.
func cardSet(t *testing.T, bin, repo, uuid string) map[string]bool {
	t.Helper()
	out, err := exec.Command(bin, "artifact", uuid, "-R", repo).Output()
	if err != nil {
		t.Fatalf("fossil artifact %s: %v", uuid, err)
	}
	cards := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) >= 2 && line[1] == ' ' && line[0] >= 'A' && line[0] <= 'Z' {
			cards[line[:1]] = true
		}
	}
	return cards
}

// serveRepo starts `fossil server` on a loopback port and returns its address.
// --localhost auto-authenticates loopback requests as the setup user, so the
// clone has read access without credentials.
func serveRepo(t *testing.T, bin, repo string) string {
	t.Helper()
	port := freeTCPPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	server := exec.CommandContext(ctx, bin, "server", repo,
		"--localhost", "--port", strconv.Itoa(port))
	server.Stdout = os.Stderr
	server.Stderr = os.Stderr
	if err := server.Start(); err != nil {
		t.Fatalf("start fossil server: %v", err)
	}
	t.Cleanup(func() { _ = server.Process.Kill() })

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	waitTCP(t, addr)
	return addr
}

// fossilCounts runs a two-column "key, count" query and returns it as a map.
func fossilCounts(t *testing.T, bin, repo, query string) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, row := range fossilRows(t, bin, repo, query) {
		key, value, ok := strings.Cut(row, "|")
		if !ok {
			t.Fatalf("unexpected row %q from %q", row, query)
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("parse count %q from %q: %v", value, query, err)
		}
		counts[key] = n
	}
	return counts
}

// fossilCount runs a single-value count query.
func fossilCount(t *testing.T, bin, repo, query string) int {
	t.Helper()
	rows := fossilRows(t, bin, repo, query)
	if len(rows) != 1 {
		t.Fatalf("query %q returned %d rows, want 1", query, len(rows))
	}
	n, err := strconv.Atoi(rows[0])
	if err != nil {
		t.Fatalf("parse count %q from %q: %v", rows[0], query, err)
	}
	return n
}

// fossilRows runs a query through the fossil binary. Both stores are read with
// canonical fossil rather than our own database layer, so the comparison cannot
// be skewed by a defect in the code under test.
func fossilRows(t *testing.T, bin, repo, query string) []string {
	t.Helper()
	cmd := exec.Command(bin, "sql", "-R", repo, query)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.Fatalf("fossil sql %q: %v\nstderr: %s", query, err, exitErr.Stderr)
		}
		t.Fatalf("fossil sql %q: %v", query, err)
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// corpusFossil runs a fossil subcommand with stdin wired to input and USER set
// to the corpus user. Several artifact-producing subcommands (`update`, for
// one) have no --user flag and refuse to run when fossil cannot work out who is
// acting, and `go test` does not pass USER through, so the environment is set
// here rather than flagged at each call site.
func corpusFossil(t *testing.T, bin, dir, input string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(input)
	cmd.Env = append(os.Environ(), "USER="+corpusUser)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fossil %v: %v\n%s", args, err, out)
	}
}

// commit records a check-in. --no-warnings keeps fossil from prompting about
// content it would otherwise ask a human to confirm.
func commit(t *testing.T, bin, work, message string) {
	t.Helper()
	corpusFossil(t, bin, work, "", "commit", "-m", message, "--no-warnings")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// unionKeys returns every key present in either map, sorted, so a type present
// in only one store is still compared (and reported) rather than skipped.
func unionKeys(a, b map[string]int) []string {
	seen := map[string]bool{}
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	return sortedKeys(seen)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
