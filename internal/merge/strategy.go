package merge

import libfossil "github.com/danmestas/go-libfossil/internal/fsltype"

// Strategy merges three versions of content.
type Strategy interface {
	Name() string
	Merge(base, local, remote []byte) (*Result, error)
}

// Result of a merge operation.
type Result struct {
	Content   []byte     // merged output (may contain conflict markers)
	Conflicts []Conflict // unresolved conflict regions
	Clean     bool       // true if no conflicts
}

// Conflict describes one conflicting region.
type Conflict struct {
	StartLine int
	EndLine   int
	Local     []byte
	Remote    []byte
	Base      []byte
}

// Fork represents two divergent checkins sharing a common ancestor.
type Fork struct {
	Ancestor  libfossil.FslID
	LocalTip  libfossil.FslID
	RemoteTip libfossil.FslID
}

// StrategyByName returns a strategy implementation by name.
func StrategyByName(name string) (Strategy, bool) {
	s, ok := strategies[name]
	return s, ok
}

var strategies = map[string]Strategy{}

// Register adds a strategy to the registry.
func Register(s Strategy) {
	strategies[s.Name()] = s
}
