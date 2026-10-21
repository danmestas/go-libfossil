package sync

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/testutil"
)

// Fossil 2.28 withholds shunned payloads in both clone and gimme responses.
// Its compressed clone may still name a shunned public delta basis; unlike
// this server's source-first clone, it does not promise to send bases first.
func TestShunSendFossilOracle(t *testing.T) {
	bin := testutil.FossilBinary()
	if bin == "" {
		t.Skip("Fossil oracle unavailable")
	}
	for _, private := range []bool{false, true} {
		for _, requested := range []bool{false, true} {
			t.Run(fmt.Sprintf("private=%v/gimme=%v", private, requested), func(t *testing.T) {
				r, baseUUID, targetUUID := shunSendFixture(t, private)
				if _, err := r.DB().Exec("INSERT INTO shun(uuid,mtime) VALUES(?,0)", baseUUID); err != nil {
					t.Fatal(err)
				}
				path := r.Path()
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
				resp := shunOracleRound(t, bin, path, shunSendCards(private, requested, baseUUID, targetUUID))
				if shunHasFile(resp, baseUUID) {
					t.Fatal("Fossil emitted shunned payload")
				}
				if !shunHasFile(resp, targetUUID) {
					t.Fatal("Fossil withheld public dependent")
				}
				for _, card := range resp.Cards {
					if c, ok := card.(*xfer.CFileCard); ok && c.UUID == targetUUID {
						t.Logf("canonical compressed dependent names shunned basis: %v", c.DeltaSrc == baseUUID)
					}
				}
			})
		}
	}
}

func shunOracleRound(t *testing.T, bin, path string, cards []xfer.Card) *xfer.Message {
	t.Helper()
	wire, err := (&xfer.Message{Cards: cards}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	head := fmt.Sprintf("POST /xfer HTTP/1.0\r\nHost: localhost\r\n"+
		"Content-Type: %s\r\nContent-Length: %d\r\n\r\n", xfer.ContentTypeCompressed, len(wire))
	dir := t.TempDir()
	request := filepath.Join(dir, "request")
	if err := os.WriteFile(request, append([]byte(head), wire...), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "http", path, "--in", request, "--ipaddr", "127.0.0.1")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+dir, "FOSSIL_HOME="+dir, "XDG_CONFIG_HOME="+dir, "USER=u")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	httpResp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(out)), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer httpResp.Body.Close()
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := xfer.Decode(body, httpResp.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("oracle decode: %v body=%q", err, body)
	}
	return resp
}
