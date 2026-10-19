package merge

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/testutil"
)

// merge3Case is one set of three-way merge inputs.
type merge3Case struct {
	name                 string
	pivot, local, remote []byte
}

// fossilMerge3 is fossil's answer for one case: the merged bytes and the
// conflict count "fossil 3-way-merge" warns about.
type fossilMerge3 struct {
	out       []byte
	conflicts int
}

var fossilConflictWarning = regexp.MustCompile(`WARNING: (\d+) merge conflicts`)

// TestMerge3MatchesFossil runs every corpus case through fossil's
// "3-way-merge" and through mergeText, and requires the same bytes and the
// same conflict count. Cases fossil cannot diff (binary) produce no output
// and no warning there, and a negative count here.
func TestMerge3MatchesFossil(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	cases := merge3Corpus()
	answers := runFossilMerge3(t, bin, cases)

	mismatches, conflicted, suggested, binary := 0, 0, 0, 0
	for i, tc := range cases {
		want := answers[i]
		out, conflicts, n := mergeText(tc.pivot, tc.local, tc.remote)
		if n > 0 {
			conflicted++
		}
		if bytes.Contains(out, []byte(markerSuggested)) {
			suggested++
		}
		if n < 0 {
			binary++
			if len(want.out) != 0 || want.conflicts != 0 {
				t.Errorf("%s: we refused it as binary; fossil merged it (%d conflicts)", tc.name, want.conflicts)
				mismatches++
			}
			continue
		}
		if !bytes.Equal(out, want.out) || n != want.conflicts || len(conflicts) != n {
			mismatches++
			if mismatches <= 5 {
				t.Errorf("%s: conflicts got %d want %d\npivot  %q\nlocal  %q\nremote %q\ngot    %q\nwant   %q",
					tc.name, n, want.conflicts, tc.pivot, tc.local, tc.remote, out, want.out)
			}
		}
	}
	t.Logf("%d cases compared with fossil (%d with conflicts, %d with a suggested resolution, %d binary): %d mismatches",
		len(cases), conflicted, suggested, binary, mismatches)
	if mismatches > 0 {
		t.Fatalf("%d of %d cases differ from fossil", mismatches, len(cases))
	}
}

// runFossilMerge3 runs fossil on every case, eight at a time.
func runFossilMerge3(t *testing.T, bin string, cases []merge3Case) []fossilMerge3 {
	t.Helper()
	root := t.TempDir()
	answers := make([]fossilMerge3, len(cases))
	errs := make([]error, len(cases))
	next := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				dir := filepath.Join(root, strconv.Itoa(i))
				answers[i], errs[i] = fossilMerge3Once(bin, dir, cases[i])
			}
		}()
	}
	for i := range cases {
		next <- i
	}
	close(next)
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	return answers
}

func fossilMerge3Once(bin, dir string, tc merge3Case) (fossilMerge3, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fossilMerge3{}, err
	}
	files := map[string][]byte{"pivot": tc.pivot, "v1": tc.local, "v2": tc.remote}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return fossilMerge3{}, err
		}
	}
	cmd := exec.Command(bin, "3-way-merge", "pivot", "v1", "v2", "out")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fossilMerge3{}, fmt.Errorf("%s: fossil 3-way-merge: %v\n%s", tc.name, err, stderr.String())
	}
	out, err := os.ReadFile(filepath.Join(dir, "out"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fossilMerge3{}, err
	}
	answer := fossilMerge3{out: out}
	if m := fossilConflictWarning.FindSubmatch(stderr.Bytes()); m != nil {
		if answer.conflicts, err = strconv.Atoi(string(m[1])); err != nil {
			return fossilMerge3{}, err
		}
	}
	return answer, nil
}

// TestMerge3ConflictBlocksFossilCommit checks fossil's own conflict
// detection (contains_merge_marker) sees our conflict output: fossil commit
// refuses the file.
func TestMerge3ConflictBlocksFossilCommit(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	dir := t.TempDir()
	co := filepath.Join(dir, "co")
	fossil := func(args ...string) ([]byte, error) {
		cmd := exec.Command(bin, args...)
		cmd.Dir = co
		return cmd.CombinedOutput()
	}
	if err := os.MkdirAll(co, 0o755); err != nil {
		t.Fatal(err)
	}
	steps := [][]string{
		{"new", "-A", "u", filepath.Join(dir, "r.fossil")},
		{"open", filepath.Join(dir, "r.fossil")},
	}
	for _, args := range steps {
		if out, err := fossil(args...); err != nil {
			t.Fatalf("fossil %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(co, "f"), []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "f"}, {"commit", "-m", "init", "--user", "u", "--no-warnings"}} {
		if out, err := fossil(args...); err != nil {
			t.Fatalf("fossil %v: %v\n%s", args, err, out)
		}
	}
	r, err := (&ThreeWayText{}).Merge([]byte("a\nb\nc\n"), []byte("a\nlocal\nc\n"), []byte("a\nremote\nc\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(co, "f"), r.Content, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := fossil("commit", "-m", "x", "--user", "u", "--no-warnings")
	if err == nil {
		t.Fatalf("fossil committed a file holding our conflict markers:\n%s", out)
	}
	if !strings.Contains(string(out), "unresolved merge conflict") {
		t.Fatalf("fossil commit failed for another reason:\n%s", out)
	}
}

// TestMerge3MatchesFossilMergeCommand runs cases through a real
// "fossil merge" in a checkout. The UTF-16 cases need it: fossil converts
// UTF-16 through its database, which "3-way-merge" does not open.
func TestMerge3MatchesFossilMergeCommand(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	utf16 := func(s string, bigEndian bool) string {
		b := []byte("\xff\xfe")
		if bigEndian {
			b = []byte("\xfe\xff")
		}
		for _, r := range s {
			lo, hi := byte(r), byte(r>>8)
			if bigEndian {
				lo, hi = hi, lo
			}
			b = append(b, lo, hi)
		}
		return string(b)
	}
	raw := []struct{ name, p, l, r string }{
		{"conflict", "a\nb\nc\n", "a\nlocal\nc\n", "a\nremote\nc\n"},
		{"crlf clean", "a\r\nb\r\nc\r\n", "A\r\nb\r\nc\r\n", "a\r\nb\r\nC\r\n"},
		{"utf16le conflict", utf16("a\nb\n", false), utf16("a\nx\n", false), utf16("a\ny\n", false)},
		{"utf16be clean", utf16("a\nb\n", true), utf16("a\nb\nc\n", true), utf16("z\na\nb\n", true)},
		{"utf16 lone surrogate", "\xff\xfea\x00\x00\xd8", "\xff\xfeb\x00\n\x00", "c\n"},
		{"utf16 odd length", "\xfe\xff\x00a\x00", "x\n", "y\n"},
		{"utf16 second bom", "\xff\xfe\xfe\xff\x00a\x00\n", "\xff\xfe\xfe\xff\x00b\x00\n", "d\n"},
	}
	for _, c := range raw {
		t.Run(c.name, func(t *testing.T) {
			want, wantConflicts := fossilMergeCommand(t, bin, c.p, c.l, c.r)
			r, err := (&ThreeWayText{}).Merge([]byte(c.p), []byte(c.l), []byte(c.r))
			if err != nil {
				t.Fatal(err)
			}
			if string(r.Content) != want || len(r.Conflicts) != wantConflicts || r.Clean != (wantConflicts == 0) {
				t.Errorf("got %q (%d conflicts), fossil merge wrote %q (%d conflicts)",
					r.Content, len(r.Conflicts), want, wantConflicts)
			}
		})
	}
}

var fossilMergeConflicts = regexp.MustCompile(`(\d+) merge conflicts? in`)

// fossilMergeCommand commits pivot, then remote on a branch and local on
// trunk, runs "fossil merge" of the branch, and returns the merged file and
// the conflict count fossil reports.
func fossilMergeCommand(t *testing.T, bin, pivot, local, remote string) (string, int) {
	t.Helper()
	dir := t.TempDir()
	co := filepath.Join(dir, "co")
	if err := os.MkdirAll(co, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(co, "f")
	writes := 0
	run := func(content string, args ...string) string {
		t.Helper()
		if content != "" {
			if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			// A distinct mtime per write, so fossil sees a same-size edit
			// made within the same second.
			writes++
			mtime := time.Unix(1_000_000_000+int64(writes)*100, 0)
			if err := os.Chtimes(f, mtime, mtime); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command(bin, args...)
		cmd.Dir = co
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fossil %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	commit := []string{"commit", "--no-warnings", "-m"}
	run("", "new", "-A", "u", filepath.Join(dir, "r.fossil"))
	run("", "open", filepath.Join(dir, "r.fossil"))
	run("", "user", "default", "u")
	run(pivot, "add", "f")
	run("", append(commit, "pivot")...)
	run(remote, append(commit, "remote", "--branch", "other")...)
	run("", "update", "trunk")
	run(local, append(commit, "local")...)
	out := run("", "merge", "other")
	merged, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	if m := fossilMergeConflicts.FindStringSubmatch(out); m != nil {
		if n, err = strconv.Atoi(m[1]); err != nil {
			t.Fatal(err)
		}
	}
	return string(merged), n
}

// merge3Corpus is the hand-written edge cases plus a seeded random corpus.
func merge3Corpus() []merge3Case {
	cases := merge3EdgeCases()
	rng := rand.New(rand.NewPCG(447, 3))
	for i := 0; i < 1200; i++ {
		p, l, r := genMerge3Case(rng)
		cases = append(cases, merge3Case{name: fmt.Sprintf("random-%d", i), pivot: p, local: l, remote: r})
	}
	return cases
}

func merge3EdgeCases() []merge3Case {
	bom := "\xef\xbb\xbf"
	long := strings.Repeat("y", 32767)
	raw := []struct{ name, p, l, r string }{
		{"all empty", "", "", ""},
		{"empty pivot both add", "", "a\n", "b\n"},
		{"empty pivot same add", "", "a\n", "a\n"},
		{"local emptied", "a\nb\n", "", "a\nB\n"},
		{"both emptied", "a\nb\n", "", ""},
		{"identical change", "a\nb\nc\n", "a\nX\nc\n", "a\nX\nc\n"},
		{"identical everything", "a\n", "a\n", "a\n"},
		{"only local", "a\nb\n", "a\nB\n", "a\nb\n"},
		{"only remote", "a\nb\n", "a\nb\n", "a\nB\n"},
		{"adjacent edits", "a\nb\nc\nd\n", "a\nB\nc\nd\n", "a\nb\nC\nd\n"},
		{"delete vs edit", "a\nb\nc\n", "a\nc\n", "a\nB\nc\n"},
		{"edit vs delete", "a\nb\nc\n", "a\nB\nc\n", "a\nc\n"},
		{"two conflicts", "1\n2\n3\n4\n5\n6\n7\n", "1\nA\n3\n4\n5\nB\n7\n", "1\nX\n3\n4\n5\nY\n7\n"},
		{"conflict at start", "a\nb\n", "x\nb\n", "y\nb\n"},
		{"conflict at end", "a\nb\n", "a\nx\n", "a\ny\n"},
		{"no newline pivot", "a\nb", "a\nb\nc\n", "a\nb\nd\n"},
		{"no newline sides", "a\nb\n", "a\nx", "a\ny"},
		{"no newline only remote", "a\nb\n", "a\nb\nc\n", "z\na\nb"},
		{"pivot equals local no newline", "a", "a", "a\nb\n"},
		{"crlf conflict", "a\r\nb\r\n", "a\r\nx\r\n", "a\r\ny\r\n"},
		{"crlf only local", "a\nb\n", "a\r\nx\r\n", "a\ny\n"},
		{"crlf no final newline", "a\r\nb", "a\r\nx", "a\r\ny"},
		{"cr only", "a\rb\r", "a\rx\r", "a\ry\r"},
		{"bom both", bom + "a\nb\n", bom + "a\nx\n", bom + "a\ny\n"},
		{"double bom", bom + bom + "a\n", bom + bom + "x\n", bom + bom + "y\n"},
		{"bom one side", "a\n", bom + "a\nx\n", "a\ny\n"},
		{"nul binary", "a\n", "a\x00\n", "b\n"},
		{"bom then nul", "a\n", bom + "x\x00y\n", "b\n"},
		{"long line ok", "a\n", long + "\n", "b\n"},
		{"long line binary", "a\n", long + "y\n", "b\n"},
		{"tokens resolve", "one two three\n", "ONE two three\n", "one two THREE\n"},
		{"tokens conflict", "f(a, b);\n", "f(a, c);\n", "f(a, d);\n"},
		{"digit nine", "x9 = 1\n", "x9 = 2\n", "y9 = 1\n"},
		{"whitespace tokens", "a  b\n", "a b\n", "a   b\n"},
		{"marker in content", "x\n", markerBegin + "\n", "y\n"},
		{"repeated lines", "a\na\na\na\n", "a\nb\na\na\na\n", "a\na\na\nc\na\n"},
		{"braces", "{\n  x;\n}\n{\n  y;\n}\n", "{\n  x;\n}\n{\n  z;\n}\n{\n  y;\n}\n", "{\n  x;\n  w;\n}\n{\n  y;\n}\n"},
	}
	cases := make([]merge3Case, 0, len(raw))
	for _, c := range raw {
		cases = append(cases, merge3Case{name: c.name, pivot: []byte(c.p), local: []byte(c.l), remote: []byte(c.r)})
	}
	return cases
}

var merge3Vocab = []string{
	"", "}", "{", "a", "b", "int x = 1;", "int y = 2;", "return x + y;",
	"    foo(bar, baz);", "    foo(bar, qux);", "// comment", "x := y + 9",
	"the quick brown fox", "jumps over the lazy dog", "\t\tindented", "  ",
	"if (a == b) {", "} else {", "end", "9 lives", "a-b_c.d",
}

var merge3Words = []string{"alpha", "beta", "gamma", "9", "0", "x", "(", ")", ",", " ", "  ", "foo", "bar"}

// genMerge3Case builds a random pivot and two edited copies of it.
func genMerge3Case(rng *rand.Rand) (pivot, local, remote []byte) {
	n := rng.IntN(16)
	if rng.IntN(20) == 0 {
		n = 150 + rng.IntN(400)
	}
	base := make([]string, n)
	for i := range base {
		base[i] = genMerge3Line(rng)
	}
	l := mutateMerge3(rng, base)
	r := mutateMerge3(rng, base)
	switch rng.IntN(10) {
	case 0:
		r = append([]string(nil), l...)
	case 1:
		// The same edits on both sides, then a few of each side's own.
		shared := mutateMerge3(rng, base)
		l, r = mutateMerge3(rng, shared), mutateMerge3(rng, shared)
	}
	crlf := rng.IntN(6) == 0
	return renderMerge3(rng, base, crlf), renderMerge3(rng, l, crlf), renderMerge3(rng, r, crlf)
}

func genMerge3Line(rng *rand.Rand) string {
	if rng.IntN(3) > 0 {
		return merge3Vocab[rng.IntN(len(merge3Vocab))]
	}
	var b strings.Builder
	for k := rng.IntN(6); k >= 0; k-- {
		b.WriteString(merge3Words[rng.IntN(len(merge3Words))])
	}
	return b.String()
}

// mutateMerge3 applies up to four random line edits to a copy of lines.
func mutateMerge3(rng *rand.Rand, lines []string) []string {
	out := append([]string(nil), lines...)
	for k := rng.IntN(5); k > 0; k-- {
		pos := 0
		if len(out) > 0 {
			pos = rng.IntN(len(out))
		}
		switch op := rng.IntN(5); {
		case op == 0 && len(out) > 0:
			cut := min(len(out), pos+1+rng.IntN(3))
			out = append(out[:pos], out[cut:]...)
		case op == 1:
			ins := []string{genMerge3Line(rng)}
			if rng.IntN(2) == 0 {
				ins = append(ins, genMerge3Line(rng))
			}
			out = append(out[:pos], append(ins, out[pos:]...)...)
		case op == 2 && len(out) > 0:
			out[pos] = genMerge3Line(rng)
		case op == 3 && len(out) > 0:
			out[pos] = tweakMerge3Line(rng, out[pos])
		case len(out) > 0:
			out = append(out[:pos+1], append([]string{out[pos]}, out[pos+1:]...)...)
		}
	}
	return out
}

// tweakMerge3Line changes one word of line, so two sides can edit the same
// line in different places and the token merge has something to resolve.
func tweakMerge3Line(rng *rand.Rand, line string) string {
	words := strings.Split(line, " ")
	i := rng.IntN(len(words))
	words[i] = merge3Words[rng.IntN(len(merge3Words))]
	return strings.Join(words, " ")
}

// renderMerge3 joins lines with LF or CRLF, sometimes leaving off the final
// line ending and sometimes mixing in a CRLF line.
func renderMerge3(rng *rand.Rand, lines []string, crlf bool) []byte {
	var b bytes.Buffer
	for i, line := range lines {
		b.WriteString(line)
		if i == len(lines)-1 && rng.IntN(8) == 0 {
			break
		}
		if crlf || rng.IntN(40) == 0 {
			b.WriteByte('\r')
		}
		b.WriteByte('\n')
	}
	return b.Bytes()
}
