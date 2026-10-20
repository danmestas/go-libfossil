package sync

import (
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/simio"
)

// CloneOpts configures a clone operation.
type CloneOpts struct {
	StoreCone *cone.Cone // nil is dense; local policy, never synchronized
	User      string     // Credentials for clone auth (also default admin user)
	Password  string
	Version   int            // Protocol version (default 3)
	Env       *simio.Env     // nil defaults to RealEnv
	Observer  Observer       // nil defaults to no-op
	Buggify   BuggifyChecker // fault injection for DST (nil = no faults)
	// PullPhantoms, when set, makes Clone fetch the artifacts the clone
	// rounds left missing with a follow-up pull session, and fail with a
	// *PhantomStallError if any are still missing after it. That pull
	// departs from Section 8.2 of the sync draft, which sends no clone,
	// pull or push card after clone_seqno 0, so it is off by default.
	PullPhantoms bool
}

// CloneResult reports what happened during a clone.
type CloneResult struct {
	Rounds          int
	BlobsRecvd      int
	ArtifactsLinked int // Manifests crosslinked into event table
	ProjectCode     string
	ServerCode      string
	Messages        []string // Informational messages from server
}
