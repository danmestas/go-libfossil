package sync

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/testutil"
)

// TestReqConfigMatchesRealFossil answers the same reqconfig requests on the
// same repository with fossil's own server and with HandleSync, and requires
// the same config cards byte for byte. The repository holds a row in every
// group, rows fossil never transfers, a row without an mtime, and a user
// table, so the comparison covers which names go out, to whom, in what order
// and in what encoding.
func TestReqConfigMatchesRealFossil(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	repoPath := filepath.Join(t.TempDir(), "cfg.fossil")
	fossilRun(t, bin, "new", repoPath)
	// Each `fossil http` would otherwise detach a backoffice process that
	// outlives the test by a minute.
	fossilRun(t, bin, "settings", "backoffice-disable", "1", "-R", repoPath)
	fossilRun(t, bin, "user", "new", "adm", "admin user", "pw", "-R", repoPath)
	fossilRun(t, bin, "user", "password", "adm", "pw", "-R", repoPath)
	fossilRun(t, bin, "user", "capabilities", "adm", "a", "-R", repoPath)
	fossilRun(t, bin, "sql", "-R", repoPath, strings.Join([]string{
		"REPLACE INTO config VALUES('project-name','Demo O''Proj',100)",
		"REPLACE INTO config VALUES('css','body{}',101.5)",
		"REPLACE INTO config VALUES('user-color-map','x',102)",
		"REPLACE INTO config VALUES('secret-setting','zzz',103)",
		"REPLACE INTO config VALUES('walias:/a','/b',104)",
		"REPLACE INTO config VALUES('interwiki:w','{}',105)",
		"REPLACE INTO config VALUES('index-page',NULL,106)",
		"REPLACE INTO config VALUES('footer','no mtime',NULL)",
		"INSERT INTO shun VALUES('abc',107,NULL)",
		"INSERT INTO concealed VALUES('h1',108,'a@b')",
		"UPDATE user SET mtime=109",
	}, ";"))

	names := []string{
		"/all", "/", "/s", "/sh", "/p", "/u", "/user", "/email", "/skinx", "/ticket",
		"/xfer", "/alias", "/interwiki", "/subscriber", "project-code", "project-name",
		// zit's scalar marker for the sub-manifest settings: fossil has no
		// such group, and neither server sends anything without the settings.
		"/config",
	}
	// Clone implies Read in fossil's trunk (page_xfer sets g.perm.Read when
	// g.perm.Clone is set) but not in the 2.28 binary the tests run. Like
	// trunk, HandleSync gives nobody="g" what fossil 2.28 gives nobody="go".
	for _, caps := range []string{"", "g", "o", "i", "e", "oe"} {
		for _, name := range names {
			fossilCaps := caps
			if caps == "g" {
				fossilCaps = "go"
			}
			fossilRun(t, bin, "user", "capabilities", "nobody", fossilCaps,
				"-R", repoPath)
			req := &xfer.Message{Cards: []xfer.Card{&xfer.ReqConfigCard{Name: name}}}
			label := fmt.Sprintf("nobody=%q reqconfig %s", caps, name)
			compareReqConfig(t, bin, repoPath, req, label, caps)
		}
	}

	// An authenticated Admin reads the user table too.
	fossilRun(t, bin, "user", "capabilities", "nobody", "", "-R", repoPath)
	rest := []xfer.Card{&xfer.ReqConfigCard{Name: "/all"}}
	pc := strings.Trim(strings.TrimSpace(fossilOut(t, bin, "sql", "-R", repoPath,
		"SELECT value FROM config WHERE name='project-code'")), "'")
	req := signedRequest(t, "adm", "pw", pc, rest...)
	got := compareReqConfig(t, bin, repoPath, req, "admin reqconfig /all", "")
	if !strings.Contains(strings.Join(got, "\n"), "/user 109 'adm' pw ") {
		t.Errorf("admin /all carried no user rows: %q", got)
	}
}

// compareReqConfig sends req to fossil and to HandleSync and fails unless
// both answer with the same config cards. It returns fossil's. nobodyCaps,
// when not empty, are nobody's capabilities for HandleSync alone.
func compareReqConfig(
	t *testing.T, bin, repoPath string, req *xfer.Message, label, nobodyCaps string,
) []string {
	t.Helper()
	want := configLines(fossilHTTPXfer(t, bin, repoPath, req))

	r, err := repo.Open(repoPath)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	defer r.Close()
	if nobodyCaps != "" {
		setNobodyCaps(t, r, nobodyCaps)
	}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("%s: HandleSync: %v", label, err)
	}
	if errs := findCards[*xfer.ErrorCard](resp); len(errs) != 0 {
		t.Errorf("%s: error cards %v", label, errs)
	}
	got := configLines(resp)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\ngo     %q\nfossil %q", label, got, want)
	}
	return want
}

// fossilHTTPXfer posts req to fossil's /xfer through `fossil http`, which
// serves one request read from stdin with ordinary permission checks.
func fossilHTTPXfer(t *testing.T, bin, repoPath string, req *xfer.Message) *xfer.Message {
	t.Helper()
	body, err := req.EncodeUncompressed()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var in bytes.Buffer
	fmt.Fprintf(&in, "POST /xfer HTTP/1.0\r\nHost: localhost\r\n"+
		"Content-Type: %s\r\nContent-Length: %d\r\n\r\n", xfer.ContentTypeUncompressed, len(body))
	in.Write(body)
	cmd := exec.Command(bin, "http", repoPath)
	cmd.Stdin = &in
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("fossil http: %v", err)
	}
	head, reply, ok := bytes.Cut(out, []byte("\r\n\r\n"))
	if !ok {
		t.Fatalf("fossil http: no header terminator in %q", out)
	}
	ctype := ""
	for _, line := range strings.Split(string(head), "\r\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(k, "Content-Type") {
			ctype = strings.TrimSpace(v)
		}
	}
	msg, err := xfer.Decode(reply, ctype)
	if err != nil {
		t.Fatalf("decode fossil reply (%s): %v", ctype, err)
	}
	return msg
}

func fossilRun(t *testing.T, bin string, args ...string) {
	t.Helper()
	fossilOut(t, bin, args...)
}

func fossilOut(t *testing.T, bin string, args ...string) string {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("fossil %v: %v\n%s", args, err, out)
	}
	return string(out)
}
