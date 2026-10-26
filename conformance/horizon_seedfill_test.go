package conformance

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/delta"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/seedcheck"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/tag"
	"github.com/danmestas/go-libfossil/internal/verify"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// A seed pushed as a delta whose base the origin lacks is judged when the
// base first arrives, by whatever path supplies it (horizon §5.2.3): here a
// pull from another peer, a local check-in and a local tag, beside the later
// push hzSeedLaterRequest covers. Each case leaves the seed refused by its
// hash in store-local state, never linked or served, and the transfer that
// supplied the base stored and linked as usual.

func horizonSeedFillCases() []Case {
	return []Case{horizonSeedFillPull, horizonSeedFillCheckin, horizonSeedFillTag}
}

var (
	horizonSeedFillPull    = Case{ID: "horizon.5.2.3.seed-base-pull", Req: "HZ-5.2-9"}
	horizonSeedFillCheckin = Case{ID: "horizon.5.2.3.seed-base-checkin", Req: "HZ-5.2-9"}
	horizonSeedFillTag     = Case{ID: "horizon.5.2.3.seed-base-tag", Req: "HZ-5.2-9"}
)

func TestHorizonSeedFill(t *testing.T) {
	check(t, horizonSeedFillPull, func(t *testing.T) error { return hzSeedFillPull(t) })
	check(t, horizonSeedFillCheckin, func(t *testing.T) error { return hzSeedFillCheckin(t) })
	check(t, horizonSeedFillTag, func(t *testing.T) error { return hzSeedFillTag(t) })
}

// hzProjectCode is r's project code.
func hzProjectCode(r *repo.Repo) (string, error) {
	var pc string
	err := r.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&pc)
	return pc, err
}

// hzPushed pushes cards to r through its handler and fails on any error
// card in the reply.
func hzPushed(r *repo.Repo, what string, cards ...xfer.Card) error {
	pc, err := hzProjectCode(r)
	if err != nil {
		return err
	}
	push := &xfer.PushCard{ServerCode: "x", ProjectCode: pc}
	req := &xfer.Message{Cards: append([]xfer.Card{push}, cards...)}
	resp, err := sync.HandleSync(context.Background(), r, req)
	if err != nil {
		return err
	}
	if errs := hzErrors(resp); len(errs) != 0 {
		return expectf("%s: errors %q", what, errs)
	}
	return nil
}

// hzDangling pushes the origin's own seed to x as a delta on baseName,
// which x lacks, and returns the seed's name.
func hzDangling(x *hzStruct, base []byte, baseName string) (string, error) {
	name, data, err := hzOwnSeed(x)
	if err != nil {
		return "", err
	}
	d, err := delta.Create(base, data)
	if err != nil {
		return "", err
	}
	card := &xfer.FileCard{UUID: name, DeltaSrc: baseName, Content: d}
	return name, hzPushed(x.s.f.r, "the dangling delta", card)
}

// hzSeedLinkRows counts the derived rows sourced from uuid: its tag rows and
// its event.
func hzSeedLinkRows(r *repo.Repo, uuid string) (int, error) {
	var n int
	err := r.DB().QueryRow(`SELECT (SELECT count(*) FROM tagxref JOIN blob ON blob.rid=tagxref.srcid
		WHERE blob.uuid=?1) + (SELECT count(*) FROM event JOIN blob ON blob.rid=event.objid
		WHERE blob.uuid=?1)`, uuid).Scan(&n)
	return n, err
}

// hzSeedInert checks that r judged the seed name and refused it: by its hash
// in store-local state and never in the shun table, never crosslinked, a
// rebuild's included, and neither sent nor announced to a pull that asks
// for it.
func hzSeedInert(r *repo.Repo, name string) error {
	if refused, err := seedcheck.IsRefusedUUID(r.DB(), name); err != nil || !refused {
		return expectf("the seed is not recorded as refused (%v)", err)
	}
	var shunned int
	err := r.DB().QueryRow("SELECT count(*) FROM shun").Scan(&shunned)
	if err != nil || shunned != 0 {
		return expectf("the refusal reached the shun table: %d rows (%v)", shunned, err)
	}
	if n, err := hzSeedLinkRows(r, name); err != nil || n != 0 {
		return expectf("the seed was crosslinked: %d rows (%v)", n, err)
	}
	if _, err := verify.Rebuild(r); err != nil {
		return err
	}
	if n, err := hzSeedLinkRows(r, name); err != nil || n != 0 {
		return expectf("a rebuild crosslinked the seed: %d rows (%v)", n, err)
	}
	pc, err := hzProjectCode(r)
	if err != nil {
		return err
	}
	pull := &xfer.Message{Cards: []xfer.Card{&xfer.PullCard{ServerCode: "x", ProjectCode: pc},
		&xfer.GimmeCard{UUID: name}}}
	served, err := sync.HandleSync(context.Background(), r, pull)
	if err != nil {
		return err
	}
	if _, sent := scSyncPayloads(served)[name]; sent {
		return expectf("the seed was sent")
	}
	if hzIGots(served)[name] {
		return expectf("the seed was announced")
	}
	return nil
}

// hzFileCheckin commits to r, on parent at when, a check-in of one file b
// holding text.
func hzFileCheckin(r *repo.Repo, parent libfossil.FslID, text []byte,
	when time.Time) (libfossil.FslID, error) {
	rid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "b", Content: text}},
		Comment: "b", User: "u", Parent: parent, Time: when})
	return rid, err
}

// hzSeedFillPull: the origin pulls the base from another peer, which holds
// it as a file of a check-in the origin lacks. The pull stores and links the
// check-in and its file; the seed, judged on the client path, is refused.
func hzSeedFillPull(t *testing.T) error {
	x := newHZStruct(t)
	r := x.s.f.r
	path := filepath.Join(t.TempDir(), "upstream.fossil")
	upstream, _, err := sync.Clone(context.Background(), path, x.peer(), sync.CloneOpts{})
	if err != nil {
		return err
	}
	t.Cleanup(func() { upstream.Close() })
	_, err = upstream.DB().Exec("UPDATE user SET cap='gio' WHERE login='nobody'")
	if err != nil {
		return err
	}
	c4, ok := blob.Exists(upstream.DB(), x.c4)
	if !ok {
		return expectf("the upstream does not hold c4")
	}
	base := []byte("a file the origin pulls, the seed's base\n")
	ci, err := hzFileCheckin(upstream, c4, base, x.s.f.when.Add(time.Hour))
	if err != nil {
		return err
	}
	baseName := hash.SHA1(base)
	if _, ok := blob.Exists(upstream.DB(), baseName); !ok {
		baseName = hash.SHA3(base)
	}
	name, err := hzDangling(x, base, baseName)
	if err != nil {
		return err
	}
	peer := &scSyncPeer{r: upstream}
	res, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true})
	if err != nil || len(res.Errors) != 0 {
		return expectf("the origin's pull: %v %q", err, res.Errors)
	}
	if !hzHeldReal(r, baseName) {
		return expectf("the pull did not store the base")
	}
	if n, err := hzSeedLinkRows(r, hzUUID(t, upstream, ci)); err != nil || n == 0 {
		return expectf("the pulled check-in was not crosslinked (%v)", err)
	}
	return hzSeedInert(r, name)
}

// hzSeedFillCheckin: a local check-in whose file is the base fills it, and
// judges the seed in its own transaction. The check-in is linked as usual.
func hzSeedFillCheckin(t *testing.T) error {
	x := newHZStruct(t)
	r := x.s.f.r
	base := []byte("a file the origin commits, the seed's base\n")
	name, err := hzDangling(x, base, hash.SHA1(base))
	if err != nil {
		return err
	}
	ci, err := hzFileCheckin(r, x.s.c4, base, x.s.f.when.Add(time.Hour))
	if err != nil {
		return err
	}
	if !hzHeldReal(r, hash.SHA1(base)) {
		return expectf("the check-in did not store the base")
	}
	if n, err := hzSeedLinkRows(r, hzUUID(t, r, ci)); err != nil || n == 0 {
		return expectf("the check-in was not crosslinked (%v)", err)
	}
	return hzSeedInert(r, name)
}

// hzSeedFillTag: a local tag whose control artifact is the base fills it.
// The same tag on an identical origin gives the control artifact's bytes
// ahead of time. The tag applies as usual; the seed is refused.
func hzSeedFillTag(t *testing.T) error {
	when := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	addTag := func(x *hzStruct) (libfossil.FslID, error) {
		return tag.AddTag(x.s.f.r, tag.TagOpts{TargetRID: x.s.c4, TagName: "release",
			TagType: tag.TagSingleton, User: "u", Time: when})
	}
	twin := newHZStruct(t)
	rid, err := addTag(twin)
	if err != nil {
		return err
	}
	base, err := content.Expand(twin.s.f.r.DB(), rid)
	if err != nil {
		return err
	}
	baseName := hzUUID(t, twin.s.f.r, rid)
	x := newHZStruct(t)
	r := x.s.f.r
	name, err := hzDangling(x, base, baseName)
	if err != nil {
		return err
	}
	rid, err = addTag(x)
	if err != nil {
		return err
	}
	if got := hzUUID(t, r, rid); got != baseName {
		return expectf("fixture: the tag is %.10s here and %.10s on the twin", got, baseName)
	}
	if n, err := hzSeedLinkRows(r, baseName); err != nil || n == 0 {
		return expectf("the tag did not apply (%v)", err)
	}
	return hzSeedInert(r, name)
}
