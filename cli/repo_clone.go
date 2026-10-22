package cli

import (
	"context"
	"fmt"
	"net/url"
	"time"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/internal/auth"
)

// RepoCloneCmd clones a remote Fossil repository.
type RepoCloneCmd struct {
	URL     string        `arg:"" optional:"" help:"Remote Fossil server URL"`
	Path    string        `arg:"" optional:"" help:"Local path for new repository file"`
	User    string        `short:"u" help:"Username for clone auth"`
	Pass    string        `short:"p" help:"Password for clone auth"`
	Invite  string        `help:"Invite token (from fossil invite)"`
	Timeout time.Duration `default:"10m" help:"Abort the clone if it runs longer than this; 0 disables the deadline"`
	Horizon string        `help:"Clone a content-horizon store: elide blobs below this check-in, given as its full 40 or 64 digit hash"`
	Leaves  bool          `help:"With --horizon, also hold every open leaf's ancestry back to the check-in"`
	Cone    []string      `name:"cone" help:"Clone a sparse store: fetch only this path's subtree (repeatable)"`
	ConeNot []string      `name:"cone-not" help:"Carve this path out of a --cone (repeatable)"`
}

// cloneContext builds the context that bounds a clone. A positive timeout caps
// the whole operation; zero (or negative) disables the deadline for a very
// large repository, returning a plain cancellable context so the caller still
// gets uniform cleanup through the returned cancel.
func cloneContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(parent, timeout)
	}
	return context.WithCancel(parent)
}

func (c *RepoCloneCmd) Run(g *Globals) error {
	if c.Invite != "" {
		token, err := auth.DecodeInviteToken(c.Invite)
		if err != nil {
			return fmt.Errorf("invalid invite token: %w", err)
		}
		if c.URL == "" {
			c.URL = token.URL
		}
		if c.User == "" {
			c.User = token.Login
		}
		if c.Pass == "" {
			c.Pass = token.Password
		}
	}
	if c.URL == "" {
		return fmt.Errorf("URL required (provide as argument or via --invite token)")
	}
	if c.Path == "" {
		return fmt.Errorf("path required")
	}

	storeCone, err := cloneConeFlags(c.Cone, c.ConeNot, c.Horizon, c.Leaves)
	if err != nil {
		return err
	}

	ctx, cancel := cloneContext(context.Background(), c.Timeout)
	defer cancel()

	transport := libfossil.NewHTTPTransport(c.URL)
	opts := libfossil.CloneOpts{
		User:      c.User,
		Password:  c.Pass,
		StoreCone: storeCone,
	}

	r, result, err := libfossil.Clone(ctx, c.Path, transport, opts)
	if err != nil {
		return fmt.Errorf("clone failed: %w", err)
	}
	defer r.Close()
	// fossil clone remembers its source as last-sync-url, the default peer
	// for later syncs, widening and required-content recovery.
	if err := r.SetConfig("last-sync-url", rememberedSyncURL(c.URL)); err != nil {
		return fmt.Errorf("clone: remember %s: %w", c.URL, err)
	}

	fmt.Printf("Cloned into %s\n", c.Path)
	fmt.Printf("  Rounds:       %d\n", result.Rounds)
	fmt.Printf("  Blobs:        %d\n", result.BlobsRecvd)
	fmt.Printf("  Project-code: %s\n", result.ProjectCode)
	if storeCone != nil {
		fmt.Printf("  Elided:       %d\n", result.Elided)
	}
	return nil
}

// rememberedSyncURL is url as fossil stores last-sync-url: a user name stays,
// a password never does. An unparsable URL is stored as given, since the
// clone already reached it.
func rememberedSyncURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User(u.User.Username())
	return u.String()
}
