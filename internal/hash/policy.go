package hash

import (
	"fmt"
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
//
// A stored value that is not one of the five policy ordinals is an error, not
// a reason to fall back: artifact-format-00 7.1 (AF-7.1-2) makes an
// unrecognized policy value fatal. Guessing a default would name new content
// with an algorithm the repo's owner never chose.
func NamingFor(q db.Querier) (Naming, error) {
	if q == nil {
		panic("hash.NamingFor: q must not be nil")
	}

	policy, fromRow, err := policyFor(q)
	if err != nil {
		return Naming{}, err
	}
	n := Naming{New: AlgSHA1, ReuseLegacy: policy < db.HashPolicySHA3Only}
	switch {
	case policy >= db.HashPolicySHA3:
		n.New = AlgSHA3
	case policy == db.HashPolicyAuto && fromRow && hasSHA3Artifact(q):
		// A repo promoted before the promotion was written down still
		// carries "auto" next to its SHA3 artifacts; they decide.
		n.New = AlgSHA3
	}
	return n, nil
}

// MustNamingFor is NamingFor for test fixtures that wrote the policy row
// themselves, so an unrecognized value there is a fixture bug and panics.
// Production code reads on-disk state and must use NamingFor.
func MustNamingFor(q db.Querier) Naming {
	n, err := NamingFor(q)
	if err != nil {
		panic(err)
	}
	return n
}

// effectivePolicy is the hash-policy in force: the stored row, or fossil's
// default when there is none.
func effectivePolicy(q db.Querier) (int, error) {
	policy, _, err := policyFor(q)
	return policy, err
}

// policyFor is effectivePolicy that also reports whether a stored row
// decided it. A default that comes out auto has already established that no
// SHA3 artifact exists, so only a stored auto needs the scan again.
//
// A stored value outside the ordinals 0 through 4 is an error (AF-7.1-2).
func policyFor(q db.Querier) (policy int, fromRow bool, err error) {
	var raw string
	if err := q.QueryRow("SELECT value FROM config WHERE name='hash-policy'").Scan(&raw); err == nil {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n < db.HashPolicySHA1 || n > db.HashPolicyShunSHA1 {
			return 0, true, fmt.Errorf("hash-policy %q is not a recognized policy value (0-4)", raw)
		}
		return n, true, nil
	}
	if hasSHA3Artifact(q) || !hasSHA1Artifact(q) {
		return db.HashPolicySHA3, false, nil
	}
	return db.HashPolicyAuto, false, nil
}

// policyNames are the policy value strings of artifact-format-00 Table 7,
// indexed by ordinal, as fossil's azPolicy (hname.c) lists them.
var policyNames = [...]string{
	db.HashPolicySHA1:     "sha1",
	db.HashPolicyAuto:     "auto",
	db.HashPolicySHA3:     "sha3",
	db.HashPolicySHA3Only: "sha3-only",
	db.HashPolicyShunSHA1: "shun-sha1",
}

// PolicyByName returns the ordinal of the policy named name. The match is
// case-sensitive and exact (artifact-format-00 7.1, AF-7.1-2), as fossil's
// hash-policy command compares with fossil_strcmp: "SHA3", " sha3" and
// "2" are all unrecognized, and an unrecognized name is an error.
func PolicyByName(name string) (int, error) {
	for ordinal, known := range policyNames {
		if name == known {
			return ordinal, nil
		}
	}
	return 0, fmt.Errorf("unknown hash policy %q: should be one of: "+
		"sha1 auto sha3 sha3-only shun-sha1", name)
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
	if len(uuid) <= sha1Len {
		return nil
	}
	policy, err := effectivePolicy(q)
	if err != nil {
		return err
	}
	if policy != db.HashPolicyAuto {
		return nil
	}
	_, err = q.Exec(
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
