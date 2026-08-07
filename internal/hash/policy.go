package hash

import (
	"strconv"

	"github.com/danmestas/go-libfossil/db"
)

// Alg is one of the two algorithms Fossil names artifacts with.
type Alg int

const (
	AlgSHA1 Alg = iota
	AlgSHA3
)

// Hash returns a's name for content. nil content is the empty artifact.
func (a Alg) Hash(content []byte) string {
	if content == nil {
		content = []byte{}
	}
	if a == AlgSHA3 {
		return SHA3(content)
	}
	return SHA1(content)
}

// Other returns the algorithm a is not.
func (a Alg) Other() Alg {
	if a == AlgSHA3 {
		return AlgSHA1
	}
	return AlgSHA3
}

func (a Alg) String() string {
	if a == AlgSHA3 {
		return "sha3"
	}
	return "sha1"
}

// Fossil's hash-policy config values, in Fossil's own order (see
// `fossil help hash-policy`). Anything at or above policySHA3 names new
// artifacts with SHA3; policyAuto does so only once the repo holds a SHA3
// artifact.
const (
	policySHA1     = 0
	policyAuto     = 1
	policySHA3     = 2
	policySHA3Only = 3
	policyShunSHA1 = 4
)

// Naming is how one repo names the artifacts it is about to create.
type Naming struct {
	// New is the algorithm for a new artifact's name.
	New Alg
	// ReuseLegacy reports whether content already stored under the other
	// algorithm's name may keep that name instead of being stored again.
	// False under sha3-only and shun-sha1, which reject legacy names.
	ReuseLegacy bool
}

// NamingFor reads the hash-policy of the repo behind q.
//
// A repo with no hash-policy row — one predating the setting, or created by
// a tool that never wrote it — is read as "auto", so its own artifacts
// decide: SHA3 if it already holds a SHA3-named artifact, SHA1 otherwise.
// An empty repo with no policy therefore stays on SHA1.
//
// Read failures fall back the same way rather than being reported: a missing
// row is the ordinary case and cannot be told apart from a broken read here,
// and a genuinely broken database fails on the caller's very next query.
func NamingFor(q db.Querier) Naming {
	if q == nil {
		panic("hash.NamingFor: q must not be nil")
	}

	policy := policyAuto
	var raw string
	if err := q.QueryRow("SELECT value FROM config WHERE name='hash-policy'").Scan(&raw); err == nil {
		if n, err := strconv.Atoi(raw); err == nil {
			policy = n
		}
	}

	n := Naming{New: AlgSHA1, ReuseLegacy: policy < policySHA3Only}
	switch {
	case policy >= policySHA3:
		n.New = AlgSHA3
	case policy == policyAuto && hasSHA3Artifact(q):
		// Fossil promotes "auto" to "sha3" as soon as any SHA3 artifact
		// enters the repo; deriving that from the artifacts themselves
		// gives the same answer without rewriting the config row.
		n.New = AlgSHA3
	}
	return n
}

// hasSHA3Artifact reports whether the repo already holds an artifact named
// with SHA3. A false answer on a read error is the safe one: it keeps naming
// on SHA1, which every Fossil version accepts.
func hasSHA3Artifact(q db.Querier) bool {
	var one int
	err := q.QueryRow("SELECT 1 FROM blob WHERE length(uuid)=64 LIMIT 1").Scan(&one)
	return err == nil
}
