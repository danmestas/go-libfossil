package conformance

import (
	"compress/zlib"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
)

// The cases of Section 4 that live in HTTP itself: paths, redirects,
// status codes and media types. go-libfossil's client runs through
// NewHTTPTransport against a scripted httptest server; where the server is
// plain HTTP, fossil(1)'s client runs against it too.

// httpRequest is one request the scripted server received.
type httpRequest struct {
	method, path, ctype, cookie string
	length                      int64 // the Content-Length header, -1 if absent
	entity                      []byte
}

// httpScript answers each request with respond, given the request's number
// from 1 and the server's own URL, and records it.
type httpScript struct {
	fx      *syncFixture
	base    string
	respond func(s *httpScript, w http.ResponseWriter, r *http.Request, n int)
	mu      sync.Mutex
	got     []httpRequest
}

func (s *httpScript) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	entity, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.got = append(s.got, httpRequest{method: r.Method, path: r.URL.Path,
		ctype: r.Header.Get("Content-Type"), cookie: r.Header.Get("Cookie"),
		length: r.ContentLength, entity: entity})
	n := len(s.got)
	s.mu.Unlock()
	if n > maxScriptedRounds+20 {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	s.respond(s, w, r, n)
}

// write sends a reply body of card lines with a media type; with
// typeCompressed it is compressed, and with no type the header is left
// out entirely.
func (s *httpScript) write(w http.ResponseWriter, status int, ctype string, cards ...string) {
	body := s.fx.body(cards...)
	if strings.EqualFold(ctype, typeCompressed) || ctype == "" {
		body = compress(body, zlib.DefaultCompression)
	}
	if ctype == "" {
		w.Header()["Content-Type"] = nil
	} else {
		w.Header().Set("Content-Type", ctype)
	}
	w.WriteHeader(status)
	w.Write(body)
}

// redirect answers with a redirect to path on the same server.
func (s *httpScript) redirect(w http.ResponseWriter, code int, path string) {
	w.Header().Set("Location", s.base+path)
	w.WriteHeader(code)
}

// httpRun is what one client did against an httpScript.
type httpRun struct {
	fx       *syncFixture
	who      string
	requests []httpRequest
	err      error
	output   string
}

func (r *httpRun) cards(i int) ([]wireCard, error) {
	if i < 1 || i > len(r.requests) {
		return nil, expectf("%s sent %d requests, no request %d (error %v)",
			r.who, len(r.requests), i, r.err)
	}
	req := r.requests[i-1]
	text, err := bodyText(req.ctype, req.entity)
	if err != nil {
		return nil, err
	}
	return parseCards(text)
}

// syncHTTPCase is a scripted HTTP exchange and what the client does in it.
type syncHTTPCase struct {
	Case
	op      clientOp
	path    string // the URL's path; "/" unless set
	respond func(s *httpScript, w http.ResponseWriter, r *http.Request, n int)
	want    func(r *httpRun) error
	// alone, when set, runs the whole case instead, for one that needs a
	// server of its own.
	alone func(fx *syncFixture, dir string) error
	fc    bool
}

// run runs fossil(1)'s client, for an fc case, then go-libfossil's. fatal
// is a fossil(1) disagreement or a harness failure; problem is
// go-libfossil's departure from the case.
func (c syncHTTPCase) run(fx *syncFixture, ref *fossilReference,
	dir string) (fatal, problem error) {
	if c.alone != nil {
		return nil, c.alone(fx, dir)
	}
	if ref != nil && c.fc {
		run, err := c.serve(fx, func(url string) *httpRun {
			return ref.httpClient(fx, c.op, url, dir)
		})
		if err != nil {
			return err, nil
		}
		if err := fossilAgrees(c.want(run), ""); err != nil {
			return expectf("%v (fossil output %.300q)", err, run.output), nil
		}
	}
	run, err := c.serve(fx, func(url string) *httpRun {
		return goHTTPClientWith(fx, c.op, url, dir, nil)
	})
	if err != nil {
		return err, nil
	}
	return nil, c.want(run)
}

// serve starts the script's server, runs a client against it, and returns
// what the server saw.
func (c syncHTTPCase) serve(fx *syncFixture, client func(url string) *httpRun) (*httpRun, error) {
	script := &httpScript{fx: fx, respond: c.respond}
	srv := httptest.NewServer(script)
	defer srv.Close()
	script.base = srv.URL
	path := c.path
	if path == "" {
		path = "/"
	}
	run := client(srv.URL + path)
	script.mu.Lock()
	run.requests = append([]httpRequest(nil), script.got...)
	script.mu.Unlock()
	return run, nil
}

// goHTTPClientWith pulls (or as op says) with go-libfossil over HTTP,
// through client when it is not nil.
func goHTTPClientWith(fx *syncFixture, op clientOp, url, dir string, client *http.Client) *httpRun {
	run := &httpRun{fx: fx, who: "go-libfossil"}
	path, err := prepare(fx, dir, "go-http.fossil", quiet)
	if err != nil {
		run.err = err
		return run
	}
	defer os.Remove(path)
	r, err := libfossil.Open(path)
	if err != nil {
		run.err = err
		return run
	}
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), clientTimeout)
	defer cancel()
	var opts []libfossil.HTTPOption
	if client != nil {
		opts = append(opts, libfossil.WithHTTPClient(client))
	}
	_, run.err = r.Sync(ctx, libfossil.NewHTTPTransport(url, opts...), syncOpts(op))
	return run
}

// httpClient pulls (or as op says) with fossil(1)'s client.
func (f *fossilReference) httpClient(fx *syncFixture, op clientOp, url, dir string) *httpRun {
	run := &httpRun{fx: fx, who: "fossil(1)"}
	path, err := prepare(fx, dir, "fossil-http.fossil", quiet)
	if err != nil {
		run.err = err
		return run
	}
	defer os.Remove(path)
	if op.login {
		user, password := op.credentials()
		url = strings.Replace(url, "http://", "http://"+user+":"+password+"@", 1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), clientTimeout)
	defer cancel()
	cmd := f.command(ctx, fossilVerb(op), url, "-R", path, "--once", "--user", "setup")
	cmd.Dir = filepath.Dir(path)
	out, err := cmd.CombinedOutput()
	run.output, run.err = string(out), err
	return run
}

func TestSyncHTTP(t *testing.T) {
	ref := newFossilReference(t)
	quietLogs(t)
	top := t.TempDir()
	fx := newSyncFixture(t, top)
	for _, c := range syncHTTPCases() {
		c.Case = markSync(c.Case)
		checkSync(t, c.Case, func() (error, error) {
			dir, err := os.MkdirTemp(top, "case")
			if err != nil {
				return err, nil
			}
			return c.run(fx, ref, dir)
		})
	}
}

// httpRounds checks the server saw exactly n requests.
func httpRounds(n int) func(r *httpRun) error {
	return func(r *httpRun) error {
		if len(r.requests) != n {
			var paths []string
			for _, req := range r.requests {
				paths = append(paths, req.method+" "+req.path)
			}
			return expectf("%s made %d requests, want %d (error %v): %q",
				r.who, len(r.requests), n, r.err, paths)
		}
		return nil
	}
}

// httpSent checks request i carries a card line equal to line.
func httpSent(i int, line string) func(r *httpRun) error {
	return func(r *httpRun) error {
		cards, err := r.cards(i)
		if err != nil {
			return err
		}
		if index(cards, r.fx.expand(line)) < 0 {
			return expectf("%s: request %d has no %q: %q", r.who, i, line, summary(cards))
		}
		return nil
	}
}

func httpFails(r *httpRun) error {
	if r.err == nil {
		return expectf("%s: the session succeeded", r.who)
	}
	return nil
}

// httpFailsWith checks the session failed for one of reasons: each
// client's wording, in go-libfossil's error or fossil(1)'s output.
func httpFailsWith(reasons ...string) func(r *httpRun) error {
	return func(r *httpRun) error {
		if err := httpFails(r); err != nil {
			return err
		}
		text := r.output + r.err.Error()
		for _, reason := range reasons {
			if strings.Contains(text, reason) {
				return nil
			}
		}
		return expectf("%s failed without one of %q: %.200q", r.who, reasons, text)
	}
}

func httpEvery(checks ...func(r *httpRun) error) func(r *httpRun) error {
	return func(r *httpRun) error {
		for _, c := range checks {
			if err := c(r); err != nil {
				return err
			}
		}
		return nil
	}
}

// answer replies to every request with the card lines given, as an
// uncompressed sync reply.
func answer(cards ...string) func(s *httpScript, w http.ResponseWriter, r *http.Request, n int) {
	return func(s *httpScript, w http.ResponseWriter, _ *http.Request, n int) {
		if n == 1 {
			s.write(w, http.StatusOK, typeUncompressed, cards...)
			return
		}
		s.write(w, http.StatusOK, typeUncompressed, "{TS}")
	}
}
