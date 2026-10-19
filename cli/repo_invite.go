package cli

import (
	"fmt"
	"time"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/internal/auth"
)

// RepoInviteCmd generates an invite token for a new user.
type RepoInviteCmd struct {
	Login string        `arg:"" help:"Username for the invitee"`
	Cap   string        `help:"Capability string (e.g. oi)" required:""`
	URL   string        `help:"Sync URL to embed in token" default:""`
	TTL   time.Duration `help:"Token time-to-live (e.g. 24h)" default:"0"`
}

func (c *RepoInviteCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	password, err := generatePassword()
	if err != nil {
		return err
	}

	if err := r.CreateUser(libfossil.UserOpts{
		Login:    c.Login,
		Password: password,
		Caps:     c.Cap,
	}); err != nil {
		return err
	}

	// The expiry is kept in go-libfossil's own config row, not user.cexpire,
	// which is fossil's web-login cookie expiry (#441).
	expiry := time.Now().Add(c.TTL)
	if c.TTL > 0 {
		if err := auth.SetInviteExpiry(r.Inner().DB(), c.Login, expiry); err != nil {
			return err
		}
	}

	url := c.URL
	if url == "" {
		val, err := r.Config("last-sync-url")
		if err == nil {
			url = val
		}
	}

	tok := auth.InviteToken{
		URL:      url,
		Login:    c.Login,
		Password: password,
		Caps:     c.Cap,
	}

	encoded := tok.Encode()

	fmt.Printf("Invite for %q (capabilities: %s", c.Login, c.Cap)
	if c.TTL > 0 {
		fmt.Printf(", expires: %s", expiry.UTC().Format(time.RFC3339))
	}
	fmt.Println("):")
	fmt.Println()
	fmt.Printf("  fossil repo clone --invite %s\n", encoded)
	fmt.Println()
	fmt.Println("Share this command with the recipient. It contains credentials - treat it like a password.")
	return nil
}
