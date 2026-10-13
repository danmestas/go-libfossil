package conformance

import (
	"path/filepath"
	"strings"

	gofossil "github.com/danmestas/go-libfossil"
)

// The cases that need an open checkout: the checkout's branch (Section 6.4)
// and the names that resolve relative to it (Section 8.3).

func checkoutAPICases() []rsAPICase {
	// c2 has two children: c3, its primary child, and m, newer, whose
	// merge parent it is. next prefers the primary link to the newer one.
	twoChildren := with(chain()[:3], checkin("m", 4, of("c3", "c2")))
	return []rsAPICase{{
		Case: Case{ID: "repostate.6.4.checkout-branch", Req: "RS-6.4-1"},
		arts: cancelledBranch(),
		run:  checkoutBranch,
	},
		checkoutResolves("repostate.8.3.current", "RS-8.3-4", twoChildren, "c2", "current", "c2"),
		checkoutResolves("repostate.8.3.prev", "RS-8.3-4", twoChildren, "c2", "prev", "c1"),
		checkoutResolves("repostate.8.3.previous", "RS-8.3-4", twoChildren, "c2", "previous", "c1"),
		checkoutResolves("repostate.8.3.next", "RS-8.3-4", twoChildren, "c2", "next", "c3"),
		branchTypeResolves("repostate.8.3.branch-type", "RS-8.3-12",
			with(branched(), checkin("b3", 6, of("b2"))), "feat", "b1"),
	}
}

// branchTypeResolves checks that a branch-type request for name yields the
// first check-in of its branch, labelled want, where an untyped request
// yields the newest. fossil(1) takes the type as test-name-to-id --type br;
// go-libfossil's ResolveVersion takes no type, so it cannot be asked.
func branchTypeResolves(id, req string, arts []rsArt, name, want string) rsAPICase {
	return rsAPICase{Case: Case{ID: id, Req: req}, arts: arts, run: func(e *rsEnv) error {
		if e.ref != nil {
			out, err := e.fossilRun("test-name-to-id", "--type", "br", name)
			if err != nil {
				return err
			}
			_, hash, _ := strings.Cut(strings.TrimSpace(out), " -> ")
			if got := labelOf(hash, e.script.labels); got != want {
				return expectf("fossil(1) resolves branch-type %q to %q; the case expects %q",
					name, got, want)
			}
		}
		untyped, err := e.goResolve(name)
		if err != nil {
			return err
		}
		return expectf("go-libfossil has no branch-type request: ResolveVersion(%q) takes "+
			"no type and gives %q, the newest check-in, where a branch-type request gives %q",
			name, untyped, want)
	}}
}

// checkoutResolves opens a checkout on the artifact labelled at, then
// checks that name resolves to the one labelled want. go-libfossil resolves
// names only through Repo.ResolveVersion, which knows of no checkout.
func checkoutResolves(id, req string, arts []rsArt, at, name, want string) rsAPICase {
	return rsAPICase{Case: Case{ID: id, Req: req}, arts: arts, run: func(e *rsEnv) error {
		if e.ref != nil {
			if err := e.fossilCheckout(at); err != nil {
				return err
			}
			got, err := e.fossilResolve(name)
			if err != nil {
				return err
			}
			if got != want {
				return expectf("fossil(1) on %s resolves %q to %q; the case expects %q",
					at, name, got, want)
			}
		}
		if _, err := e.goCheckout(at); err != nil {
			return err
		}
		got, err := e.goResolve(name)
		if err != nil {
			return err
		}
		if got != want {
			return expectf("with a checkout on %s, ResolveVersion(%q) = %q, want %q",
				at, name, got, want)
		}
		return nil
	}}
}

// cancelledBranch is c1, the root, and c2, which carries its own trunk tag;
// a control artifact then cancels c1's branch tag with the value feat. c1
// keeps a branch row whose tagtype is 0 and whose value is feat.
func cancelledBranch() []rsArt {
	return []rsArt{
		checkin("c1", 1, nil, rsRoot...),
		checkin("c2", 2, of("c1"), "*branch * trunk"),
		control("t", 9, "-branch {c1} feat"),
	}
}

// checkoutBranch opens a checkout on c1, whose only branch row is the
// cancelled feat. Read with the active-tag rule, the checkout is on trunk,
// the default branch, where c2 already follows c1: a commit would fork.
// Read without it, the checkout would be on feat, where c1 has no child.
func checkoutBranch(e *rsEnv) error {
	if e.ref != nil {
		if err := fossilBranchCurrent(e); err != nil {
			return err
		}
	}
	co, err := e.goCheckout("c1")
	if err != nil {
		return err
	}
	fork, err := co.WouldFork()
	if err != nil {
		return err
	}
	if !fork {
		return expectf("WouldFork on c1 = false: the checkout's branch was read as feat, " +
			"from c1's cancelled branch row; want trunk, where c2 follows c1")
	}
	return nil
}

// fossilBranchCurrent records where fossil(1) differs: `fossil branch
// current` reads the checkout's branch row without tagtype > 0
// (src/branch.c), the inconsistency Section 6.4 names, so it prints feat.
// The case fails once fossil prints trunk.
func fossilBranchCurrent(e *rsEnv) error {
	if err := e.fossilCheckout("c1"); err != nil {
		return err
	}
	out, err := e.fossilRun("branch", "current")
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(out); got != "feat" {
		return expectf("fossil branch current on c1 = %q, where fossil 2.28 prints feat; "+
			"if it now prints trunk, remove the note in fossilBranchCurrent", got)
	}
	return nil
}

// goCheckout opens a go-libfossil checkout on the artifact labelled at; the
// test closes it.
func (e *rsEnv) goCheckout(at string) (*gofossil.Checkout, error) {
	r, err := e.open()
	if err != nil {
		return nil, err
	}
	rid, err := e.goRid(r.Inner(), at)
	if err != nil {
		return nil, err
	}
	co, err := r.CreateCheckout(filepath.Join(e.t.TempDir(), "co"), gofossil.CheckoutCreateOpts{})
	if err != nil {
		return nil, err
	}
	e.t.Cleanup(func() { co.Close() })
	if err := co.Extract(int64(rid), gofossil.ExtractOpts{Force: true}); err != nil {
		return nil, err
	}
	return co, nil
}

// fossilCheckout moves fossil's checkout to the artifact labelled at.
func (e *rsEnv) fossilCheckout(at string) error {
	_, err := e.fossilRun("checkout", "--force", e.script.hashes[at])
	return err
}
