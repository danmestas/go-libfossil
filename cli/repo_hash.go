package cli

import (
	"fmt"
	"os"

	"github.com/danmestas/go-libfossil/internal/hash"
)

// RepoHashCmd hashes files using SHA1 or SHA3-256.
type RepoHashCmd struct {
	Files []string `arg:"" required:"" help:"Files to hash"`
	SHA3  bool     `name:"sha3" help:"Use SHA3-256 instead of SHA1"`
}

func (c *RepoHashCmd) Run(g *Globals) error {
	for _, path := range c.Files {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		var h string
		if c.SHA3 {
			h = hash.SHA3(data)
		} else {
			h = hash.SHA1(data)
		}
		fmt.Printf("%s  %s\n", h, path)
	}
	return nil
}
