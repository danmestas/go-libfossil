package repo

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/delta"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/simio"
)

// Verify checks every held blob against its name, including blobs stored deep
// in a delta chain. Method: build a 200-delta chain, check it verifies, then
// rename one blob in the middle of the chain and check Verify reports it.
func TestVerifyDeltaChain(t *testing.T) {
	r, err := Create(filepath.Join(t.TempDir(), "r.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	naming, err := hash.NamingFor(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	text := "line\n"
	src, _, err := blob.Store(r.DB(), []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	var middle libfossil.FslID
	for i := range 200 {
		prev := text
		text += fmt.Sprintf("revision %d\n", i)
		d, err := delta.Create([]byte(prev), []byte(text))
		if err != nil {
			t.Fatal(err)
		}
		rid, err := blob.StoreDeltaRaw(r.DB(), naming.New.Hash([]byte(text)), d, src, nil)
		if err != nil {
			t.Fatal(err)
		}
		if i == 100 {
			middle = rid
		}
		src = rid
	}
	if err := r.Verify(); err != nil {
		t.Fatalf("Verify of an intact chain: %v", err)
	}

	wrong := strings.Repeat("ab", 32)
	if _, err := r.DB().Exec("UPDATE blob SET uuid=? WHERE rid=?", wrong, middle); err != nil {
		t.Fatal(err)
	}
	err = r.Verify()
	if err == nil {
		t.Fatal("Verify passed a blob whose content does not match its name")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("rid %d", middle)) {
		t.Fatalf("Verify error %q does not name rid %d", err, middle)
	}
}
