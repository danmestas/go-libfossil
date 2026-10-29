//go:build tinygo

package sync

// dnsFault: TinyGo's net package has no DNSError, so no failure is classified
// as name resolution; the other network checks still apply.
func dnsFault(error) (transient, ok bool) { return false, false }
