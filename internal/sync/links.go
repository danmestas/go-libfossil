package sync

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// Share-links mode (§9.5) is fossil's undocumented --share-links: the
// client asks the server for the URLs of other copies of the repository
// and keeps each one as config link:URL = ARG. fossil's client_sync takes
// the flag only when pulling, so a push-only session ignores it here too.

// shareLinks reports whether the session runs in share-links mode.
func (s *session) shareLinks() bool {
	return s.opts.ShareLinks && s.opts.Pull
}

// sendsReqLinks reports whether request cycle carries pragma req-links.
// fossil sends it once, in the first request, and not when the client
// names another project's code (client_sync's zAltPCode): links describe
// the client's own repository.
func (s *session) sendsReqLinks(cycle int) bool {
	if cycle < 0 {
		panic("session.sendsReqLinks: negative cycle")
	}
	if cycle != 0 || !s.shareLinks() {
		return false
	}
	if s.opts.ProjectCode == "" {
		return true
	}
	own, _ := s.repo.Config("project-code")
	// fossil compares with sqlite3_stricmp (xfer.c), so case does not matter.
	return strings.EqualFold(s.opts.ProjectCode, own)
}

// handleLink keeps a pragma link URL ARG MTIME as config link:URL = ARG,
// as fossil's client_sync does: URL and ARG defossilized and otherwise
// verbatim, an existing row replaced only by a strictly newer MTIME. A
// link outside share-links mode, with the wrong token count, a URL that
// is not HTTP(S) or an MTIME that is not a positive decimal is dropped
// whole. ARG is opaque: it is stored, never parsed or sent on.
func (s *session) handleLink(c *xfer.PragmaCard) error {
	if c == nil || c.Name != "link" {
		panic("session.handleLink: c must be a link pragma")
	}
	if !s.shareLinks() || len(c.Values) != 3 {
		return nil
	}
	url, arg := deck.FossilDecode(c.Values[0]), deck.FossilDecode(c.Values[1])
	mtime, ok := linkMTime(c.Values[2])
	if !ok || !linkURLIsHTTP(url) {
		return nil
	}
	_, err := s.repo.DB().Exec(`INSERT INTO config(name, value, mtime) VALUES(?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET value=excluded.value, mtime=excluded.mtime
		WHERE mtime<excluded.mtime`, "link:"+url, arg, mtime)
	if err != nil {
		return fmt.Errorf("sync: store link: %w", err)
	}
	return nil
}

// linkMTime parses a link's MTIME, which Section 9.5 makes a positive
// decimal count of Unix seconds. fossil(1) reads it with strtoll base 0,
// so it also keeps 0x10 as 16 and 12z as 12; this follows the draft and
// drops both.
func linkMTime(field string) (int64, bool) {
	if field == "" || strings.Trim(field, "0123456789") != "" {
		return 0, false
	}
	mtime, err := strconv.ParseInt(field, 10, 64)
	return mtime, err == nil && mtime > 0
}

// linkURLIsHTTP reports whether url parses as HTTP(S) the way fossil's
// url_parse_local reads it: an exact lowercase http:// or https:// prefix
// and, when the authority names a port, a run of decimal digits followed
// by / or the end. fossil(1) fails fatally on a bad port, and on a form
// that is neither a URL nor a local repository; Section 9.5 discards the
// link instead, and so does this.
func linkURLIsHTTP(url string) bool {
	rest, ok := strings.CutPrefix(url, "http://")
	if !ok {
		rest, ok = strings.CutPrefix(url, "https://")
	}
	if !ok {
		return false
	}
	// With user information the host runs from the @ to a / or :, and
	// brackets mean nothing; without it an IPv6 literal's colons are
	// inside [ ].
	userinfo := false
	if at := strings.IndexAny(rest, "/@"); at >= 0 && rest[at] == '@' {
		rest, userinfo = rest[at+1:], true
	}
	i, inSquare := 0, false
	for ; i < len(rest) && rest[i] != '/'; i++ {
		if rest[i] == ':' && (userinfo || !inSquare) {
			break
		}
		if !userinfo && rest[i] == '[' {
			inSquare = true
		}
		if !userinfo && rest[i] == ']' {
			inSquare = false
		}
	}
	if i == len(rest) || rest[i] != ':' {
		return true
	}
	for i++; i < len(rest) && rest[i] >= '0' && rest[i] <= '9'; i++ {
	}
	return i == len(rest) || rest[i] == '/'
}
