package conformance

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/db"
)

// A server case is a raw request, sent to go-libfossil's HTTP handler and,
// through `fossil http --in`, to fossil(1) on a copy of the same
// repository. fossil test-xfer cannot be the reference: in 2.28 it renders
// the robot-defense page and crashes.

// syncRequest is the HTTP request a server case sends.
type syncRequest struct {
	method string // POST unless set
	path   string // "/" unless set
	ctype  string // typeCompressed unless set
	cookie string // the Cookie header, if any
	text   []byte // card text, compressed on the wire when ctype says so
	raw    []byte // the entity as sent, when set; text is then unused
}

// entity is the request body as sent.
func (r syncRequest) entity() []byte {
	if r.raw != nil {
		return r.raw
	}
	if r.ctype == typeCompressed {
		return compress(r.text, zlib.DefaultCompression)
	}
	return r.text
}

// serverReply is what one implementation answered.
type serverReply struct {
	fx     *syncFixture
	who    string // "go-libfossil" or "fossil(1)"
	status int
	ctype  string
	raw    []byte // the reply entity as received
	text   []byte // the card text: raw, decompressed when compressed
	cards  []wireCard
	repo   string // the repository after the request
}

// replyOf decodes a reply entity into cards. A reply that is not a sync
// reply (a status other than 200, or an HTML page) keeps its text and has
// no cards.
func replyOf(fx *syncFixture, who string, status int, ctype string,
	raw []byte) (*serverReply, error) {
	r := &serverReply{fx: fx, who: who, status: status, ctype: ctype, raw: raw, text: raw}
	if status != http.StatusOK || !strings.HasPrefix(ctype, "application/x-fossil") {
		return r, nil
	}
	text, err := bodyText(ctype, raw)
	if err != nil {
		return nil, fmt.Errorf("%s reply: %v", who, err)
	}
	r.text = text
	if r.cards, err = parseCards(text); err != nil {
		return nil, fmt.Errorf("%s reply: %v", who, err)
	}
	return r, nil
}

// serveGo sends req to go-libfossil's XferHandler on the repository at path.
// A panic in the handler is reported as the failure it is.
func serveGo(fx *syncFixture, path string, req syncRequest) (reply *serverReply, err error) {
	r, err := libfossil.Open(path)
	if err != nil {
		return nil, err
	}
	rec := httptest.NewRecorder()
	func() {
		defer func() {
			if p := recover(); p != nil {
				err = expectf("go-libfossil's handler panicked: %v", p)
			}
		}()
		httpReq := httptest.NewRequest(req.method, req.path, bytes.NewReader(req.entity()))
		httpReq.Header.Set("Content-Type", req.ctype)
		if req.cookie != "" {
			httpReq.Header.Set("Cookie", req.cookie)
		}
		r.XferHandler().ServeHTTP(rec, httpReq)
	}()
	if closeErr := r.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	reply, err = replyOf(fx, "go-libfossil", rec.Code, rec.Header().Get("Content-Type"),
		rec.Body.Bytes())
	if reply != nil {
		reply.repo = path
	}
	return reply, err
}

// serve sends req to fossil(1) on the repository at path, through
// `fossil http`, which reads one raw HTTP request and writes the reply.
func (f *fossilReference) serve(fx *syncFixture, path string,
	req syncRequest) (*serverReply, error) {
	status, ctype, raw, err := f.serveEntity(path, req)
	if err != nil {
		return nil, err
	}
	reply, err := replyOf(fx, "fossil(1)", status, ctype, raw)
	if reply != nil {
		reply.repo = path
	}
	return reply, err
}

// serveEntity sends one HTTP request to fossil(1) on the repository at
// path, and returns the reply's status, Content-Type and entity.
func (f *fossilReference) serveEntity(path string, req syncRequest) (int, string, []byte, error) {
	f.count++
	reqFile := filepath.Join(f.dir, fmt.Sprintf("request-%d", f.count))
	entity := req.entity()
	head := fmt.Sprintf("%s %s HTTP/1.0\r\nHost: 127.0.0.1\r\nContent-Type: %s\r\n"+
		"Content-Length: %d\r\n", req.method, req.path, req.ctype, len(entity))
	if req.cookie != "" {
		head += "Cookie: " + req.cookie + "\r\n"
	}
	head += "\r\n"
	if err := os.WriteFile(reqFile, append([]byte(head), entity...), 0o644); err != nil {
		return 0, "", nil, err
	}
	defer os.Remove(reqFile)
	ctx, cancel := context.WithTimeout(context.Background(), fossilTimeout)
	defer cancel()
	cmd := f.command(ctx, "http", path, "--in", reqFile, "--ipaddr", "127.0.0.1")
	cmd.Dir = f.dir
	out, err := cmd.Output()
	if err != nil {
		return 0, "", nil, fmt.Errorf("fossil http: %v: %.200q", err, out)
	}
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(out)), nil)
	if err != nil {
		return 0, "", nil, fmt.Errorf("fossil http reply: %v: %.200q", err, out)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), raw, err
}

// prepare copies F into dir and applies a case's SQL to the copy.
func prepare(fx *syncFixture, dir, name string, setup []string) (string, error) {
	path, err := fx.copyTo(dir, name)
	if err != nil || len(setup) == 0 {
		return path, err
	}
	conn, err := db.OpenSQL(path, db.OpenConfig{}, nil)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	for _, stmt := range setup {
		if _, err := conn.Exec(fx.expand(stmt)); err != nil {
			return "", fmt.Errorf("setup %q: %v", stmt, err)
		}
	}
	return path, nil
}

// queryInt runs a query that returns one integer against the repository at
// path, after its placeholders are expanded.
func queryInt(fx *syncFixture, path, query string) (int, error) {
	conn, err := db.OpenSQL(path, db.OpenConfig{}, nil)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	var text string
	if err := conn.QueryRow(fx.expand(query)).Scan(&text); err != nil {
		return 0, fmt.Errorf("%s: %v", query, err)
	}
	return strconv.Atoi(text)
}
