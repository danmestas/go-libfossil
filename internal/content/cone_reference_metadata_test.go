package content

import (
	"database/sql"
	"math/rand"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
)

// length(BLOB) and typeof use SQLite's metadata-only OP_Column flags. A CAST
// or substr in this first pass removes that optimization and materializes the
// entire content value before its stored-byte budget has been proved. The
// one plain read is the chunked arm, CASE typeof(content) WHEN 'integer',
// which only runs on a row whose content is an 8-byte INTEGER length.
func TestConeReferenceMetadataDoesNotMaterializeContent(t *testing.T) {
	d := setupTestDB(t)
	assertMetadataLazy(t, d)

	t.Cleanup(blob.SetMaxValueBytes(1_990_000))
	big := make([]byte, 2_100_000)
	_, _ = rand.New(rand.NewSource(1)).Read(big)
	if _, _, err := blob.Store(d, big); err != nil {
		t.Fatal(err)
	}
	assertMetadataLazy(t, d)
}

func assertMetadataLazy(t *testing.T, d *db.DB) {
	t.Helper()
	var cid int
	if err := d.QueryRow("SELECT cid FROM pragma_table_info('blob') WHERE name='content'").Scan(&cid); err != nil {
		t.Fatal(err)
	}
	rows, err := d.Query("EXPLAIN " + referenceMetadataSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, plain := 0, 0
	for rows.Next() {
		var addr, p1, p2, p3, p5 int
		var opcode string
		var p4, comment sql.NullString
		if err := rows.Scan(&addr, &opcode, &p1, &p2, &p3, &p4, &p5, &comment); err != nil {
			t.Fatal(err)
		}
		if opcode == "Column" && p2 == cid {
			columns++
			// SQLite OPFLAG_LENGTHARG=0x40, OPFLAG_TYPEOFARG=0x80.
			if p5&0xc0 == 0 {
				plain++
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if columns == 0 {
		t.Fatal("did not inspect any content-column reads")
	}
	if plain != 1 || !strings.Contains(referenceMetadataSQL, "WHEN 'integer' THEN b.content") {
		t.Fatalf("preflight reads content plainly %d times, want only the integer arm", plain)
	}
}
