package conformance

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	gofossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/db"
)

// The checkout-inclusive request (Section 8.3), fossil's "ci+" type, which
// Checkout.ResolveWorking is. "ckout" names the working files there while
// the checkout has a version, and only as the whole name given; anywhere
// else it is an ordinary name (symbolic_name_to_rid, src/name.c).

func workingAPICases() []rsAPICase {
	twoChildren := with(chain()[:3], checkin("m", 4, of("c3", "c2")))
	// A tag named ckout, on c1: inside the inclusive request the keyword
	// wins; everywhere else the name is the tag.
	ckoutTag := with(twoChildren, control("t", 9, "+sym-ckout {c1}"))
	// A technote tagged sym-note: the inclusive request is a check-in
	// request, so only a check-in can answer it.
	noted := with(twoChildren, technote("e", 9, "+sym-note *"))
	return []rsAPICase{
		workingResolves("repostate.8.3.ckout-inclusive", twoChildren, "c2", "ckout", "c2", true),
		workingResolves("repostate.8.3.ckout-inclusive-over-tag", ckoutTag, "c2", "ckout",
			"c2", true),
		// The trailing-punctuation retry resolves ckout under ci, where it is
		// an ordinary name: ckout. names the tag, not the working files.
		workingResolves("repostate.8.3.ckout-inclusive-whole-name", ckoutTag, "c2", "ckout.",
			"c1", false),
		// With no checkout version, ckout falls through to the other forms.
		workingResolves("repostate.8.3.ckout-no-version", ckoutTag, "", "ckout", "c1", false),
		workingResolves("repostate.8.3.ckout-no-version-untagged", twoChildren, "", "ckout",
			"", false),
		workingResolves("repostate.8.3.inclusive-current", twoChildren, "c2", "current",
			"c2", false),
		workingResolves("repostate.8.3.inclusive-checkin-only", noted, "c2", "note", "", false),
		// The untyped request reads ckout as an ordinary name, the tag.
		checkoutResolves("repostate.8.3.ckout-untyped-tag", "RS-8.3-4", ckoutTag, "c2",
			"ckout", "c1"),
	}
}

// workingResolves checks the checkout-inclusive request for name, with a
// checkout on the artifact labelled at, or with no checkout version when at
// is empty. It must name the check-in labelled want, or nothing when want is
// empty, and say whether it is the working files. fossil(1) takes the request
// as test-name-to-id --type ci+, which prints the checkout's hash for the
// working files.
func workingResolves(id string, arts []rsArt, at, name, want string, working bool) rsAPICase {
	if working && (want != at || at == "") {
		panic("conformance: the working files are on the checkout's version")
	}
	return rsAPICase{Case: Case{ID: id, Req: "RS-8.3-4"}, arts: arts, run: func(e *rsEnv) error {
		if e.ref != nil {
			if err := e.fossilWorking(at, name, want); err != nil {
				return err
			}
		}
		co, err := e.goCheckoutOrEmpty(at)
		if err != nil {
			return err
		}
		got, gotWorking, err := e.goResolveWorking(co, name)
		if err != nil {
			return err
		}
		if got != want || gotWorking != working {
			return expectf("with a checkout on %q, Checkout.ResolveWorking(%q) = %q "+
				"(working %t), want %q (working %t)", at, name, got, gotWorking, want, working)
		}
		return nil
	}}
}

// fossilWorking checks fossil(1)'s answer to the inclusive request. fossil's
// checkout opens on an empty repository, so it has no version until moved.
func (e *rsEnv) fossilWorking(at, name, want string) error {
	if at != "" {
		if err := e.fossilCheckout(at); err != nil {
			return err
		}
	}
	out, err := e.fossilRun("test-name-to-id", "--type", "ci+", name)
	if err != nil {
		return err
	}
	_, result, ok := strings.Cut(strings.TrimSpace(out), " -> ")
	if !ok {
		return fmt.Errorf("test-name-to-id: unrecognized output %q", out)
	}
	got := ""
	if !strings.HasPrefix(result, "ERROR") {
		got = labelOf(result, e.script.labels)
	}
	if got != want {
		return expectf("fossil(1) with a checkout on %q resolves ci+ %q to %q; "+
			"the case expects %q", at, name, got, want)
	}
	return nil
}

// goCheckoutOrEmpty is goCheckout on the artifact labelled at, or, when at
// is empty, a checkout whose recorded version is blanked: go-libfossil opens
// a checkout on a version, so none can be created without one.
func (e *rsEnv) goCheckoutOrEmpty(at string) (*gofossil.Checkout, error) {
	if at != "" {
		return e.goCheckout(at)
	}
	co, err := e.goCheckout("c1")
	if err != nil {
		return nil, err
	}
	ck, err := sql.Open(db.RegisteredDriver().Name, filepath.Join(co.Dir(), ".fslckout"))
	if err != nil {
		return nil, err
	}
	defer ck.Close()
	_, err = ck.Exec("UPDATE vvar SET value = '' WHERE name = 'checkout'")
	return co, err
}

// goResolveWorking is the label and working flag co.ResolveWorking gives
// name, or "" for an error that says the name is unknown or ambiguous.
func (e *rsEnv) goResolveWorking(co *gofossil.Checkout, name string) (string, bool, error) {
	r, err := e.open()
	if err != nil {
		return "", false, err
	}
	v, err := co.ResolveWorking(name)
	if errors.Is(err, gofossil.ErrVersionNotFound) || errors.Is(err, gofossil.ErrAmbiguousVersion) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return e.labelOfRid(r.Inner(), v.RID), v.Working, nil
}
