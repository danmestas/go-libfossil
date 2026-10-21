package cli

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	libfossil "github.com/danmestas/go-libfossil"
)

// RecoveryFlags selects only the peer and credentials used to obtain content a
// local operation requires. It does not enable ordinary pull, push or autosync.
type RecoveryFlags struct {
	Remote      string        `help:"Required-content peer URL (default: saved last-sync-url)"`
	SyncUser    string        `help:"Required-content authentication user"`
	SyncPass    string        `help:"Required-content password (never saved)"`
	SyncPrivate bool          `help:"Allow recovery of private required artifacts"`
	Timeout     time.Duration `default:"10m" help:"Required-content deadline; 0 disables it"`
}

func (f RecoveryFlags) requiredContent(r *libfossil.Repo) (libfossil.RequiredContentOpts, error) {
	opts := libfossil.RequiredContentOpts{
		Sync: libfossil.SyncOpts{User: f.SyncUser, Password: f.SyncPass, Private: f.SyncPrivate},
	}
	remote := f.Remote
	if remote == "" {
		var err error
		remote, err = r.Config("last-sync-url")
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return opts, err
		}
	}
	if remote == "" {
		return opts, nil
	}
	u, err := url.Parse(remote)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return opts, fmt.Errorf("required-content peer must be an http:// or https:// URL")
	}
	if u.User != nil {
		user := u.User.Username()
		if opts.Sync.User == "" {
			opts.Sync.User = user
		}
		// As zit's saved-remote resolver does, use only user@ from a URL;
		// plaintext credentials must be supplied explicitly for this run.
		u.User = url.User(user)
	}
	opts.Peer = libfossil.NewHTTPTransport(u.String())
	return opts, nil
}
