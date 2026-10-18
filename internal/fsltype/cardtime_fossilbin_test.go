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

// cardTimeCases are dates whose seconds probe every way the fraction can
// round: random nanoseconds, values whose double lies just below or above a
// half millisecond, exact binary halves (k/16 of a second), the .999 cap, and
// the first and last instants of a minute and a day.
func cardTimeCases() []time.Time {
	base := time.Date(2026, 10, 18, 12, 34, 0, 0, time.UTC)
	var out []time.Time
	add := func(sec, nsec int) {
		out = append(out, base.Add(time.Duration(sec)*time.Second+time.Duration(nsec)))
	}
	for k := 0; k < 16; k++ {
		add(56, k*62500000)
	}
	for _, nsec := range []int{0, 1, 499999, 500000, 999999, 789500000, 789600000,
		999000000, 999499999, 999500000, 999600000, 999999999} {
		add(0, nsec)
		add(59, nsec)
	}
	rng := rand.New(rand.NewSource(440))
	for i := 0; i < 400; i++ {
		add(rng.Intn(60), rng.Intn(1e9))
	}
	for i := 0; i < 200; i++ {
		add(rng.Intn(60), rng.Intn(1e5)*10000+5000)
	}
	out = append(out, time.Date(2026, 12, 31, 23, 59, 59, 999700000, time.UTC))
	return out
}

// TestCardTimeMatchesFossilBinary asks fossil, through test-date-format (the
// date_in_standard_format a check-in, tag or branch date goes through), what
// it writes for each date given to the nanosecond, and requires CardTime to
// agree on every one.
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
