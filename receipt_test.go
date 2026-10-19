package libfossil

import (
	"path/filepath"
	"testing"
	"time"

	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// `fossil uv add` records its own receipt, credited to the local user, with
// the command named where a peer's address would be.
func TestUVWriteRecordsFossilsReceipt(t *testing.T) {
	r, err := Create(filepath.Join(t.TempDir(), "test.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.UVWrite("a.txt", []byte("a\n"), time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
	var login, ip string
	var nonceNull bool
	err = r.inner.DB().QueryRow("SELECT user.login, rcvfrom.ipaddr, rcvfrom.nonce IS NULL"+
		" FROM unversioned JOIN rcvfrom USING(rcvid) JOIN user ON user.uid=rcvfrom.uid"+
		" WHERE name='a.txt'").Scan(&login, &ip, &nonceNull)
	if err != nil {
		t.Fatalf("receipt of a.txt: %v", err)
	}
	if login != "test" || ip != "#!fossil unversioned add" || !nonceNull {
		t.Fatalf("receipt = (%q, %q, nonce NULL %v), want (\"test\", \"#!fossil unversioned add\", true)",
			login, ip, nonceNull)
	}
}
