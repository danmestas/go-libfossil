package sync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxRedirects is fossil's MAX_REDIRECTS (src/xfer.c). http_exchange
// decrements it on each redirect and fails when it reaches zero, so 19
// redirects are followed and the twentieth is fatal (draft §4).
const maxRedirects = 20

// ErrRedirectDowngrade reports a redirect from HTTPS to plain HTTP. Following
// it would resend the request, login card included, in cleartext.
var ErrRedirectDowngrade = errors.New("sync: cannot redirect from HTTPS to HTTP")

// ErrRedirectLimit reports a twentieth redirect.
var ErrRedirectLimit = errors.New("sync: redirect limit exceeded")

// PostXfer POSTs body to target and follows redirects as fossil's
// http_exchange does (src/http.c), not as net/http does. net/http turns a 301
// or 302 into a GET without the body, follows a downgrade from HTTPS to HTTP,
// and stops after 10. Fossil re-POSTs the same body for 301, 302, 307 and
// 308, strips a trailing /xfer from the new location, refuses a downgrade,
// and fails on the twentieth redirect.
//
// The client's own redirect policy is replaced for these requests only, so
// a caller's client is used as given but cannot loosen these rules. A redirect
// without a Location is returned as is, for the caller to report its status.
func PostXfer(ctx context.Context, client *http.Client, target, contentType string,
	body []byte) (*http.Response, error) {
	if client == nil {
		panic("sync.PostXfer: client must not be nil")
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	for left := maxRedirects; ; {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target,
			bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", contentType)
		resp, err := c.Do(req)
		if err != nil {
			return nil, err
		}
		loc := resp.Header.Get("Location")
		if !isFollowedRedirect(resp.StatusCode) || loc == "" {
			return resp, nil
		}
		// Drain a little so the connection can be reused, then let it go.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		left--
		if left == 0 {
			return nil, ErrRedirectLimit
		}
		if target, err = redirectTarget(target, loc); err != nil {
			return nil, err
		}
	}
}

func isFollowedRedirect(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// redirectTarget resolves loc against from and applies fossil's rules to it.
// Fossil strips one trailing /xfer, though draft §4 says "repeatedly": its
// loop truncates the string at the match, after which the substring it
// compares is four characters long and can never equal "/xfer" again.
func redirectTarget(from, loc string) (string, error) {
	base, err := url.Parse(from)
	if err != nil {
		return "", fmt.Errorf("sync: redirect from %q: %w", from, err)
	}
	next, err := base.Parse(loc)
	if err != nil {
		return "", fmt.Errorf("sync: malformed redirect %q: %w", loc, err)
	}
	if next.Scheme != "http" && next.Scheme != "https" {
		return "", fmt.Errorf("sync: cannot redirect to %q", loc)
	}
	if base.Scheme == "https" && next.Scheme == "http" {
		return "", ErrRedirectDowngrade
	}
	return strings.TrimSuffix(next.String(), "/xfer"), nil
}
