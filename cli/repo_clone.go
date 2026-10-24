package cli

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/internal/auth"
)

// RepoCloneCmd clones a remote Fossil repository.
type RepoCloneCmd struct {
	URL        string        `arg:"" optional:"" help:"Remote Fossil server URL"`
	Path       string        `arg:"" optional:"" help:"Local path for new repository file"`
	User       string        `short:"u" help:"Username for clone auth"`
	Pass       string        `short:"p" help:"Password for clone auth"`
	Invite     string        `help:"Invite token (from fossil invite)"`
	Timeout    time.Duration `default:"10m" help:"Abort the clone if it runs longer than this; 0 disables the deadline"`
	Horizon    string        `help:"Clone a horizon store: elide blobs below this check-in, given as tip, a branch or tag, or its full 40 or 64 digit hash; the origin resolves a name, or the clone does over what an origin that cannot sends it; what each rule would hold needs a local repository ('repo cone cost') or a peer that exposes it out of band, as the origin's horizon-served URL may"`
	Depth      int           `help:"With --horizon, step the check-in back this many check-ins along primary parents, stopping at a root"`
	Leaves     bool          `help:"With --horizon, also hold every open leaf's ancestry back to the check-in, for merging across live branches offline; costly: on a repository with old unmerged branches that is nearly the whole history"`
	Structural bool          `help:"With --horizon, also elide the manifests below the check-in, deriving their branch and tag state from the origin's seed"`
	Cone       []string      `name:"cone" help:"Clone a sparse store: fetch only this path's subtree (repeatable)"`
	ConeNot    []string      `name:"cone-not" help:"Carve this path out of a --cone (repeatable)"`
	SeedKey    string        `name:"seed-key" help:"The origin's public key, an SSH public key line or an OpenPGP fingerprint: with --structural, a horizon seed not signed by it is discarded and the clone falls back to the content horizon; kept as the horizon-seed-key setting"`
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

	checkin, name, err := horizonNameFlags(c.Horizon, c.Depth)
	if err != nil {
		return err
	}
	storeCone, err := cloneConeFlags(c.Cone, c.ConeNot, checkin, c.Leaves, c.Structural)
	if err != nil {
		return err
	}

	if c.Leaves {
		warnLeavesCost()
	}

	ctx, cancel := cloneContext(context.Background(), c.Timeout)
	defer cancel()

	transport := libfossil.NewHTTPTransport(c.URL)
	opts := libfossil.CloneOpts{
		User:         c.User,
		Password:     c.Pass,
		StoreCone:    storeCone,
		SeedKey:      c.SeedKey,
		HorizonName:  name,
		HorizonDepth: c.Depth,
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
	if name != "" {
		reportResolved(name, c.Depth, result)
	}
	if c.Structural {
		// Horizon §6.2.5: a client reports which mode a transfer used.
		if result.HorizonFallback {
			fmt.Println("  Horizon:      content (the origin did not serve the structural horizon)")
		} else {
			fmt.Println("  Horizon:      structural")
		}
	}
	if result.HorizonHistoryURL != "" {
		fmt.Printf("  History:      %s\n", result.HorizonHistoryURL)
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

// namedStandIn is the check-in a named horizon's store cone carries until
// the name resolves (CloneOpts.HorizonName); the store never records it.
const namedStandIn = "0000000000000000000000000000000000000000"

// horizonNameFlags splits --horizon and --depth into the check-in the store
// cone is built at and the name the origin is asked to resolve: a full hash
// at depth 0 is sent as the hash itself, which every horizon origin reads,
// and anything else as a name (draft-fossil-horizon-00 §6.2.7).
func horizonNameFlags(horizon string, depth int) (checkin, name string, err error) {
	if depth < 0 {
		return "", "", fmt.Errorf("--depth must not be negative")
	}
	if horizon == "" {
		if depth > 0 {
			return "", "", fmt.Errorf("--depth needs --horizon")
		}
		return "", "", nil
	}
	if depth == 0 && isFullHash(horizon) {
		return horizon, "", nil
	}
	return namedStandIn, horizon, nil
}

// isFullHash reports a 40 or 64 digit hex string.
func isFullHash(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	return strings.Trim(strings.ToLower(s), "0123456789abcdef") == ""
}

// reportResolved says which check-in a named horizon resolved to, who
// resolved it, and when history ran out before the depth asked for.
func reportResolved(name string, depth int, result *libfossil.CloneResult) {
	by := "origin"
	if result.HorizonResolvedLocally {
		by = "this clone; the origin does not resolve horizon names"
	}
	fmt.Printf("  Horizon at:   %s (%s, resolved by %s)\n", result.HorizonResolved, name, by)
	if result.HorizonSteps < depth {
		fmt.Printf("  History ran out after %d of %d steps back\n", result.HorizonSteps, depth)
	}
}
