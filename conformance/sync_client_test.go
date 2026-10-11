package conformance

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"time"

	libfossil "github.com/danmestas/go-libfossil"
)

// A client case is a scripted peer: the n-th request gets the n-th reply.
// go-libfossil's public Sync and Clone talk to it through a
// FramedTransport; for the cases marked fc, fossil(1)'s client talks to
// the same script over HTTP, and its requests must meet the same
// expectation.

// maxScriptedRounds stops a client that does not terminate. No case needs
// more than a few rounds; a client still asking after this many is looping.
const maxScriptedRounds = 12

// clientTimeout bounds one client session against a scripted peer.
const clientTimeout = 30 * time.Second

// peerRequest is one request the scripted peer received.
type peerRequest struct {
	cards  []wireCard
	entity []byte // the body as received
	text   []byte // its card text
	cookie string // the Cookie header, when the request came over HTTP
	path   string // the URL path, when the request came over HTTP
}

// scriptedPeer is the far end of a client case.
type scriptedPeer struct {
	fx      *syncFixture
	replies [][]byte
	ctype   string // the replies' media type
	mu      sync.Mutex
	got     []peerRequest
}

// answer records a request and returns the reply scripted for it: after
// the script runs out, a reply holding only the timestamp comment.
func (p *scriptedPeer) answer(ctype string, entity []byte, req peerRequest) ([]byte, error) {
	text, err := bodyText(ctype, entity)
	if err != nil {
		return nil, fmt.Errorf("scripted peer: request %d: %v", len(p.got)+1, err)
	}
	req.entity, req.text = entity, text
	if req.cards, err = parseCards(text); err != nil {
		return nil, fmt.Errorf("scripted peer: request %d: %v", len(p.got)+1, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, req)
	n := len(p.got)
	if n > maxScriptedRounds {
		return nil, fmt.Errorf("scripted peer: more than %d requests", maxScriptedRounds)
	}
	if n <= len(p.replies) {
		return p.replies[n-1], nil
	}
	return []byte(p.fx.expand("{TS}") + "\n"), nil
}

func (p *scriptedPeer) requests() []peerRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]peerRequest(nil), p.got...)
}

// RoundTrip and RoundTripFramed make the peer a libfossil.FramedTransport.
// go-libfossil compresses every request.
func (p *scriptedPeer) RoundTrip(ctx context.Context, payload []byte) ([]byte, error) {
	reply, _, err := p.RoundTripFramed(ctx, payload)
	return reply, err
}

func (p *scriptedPeer) RoundTripFramed(_ context.Context, payload []byte) ([]byte, string, error) {
	reply, err := p.answer(typeCompressed, payload, peerRequest{})
	return reply, p.ctype, err
}

// ServeHTTP makes the peer an HTTP server, for fossil(1)'s client.
func (p *scriptedPeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	entity, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req := peerRequest{cookie: r.Header.Get("Cookie"), path: r.URL.Path}
	reply, err := p.answer(r.Header.Get("Content-Type"), entity, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", p.ctype)
	w.Write(reply)
}

// clientRun is what one client did in a case.
type clientRun struct {
	fx       *syncFixture
	who      string
	requests []peerRequest
	err      error  // the session's error, or fossil(1)'s exit status
	output   string // fossil(1)'s output
	repo     string // the client's repository afterwards
	result   *libfossil.SyncResult
	clone    *libfossil.CloneResult
}

// clientOp is what a client case asks the client to do.
type clientOp struct {
	pull, push, clone bool
	private, uv       bool
	login             bool   // as user, alice unless set, password "secret" unless set
	user, password    string // the login, when not alice and secret
}

// credentials are the login and password a logged-in client uses.
func (op clientOp) credentials() (string, string) {
	user, password := "alice", "secret"
	if op.user != "" {
		user = op.user
	}
	if op.password != "" {
		password = op.password
	}
	return user, password
}

var (
	opPull  = clientOp{pull: true}
	opPush  = clientOp{push: true}
	opSync  = clientOp{pull: true, push: true}
	opClone = clientOp{clone: true}
)

// runGoClient runs go-libfossil's client against peer, on a copy of F (or,
// for a clone, a new repository) in dir. A panic is reported as the failure
// it is.
func runGoClient(fx *syncFixture, op clientOp, setup []string, peer *scriptedPeer,
	dir string) (run *clientRun, err error) {
	defer func() {
		if p := recover(); p != nil {
			run, err = nil, expectf("go-libfossil's client panicked: %v", p)
		}
	}()
	run = &clientRun{fx: fx, who: "go-libfossil"}
	ctx, cancel := context.WithTimeout(context.Background(), clientTimeout)
	defer cancel()
	if op.clone {
		run.repo = filepath.Join(dir, "clone.fossil")
		r, res, err := libfossil.Clone(ctx, run.repo, peer, cloneOpts(op))
		run.err, run.clone = err, res
		if r != nil {
			r.Close()
		}
		run.requests = peer.requests()
		return run, nil
	}
	path, err := prepare(fx, dir, "client.fossil", setup)
	if err != nil {
		return nil, err
	}
	r, err := libfossil.Open(path)
	if err != nil {
		return nil, err
	}
	run.repo = path
	run.result, run.err = r.Sync(ctx, peer, syncOpts(op))
	if err := r.Close(); err != nil {
		return nil, err
	}
	run.requests = peer.requests()
	return run, nil
}

func syncOpts(op clientOp) libfossil.SyncOpts {
	opts := libfossil.SyncOpts{Pull: op.pull, Push: op.push, Private: op.private, UV: op.uv}
	if op.login {
		opts.User, opts.Password = op.credentials()
	}
	return opts
}

func cloneOpts(op clientOp) libfossil.CloneOpts {
	if op.login {
		user, password := op.credentials()
		return libfossil.CloneOpts{User: user, Password: password}
	}
	return libfossil.CloneOpts{}
}

// runFossilClient runs fossil(1)'s client against peer over HTTP, on its own
// copy of F in dir.
func (f *fossilReference) runFossilClient(fx *syncFixture, op clientOp, setup []string,
	peer *scriptedPeer, dir string) (*clientRun, error) {
	srv := httptest.NewServer(peer)
	defer srv.Close()
	url := srv.URL + "/"
	if op.login {
		user, password := op.credentials()
		url = strings.Replace(url, "http://", "http://"+user+":"+password+"@", 1)
	}
	run := &clientRun{fx: fx, who: "fossil(1)"}
	var args []string
	if op.clone {
		run.repo = filepath.Join(dir, "fossil-clone.fossil")
		args = []string{"clone", url, run.repo}
	} else {
		path, err := prepare(fx, dir, "fossil-client.fossil", setup)
		if err != nil {
			return nil, err
		}
		run.repo = path
		args = []string{fossilVerb(op), url, "-R", path, "--once", "--user", "setup"}
		if op.private {
			args = append(args, "--private")
		}
		if op.uv {
			// Only fossil sync takes -u: a case whose client pulls
			// unversioned files without pushing has no fossil(1) run.
			if fossilVerb(op) != "sync" {
				panic("conformance: fossil(1) syncs unversioned files only in both directions")
			}
			args = append(args, "-u")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), clientTimeout)
	defer cancel()
	cmd := f.command(ctx, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	run.output, run.err = string(out), err
	run.requests = peer.requests()
	return run, nil
}

func fossilVerb(op clientOp) string {
	switch {
	case op.pull && op.push:
		return "sync"
	case op.push:
		return "push"
	}
	return "pull"
}
