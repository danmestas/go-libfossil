package conformance

import "testing"

// syncGaps marks the sync cases go-libfossil is known to fail, by case ID,
// grouped by the code at fault.
var syncGaps = map[string]string{
	// The server records a client-sent error card and reads on, where fossil
	// answers it with "bad command" (internal/sync/handler.go handleDataCard
	// has no ErrorCard case).
	"sync.5.2.client-error-card": "#300",
	// The client records an error card and reads on, so it does not end the
	// reply-card loop (internal/sync/client.go processResponse).
	"sync.10.1.error-prefix": "#300",
	// Termination is not Section 3.7's: a phantom is asked for until it
	// has aged three rounds (internal/sync/client.go:605-613), and any
	// file received continues the session (:620).
	"sync.3.7.phantom-no-progress": "#301",
	"sync.3.7.content-alone-stops": "#301",
	// A gimme the client cannot answer stays in pendingSend
	// (internal/sync/client.go:537, :332-336), so the session never ends.
	"sync.3.3.gimme-absent-client": "#301",
	// An igot's phantom lives only in the session's map, never in the
	// phantom table (internal/sync/client.go:520-527).
	"sync.3.3.igot-phantom-persists": "#301",
	// The phantom scan does not leave out shunned names
	// (internal/sync/client.go:381).
	"sync.3.3.gimme-skips-shunned": "#302",
	"sync.7.7.shunned-basis":       "#302",
	// Received artifacts are stored without a shun check: by the server
	// (internal/sync/handler.go:625) and by the client
	// (internal/sync/client.go:714).
	"sync.7.7.shunned-file-server": "#302",
	"sync.7.7.shunned-file-client": "#302",
	"sync.7.7.shunned-cfile":       "#302",
	// The server answers no client-version with a server-version
	// (internal/sync/handler.go:458).
	"sync.3.2.server-version-before-clone": "#303",
	"sync.9.1.server-version-full":         "#303",
	// No SHA3 peer-version gate where the server sends content: gimme
	// (internal/sync/handler.go:590), clone (:1070), uvgimme
	// (internal/sync/handler_uv.go:93).
	"sync.7.6.server-gate":            "#303",
	"sync.7.6.server-gate-no-version": "#303",
	"sync.7.6.clone-gate":             "#303",
	"sync.7.6.uvgimme-gate":           "#303",
	"sync.10.1.sha3-gate-appends":     "#303",
	// The client keeps no peer version from server-version
	// (internal/sync/client.go:550) and has no SHA3 gate where it sends
	// content (:357).
	"sync.7.6.client-gate":          "#303",
	"sync.9.1.client-records-short": "#303",
	// The client always logs in in the body (internal/sync/client.go:196),
	// and the server reads no login cookie (internal/sync/serve_http.go:71).
	"sync.6.5.cookie-login-after-gate": "#304",
	"sync.6.5.server-cookie-login":     "#304",
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
	// A message card is only recorded; "pull only" has no effect
	// (internal/sync/client.go:547).
	"sync.10.2.pull-only":       "#300",
	"sync.10.2.pull-only-gimme": "#300",
	// pragma send-private goes after the operation cards in every request
	// (internal/sync/client.go:120).
	"sync.3.2.send-private-first": "#307",
	// A delta's missing basis becomes a public phantom whatever the card's
	// private marker (internal/sync/client.go:762).
	"sync.7.4.private-basis-phantom": "#307",
	// After the first clone round a server error aborts the clone, where
	// the draft ends the reply-card loop and continues
	// (internal/sync/clone.go:544).
	"sync.8.3.later-error": "#301",
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
	"sync.7.3.no-push-needed":  "#296",
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
	// Push and pull cards are accepted whatever their project code
	// (internal/sync/handler.go:459-474).
	"sync.8.5.project-mismatch-pull": "#309",
	"sync.8.5.project-mismatch-push": "#309",
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
	// The server refuses application/x-fossil-debug with HTTP 415
	// (internal/sync/serve_http.go:101).
	"sync.4.debug-type-dispatches": "#310",
	// Any 2xx status is taken as a sync reply (transport.go:94).
	"sync.4.status-201": "#310",
	// A reply's media type selects compressed framing only when it is
	// application/x-fossil; a missing type and a foreign one are both read
	// as uncompressed cards (internal/xfer/message.go:160-167).
	"sync.4.missing-content-type": "#310",
	"sync.4.content-type-foreign": "#310",
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
