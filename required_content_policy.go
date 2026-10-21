package libfossil

import (
	"fmt"
	"sort"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
)

// Explicit widening is lasting store intent, including when the subsequent
// fetch fails. Rebase only our policy field, never accept a fresh stale snapshot.
func (c *Checkout) requiredStorePolicy(next *Cone, state string) (string, error) {
	if err := c.checkRequiredState(state); err != nil {
		return "", err
	}
	lines := strings.Split(state, "\n")
	if len(lines) < 2 {
		panic("libfossil.requiredStorePolicy: incomplete state")
	}
	err := c.inner.Repo().WithTx(func(tx *db.Tx) error {
		stored, err := content.StoreCone(tx)
		if err != nil {
			return err
		}
		digest := ""
		if stored != nil {
			digest = stored.Digest()
		}
		if digest != lines[1] {
			return fmt.Errorf("libfossil: store policy changed before widening")
		}
		widened, err := requiredStoreUnion(wrapCone(stored), next)
		if err != nil {
			return err
		}
		if err := content.SetStoreCone(tx, unwrapCone(widened)); err != nil {
			return err
		}
		lines[1] = ""
		if widened != nil {
			lines[1] = widened.Digest()
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	state = strings.Join(lines, "\n")
	return state, c.checkRequiredState(state)
}

// Coverage changes only at member boundaries. Taking OR there preserves both
// cones, including reinclusions, without letting one cone's cuts narrow another.
func requiredStoreUnion(left, right *Cone) (*Cone, error) {
	if left == nil || right == nil {
		return nil, nil
	}
	members := make(map[string]bool)
	for _, c := range []*Cone{left, right} {
		for _, name := range append(c.Include(), c.Exclude()...) {
			members[name] = true
		}
	}
	paths := make([]string, 0, len(members))
	for name := range members {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	var include, exclude []string
	decisions := make(map[string]bool)
	for _, name := range paths {
		covered := left.Covers(name) || right.Covers(name)
		parentCovered, parentLength := false, 0
		for parent, decision := range decisions {
			if len(parent) > parentLength && strings.HasPrefix(name, parent+"/") {
				parentCovered, parentLength = decision, len(parent)
			}
		}
		if covered == parentCovered {
			continue
		}
		decisions[name] = covered
		if covered {
			include = append(include, name)
		} else {
			exclude = append(exclude, name)
		}
	}
	return NewCone(include, exclude)
}
