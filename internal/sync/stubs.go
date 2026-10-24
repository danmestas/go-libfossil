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
	// SeedKey, when set, is the origin's public key: an SSH public key line
	// or an OpenPGP fingerprint. The clone keeps it as its horizon-seed-key
	// setting and verifies every structural seed against it
	// (draft-fossil-horizon-00 §5.2.4); empty accepts an unsigned seed.
	SeedKey string
	// Origin names the peer the clone addresses, keying the horizon cursor
	// a converged horizon clone records (draft-fossil-horizon-00 §6.2.6) as
	// SyncOpts.Origin keys a sync's. Empty keeps none.
	Origin string
	// HorizonName, when set, names StoreCone's horizon check-in for the
	// origin to resolve (draft-fossil-horizon-00 §6.2.7): tip, a symbolic
	// name or a full hash, stepped back HorizonDepth check-ins along primary
	// parents. StoreCone must carry a horizon; its rule, level and path
	// members stand, and its check-in is replaced by the resolved one, the
	// only one the store records. An origin that does not resolve it serves
	// densely, and the clone resolves it over what it received at the
	// content horizon (§6.3).
	HorizonName  string
	HorizonDepth uint32
}

// CloneResult reports what happened during a clone.
type CloneResult struct {
	Elided        int // artifacts deliberately absent under the store cone
	ConeServed    int // replies acknowledging the store cone (sparse-cone §6.3); the rest were filtered locally
	HorizonServed int // replies serving the store's horizon at either level (horizon §6.2.5); the rest were classified locally
	// StructuralServed counts the replies that served the structural level,
	// and HorizonFallback reports a structural clone that kept the content
	// horizon instead: its origin did not serve the structural level with a
	// seed in the first reply, or the seed failed its provenance (§5.1,
	// §5.2.4, §6.3).
	StructuralServed int
	HorizonFallback  bool
	Rounds           int
	BlobsRecvd       int
	ArtifactsLinked  int // Manifests crosslinked into event table
	ProjectCode      string
	ServerCode       string
	Messages         []string // Informational messages from server
	// HorizonHistoryURL is the interface outside the protocol at which the
	// origin says history below a horizon can be queried (horizon-served
	// URL, §6.2.5), the last a reply named; "" when none did.
	HorizonHistoryURL string
	// HorizonResolved is the check-in a clone's HorizonName resolved to and
	// HorizonSteps the steps back it took, fewer than HorizonDepth when
	// history ran out; HorizonResolvedLocally reports the clone resolved it
	// itself, its origin having answered no horizon-resolved (§6.3).
	HorizonResolved        string
	HorizonSteps           uint32
	HorizonResolvedLocally bool
}
