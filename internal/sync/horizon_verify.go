package sync

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/horizon/seed"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/tag"
)

// ErrNoSeedToVerify is returned by VerifySeed for a store that is not a
// structural horizon holding a seed.
var ErrNoSeedToVerify = errors.New("horizon: the store holds no seed to verify")

// SeedReport is what VerifySeed found: how many of the seed's T cards it
// checked, and every tag at an entry where the seed and the derivation over
// real artifacts disagree.
type SeedReport struct {
	Checked    int
	Mismatches []SeedMismatch
}

// SeedMismatch is one tag at one entry of K on which the seed and the
// derivation disagree. Seeded and Derived spell the tag as a T card does,
// its sign then its value ("*red", "+"), and are "" where it is absent.
type SeedMismatch struct {
	Checkin, Tag, Seeded, Derived string
}

// VerifySeed is the verification mode of draft-fossil-horizon-00 §5.9:
// trusting the seed becomes a choice with a known price. It deepens the
// store to the content depth at the same C through peer, which fetches every
// manifest below C and so the real tag artifacts and primary-parent chains
// the seed summarized, and discards the seed (§5.8). The store, now deriving
// from real artifacts alone, is compared with what the seed asserted at each
// entry. The price is every manifest below C, and the store stays a
// content-horizon store, which holds no seed; a transfer that fails puts the
// structural cone and the seed back.
func VerifySeed(ctx context.Context, r *repo.Repo, peer Transport, opts SyncOpts) (SeedReport, error) {
	if ctx == nil || r == nil || peer == nil {
		panic("sync.VerifySeed: nil context, repo or peer")
	}
	sc, err := content.StoreCone(r.DB())
	if err != nil {
		return SeedReport{}, err
	}
	h, ok := horizonOf(sc)
	held, err := seed.Held(r.DB())
	if err != nil {
		return SeedReport{}, err
	}
	if !ok || h.Depth() != cone.DepthStructural || held == nil {
		return SeedReport{}, ErrNoSeedToVerify
	}
	d, err := deck.Parse(held)
	if err != nil {
		return SeedReport{}, fmt.Errorf("horizon: the held seed: %w", err)
	}
	// The binding card asserts no tag state (§5.2.1), so there is nothing
	// to compare it with.
	seeded := map[string]map[string]string{}
	checked := 0
	for _, t := range d.T {
		if t.Name == seed.BindingTag {
			continue
		}
		checked++
		if seeded[t.UUID] == nil {
			seeded[t.UUID] = map[string]string{}
		}
		seeded[t.UUID][t.Name] = string(t.Type) + t.Value
	}
	if _, err := FetchCone(ctx, r, peer, withDepth(sc, cone.DepthContent), opts); err != nil {
		return SeedReport{}, fmt.Errorf("horizon: verifying the seed: %w", err)
	}
	report := SeedReport{Checked: checked}
	for entry, cards := range seeded {
		derived, err := derivedTags(r.DB(), entry)
		if err != nil {
			return SeedReport{}, err
		}
		for name := range derived {
			if _, ok := cards[name]; !ok {
				cards[name] = ""
			}
		}
		for name, want := range cards {
			if got := derived[name]; got != want {
				report.Mismatches = append(report.Mismatches, SeedMismatch{Checkin: entry, Tag: name, Seeded: want, Derived: got})
			}
		}
	}
	slices.SortFunc(report.Mismatches, func(a, b SeedMismatch) int {
		return cmp.Or(cmp.Compare(a.Checkin, b.Checkin), cmp.Compare(a.Tag, b.Tag))
	})
	return report, nil
}

// derivedTags is the tag state the store derives at the check-in named uuid,
// each tag spelled as a T card's sign and value.
func derivedTags(q db.Querier, uuid string) (map[string]string, error) {
	rows, err := q.Query(`SELECT tag.tagname, tagxref.tagtype, coalesce(tagxref.value, '')
		FROM tagxref JOIN tag USING(tagid) JOIN blob ON blob.rid=tagxref.rid
		WHERE blob.uuid=? AND tagxref.tagtype>0`, uuid)
	if err != nil {
		return nil, fmt.Errorf("horizon: verifying the seed: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, value string
		var typ int
		if err := rows.Scan(&name, &typ, &value); err != nil {
			return nil, fmt.Errorf("horizon: verifying the seed: %w", err)
		}
		sign := deck.TagSingleton
		if typ == tag.TagPropagating {
			sign = deck.TagPropagating
		}
		out[name] = string(sign) + value
	}
	return out, rows.Err()
}
