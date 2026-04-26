package libfossil

import (
	"errors"
	"fmt"
)

type RC int

const (
	RCOK               RC = 0
	RCError            RC = 100
	RCNYI              RC = 101
	RCOOM              RC = 102
	RCMisuse           RC = 103
	RCRange            RC = 104
	RCAccess           RC = 105
	RCIO               RC = 106
	RCNotFound         RC = 107
	RCAlreadyExists    RC = 108
	RCConsistency      RC = 109
	RCRepoNeedsRebuild RC = 110
	RCNotARepo         RC = 111
	RCRepoVersion      RC = 112
	RCDB               RC = 113
	RCBreak            RC = 114
	RCStepRow          RC = 115
	RCStepDone         RC = 116
	RCStepError        RC = 117
	RCType             RC = 118
	RCNotACkout        RC = 119
	RCRepoMismatch     RC = 120
	RCChecksumMismatch RC = 121
	RCLocked           RC = 122
	RCConflict         RC = 123
	RCSizeMismatch     RC = 124
	RCPhantom          RC = 125
	RCUnsupported      RC = 126
)

var rcNames = map[RC]string{
	RCOK: "ok", RCError: "error", RCNYI: "nyi", RCOOM: "oom",
	RCMisuse: "misuse", RCRange: "range", RCAccess: "access", RCIO: "io",
	RCNotFound: "not_found", RCAlreadyExists: "already_exists",
	RCConsistency: "consistency", RCRepoNeedsRebuild: "repo_needs_rebuild",
	RCNotARepo: "not_a_repo", RCRepoVersion: "repo_version", RCDB: "db",
	RCBreak: "break", RCStepRow: "step_row", RCStepDone: "step_done",
	RCStepError: "step_error", RCType: "type", RCNotACkout: "not_a_ckout",
	RCRepoMismatch: "repo_mismatch", RCChecksumMismatch: "checksum_mismatch",
	RCLocked: "locked", RCConflict: "conflict", RCSizeMismatch: "size_mismatch",
	RCPhantom: "phantom", RCUnsupported: "unsupported",
}

func (rc RC) String() string {
	if s, ok := rcNames[rc]; ok {
		return s
	}
	return fmt.Sprintf("rc_%d", int(rc))
}

type FslError struct {
	Code  RC
	Msg   string
	Cause error
}

func (e *FslError) Error() string {
	return fmt.Sprintf("fossil(%s): %s", e.Code, e.Msg)
}

func (e *FslError) Unwrap() error {
	return e.Cause
}

// ErrVersionNotFound is returned by ResolveVersion when the requested version
// string does not match any artifact in the repository. Callers can match
// with errors.Is.
var ErrVersionNotFound = errors.New("libfossil: version not found")

// ErrAmbiguousVersion is returned by ResolveVersion when a UUID prefix
// matches more than one artifact (collision). Callers can match with
// errors.Is.
var ErrAmbiguousVersion = errors.New("libfossil: ambiguous version prefix")
