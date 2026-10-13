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
	// deviates, for a case marked Deviation, is what go-libfossil does
	// instead; a run that is neither is fatal.
	deviates clientCheck
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

// run runs fossil(1)'s client, for an fc case, then go-libfossil's. fatal
// is a fossil(1) disagreement or a harness failure; problem is
// go-libfossil's departure from the case.
func (c syncClientCase) run(fx *syncFixture, ref *fossilReference,
	dir string) (fatal, problem error) {
	if ref != nil && c.fc {
		run, err := ref.runFossilClient(fx, c.op, c.setup, c.peer(fx), dir)
		if err != nil {
			return err, nil
		}
		if err := fossilAgrees(c.want(run), c.fossil); err != nil {
			return expectf("%v (fossil output %.300q)", err, run.output), nil
		}
	}
	run, err := runGoClient(fx, c.op, c.setup, c.peer(fx), dir)
	if err != nil {
		return nil, err
	}
	problem = c.want(run)
	if problem != nil && c.deviates != nil {
		if err := c.deviates(run); err != nil {
			return expectf("not the deviation the case names: %v", err), nil
		}
	}
	return nil, problem
}

func TestSyncClient(t *testing.T) {
	ref := newFossilReference(t)
	top := t.TempDir()
	fx := newSyncFixture(t, top)
	for _, c := range syncClientCases() {
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

func syncClientCases() []syncClientCase {
	var all []syncClientCase
	for _, group := range [][]syncClientCase{
		clientRoundCases(), clientPhantomCases(), clientGimmeCases(), clientTerminationCases(),
		clientFramingCases(), clientPayloadCases(), clientCardCases(), clientCardNameCases(),
		clientAuthCases(),
		clientFileCases(), clientCFileCases(), clientUVFileCases(), clientDeltaCases(),
		clientPrivateCases(), clientCloneCases(), clientCloneStallCases(),
		clientCloneErrorCases(), clientUVCases(), clientUVStatusCases(), clientMessageCases(),
		clientGateCases(), clientAdmissionCases(), clientPullOnlyCases(),
		clientShortUVIGotCases(), clientRefusalCases(), clientCloneBadCardCases(),
		clientLinkCases(), clientIntegrityCases(),
	} {
		all = append(all, group...)
	}
	return all
}
