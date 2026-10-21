package sync

import "context"

// SessionStart describes the beginning of a sync or clone operation.
type SessionStart struct {
	Operation   string // "sync" or "clone"
	Push, Pull  bool
	UV          bool
	ProjectCode string
	PeerID      string // identifies the leaf agent instance
}

// SessionEnd describes the result of a sync or clone operation.
type SessionEnd struct {
	Operation                 string
	Rounds                    int
	FilesSent, FilesRecvd     int
	UVFilesSent, UVFilesRecvd int
	UVGimmesSent              int
	BytesSent, BytesRecvd     int64
	ProjectCode               string
	Errors                    []string
}

// RoundStats reports per-round activity for observer callbacks.
// Value type — no allocations on the nop path.
type RoundStats struct {
	FilesSent     int
	FilesReceived int
	GimmesSent    int
	IgotsSent     int
	BytesSent     int64
	BytesReceived int64
}

// HandleStart describes the beginning of a server-side sync request.
type HandleStart struct {
	Operation   string // "sync" or "clone"
	ProjectCode string
	RemoteAddr  string
}

// HandleEnd describes the result of a server-side sync request.
type HandleEnd struct {
	CardsProcessed int
	FilesSent      int
	FilesReceived  int
	// SparseClassified counts the artifacts a cone request's keep-set
	// discovery read and classified (§6.2); zero for a dense request.
	SparseClassified int
	Err              error
}

// TableSyncStart describes the beginning of a table sync operation.
type TableSyncStart struct {
	Table     string
	LocalRows int
}

// TableSyncEnd describes the result of a table sync operation.
type TableSyncEnd struct {
	Table    string
	Sent     int
	Received int
}

// Observer receives lifecycle callbacks during sync and clone operations.
// A single Observer instance may be shared across multiple concurrent sessions.
// Pass nil for no-op default.
//
// Performance: nopObserver implements all methods as empty functions.
// The only cost on the hot path is one indirect call per invocation (~2ns).
type Observer interface {
	// Client-side session lifecycle.
	Started(ctx context.Context, info SessionStart) context.Context
	RoundStarted(ctx context.Context, round int) context.Context
	RoundCompleted(ctx context.Context, round int, stats RoundStats)
	Completed(ctx context.Context, info SessionEnd, err error)

	// Per-error recording — called on individual protocol errors.
	Error(ctx context.Context, err error)

	// Server-side request lifecycle.
	HandleStarted(ctx context.Context, info HandleStart) context.Context
	HandleCompleted(ctx context.Context, info HandleEnd)

	// Table sync lifecycle.
	TableSyncStarted(ctx context.Context, info TableSyncStart)
	TableSyncCompleted(ctx context.Context, info TableSyncEnd)
}

// nopObserver is the default observer that does nothing.
type nopObserver struct{}

func (nopObserver) Started(ctx context.Context, _ SessionStart) context.Context      { return ctx }
func (nopObserver) RoundStarted(ctx context.Context, _ int) context.Context          { return ctx }
func (nopObserver) RoundCompleted(_ context.Context, _ int, _ RoundStats)            {}
func (nopObserver) Completed(_ context.Context, _ SessionEnd, _ error)               {}
func (nopObserver) Error(_ context.Context, _ error)                                 {}
func (nopObserver) HandleStarted(ctx context.Context, _ HandleStart) context.Context { return ctx }
func (nopObserver) HandleCompleted(_ context.Context, _ HandleEnd)                   {}
func (nopObserver) TableSyncStarted(_ context.Context, _ TableSyncStart)             {}
func (nopObserver) TableSyncCompleted(_ context.Context, _ TableSyncEnd)             {}

// resolveObserver returns obs if non-nil, otherwise nopObserver{}.
func resolveObserver(obs Observer) Observer {
	if obs == nil {
		return nopObserver{}
	}
	return obs
}
