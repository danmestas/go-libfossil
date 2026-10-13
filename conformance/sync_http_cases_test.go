package conformance

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
)

type respondFunc = func(s *httpScript, w http.ResponseWriter, r *http.Request, n int)

// redirects sends the first hops requests along /hop/1, /hop/2, ... with
// code, then answers with an igot of U and, after, empty replies.
func redirects(code, hops int) respondFunc {
	return func(s *httpScript, w http.ResponseWriter, r *http.Request, n int) {
		switch {
		case n <= hops:
			s.redirect(w, code, fmt.Sprintf("/hop/%d", n))
		case n == hops+1:
			s.write(w, http.StatusOK, typeUncompressed, reply("igot {U}")...)
		default:
			s.write(w, http.StatusOK, typeUncompressed, reply()...)
		}
	}
}

// typed answers the first request with an igot of U, sent as ctype, and
// later ones with empty replies.
func typed(ctype string) respondFunc {
	return func(s *httpScript, w http.ResponseWriter, _ *http.Request, n int) {
		if n == 1 {
			s.write(w, http.StatusOK, ctype, reply("igot {U}")...)
			return
		}
		s.write(w, http.StatusOK, typeUncompressed, reply()...)
	}
}

func syncHTTPCases() []syncHTTPCase {
	return append(append(httpRequestCases(), httpReplyCases()...), httpLoginCases()...)
}

// httpRequestCases are Section 4's rules for the request and for
// redirects.
func httpRequestCases() []syncHTTPCase {
	return []syncHTTPCase{{
		Case:    Case{ID: "sync.4.no-xfer-suffix", Req: "SP-4-1"},
		op:      opPull,
		path:    "/repo",
		respond: answer(reply()...),
		want: func(r *httpRun) error {
			if len(r.requests) == 0 || r.requests[0].path != "/repo" {
				return expectf("%s: requests %v, want a POST to /repo", r.who, r.requests)
			}
			return nil
		},
		fc: true,
	}, {
		Case:    Case{ID: "sync.4.content-length", Req: "SP-4-2"},
		op:      opPush,
		respond: answer(reply("gimme {A1}")...),
		want: func(r *httpRun) error {
			for i, req := range r.requests {
				if req.length <= 0 || req.length != int64(len(req.entity)) {
					return expectf("%s: request %d: Content-Length %d, body %d bytes",
						r.who, i+1, req.length, len(req.entity))
				}
			}
			return httpRounds(2)(r)
		},
		fc: true,
	}, {
		Case:    Case{ID: "sync.4.1.request-type", Req: "SP-4.1-2"},
		op:      clientOp{pull: true, login: true},
		respond: answer(reply()...),
		want: func(r *httpRun) error {
			if len(r.requests) == 0 || r.requests[0].ctype != typeCompressed {
				return expectf("%s: Content-Type %v, want %s", r.who, r.requests, typeCompressed)
			}
			return nil
		},
		fc: true,
	}, {
		// A 302 is followed with the same POST, body and all.
		Case:    Case{ID: "sync.4.redirect-302-reposts", Req: "SP-4-4"},
		op:      opPull,
		respond: redirects(http.StatusFound, 1),
		want:    httpEvery(samePost(1, 2), httpSent(3, "gimme {U}")),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.4.redirect-19-followed", Req: "SP-4-4"},
		op:      opPull,
		respond: redirects(http.StatusTemporaryRedirect, 19),
		want:    httpEvery(samePost(1, 20), httpSent(21, "gimme {U}")),
		fc:      true,
	}, {
		// The twentieth redirect is not followed: the session fails.
		Case:    Case{ID: "sync.4.redirect-20-fatal", Req: "SP-4-4"},
		op:      opPull,
		respond: redirects(http.StatusTemporaryRedirect, 20),
		want:    httpEvery(httpFailsWith("redirect limit exceeded"), httpRounds(20)),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.4.redirect-strips-xfer", Req: "SP-4-6"},
		op:      opPull,
		respond: stripXfer,
		want: func(r *httpRun) error {
			if len(r.requests) < 2 || r.requests[1].path != "/b" {
				return expectf("%s: requests %v, want the second at /b", r.who, paths(r))
			}
			return nil
		},
		fc: true,
	}, {
		Case:  Case{ID: "sync.4.redirect-no-downgrade", Req: "SP-4-5"},
		alone: noDowngrade,
	}}
}

// stripXfer redirects to /b/xfer. The draft strips a trailing /xfer
// repeatedly; fossil(1) 2.28 strips one, sending /b/xfer/xfer to /b/xfer,
// so the case has one to strip.
func stripXfer(s *httpScript, w http.ResponseWriter, r *http.Request, n int) {
	if n == 1 {
		s.redirect(w, http.StatusTemporaryRedirect, "/b/xfer")
		return
	}
	s.write(w, http.StatusOK, typeUncompressed, reply()...)
}

// samePost checks requests i and j are POSTs with the same body.
func samePost(i, j int) func(r *httpRun) error {
	return func(r *httpRun) error {
		if len(r.requests) < j {
			return expectf("%s made %d requests, want at least %d: %q (error %v)",
				r.who, len(r.requests), j, paths(r), r.err)
		}
		a, b := r.requests[i-1], r.requests[j-1]
		if b.method != http.MethodPost || string(a.entity) != string(b.entity) {
			return expectf("%s: request %d is %s with %d bytes, request %d POST with %d",
				r.who, j, b.method, len(b.entity), i, len(a.entity))
		}
		return nil
	}
}

func paths(r *httpRun) []string {
	var out []string
	for _, req := range r.requests {
		out = append(out, req.method+" "+req.path)
	}
	return out
}

// httpReplyCases are Section 4's rules for the reply: status and media
// type.
func httpReplyCases() []syncHTTPCase {
	return []syncHTTPCase{{
		// A 201 is not a protocol reply: its igot is not read.
		Case: Case{ID: "sync.4.status-201", Req: "SP-4-7"},
		op:   opPull,
		respond: func(s *httpScript, w http.ResponseWriter, _ *http.Request, _ int) {
			s.write(w, http.StatusCreated, typeUncompressed, reply("igot {U}")...)
		},
		want: httpEvery(httpRounds(1), httpFailsWith("201")),
		fc:   true,
	}, {
		Case: Case{ID: "sync.4.status-404", Req: "SP-4-7"},
		op:   opPull,
		respond: func(s *httpScript, w http.ResponseWriter, _ *http.Request, _ int) {
			s.write(w, http.StatusNotFound, typeUncompressed, reply("igot {U}")...)
		},
		want: httpEvery(httpRounds(1), httpFailsWith("404")),
		fc:   true,
	}, {
		Case:    Case{ID: "sync.4.missing-content-type", Req: "SP-4-8"},
		op:      opPull,
		respond: typed(""),
		want:    httpSent(2, "gimme {U}"),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.4.content-type-parameters", Req: "SP-4-8"},
		op:      opPull,
		respond: typed(typeUncompressed + "; x=1"),
		want:    httpSent(2, "gimme {U}"),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.4.content-type-case", Req: "SP-4-8"},
		op:      opPull,
		respond: typed(strings.ToUpper(typeCompressed)),
		want:    httpSent(2, "gimme {U}"),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.4.content-type-debug", Req: "SP-4-8"},
		op:      opPull,
		respond: typed(typeDebug),
		want:    httpSent(2, "gimme {U}"),
		fc:      true,
	}, {
		Case:    Case{ID: "sync.4.content-type-foreign", Req: "SP-4-8"},
		op:      opPull,
		respond: typed("text/html"),
		want:    httpEvery(httpRounds(1), httpFailsWith("text/html", "server sends error")),
		fc:      true,
	}}
}

// httpLoginCases are Section 6.5's two ways to log in.
func httpLoginCases() []syncHTTPCase {
	return []syncHTTPCase{{
		// Before any server-version, and after one below the gate, the
		// login rides in the body and no cookie carries one.
		Case: Case{ID: "sync.6.5.body-login-before-gate", Req: "SP-6.5-2"},
		op:   clientOp{pull: true, login: true},
		respond: func(s *httpScript, w http.ResponseWriter, _ *http.Request, n int) {
			if n == 1 {
				s.write(w, http.StatusOK, typeUncompressed, "{SV0}", "igot {U}", "{TS}")
				return
			}
			s.write(w, http.StatusOK, typeUncompressed, "{SV0}", "{TS}")
		},
		want: bodyLoginOnly,
		fc:   true,
	}, {
		// The first request has seen no server-version, so it logs in in
		// the body; the reply's server-version passes the gate.
		Case:    Case{ID: "sync.6.5.cookie-login-after-gate", Req: "SP-6.5-1"},
		op:      clientOp{pull: true, login: true},
		respond: typed(typeUncompressed),
		want:    cookieLoginAfterGate,
		fc:      true,
	}}
}

// cookieLoginAfterGate checks Section 6.5: the second request, sent after
// a server-version that passes the gate, logs in through the cookie and
// not in the body.
func cookieLoginAfterGate(r *httpRun) error {
	cards, err := r.cards(2)
	if err != nil {
		return err
	}
	if len(cards) > 0 && cards[0].op() == "login" {
		return expectf("%s: request 2 logs in in the body", r.who)
	}
	if !strings.Contains(r.requests[1].cookie, "x-f-l-c=login+alice+") {
		return expectf("%s: request 2 Cookie %q has no login", r.who, r.requests[1].cookie)
	}
	return nil
}

func bodyLoginOnly(r *httpRun) error {
	if len(r.requests) < 2 {
		return httpRounds(2)(r)
	}
	for i := range r.requests {
		cards, err := r.cards(i + 1)
		if err != nil {
			return err
		}
		if len(cards) == 0 || cards[0].op() != "login" {
			return expectf("%s: request %d has no body login", r.who, i+1)
		}
		if strings.Contains(r.requests[i].cookie, "x-f-l-c=") {
			return expectf("%s: request %d sent a login cookie", r.who, i+1)
		}
	}
	return nil
}

// noDowngrade runs go-libfossil's client against an HTTPS server that
// redirects it to a plain HTTP one, which must see no request. fossil(1)
// would need the test certificate accepted, so it does not run.
func noDowngrade(fx *syncFixture, dir string) error {
	plain := &httpScript{fx: fx, respond: answer(reply()...)}
	plainSrv := httptest.NewServer(plain)
	defer plainSrv.Close()
	secure := &httpScript{fx: fx, respond: func(s *httpScript, w http.ResponseWriter,
		_ *http.Request, _ int) {
		w.Header().Set("Location", plainSrv.URL+"/")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}}
	secureSrv := httptest.NewTLSServer(secure)
	defer secureSrv.Close()
	run := goHTTPClientWith(fx, opPull, secureSrv.URL+"/", dir, secureSrv.Client())
	if n := len(plain.got); n != 0 {
		return expectf("go-libfossil followed the redirect to plain HTTP: %d requests there", n)
	}
	return httpFails(run)
}
