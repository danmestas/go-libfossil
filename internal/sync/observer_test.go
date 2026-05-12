package sync

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/simio"
	"github.com/danmestas/libfossil/internal/xfer"
)

// recordingObserver records all lifecycle calls for test assertions.
type recordingObserver struct {
	started         int
	roundsStarted   []int
	roundsCompleted []int
	roundStats      []RoundStats
	completed       int
	lastInfo        SessionStart
	lastEnd         SessionEnd
	lastErr         error
	errors          []error
	handleStarted   int
	handleCompleted int
}

func (r *recordingObserver) Started(ctx context.Context, info SessionStart) context.Context {
	r.started++
	r.lastInfo = info
	return ctx
}

func (r *recordingObserver) RoundStarted(ctx context.Context, round int) context.Context {
	r.roundsStarted = append(r.roundsStarted, round)
	return ctx
}

func (r *recordingObserver) RoundCompleted(ctx context.Context, round int, stats RoundStats) {
	r.roundsCompleted = append(r.roundsCompleted, round)
	r.roundStats = append(r.roundStats, stats)
}

func (r *recordingObserver) Completed(ctx context.Context, info SessionEnd, err error) {
	r.completed++
	r.lastEnd = info
	r.lastErr = err
}

func (r *recordingObserver) Error(_ context.Context, err error) {
	r.errors = append(r.errors, err)
}

func (r *recordingObserver) HandleStarted(ctx context.Context, _ HandleStart) context.Context {
	r.handleStarted++
	return ctx
}

func (r *recordingObserver) HandleCompleted(_ context.Context, _ HandleEnd) {
	r.handleCompleted++
}

func (r *recordingObserver) TableSyncStarted(_ context.Context, _ TableSyncStart) {}

func (r *recordingObserver) TableSyncCompleted(_ context.Context, _ TableSyncEnd) {}

func TestNopObserverDoesNotPanic(t *testing.T) {
	var obs nopObserver
	ctx := context.Background()
	ctx = obs.Started(ctx, SessionStart{Operation: "sync"})
	ctx = obs.RoundStarted(ctx, 0)
	obs.RoundCompleted(ctx, 0, RoundStats{FilesSent: 5, FilesReceived: 3})
	obs.Completed(ctx, SessionEnd{Operation: "sync", Rounds: 1}, nil)
	obs.Error(ctx, nil)
	ctx = obs.HandleStarted(ctx, HandleStart{Operation: "sync"})
	obs.HandleCompleted(ctx, HandleEnd{})
	obs.TableSyncStarted(ctx, TableSyncStart{Table: "test", LocalRows: 10})
	obs.TableSyncCompleted(ctx, TableSyncEnd{Table: "test", Sent: 5, Received: 3})
}

func TestResolveObserverNil(t *testing.T) {
	obs := resolveObserver(nil)
	if obs == nil {
		t.Fatal("resolveObserver(nil) should return nopObserver, not nil")
	}
	// Should not panic
	ctx := obs.Started(context.Background(), SessionStart{})
	obs.RoundStarted(ctx, 0)
	obs.RoundCompleted(ctx, 0, RoundStats{})
	obs.Error(ctx, nil)
	ctx = obs.HandleStarted(ctx, HandleStart{})
	obs.HandleCompleted(ctx, HandleEnd{})
	obs.TableSyncStarted(ctx, TableSyncStart{})
	obs.TableSyncCompleted(ctx, TableSyncEnd{})
}

func TestSyncCallsObserverHooks(t *testing.T) {
	clientPath := filepath.Join(t.TempDir(), "client.fossil")
	serverPath := filepath.Join(t.TempDir(), "server.fossil")
	env := simio.RealEnv()

	server, err := repo.Create(serverPath, "test", env.Rand, "")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	client, err := repo.Create(clientPath, "test", env.Rand, "")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var projCode, srvCode string
	client.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&projCode)
	client.DB().QueryRow("SELECT value FROM config WHERE name='server-code'").Scan(&srvCode)

	mt := &MockTransport{
		Handler: func(req *xfer.Message) *xfer.Message {
			resp, _ := HandleSync(context.Background(), server, req)
			return resp
		},
	}

	rec := &recordingObserver{}
	result, err := Sync(context.Background(), client, mt, SyncOpts{
		Push:        true,
		Pull:        true,
		ProjectCode: projCode,
		ServerCode:  srvCode,
		Observer:    rec,
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if rec.started != 1 {
		t.Errorf("Started called %d times, want 1", rec.started)
	}
	if rec.completed != 1 {
		t.Errorf("Completed called %d times, want 1", rec.completed)
	}
	if len(rec.roundsStarted) != result.Rounds {
		t.Errorf("RoundStarted called %d times, want %d", len(rec.roundsStarted), result.Rounds)
	}
	if len(rec.roundsCompleted) != result.Rounds {
		t.Errorf("RoundCompleted called %d times, want %d", len(rec.roundsCompleted), result.Rounds)
	}
	if rec.lastInfo.Operation != "sync" {
		t.Errorf("Operation = %q, want %q", rec.lastInfo.Operation, "sync")
	}
	if rec.lastEnd.Rounds != result.Rounds {
		t.Errorf("SessionEnd.Rounds = %d, want %d", rec.lastEnd.Rounds, result.Rounds)
	}
	if rec.lastErr != nil {
		t.Errorf("SessionEnd err = %v, want nil", rec.lastErr)
	}
}

func TestCloneCallsObserverHooks(t *testing.T) {
	clonePath := filepath.Join(t.TempDir(), "clone.fossil")

	// Simple mock transport that simulates an empty clone (2 rounds minimum).
	round := 0
	mt := &MockTransport{
		Handler: func(req *xfer.Message) *xfer.Message {
			defer func() { round++ }()
			if round == 0 {
				// First round: send project-code, server-code, and seqno 0
				return &xfer.Message{
					Cards: []xfer.Card{
						&xfer.PushCard{
							ProjectCode: "test-project",
							ServerCode:  "test-server",
						},
						&xfer.CloneSeqNoCard{SeqNo: 0},
					},
				}
			}
			// Round 1+: empty response with seqno 0 to converge
			return &xfer.Message{
				Cards: []xfer.Card{
					&xfer.CloneSeqNoCard{SeqNo: 0},
				},
			}
		},
	}

	rec := &recordingObserver{}
	r, result, err := Clone(context.Background(), clonePath, mt, CloneOpts{
		Observer: rec,
	})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	defer r.Close()

	if rec.started != 1 {
		t.Errorf("Started called %d times, want 1", rec.started)
	}
	if rec.completed != 1 {
		t.Errorf("Completed called %d times, want 1", rec.completed)
	}
	if len(rec.roundsStarted) != result.Rounds {
		t.Errorf("RoundStarted called %d times, want %d", len(rec.roundsStarted), result.Rounds)
	}
	if rec.lastInfo.Operation != "clone" {
		t.Errorf("Operation = %q, want %q", rec.lastInfo.Operation, "clone")
	}
	if rec.lastEnd.Rounds != result.Rounds {
		t.Errorf("SessionEnd.Rounds = %d, want %d", rec.lastEnd.Rounds, result.Rounds)
	}
}
