package blob

import (
	"bytes"
	"testing"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// TestInflater_LoadMatchesLoad pins the contract: an Inflater must return the
// same bytes as the plain Load for the same rid, on the first call and on
// every reuse of its reader. Goal: prove the reader-reuse path decompresses
// identically to the fresh-reader path, including the second call that
// actually exercises the reset.
func TestInflater_LoadMatchesLoad(t *testing.T) {
	d := setupTestDB(t)
	// Compressible payload large enough that Store keeps it zlib-compressed,
	// so Inflater.Load takes the decompression path under test.
	content := bytes.Repeat([]byte("delta chain payload line\n"), 500)
	rid, _, err := Store(d, content)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	want, err := Load(d, rid)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var inf Inflater
	for call := 0; call < 3; call++ {
		got, err := inf.Load(d, rid)
		if err != nil {
			t.Fatalf("Inflater.Load call %d: %v", call, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("Inflater.Load call %d differs from Load", call)
		}
	}
}

// TestInflater_ReusesReaderAcrossLoads is the reason Inflater exists: loading a
// run of compressed blobs through one Inflater must allocate less than loading
// them through the plain Load, because the expensive zlib reader (a ~32 KiB
// history window plus its flate machinery) is built once and reset per blob
// rather than rebuilt each time. Goal: prove the reuse measurably lowers
// allocations over a sequence, measured with AllocsPerRun so it does not
// depend on the exact per-call count.
func TestInflater_ReusesReaderAcrossLoads(t *testing.T) {
	d := setupTestDB(t)

	const blobs = 8
	rids := make([]libfossil.FslID, blobs)
	for i := range rids {
		content := bytes.Repeat([]byte{byte('a' + i)}, 4096)
		rid, _, err := Store(d, content)
		if err != nil {
			t.Fatalf("Store %d: %v", i, err)
		}
		rids[i] = rid
	}

	plainAllocs := testing.AllocsPerRun(50, func() {
		for _, rid := range rids {
			if _, err := Load(d, rid); err != nil {
				t.Fatalf("Load: %v", err)
			}
		}
	})

	var inf Inflater
	reuseAllocs := testing.AllocsPerRun(50, func() {
		for _, rid := range rids {
			if _, err := inf.Load(d, rid); err != nil {
				t.Fatalf("Inflater.Load: %v", err)
			}
		}
	})

	if reuseAllocs >= plainAllocs {
		t.Fatalf("Inflater.Load allocated %.0f over %d blobs, want fewer than "+
			"plain Load's %.0f -- the reader was not reused",
			reuseAllocs, blobs, plainAllocs)
	}
}
