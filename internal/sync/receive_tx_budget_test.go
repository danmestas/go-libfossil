package sync

import (
	"fmt"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// receiveBudgetArtifacts is large enough that a per-artifact write is
// unmistakable against a per-round one, and small enough to stay a
// sub-second test.
const receiveBudgetArtifacts = 200

// Receiving artifacts must not cost a write outside a transaction per
// artifact. Each such write is its own BEGIN IMMEDIATE and fsync and its own
// independent contest for the write lock, so a pull that issues one per
// artifact spends minutes on a large repository and fails outright the moment
// anything else holds the lock (issue #200).
func TestPullDoesNotWritePerArtifactOutsideTransaction(t *testing.T) {
	server, client := newPrivateTestPair(t, "oix")
	for i := range receiveBudgetArtifacts {
		if _, _, err := blob.Store(server.DB(), []byte(fmt.Sprintf("budget-artifact-%d", i))); err != nil {
			t.Fatalf("blob.Store: %v", err)
		}
	}

	beforeExecs := client.DB().PoolExecs()
	beforeTxns := client.DB().WriteTxns()
	res := syncViaHandler(t, server, client, SyncOpts{Pull: true})
	got := client.DB().PoolExecs() - beforeExecs
	gotTxns := client.DB().WriteTxns() - beforeTxns

	if res.FilesRecvd < receiveBudgetArtifacts {
		t.Fatalf("received %d artifacts, want at least %d", res.FilesRecvd, receiveBudgetArtifacts)
	}
	// A handful of per-round bookkeeping statements are expected; anything
	// proportional to the artifact count is the defect.
	budget := int64(res.Rounds * 4)
	if got > budget {
		t.Fatalf("pull executed %d statements outside a transaction for %d artifacts over %d rounds; want <= %d (O(rounds), not O(artifacts))",
			got, res.FilesRecvd, res.Rounds, budget)
	}
	// PoolExecs alone does not pin the cost issue #200 measured: moving a
	// per-artifact write from the pool into its own transaction leaves this
	// counter unchanged while every artifact still pays a BEGIN IMMEDIATE and
	// an fsync, and still contests the write lock on its own. Count the
	// transactions too. The storing transaction per received artifact is
	// expected; what must not scale with artifacts is anything beyond it.
	txnBudget := int64(res.FilesRecvd) + int64(res.Rounds*4)
	if gotTxns > txnBudget {
		t.Fatalf("pull began %d write transactions for %d artifacts over %d rounds; want <= %d "+
			"(one store per artifact plus per-round bookkeeping, not one per side-effect)",
			gotTxns, res.FilesRecvd, res.Rounds, txnBudget)
	}
}

// A pull round must survive another connection holding the repository's write
// lock, waiting for it rather than failing outright.
//
// This is a guard, not a demonstration: it passes before the batching change
// as well as after, because batching changes how many times a round contests
// the write lock, not what happens when it loses a contest.
// TestPullDoesNotWritePerArtifactOutsideTransaction is what measures the
// change. What this pins is that contention stays survivable at all -- the
// property that makes any future external lock holder slow rather than fatal.
func TestPullSurvivesConcurrentWriteLockHolder(t *testing.T) {
	server, client := newPrivateTestPair(t, "oix")
	for i := range receiveBudgetArtifacts {
		if _, _, err := blob.Store(server.DB(), []byte(fmt.Sprintf("contended-artifact-%d", i))); err != nil {
			t.Fatalf("blob.Store: %v", err)
		}
	}

	// A second connection to the same file takes and releases the write lock
	// in short bursts for the duration of the pull, yielding between bursts.
	//
	// The yield is load-bearing. Without it the rival re-acquires immediately
	// on release, and on a loaded machine it can starve the pull's BEGIN past
	// busy_timeout -- which fails this test even when the batching under test
	// is perfect, because a single BEGIN is still a BEGIN. Occasional writes
	// from another process is the situation worth modelling; a rival that
	// never yields is testing SQLite's fairness, not our transaction budget.
	rival, err := db.Open(client.DB().Path())
	if err != nil {
		t.Fatalf("open rival connection: %v", err)
	}
	defer rival.Close()

	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = rival.WithTx(func(tx *db.Tx) error {
				if _, err := tx.Exec("UPDATE config SET value=value WHERE name='project-code'"); err != nil {
					return err
				}
				time.Sleep(time.Millisecond)
				return nil
			})
			time.Sleep(10 * time.Millisecond)
		}
	}()

	res := syncViaHandler(t, server, client, SyncOpts{Pull: true})
	close(stop)
	<-stopped

	if res.FilesRecvd < receiveBudgetArtifacts {
		t.Fatalf("received %d artifacts under write-lock contention, want at least %d",
			res.FilesRecvd, receiveBudgetArtifacts)
	}
}

// Folding the visibility write into the storing transaction must not change
// what it records: a private card still makes its artifact private, and an
// artifact arriving without one is public.
func TestPullRecordsVisibilityWhenBatched(t *testing.T) {
	server, client := newPrivateTestPair(t, "oix")

	publicUUIDs := make([]string, 3)
	for i := range publicUUIDs {
		_, uuid, err := blob.Store(server.DB(), []byte(fmt.Sprintf("batched-public-%d", i)))
		if err != nil {
			t.Fatalf("blob.Store: %v", err)
		}
		publicUUIDs[i] = uuid
	}
	privateUUIDs := make([]string, 3)
	for i := range privateUUIDs {
		rid, uuid, err := blob.Store(server.DB(), []byte(fmt.Sprintf("batched-private-%d", i)))
		if err != nil {
			t.Fatalf("blob.Store: %v", err)
		}
		if err := content.MakePrivate(server.DB(), int64(rid)); err != nil {
			t.Fatalf("MakePrivate: %v", err)
		}
		privateUUIDs[i] = uuid
	}

	syncViaHandler(t, server, client, SyncOpts{Pull: true, Private: true})

	for _, uuid := range publicUUIDs {
		rid, ok := blob.Exists(client.DB(), uuid)
		if !ok {
			t.Fatalf("client missing public artifact %s", uuid)
		}
		if content.IsPrivate(client.DB(), int64(rid)) {
			t.Fatalf("artifact %s arrived public but is recorded private", uuid)
		}
	}
	for _, uuid := range privateUUIDs {
		rid, ok := blob.Exists(client.DB(), uuid)
		if !ok {
			t.Fatalf("client missing private artifact %s", uuid)
		}
		if !content.IsPrivate(client.DB(), int64(rid)) {
			t.Fatalf("artifact %s arrived private but is recorded public", uuid)
		}
	}
}

// An igot card for an artifact the client already holds updates its visibility
// too, and that update is likewise a batched write rather than one per card.
func TestIGotVisibilityUpdatesAreBatched(t *testing.T) {
	server, client := newPrivateTestPair(t, "oix")

	uuids := make([]string, 50)
	for i := range uuids {
		data := []byte(fmt.Sprintf("igot-artifact-%d", i))
		if _, _, err := blob.Store(server.DB(), data); err != nil {
			t.Fatalf("blob.Store server: %v", err)
		}
		rid, uuid, err := blob.Store(client.DB(), data)
		if err != nil {
			t.Fatalf("blob.Store client: %v", err)
		}
		// Locally private: the server's public igot must clear that.
		if err := content.MakePrivate(client.DB(), int64(rid)); err != nil {
			t.Fatalf("MakePrivate: %v", err)
		}
		uuids[i] = uuid
	}

	before := client.DB().PoolExecs()
	beforeTxns := client.DB().WriteTxns()
	res := syncViaHandler(t, server, client, SyncOpts{Pull: true})
	got := client.DB().PoolExecs() - before
	gotTxns := client.DB().WriteTxns() - beforeTxns

	for _, uuid := range uuids {
		rid, ok := blob.Exists(client.DB(), uuid)
		if !ok {
			t.Fatalf("client lost artifact %s", uuid)
		}
		if content.IsPrivate(client.DB(), int64(rid)) {
			t.Fatalf("artifact %s stayed private after a public igot", uuid)
		}
	}
	budget := int64(res.Rounds * 4)
	if got > budget {
		t.Fatalf("pull executed %d statements outside a transaction for %d igot cards over %d rounds; want <= %d",
			got, len(uuids), res.Rounds, budget)
	}
	// PoolExecs alone does not pin this: folding the per-card writes into one
	// transaction each would leave it unchanged while restoring the real cost
	// issue #200 measured. Count transactions too.
	txnBudget := int64(res.Rounds * 4)
	if gotTxns > txnBudget {
		t.Fatalf("pull began %d write transactions for %d igot cards over %d rounds; want <= %d "+
			"(one batched visibility transaction per round, not one per card)",
			gotTxns, len(uuids), res.Rounds, txnBudget)
	}
}
