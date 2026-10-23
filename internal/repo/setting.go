package repo

import (
	"fmt"
	"net/url"
	"runtime"
	"strings"
)

// SettingBoolean is fossil's db_get_boolean on a stored value: is_truth
// ("on", "yes", "true" in any case, or a nonzero atoi) gives true, is_false
// ("off", "no", "false", "0" in any case) gives false, and anything else
// gives dflt.
func SettingBoolean(value string, dflt bool) bool {
	switch strings.ToLower(value) {
	case "on", "yes", "true":
		return true
	case "off", "no", "false", "0":
		return false
	}
	if cAtoi(value) != 0 {
		return true
	}
	return dflt
}

// cAtoi is C's atoi as fossil's is_truth uses it: leading white space, an
// optional sign, then decimal digits up to the first other byte; no digits
// give 0. Overflow is not modelled, since only zero versus nonzero matters.
func cAtoi(s string) int64 {
	i := 0
	for i < len(s) && strings.IndexByte(" \t\n\v\f\r", s[i]) >= 0 {
		i++
	}
	negative := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		negative = s[i] == '-'
		i++
	}
	var n int64
	for ; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		if n < 1<<40 {
			n = n*10 + int64(s[i]-'0')
		}
	}
	if negative {
		return -n
	}
	return n
}

// FilenamesCaseSensitive reports whether file names that differ only in case
// are distinct, as fossil's filenames_are_case_sensitive does: the
// repository's case-sensitive setting, else fossil's platform default, which
// is off on macOS and Windows and on elsewhere. Fossil falls back to a global
// setting in ~/.fossil that go-libfossil does not read.
func (r *Repo) FilenamesCaseSensitive() bool {
	dflt := runtime.GOOS != "darwin" && runtime.GOOS != "windows"
	value, err := r.Config("case-sensitive")
	if err != nil {
		return dflt
	}
	return SettingBoolean(value, dflt)
}

// SettingHorizonHistoryURL names the repository setting an origin appends
// to pragma horizon-served as the interface, outside the sync protocol, at
// which history below a horizon can be queried (draft-fossil-horizon-00
// §6.2.5, §5.5). Unset or empty, an origin names none.
const SettingHorizonHistoryURL = "horizon-history-url"

// MaxHistoryURLLen bounds a history URL, which every horizon reply carries.
const MaxHistoryURLLen = 2048

// CheckHistoryURL reports why s cannot be a horizon-served URL. It rides
// the pragma as one card token, so it holds no white space or control
// byte; it names an http or https interface with a host, and no
// credentials, since every horizon client is sent it.
func CheckHistoryURL(s string) error {
	if s == "" || len(s) > MaxHistoryURLLen {
		return fmt.Errorf("history URL must be 1 to %d bytes, not %d", MaxHistoryURLLen, len(s))
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] == 0x7f {
			return fmt.Errorf("history URL %q must be a single token: byte %d is white space or a control byte", s, i)
		}
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("history URL %q: %w", s, err)
	}
	if scheme := strings.ToLower(u.Scheme); scheme != "http" && scheme != "https" {
		return fmt.Errorf("history URL %q must be http or https", s)
	}
	if u.Host == "" {
		return fmt.Errorf("history URL %q names no host", s)
	}
	if u.User != nil {
		return fmt.Errorf("history URL %q must not carry credentials", s)
	}
	return nil
}
