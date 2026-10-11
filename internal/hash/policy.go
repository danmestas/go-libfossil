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

// Naming is how one repo names the artifacts it is about to create.
type Naming struct {
	// New is the algorithm for a new artifact's name.
	New Alg
	// ReuseLegacy reports whether content already stored under the other
	// algorithm's name may keep that name instead of being stored again.
	// False under sha3-only and shun-sha1, which reject legacy names.
	ReuseLegacy bool
}

// sha1Len is the length of a SHA1 artifact name; a longer name is SHA3.
const sha1Len = 40

// NamingFor reads the hash-policy of the repo behind q.
//
// A repo with no hash-policy row — one predating the setting, or created by
// a tool that never wrote it — takes fossil's default (hname_default_policy,
// 7.2): sha3 if it holds a SHA3 artifact or no SHA1 one, auto otherwise.
// An empty repo with no policy therefore names new content with SHA3.
//
// Read failures fall back the same way rather than being reported: a missing
// row is the ordinary case and cannot be told apart from a broken read here,
// and a genuinely broken database fails on the caller's very next query.
func NamingFor(q db.Querier) Naming {
	if q == nil {
		panic("hash.NamingFor: q must not be nil")
	}

	policy, fromRow := policyFor(q)
	n := Naming{New: AlgSHA1, ReuseLegacy: policy < db.HashPolicySHA3Only}
	switch {
	case policy >= db.HashPolicySHA3:
		n.New = AlgSHA3
	case policy == db.HashPolicyAuto && fromRow && hasSHA3Artifact(q):
		// A repo promoted before the promotion was written down still
		// carries "auto" next to its SHA3 artifacts; they decide.
		n.New = AlgSHA3
	}
	return n
}

// effectivePolicy is the hash-policy in force: the stored row, or fossil's
// default when there is none.
func effectivePolicy(q db.Querier) int {
	policy, _ := policyFor(q)
	return policy
}

// policyFor is effectivePolicy that also reports whether a stored row
// decided it. A default that comes out auto has already established that no
// SHA3 artifact exists, so only a stored auto needs the scan again.
func policyFor(q db.Querier) (policy int, fromRow bool) {
	var raw string
	if err := q.QueryRow("SELECT value FROM config WHERE name='hash-policy'").Scan(&raw); err == nil {
		if n, err := strconv.Atoi(raw); err == nil {
			return n, true
		}
	}
	if hasSHA3Artifact(q) || !hasSHA1Artifact(q) {
		return db.HashPolicySHA3, false
	}
	return db.HashPolicyAuto, false
}

// PromoteFor is called before an artifact named uuid enters the repo. Under
// the auto policy a SHA3 name promotes the repo to sha3, and the promotion is
// written to the hash-policy setting (7.4) so fossil opening the repo sees
// it, as fossil's content_put_ex does. Any other policy, or a SHA1 name,
// changes nothing.
func PromoteFor(q db.Querier, uuid string) error {
	if q == nil {
		panic("hash.PromoteFor: q must not be nil")
	}
	if len(uuid) <= sha1Len || effectivePolicy(q) != db.HashPolicyAuto {
		return nil
	}
	_, err := q.Exec(
		"INSERT OR REPLACE INTO config(name, value, mtime) VALUES('hash-policy', ?, strftime('%s','now'))",
		strconv.Itoa(db.HashPolicySHA3),
	)
	return err
}

// hasSHA3Artifact reports whether the repo already holds an artifact named
// with SHA3. A false answer on a read error is the safe one: it keeps naming
// on SHA1, which every Fossil version accepts.
func hasSHA3Artifact(q db.Querier) bool {
	var one int
	err := q.QueryRow("SELECT 1 FROM blob WHERE length(uuid)>?", sha1Len).Scan(&one)
	return err == nil
}

// hasSHA1Artifact reports whether the repo holds an artifact named with SHA1.
func hasSHA1Artifact(q db.Querier) bool {
	var one int
	err := q.QueryRow("SELECT 1 FROM blob WHERE length(uuid)=?", sha1Len).Scan(&one)
	return err == nil
}
