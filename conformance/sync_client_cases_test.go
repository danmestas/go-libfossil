package conformance

import (
	"os"
	"testing"
)

// syncClientCase is a scripted exchange and what the draft says a client
// does in it.
type syncClientCase struct {
	Case
	op    clientOp
	setup []string // SQL on the client's copy of F
	// replies are the scripted replies' card lines; see syncFixture.body.
	replies [][]string
	// raw, when set, builds the reply entities instead, for cases about
	// framing.
	raw   func(fx *syncFixture) [][]byte
	ctype string // the replies' media type; uncompressed unless set
	want  clientCheck
	// fc also runs fossil(1)'s client against the script, whose requests
	// must meet want.
	fc bool
	// fossil is set only when fossil(1)'s client does not do what the
	// case expects, and says why the case follows the draft.
	fossil string
}

func (c syncClientCase) peer(fx *syncFixture) *scriptedPeer {
	p := &scriptedPeer{fx: fx, ctype: c.ctype}
	if p.ctype == "" {
		p.ctype = typeUncompressed
	}
	if c.raw != nil {
		p.replies = c.raw(fx)
		return p
	}
	for _, reply := range c.replies {
		p.replies = append(p.replies, fx.body(reply...))
	}
	return p
}

func (c syncClientCase) run(fx *syncFixture, ref *fossilReference, dir string) error {
	if ref != nil && c.fc {
		run, err := ref.runFossilClient(fx, c.op, c.setup, c.peer(fx), dir)
		if err != nil {
			return err
		}
		if err := fossilAgrees(c.want(run), c.fossil); err != nil {
			return expectf("%v (fossil output %.300q)", err, run.output)
		}
	}
	run, err := runGoClient(fx, c.op, c.setup, c.peer(fx), dir)
	if err != nil {
		return err
	}
	return c.want(run)
}

func TestSyncClient(t *testing.T) {
	ref := newFossilReference(t)
	top := t.TempDir()
	fx := newSyncFixture(t, top)
	for _, c := range syncClientCases() {
		c.Case = markSync(c.Case)
		check(t, c.Case, func() error {
			dir, err := os.MkdirTemp(top, "case")
			if err != nil {
				return err
			}
			return c.run(fx, ref, dir)
		})
	}
}

func syncClientCases() []syncClientCase {
	var all []syncClientCase
	for _, group := range [][]syncClientCase{
		clientRoundCases(), clientPhantomCases(), clientGimmeCases(), clientTerminationCases(),
		clientFramingCases(), clientPayloadCases(), clientCardCases(), clientAuthCases(),
		clientFileCases(),
		clientCFileCases(), clientUVFileCases(), clientDeltaCases(), clientPrivateCases(),
		clientCloneCases(), clientCloneErrorCases(), clientUVCases(), clientUVStatusCases(),
		clientMessageCases(),
	} {
		all = append(all, group...)
	}
	return all
}
