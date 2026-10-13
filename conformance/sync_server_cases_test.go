package conformance

import (
	"os"
	"strings"
	"testing"
)

// syncServerCase is a request and what the draft says the server does with
// it. The expectation is checked against fossil(1)'s reply on every run.
type syncServerCase struct {
	Case
	// setup is SQL run on the copy of F before the request.
	setup  []string
	method string
	path   string
	ctype  string
	// body is the request's card lines; see syncFixture.body.
	body []string
	// cookie, when set, is "USER PASSWORD [CHANGE]": the request carries a
	// login cookie for that user over its body (Section 6.5). CHANGE is a
	// loginLine change, such as "stale" for a nonce over another body.
	cookie string
	// rawCookie, when set, is the request's Cookie header as it stands.
	rawCookie string
	want      replyCheck
	// deviates, for a case marked Deviation, is what go-libfossil does
	// instead; a reply that is neither is fatal, as the mark excuses only
	// the deviation it names.
	deviates replyCheck
	// notSent says why the request is not sent to fossil(1) at all.
	notSent string
	// fossil, when set, says why fossil(1) does not do what the case
	// expects; the case fails once fossil(1) does.
	fossil string
}

func (c syncServerCase) request(fx *syncFixture) syncRequest {
	req := syncRequest{method: c.method, path: c.path, ctype: c.ctype, text: fx.body(c.body...)}
	if c.cookie != "" {
		f := append(strings.Fields(c.cookie), "")
		login := loginLine(fx.vars["PC"], f[0], f[1], f[2], string(req.text))
		req.cookie = "x-f-l-c=" + strings.ReplaceAll(login, " ", "+")
	}
	if c.rawCookie != "" {
		req.cookie = c.rawCookie
	}
	if req.method == "" {
		req.method = "POST"
	}
	if req.path == "" {
		req.path = "/"
	}
	if req.ctype == "" {
		req.ctype = typeCompressed
	}
	return req
}

// run sends the request to fossil(1), when present, then to go-libfossil,
// each on its own copy of F. fatal is a fossil(1) disagreement or a
// harness failure; problem is go-libfossil's departure from the case.
func (c syncServerCase) run(fx *syncFixture, ref *fossilReference,
	dir string) (fatal, problem error) {
	req := c.request(fx)
	if ref != nil && c.notSent == "" {
		path, err := prepare(fx, dir, "fossil.fossil", c.setup)
		if err != nil {
			return err, nil
		}
		reply, err := ref.serve(fx, path, req)
		if err != nil {
			return err, nil
		}
		if err := fossilAgrees(c.want(reply), c.fossil); err != nil {
			return err, nil
		}
	}
	path, err := prepare(fx, dir, "go.fossil", c.setup)
	if err != nil {
		return err, nil
	}
	reply, err := serveGo(fx, path, req)
	if err != nil {
		return nil, err
	}
	problem = c.want(reply)
	if problem != nil && c.deviates != nil {
		if err := c.deviates(reply); err != nil {
			return expectf("not the deviation the case names: %v", err), nil
		}
	}
	return nil, problem
}

// fossilAgrees turns fossil(1)'s verdict on a case into a fatal problem:
// none when fossil does what the case expects, or when it does not and the
// case says why.
func fossilAgrees(problem error, differs string) error {
	switch {
	case differs == "" && problem != nil:
		return expectf("fossil(1) disagrees with the case: %v", problem)
	case differs != "" && problem == nil:
		return expectf("fossil(1) now does what the case expects; remove fossil: %q", differs)
	}
	return nil
}

func TestSyncServer(t *testing.T) {
	ref := newFossilReference(t)
	quietLogs(t)
	top := t.TempDir()
	fx := newSyncFixture(t, top)
	for _, c := range syncServerCases() {
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

func syncServerCases() []syncServerCase {
	var all []syncServerCase
	for _, group := range [][]syncServerCase{
		serverRoundCases(), serverTransportCases(), serverCardCases(),
		serverCardFailureCases(), serverErrorCases(), serverAuthCases(),
		serverLoginCases(), serverContentCases(), serverContentIGotCases(),
		serverUVCases(), serverUVGimmeCases(), serverCloneCases(),
		serverDirectionCases(), serverPragmaCases(), serverPragmaLockCases(),
		serverConfigCases(), serverVersionCases(), serverGateCases(), serverAdmissionCases(),
		serverCookieRefusalCases(), serverPragmaMoreCases(), serverRefusalCases(),
		serverOrderCases(),
	} {
		all = append(all, group...)
	}
	return all
}
