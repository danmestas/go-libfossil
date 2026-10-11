package auth

import (
	"strings"
	"time"
)

type User struct {
	UID     int
	Login   string
	Cap     string
	CExpire time.Time // zero value = no expiry
	Info    string
	MTime   time.Time
}

func HasCapability(caps string, required byte) bool {
	return strings.IndexByte(caps, required) >= 0
}

func CanPush(caps string) bool        { return HasCapability(caps, 'i') }
func CanPull(caps string) bool        { return HasCapability(caps, 'o') }
func CanClone(caps string) bool       { return HasCapability(caps, 'g') }
func CanPushUV(caps string) bool      { return HasCapability(caps, 'y') }
func CanSyncPrivate(caps string) bool { return HasCapability(caps, 'x') }

// IsAdmin reports fossil's Admin permission: a, or s (Setup), which implies
// it (login.c login_set_capabilities).
func IsAdmin(caps string) bool { return HasCapability(caps, 'a') || HasCapability(caps, 's') }

// CanRead reports fossil's Read permission, which i (Write) and Admin imply.
// It differs from CanPull, which accepts only o.
func CanRead(caps string) bool {
	return HasCapability(caps, 'o') || HasCapability(caps, 'i') || IsAdmin(caps)
}
