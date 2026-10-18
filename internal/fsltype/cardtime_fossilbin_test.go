package fsltype_test

import (
	"math/rand"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/testutil"
)

// isTie reports whether t falls exactly on a half millisecond, where
// fossil's rounding depends on its SQLite version and is not compared.
func isTie(t time.Time) bool { return t.Nanosecond()%1000000 == 500000 }

// cardTimeCases are dates whose fraction probes every way it can round:
// random nanoseconds at random dates, values a nanosecond either side of a
// half millisecond, and fractions past .999 at the end of a second, minute,
// day and year, which must not carry. Exact half-millisecond ties are left
// out (TestCardTimeRoundsTiesUp covers them).
func cardTimeCases() []time.Time {
	var out []time.Time
	add := func(t time.Time) {
		if !isTie(t) {
			out = append(out, t)
		}
	}
	rng := rand.New(rand.NewSource(440))
	start := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	for i := 0; i < 3000; i++ {
		add(time.Unix(start+rng.Int63n(60*365*86400), rng.Int63n(1e9)).UTC())
	}
	base := time.Date(2026, 10, 18, 12, 34, 56, 0, time.UTC)
	for i := 0; i < 1000; i++ {
		ms := rng.Int63n(1000)
		add(base.Add(time.Duration(ms*1000000 + 499999)))
		add(base.Add(time.Duration(ms*1000000 + 500001)))
	}
	ends := []time.Time{
		time.Date(2026, 10, 18, 12, 34, 56, 0, time.UTC),
		time.Date(2026, 10, 18, 12, 34, 59, 0, time.UTC),
		time.Date(2026, 10, 18, 23, 59, 59, 0, time.UTC),
		time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC),
	}
	for _, e := range ends {
		for _, nsec := range []int{998999999, 999000000, 999000001, 999499999,
			999500001, 999700000, 999999999} {
			add(e.Add(time.Duration(nsec)))
		}
	}
	return out
}

// TestCardTimeMatchesFossilBinary asks fossil, through test-date-format (the
// date_in_standard_format a check-in, tag or branch date goes through), what
// it writes for each date given to the nanosecond, and requires CardTime to
// agree on every one that is not a half-millisecond tie.
func TestCardTimeMatchesFossilBinary(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "dates.fossil")
	if out, err := exec.Command(bin, "init", repoPath).CombinedOutput(); err != nil {
		t.Fatalf("fossil init: %v\n%s", err, out)
	}
	cases := cardTimeCases()
	args := []string{"test-date-format", "-R", repoPath}
	for _, c := range cases {
		args = append(args, c.Format("2006-01-02T15:04:05.000000000"))
	}
	out, err := exec.Command(bin, args...).Output()
	if err != nil {
		t.Fatalf("fossil test-date-format: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != len(cases) {
		t.Fatalf("fossil answered %d dates, asked %d", len(lines), len(cases))
	}
	for i, c := range cases {
		in := c.Format("2006-01-02T15:04:05.000000000")
		want := strings.TrimPrefix(lines[i], in+" -> ")
		if want == lines[i] {
			t.Fatalf("unexpected fossil output line %q", lines[i])
		}
		got := fsltype.CardTime(c).Format("2006-01-02T15:04:05.000")
		if got != want {
			t.Errorf("CardTime(%s) = %s, fossil writes %s", in, got, want)
		}
	}
}
