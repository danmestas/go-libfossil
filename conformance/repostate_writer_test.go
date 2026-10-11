package conformance

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	gofossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/internal/branch"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/merge"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/tag"
	"github.com/danmestas/go-libfossil/simio"
)

// The cases that write: committing, branching and tagging through
// go-libfossil's APIs, then checking what was stored.

// rsAt is second s of the scripted minute, for APIs that take a time.
func rsAt(s int) time.Time { return time.Date(2026, 1, 1, 0, 0, s, 0, time.UTC) }

// firstCommit commits one file to a new repository: its first check-in.
func firstCommit(e *rsEnv) (*repo.Repo, libfossil.FslID, error) {
	r, err := repo.Create(filepath.Join(e.t.TempDir(), "w.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		return nil, 0, err
	}
	e.t.Cleanup(func() { r.Close() })
	rid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("a\n")}},
		Comment: "c1", User: "u", Time: rsAt(1),
	})
	return r, rid, err
}

// tagRows is rid's active tags as "name value".
func tagRows(r *repo.Repo, rid libfossil.FslID) (string, error) {
	got, err := queryNormalized(r.DB(),
		fmt.Sprintf("SELECT tagname, tagtype, coalesce(value, '')"+
			" FROM tagxref JOIN tag USING (tagid)"+
			" WHERE rid = %d", rid), nil)
	return strings.Join(got, ", "), err
}

// storedDeck parses the artifact stored as rid.
func storedDeck(r *repo.Repo, rid libfossil.FslID) (*deck.Deck, []byte, error) {
	data, err := content.Expand(r.DB(), rid)
	if err != nil {
		return nil, nil, err
	}
	d, err := deck.Parse(data)
	return d, data, err
}

// hasCard reports whether d carries the T-card typ name * value.
func hasCard(d *deck.Deck, typ deck.TagType, name, value string) bool {
	for _, card := range d.T {
		if card.Type == typ && card.Name == name && card.UUID == "*" && card.Value == value {
			return true
		}
	}
	return false
}

// newBranch commits c1 and branches feat from it, returning the new
// branch's root as parsed from storage.
func newBranch(e *rsEnv) (*deck.Deck, error) {
	r, c1, err := firstCommit(e)
	if err != nil {
		return nil, err
	}
	opts := branch.CreateOpts{Name: "feat", Parent: c1, User: "u", Time: rsAt(2)}
	rid, _, err := branch.Create(r, opts)
	if err != nil {
		return nil, err
	}
	d, data, err := storedDeck(r, rid)
	if err != nil {
		return nil, err
	}
	if e.ref != nil {
		if accepted, reason, err := e.ref.parseArtifact(data); err != nil || !accepted {
			return nil, expectf("fossil(1) rejects the branch root (%v): %s", err, reason)
		}
	}
	return d, nil
}

func branchAPICases() []rsAPICase {
	closedB1 := with(branched(), control("t", 9, "+closed {b1}"))
	return []rsAPICase{{
		Case: Case{ID: "repostate.6.1.initial-sym", Req: "RS-6.1-3"},
		run: func(e *rsEnv) error {
			r, c1, err := firstCommit(e)
			if err != nil {
				return err
			}
			got, err := tagRows(r, c1)
			if want := "branch 2 trunk, sym-trunk 2 "; got != want || err != nil {
				return expectf("first check-in's tags = %q (%v), want %q", got, err, want)
			}
			return nil
		},
	}, {
		Case: Case{ID: "repostate.6.2.branch-cards", Req: "RS-6.2-1"},
		run: func(e *rsEnv) error {
			d, err := newBranch(e)
			if err != nil {
				return err
			}
			if !hasCard(d, deck.TagPropagating, "branch", "feat") ||
				!hasCard(d, deck.TagPropagating, "sym-feat", "") {
				return expectf("branch root T-cards %+v lack *branch * feat or *sym-feat *", d.T)
			}
			return nil
		},
	}, {
		Case: Case{ID: "repostate.6.2.cancel-syms", Req: "RS-6.2-2"},
		run: func(e *rsEnv) error {
			d, err := newBranch(e)
			if err != nil {
				return err
			}
			if !hasCard(d, deck.TagCancel, "sym-trunk", "") {
				return expectf("branch root T-cards %+v lack -sym-trunk *", d.T)
			}
			return nil
		},
	}, {
		Case: Case{ID: "repostate.6.2.new-branch-duplicate", Req: "RS-6.2-3"},
		run:  duplicateBranch,
	}, {
		Case: Case{ID: "repostate.6.3.branch-listing-closed", Req: "RS-6.3-2"},
		arts: closedB1,
		run:  branchListedOpen,
	}, {
		// Section 6.3 gives each branch its check-in count and the newest
		// check-in. fossil(1) prints neither, so the case follows the text.
		Case: Case{ID: "repostate.6.3.branch-listing", Req: "RS-6.3-1"},
		arts: branched(),
		run:  branchListing,
	}}
}

// duplicateBranch creates feat twice while it is open: the second must fail
// and store nothing. fossil(1) refuses the same on a copy of the repository
// as it stands after the first.
func duplicateBranch(e *rsEnv) error {
	r, c1, err := firstCommit(e)
	if err != nil {
		return err
	}
	opts := branch.CreateOpts{Name: "feat", Parent: c1, User: "u", Time: rsAt(2)}
	if _, _, err := branch.Create(r, opts); err != nil {
		return err
	}
	if err := r.Close(); err != nil {
		return err
	}
	if e.ref != nil {
		if err := fossilRefusesBranch(e, r.Path(), "feat"); err != nil {
			return err
		}
	}
	r, err = repo.Open(r.Path())
	if err != nil {
		return err
	}
	defer r.Close()
	var before, after int
	r.DB().QueryRow("SELECT count(*) FROM blob").Scan(&before)
	opts.Time = rsAt(3)
	_, _, createErr := branch.Create(r, opts)
	r.DB().QueryRow("SELECT count(*) FROM blob").Scan(&after)
	if createErr == nil || after != before {
		return expectf("second Create of open branch feat: err=%v, blobs %d -> %d; want an error "+
			"and no new blob", createErr, before, after)
	}
	return nil
}

// fossilRefusesBranch runs `fossil branch new` on a copy of the repository
// at path.
func fossilRefusesBranch(e *rsEnv, path, name string) error {
	dir := filepath.Join(e.t.TempDir(), "ck")
	copied := dir + ".fossil"
	if err := os.Mkdir(dir, 0o755); err != nil {
		return err
	}
	if err := copyFile(path, copied); err != nil {
		return err
	}
	if _, err := e.ref.run(dir, "open", copied); err != nil {
		return err
	}
	out, err := e.ref.run(dir, "branch", "new", name, "trunk")
	if err == nil || !strings.Contains(out, "already exists") {
		return expectf("fossil branch new %s on an open %s: %q (%v); the case expects a refusal",
			name, name, out, err)
	}
	return nil
}

// branchListedOpen closes b1, which is not feat's newest check-in: the
// listing reports feat open, as `fossil branch ls -c` does.
func branchListedOpen(e *rsEnv) error {
	if e.ref != nil {
		out, err := e.fossilRun("branch", "ls", "-c")
		if err != nil {
			return err
		}
		if strings.Contains(out, "feat") {
			return expectf("fossil branch ls -c lists feat: %q; the case expects it open", out)
		}
	}
	list, err := goBranches(e)
	if err != nil {
		return err
	}
	if feat, ok := list["feat"]; !ok || feat.IsClosed {
		return expectf("branch.List feat = %+v (listed %v), want open", feat, ok)
	}
	return nil
}

func branchListing(e *rsEnv) error {
	list, err := goBranches(e)
	if err != nil {
		return err
	}
	want := map[string]string{"feat": "2 b2", "trunk": "2 c2"}
	for name, w := range want {
		b := list[name]
		got := fmt.Sprintf("%d %s", b.CheckinCount, labelOf(b.LatestUUID, e.script.labels))
		if got != w {
			return expectf("branch.List %s: count and newest %q, want %q", name, got, w)
		}
	}
	return nil
}

func goBranches(e *rsEnv) (map[string]branch.Branch, error) {
	r, err := e.open()
	if err != nil {
		return nil, err
	}
	list, err := branch.List(r.Inner())
	if err != nil {
		return nil, err
	}
	byName := map[string]branch.Branch{}
	for _, b := range list {
		byName[b.Name] = b
	}
	return byName, nil
}

func writerAPICases() []rsAPICase {
	return []rsAPICase{
		{Case: Case{ID: "repostate.2.sym-prefix", Req: "RS-2-1"}, run: symPrefix},
		{Case: Case{ID: "repostate.2.reserved-prefix", Req: "RS-2-2"}, run: reservedPrefix},
		{Case: Case{ID: "repostate.3.2.first-parent-walk", Req: "RS-3.2-3"}, arts: merged(),
			run: firstParentWalk},
		{Case: Case{ID: "repostate.3.2.merge-ancestry", Req: "RS-3.2-4"}, arts: merged(),
			run: mergeAncestry},
		{Case: Case{ID: "repostate.4.6.tag-artifact", Req: "RS-4.6-1"}, arts: chain(),
			run: tagArtifact},
		{Case: Case{ID: "repostate.4.2.relink-unchanged", Req: "RS-4.2-7"}, run: relinkUnchanged},
	}
}

// symPrefix tags c1 release: the row must be sym-release, so release
// resolves to c1.
func symPrefix(e *rsEnv) error {
	return userTag(e, "release", func(r *gofossil.Repo, c1 libfossil.FslID, err error) error {
		got, _ := tagRows(r.Inner(), c1)
		resolved, resolveErr := r.ResolveVersion("release")
		if err != nil || !strings.Contains(got, "sym-release") || resolved != int64(c1) {
			return expectf("Tag release: %v; c1's tags %q; release resolves to %d (%v), "+
				"want sym-release and rid %d", err, got, resolved, resolveErr, c1)
		}
		return nil
	})
}

// reservedPrefix tags c1 wiki-x, a name only wiki pages may carry.
func reservedPrefix(e *rsEnv) error {
	return userTag(e, "wiki-x", func(_ *gofossil.Repo, _ libfossil.FslID, err error) error {
		if err == nil {
			return expectf("Tag wiki-x succeeded, want a refusal")
		}
		return nil
	})
}

// userTag commits c1, tags it name through Repo.Tag and passes the result on.
func userTag(
	e *rsEnv, name string, judge func(*gofossil.Repo, libfossil.FslID, error) error,
) error {
	inner, c1, err := firstCommit(e)
	if err != nil {
		return err
	}
	path := inner.Path()
	if err := inner.Close(); err != nil {
		return err
	}
	r, err := gofossil.Open(path)
	if err != nil {
		return err
	}
	defer r.Close()
	_, tagErr := r.Tag(gofossil.TagOpts{Name: name, TargetID: int64(c1), User: "u", Time: rsAt(2)})
	return judge(r, c1, tagErr)
}

// firstParentWalk walks m's ancestry: c2, its primary parent, then c1, never
// b1, the merge parent.
func firstParentWalk(e *rsEnv) error {
	r, err := e.open()
	if err != nil {
		return err
	}
	m, err := e.goRid(r.Inner(), "m")
	if err != nil {
		return err
	}
	entries, err := r.Ancestry(gofossil.LogOpts{Start: int64(m)})
	if err != nil {
		return err
	}
	var got []string
	for _, entry := range entries {
		got = append(got, labelOf(entry.UUID, e.script.labels))
	}
	if strings.Join(got, " ") != "m c2 c1" {
		return expectf("Ancestry(m) = %v, want [m c2 c1]", got)
	}
	return nil
}

// mergeAncestry finds the merge base of m and b1, which is b1: reached only
// through the merge link. fossil's merge-base agrees.
func mergeAncestry(e *rsEnv) error {
	if e.ref != nil {
		out, err := e.fossilRun("merge-base", e.script.hashes["m"], e.script.hashes["b1"])
		if err != nil {
			return err
		}
		pivot := strings.TrimPrefix(strings.TrimSpace(out), "pivot=")
		if got := labelOf(pivot, e.script.labels); got != "b1" {
			return expectf("fossil merge-base m b1 = %q; the case expects b1", got)
		}
	}
	r, err := e.open()
	if err != nil {
		return err
	}
	m, err := e.goRid(r.Inner(), "m")
	if err != nil {
		return err
	}
	b1, err := e.goRid(r.Inner(), "b1")
	if err != nil {
		return err
	}
	base, err := merge.FindCommonAncestor(r.Inner(), m, b1)
	if err != nil {
		return err
	}
	if got := e.labelOfRid(r.Inner(), int64(base)); got != "b1" {
		return expectf("FindCommonAncestor(m, b1) = %s, want b1", got)
	}
	return nil
}

// tagArtifact tags c2 through tag.AddTag. What it stores must be a control
// artifact with one T-card, and the rows it derives must be those fossil(1)
// derives from loading that artifact.
func tagArtifact(e *rsEnv) error {
	r, err := repo.Open(e.goPath)
	if err != nil {
		return err
	}
	defer r.Close()
	c2, err := e.goRid(r, "c2")
	if err != nil {
		return err
	}
	rid, err := tag.AddTag(r, tag.TagOpts{TargetRID: c2, TagName: "foo",
		TagType: tag.TagPropagating, Value: "v", User: "u", Time: rsAt(9)})
	if err != nil {
		return err
	}
	d, data, err := storedDeck(r, rid)
	if err != nil || d.Type != deck.Control || len(d.T) != 1 {
		return expectf("tag.AddTag stored %q (%v), want a control artifact with one T-card",
			data, err)
	}
	var uuid string
	if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid = ?", rid).Scan(&uuid); err != nil {
		return err
	}
	return e.sameTagRows(data, uuid)
}

// sameTagRows loads the tag artifact data after the script into fossil(1)
// and compares the foo rows each side derives.
func (e *rsEnv) sameTagRows(data []byte, uuid string) error {
	if e.ref == nil {
		return nil
	}
	arts := with(chain(), rsArt{label: "tag", body: string(data), file: true})
	script := rsBuild(arts)
	if script.hashes["tag"] != uuid {
		return fmt.Errorf("tag artifact hashes to %s, stored as %s", script.hashes["tag"], uuid)
	}
	loaded, err := e.ref.loadScript(script, nil)
	if err != nil {
		return err
	}
	tables, tags := []string{"tagxref"}, []string{"foo"}
	want, err := readState(loaded.path, script.labels, tables, tags)
	if err != nil {
		return err
	}
	got, err := readState(e.goPath, script.labels, tables, tags)
	if err != nil {
		return err
	}
	if d := diffState(want, got, "fossil(1)", "tag.AddTag"); d != "" {
		return expectf("%s", d)
	}
	return nil
}

// relinkUnchanged sweeps every scripted case into go-libfossil, then
// crosslinks every artifact a second time over the rows the first left: it
// deletes only what the crosslink takes as an artifact's "already linked"
// mark, its event row and the tag rows it sourced (crosslink.go:385-386),
// and sweeps again. Every table must come out as it was. A case whose
// order matters, which edits its tables, or which is a known gap is left
// out: a gap's failure has a cause of its own.
func relinkUnchanged(e *rsEnv) error {
	var problems []string
	for _, c := range repostateCases() {
		if c.each || c.doctor != nil || repostateGaps[c.ID] != "" {
			continue
		}
		if d, err := relinked(e, rsBuild(c.arts)); err != nil || d != "" {
			problems = append(problems, fmt.Sprintf("%s: %s%v", c.ID, d, err))
		}
	}
	if len(problems) > 0 {
		return expectf("%s", strings.Join(problems, " | "))
	}
	return nil
}

// relinked is how the tables differ after a second crosslink of every
// artifact, or "" when they do not.
func relinked(e *rsEnv, script rsScript) (string, error) {
	path, err := goLoad(e.t, script, loadSweep, nil)
	if err != nil {
		return "", err
	}
	first, err := readState(path, script.labels, rsAllTables, nil)
	if err != nil {
		return "", err
	}
	r, err := repo.Open(path)
	if err != nil {
		return "", err
	}
	linked, err := relink(r)
	if closeErr := r.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if linked.got != linked.want {
		return fmt.Sprintf("second sweep linked %d artifacts, want %d",
			linked.got, linked.want), nil
	}
	again, err := readState(path, script.labels, rsAllTables, nil)
	if err != nil {
		return "", err
	}
	return diffState(first, again, "first crosslink", "second crosslink"), nil
}

// relinkCount is how many artifacts a second sweep linked, and how many
// had been linked.
type relinkCount struct{ got, want int }

func relink(r *repo.Repo) (relinkCount, error) {
	var count relinkCount
	err := r.DB().QueryRow("SELECT count(*) FROM (SELECT objid FROM event" +
		" UNION SELECT srcid FROM tagxref WHERE srcid > 0)").Scan(&count.want)
	if err != nil {
		return count, err
	}
	for _, clear := range []string{"DELETE FROM event", "DELETE FROM tagxref WHERE srcid > 0"} {
		if _, err := r.DB().Exec(clear); err != nil {
			return count, err
		}
	}
	count.got, err = manifest.Crosslink(r)
	return count, err
}
