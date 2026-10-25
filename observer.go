package libfossil

import (
	"fmt"
	"os"
)

// SyncObserver receives lifecycle callbacks during sync operations.
// Use NopSyncObserver() for a silent no-op, or StdoutSyncObserver() for stderr logging.
type SyncObserver interface {
	Started(info SessionStart)
	RoundStarted(round int)
	RoundCompleted(round int, stats RoundStats)
	Completed(info SessionEnd)
	Error(err error)
	HandleStarted(info HandleStart)
	HandleCompleted(info HandleEnd)
	TableSyncStarted(info TableSyncStart)
	TableSyncCompleted(info TableSyncEnd)
}

// CheckoutObserver receives lifecycle callbacks during checkout/commit operations.
// Use NopCheckoutObserver() for a silent no-op, or StdoutCheckoutObserver() for stderr logging.
type CheckoutObserver interface {
	ExtractStarted(info ExtractStart)
	ExtractFileCompleted(name string, change UpdateChange)
	ExtractCompleted(info ExtractEnd)
	ScanStarted(dir string)
	ScanCompleted(info ScanEnd)
	CommitStarted(info CommitStart)
	CommitCompleted(info CommitEnd)
	Error(err error)
}

// BuggifyChecker controls fault injection for deterministic simulation testing.
type BuggifyChecker interface {
	Check(site string, probability float64) bool
}

// --- Info structs ---

// SessionStart describes the beginning of a sync session.
type SessionStart struct {
	ProjectCode    string
	Push, Pull, UV bool
}

// SessionEnd describes the completion of a sync session.
type SessionEnd struct {
	Rounds, FilesSent, FilesRecvd int
}

// RoundStats describes the outcome of a single sync round.
type RoundStats struct {
	FilesSent, FilesRecvd, BytesSent, BytesRecvd, Gimmes, IGots int
}

// HandleStart describes the beginning of a server-side sync handle.
type HandleStart struct {
	RemoteAddr string
}

// HandleEnd describes the completion of a server-side sync handle.
type HandleEnd struct {
	FilesSent, FilesRecvd int
}

// TableSyncStart describes the beginning of a config table sync.
type TableSyncStart struct {
	Table string
}

// TableSyncEnd describes the completion of a config table sync.
type TableSyncEnd struct {
	Table               string
	RowsSent, RowsRecvd int
}

// ExtractStart describes the beginning of a checkout extraction.
type ExtractStart struct {
	RID int64
	Dir string
}

// ExtractEnd describes the completion of a checkout extraction.
type ExtractEnd struct {
	FilesWritten int
}

// UpdateChange classifies how a file changed.
type UpdateChange string

const (
	ChangeAdded    UpdateChange = "added"
	ChangeModified UpdateChange = "modified"
	ChangeDeleted  UpdateChange = "deleted"
)

// ScanEnd describes the completion of a working-tree scan.
type ScanEnd struct {
	FilesScanned int
}

// CommitStart describes the beginning of a commit operation.
type CommitStart struct {
	Comment string
	User    string
	Files   int
}

// CommitEnd describes the completion of a commit operation.
type CommitEnd struct {
	UUID string
	RID  int64
}

// --- Nop implementations ---

type nopSyncObserver struct{}

func (nopSyncObserver) Started(SessionStart)            {}
func (nopSyncObserver) RoundStarted(int)                {}
func (nopSyncObserver) RoundCompleted(int, RoundStats)  {}
func (nopSyncObserver) Completed(SessionEnd)            {}
func (nopSyncObserver) Error(error)                     {}
func (nopSyncObserver) HandleStarted(HandleStart)       {}
func (nopSyncObserver) HandleCompleted(HandleEnd)       {}
func (nopSyncObserver) TableSyncStarted(TableSyncStart) {}
func (nopSyncObserver) TableSyncCompleted(TableSyncEnd) {}

// NopSyncObserver returns a SyncObserver that silently discards all events.
func NopSyncObserver() SyncObserver { return nopSyncObserver{} }

type nopCheckoutObserver struct{}

func (nopCheckoutObserver) ExtractStarted(ExtractStart)               {}
func (nopCheckoutObserver) ExtractFileCompleted(string, UpdateChange) {}
func (nopCheckoutObserver) ExtractCompleted(ExtractEnd)               {}
func (nopCheckoutObserver) ScanStarted(string)                        {}
func (nopCheckoutObserver) ScanCompleted(ScanEnd)                     {}
func (nopCheckoutObserver) CommitStarted(CommitStart)                 {}
func (nopCheckoutObserver) CommitCompleted(CommitEnd)                 {}
func (nopCheckoutObserver) Error(error)                               {}

// NopCheckoutObserver returns a CheckoutObserver that silently discards all events.
func NopCheckoutObserver() CheckoutObserver { return nopCheckoutObserver{} }

// --- Stdout (stderr) implementations ---

type stdoutSyncObserver struct{}

func (stdoutSyncObserver) Started(info SessionStart) {
	fmt.Fprintf(os.Stderr, "[sync] started project=%s push=%v pull=%v uv=%v\n",
		info.ProjectCode, info.Push, info.Pull, info.UV)
}
func (stdoutSyncObserver) RoundStarted(round int) {
	fmt.Fprintf(os.Stderr, "[sync] round %d started\n", round)
}
func (stdoutSyncObserver) RoundCompleted(round int, s RoundStats) {
	fmt.Fprintf(os.Stderr, "[sync] round %d completed sent=%d recv=%d\n",
		round, s.FilesSent, s.FilesRecvd)
}
func (stdoutSyncObserver) Completed(info SessionEnd) {
	fmt.Fprintf(os.Stderr, "[sync] completed rounds=%d sent=%d recv=%d\n",
		info.Rounds, info.FilesSent, info.FilesRecvd)
}
func (stdoutSyncObserver) Error(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "[sync] error: %v\n", err)
	}
}
func (stdoutSyncObserver) HandleStarted(info HandleStart) {
	fmt.Fprintf(os.Stderr, "[sync] handle started remote=%s\n", info.RemoteAddr)
}
func (stdoutSyncObserver) HandleCompleted(info HandleEnd) {
	fmt.Fprintf(os.Stderr, "[sync] handle completed sent=%d recv=%d\n",
		info.FilesSent, info.FilesRecvd)
}
func (stdoutSyncObserver) TableSyncStarted(info TableSyncStart) {
	fmt.Fprintf(os.Stderr, "[sync] table sync started table=%s\n", info.Table)
}
func (stdoutSyncObserver) TableSyncCompleted(info TableSyncEnd) {
	fmt.Fprintf(os.Stderr, "[sync] table sync completed table=%s sent=%d recv=%d\n",
		info.Table, info.RowsSent, info.RowsRecvd)
}

// StdoutSyncObserver returns a SyncObserver that logs events to stderr.
func StdoutSyncObserver() SyncObserver { return stdoutSyncObserver{} }

type stdoutCheckoutObserver struct{}

func (stdoutCheckoutObserver) ExtractStarted(info ExtractStart) {
	fmt.Fprintf(os.Stderr, "[checkout] extract started rid=%d dir=%s\n", info.RID, info.Dir)
}
func (stdoutCheckoutObserver) ExtractFileCompleted(name string, change UpdateChange) {
	fmt.Fprintf(os.Stderr, "[checkout] %s %s\n", change, name)
}
func (stdoutCheckoutObserver) ExtractCompleted(info ExtractEnd) {
	fmt.Fprintf(os.Stderr, "[checkout] extract completed files=%d\n", info.FilesWritten)
}
func (stdoutCheckoutObserver) ScanStarted(dir string) {
	fmt.Fprintf(os.Stderr, "[checkout] scan started dir=%s\n", dir)
}
func (stdoutCheckoutObserver) ScanCompleted(info ScanEnd) {
	fmt.Fprintf(os.Stderr, "[checkout] scan completed files=%d\n", info.FilesScanned)
}
func (stdoutCheckoutObserver) CommitStarted(info CommitStart) {
	fmt.Fprintf(os.Stderr, "[checkout] commit started user=%s files=%d\n", info.User, info.Files)
}
func (stdoutCheckoutObserver) CommitCompleted(info CommitEnd) {
	short := info.UUID
	if len(short) > 10 {
		short = short[:10]
	}
	fmt.Fprintf(os.Stderr, "[checkout] commit completed uuid=%s rid=%d\n", short, info.RID)
}
func (stdoutCheckoutObserver) Error(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "[checkout] error: %v\n", err)
	}
}

// StdoutCheckoutObserver returns a CheckoutObserver that logs events to stderr.
func StdoutCheckoutObserver() CheckoutObserver { return stdoutCheckoutObserver{} }
