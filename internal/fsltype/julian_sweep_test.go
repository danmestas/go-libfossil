package fsltype_test

import (
	"crypto/md5"
	"fmt"
	"math/rand"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/testutil"
)

// sweepMillis are card times to the millisecond: random ones from 1970 to
// 2100, every millisecond of one second, and the times of the fossil
// repository's rows that told the conversions apart.
func sweepMillis(n int) []int64 {
	rng := rand.New(rand.NewSource(13411))
	out := []int64{1302747281361, 1313399551889, 1456907938278, 1499253127655,
		1500607170788, 1597715665130, 1703782442848}
	for i := 0; i < n; i++ {
		out = append(out, rng.Int63n(4102444800000))
	}
	base := time.Date(2011, 8, 15, 9, 12, 31, 0, time.UTC).UnixMilli()
	for ms := int64(0); ms < 1000; ms++ {
		out = append(out, base+ms)
	}
	return out
}

func cardText(millis int64) string {
	return time.UnixMilli(millis).UTC().Format("2006-01-02T15:04:05.000")
}

// TestJulianDayMatchesSQLiteJulianday checks JulianDayFromMillis against
// julianday() of the card text in the SQLite this build links, bit for bit.
func TestJulianDayMatchesSQLiteJulianday(t *testing.T) {
	d, err := db.OpenSQL(filepath.Join(t.TempDir(), "j.db"), db.OpenConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	for _, ms := range sweepMillis(20000) {
		var want float64
		if err := d.QueryRow("SELECT julianday(?)", cardText(ms)).Scan(&want); err != nil {
			t.Fatal(err)
		}
		if got := fsltype.JulianDayFromMillis(ms); got != want {
			t.Fatalf("%s: JulianDayFromMillis = %.17g, julianday() = %.17g", cardText(ms), got, want)
		}
	}
}

// TestTextJulianDayMatchesFossilBinary stores a control artifact for each
// sweep time in a repository fossil made, has fossil rebuild derive their
// event rows -- each time written as SQL text by fossil's printf and read
// back by its SQLite -- and requires TimeToTextJulian's value for every one.
func TestTextJulianDayMatchesFossilBinary(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	path := filepath.Join(t.TempDir(), "t.fossil")
	if out, err := exec.Command(bin, "init", path).CombinedOutput(); err != nil {
		t.Fatalf("fossil init: %v\n%s", err, out)
	}
	r, err := repo.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var target string
	if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=1").Scan(&target); err != nil {
		t.Fatal(err)
	}
	want := map[int64]float64{}
	for i, ms := range sweepMillis(3000) {
		body := fmt.Sprintf("D %s\nT +sym-t%d %s\nU u\n", cardText(ms), i, target)
		art := fmt.Sprintf("%sZ %x\n", body, md5.Sum([]byte(body)))
		rid, _, err := blob.Store(r.DB(), []byte(art))
		if err != nil {
			t.Fatal(err)
		}
		want[int64(rid)] = fsltype.TimeToTextJulian(time.UnixMilli(ms))
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, "rebuild", path).CombinedOutput(); err != nil {
		t.Fatalf("fossil rebuild: %v\n%s", err, out)
	}
	d, err := db.OpenSQL(path, db.OpenConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rows, err := d.Query("SELECT objid, CAST(mtime AS REAL) FROM event WHERE type='g'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var rid int64
		var got float64
		if err := rows.Scan(&rid, &got); err != nil {
			t.Fatal(err)
		}
		n++
		if got != want[rid] {
			t.Errorf("rid %d: fossil stored %.17g, TimeToTextJulian = %.17g", rid, got, want[rid])
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != len(want) {
		t.Fatalf("fossil derived %d tag events for %d control artifacts", n, len(want))
	}
}
