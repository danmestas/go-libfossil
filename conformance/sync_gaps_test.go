package conformance

import "testing"

// syncGaps marks the sync cases go-libfossil is known to fail, by case ID,
// grouped by the code at fault.
var syncGaps = map[string]string{
	// The client sends an unversioned file named by SHA3 to a server older
	// than SHA3 support, where fossil sends the refusal in its place
	// (internal/sync/client.go buildUVFileCards).
	"sync.7.6.client-uv-gate": "#303",
	// The client always logs in in the body (internal/sync/client.go:196).
	"sync.6.5.cookie-login-after-gate": "#304",
	// A login is checked against the stored password only as a shared
	// secret, with no fallback for one stored in clear
	// (internal/auth/verify.go:87).
	"sync.6.4.cleartext-fallback": "#304",
	// The server has no case for a pushed config card
	// (internal/sync/handler.go:522).
	"sync.9.3.config-needs-admin": "#305",
	// Pragmas the server records or ignores: ci-unlock is not handled
	// (internal/sync/handler.go:427); req-clusters is recorded (:438) but
	// sendAllClusters (:778) is never called.
	"sync.9.4.ci-unlock":        "#305",
	"sync.5.3.cluster-brackets": "#305",
	// A clone reply carries no unversioned catalog
	// (internal/sync/handler.go:475).
	"sync.8.4.clone-uv-catalog": "#306",
	// The client's unversioned sync: a received uvfile of status 2 is
	// dropped, not applied as an mtime (internal/sync/client.go:1136),
	// and tombstones are put in the send table (internal/sync/session.go:152).
	"sync.7.3.status2-mtime-client":       "#308",
	"sync.8.5.1.seed-excludes-tombstones": "#308",
	// The server's unversioned sync: a uvgimme for an absent name is
	// answered with a tombstone (:102); a uvfile needs a push card (:134);
	// one of status 2, or with its content omitted, changes nothing
	// (:170, :178).
	"sync.7.3.uvgimme-absent":  "#308",
	"sync.7.3.status2-mtime":   "#308",
	"sync.7.3.omitted-content": "#308",
	// A received unversioned file is stored under its content's hash by
	// the repository's policy, not the HASH it came with
	// (internal/uv/uv.go:57).
	// The read gates treat only o, i and Admin as Read, where fossil's
	// trunk and the s7.3 erratum let Clone imply it
	// (internal/auth/auth.go:42).
	"sync.7.3.uvgimme-clone-implies-read": "#312",
	"sync.7.3.received-hash":              "#308",
	// A clone refusal is appended to the reply, with no reset and no push
	// card before it (internal/sync/handler.go:474-480), and a SEQNO of -1
	// restarts the clone at rid 1 (:501-505).
	"sync.8.2.refusal-shape":       "#306",
	"sync.10.1.reset-before-error": "#306",
	"sync.8.1.negative-seqno":      "#306",
	// Clone replies: each cfile is recompressed from the artifact rather
	// than sent as stored, with no LF after a payload that does not end in
	// one (internal/xfer/encode.go:284-310), and the reply is the
	// compressed application/x-fossil (internal/sync/serve_http.go:150).
	"sync.7.2.clone-verbatim":     "#306",
	"sync.5.1.cfile-padding":      "#306",
	"sync.4.1.clone-uncompressed": "#306",
}

// syncDeviations marks the cases where go-libfossil differs from the draft
// on purpose, by case ID, with the reason.
var syncDeviations = map[string]string{
	"sync.5.2.client-clone-seqno": "the server tolerates a client-sent clone_seqno, " +
		"which no conforming client sends, rather than answering bad command (#255)",
	"sync.8.2.clone-zero-phantom": "Clone fetches the phantoms a clone leaves with a " +
		"follow-up pull, so that a clone it reports done is complete (#255)",
}

// TestSyncGapKeys checks that every marked case exists.
func TestSyncGapKeys(t *testing.T) {
	ids := map[string]bool{}
	for _, c := range syncSuiteCases() {
		ids[c.ID] = true
	}
	for id := range syncGaps {
		if !ids[id] {
			t.Errorf("syncGaps names %s, which is not a case", id)
		}
	}
	for id := range syncDeviations {
		if !ids[id] {
			t.Errorf("syncDeviations names %s, which is not a case", id)
		}
	}
}
