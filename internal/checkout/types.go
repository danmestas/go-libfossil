package checkout

import (
	"time"

	libfossil "github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/simio"
)

// VFileChange — vfile.chnged column values (matches Fossil's vfile states)
type VFileChange int

const (
	VFileNone        VFileChange = 0 // unchanged
	VFileMod         VFileChange = 1 // modified
	VFileMergeMod    VFileChange = 2 // modified via merge
	VFileMergeAdd    VFileChange = 3 // added via merge
	VFileIntMod      VFileChange = 4 // modified via integrate
	VFileIntAdd      VFileChange = 5 // added via integrate
	VFileIsExec      VFileChange = 6 // became executable
	VFileBecameLink  VFileChange = 7 // became symlink
	VFileNotExec     VFileChange = 8 // lost executable
	VFileNotLink     VFileChange = 9 // lost symlink
)

// FileChange — checkout-level change types (maps to fsl_ckout_change_e)
type FileChange int

const (
	ChangeNone     FileChange = iota
	ChangeAdded
	ChangeRemoved
	ChangeMissing
	ChangeRenamed
	ChangeModified
)

// UpdateChange — file states during extract/update (maps to fsl_ckup_fchange_e)
type UpdateChange int

const (
	UpdateNone               UpdateChange = iota
	UpdateAdded
	UpdateAddPropagated
	UpdateRemoved
	UpdateRmPropagated
	UpdateUpdated
	UpdateUpdatedBinary
	UpdateMerged
	UpdateConflictMerged
	UpdateConflictAdded
	UpdateConflictUnmanaged
	UpdateConflictRm
	UpdateConflictSymlink
	UpdateConflictBinary
	UpdateRenamed
	UpdateEdited
)

// RevertChange — revert operation types (maps to fsl_ckout_revert_e)
type RevertChange int

const (
	RevertNone        RevertChange = iota
	RevertUnmanage
	RevertRemove
	RevertRename
	RevertPermissions
	RevertContents
)

// ScanFlags — controls ScanChanges behavior
type ScanFlags uint32

const (
	ScanHash       ScanFlags = 1 << iota // hash file content (not just mtime)
	ScanENotFile                          // mark non-regular files
	ScanSetMTime                          // update mtime in vfile
	ScanKeepOthers                        // keep other version entries
)

// ManifestFlags — controls WriteManifest behavior
type ManifestFlags int

const (
	ManifestMain ManifestFlags = 1 << iota // write manifest file
	ManifestUUID                            // write manifest.uuid file
)

// OpenOpts configures opening an existing checkout.
type OpenOpts struct {
	Env           *simio.Env // nil → RealEnv
	Observer      Observer   // nil → nopObserver
	SearchParents bool       // search parent dirs for .fslckout
}

// CreateOpts configures creating a new checkout from a repo.
type CreateOpts struct {
	Env      *simio.Env // nil → RealEnv
	Observer Observer   // nil → nopObserver
}

// ExtractOpts configures file extraction from a checkin.
type ExtractOpts struct {
	Callback func(name string, change UpdateChange) error // per-file notification
	SetMTime bool // set file mtime to checkin timestamp
	DryRun   bool
	Force    bool // overwrite locally modified files
}

// UpdateOpts configures updating to a new version with merge.
type UpdateOpts struct {
	TargetRID libfossil.FslID // 0 → auto-calculate via CalcUpdateVersion
	Callback  func(name string, change UpdateChange) error
	SetMTime  bool // set file mtime to checkin timestamp
	DryRun    bool
}

// UpdateResult reports what Update actually did. Paths, not counts: a
// caller needs to know which files to show a user, not merely how many
// changed. All three slices are sorted (sort.Strings) before Update
// returns, so the order is deterministic and safe to compare with
// reflect.DeepEqual — it does not depend on Go's randomized map iteration.
//
// Conflicted is non-empty when three-way merge could not resolve a file
// cleanly and wrote "<<<<<<<"-style conflict markers into it on disk. That
// is a successful update, not a failure — Update still returns a nil error
// in that case. A caller that only checks the error gets today's behavior;
// a caller that wants to detect marker text in the working tree must
// inspect Conflicted. Every path in Conflicted also appears in
// FilesWritten: the marker text is what was written there.
type UpdateResult struct {
	FilesWritten []string // paths written (added, updated, or merged, clean or not); includes every Conflicted path
	FilesRemoved []string // paths deleted from the working tree
	Conflicted   []string // paths that now contain conflict markers; also present in FilesWritten
}

// ManageOpts configures adding files to tracking.
type ManageOpts struct {
	Paths    []string
	Callback func(name string, added bool) error
}

// ManageCounts reports the result of a Manage operation.
type ManageCounts struct {
	Added, Updated, Skipped int
}

// UnmanageOpts configures removing files from tracking.
type UnmanageOpts struct {
	Paths    []string            // pathnames to unmanage
	VFileIDs []libfossil.FslID   // alternative: pass IDs directly
	Callback func(name string) error
}

// EnqueueOpts configures staging files for commit.
type EnqueueOpts struct {
	Paths    []string
	Callback func(name string) error
}

// DequeueOpts configures unstaging files.
type DequeueOpts struct {
	Paths []string // empty → dequeue all
}

// CommitOpts configures creating a checkin from staged files.
type CommitOpts struct {
	Message  string
	User     string
	Branch   string    // empty → current branch
	Tags     []string  // additional T-cards
	Delta    bool
	Time           time.Time    // zero → env.Clock.Now()
	PreCommitCheck func() error // nil = no check; non-nil error aborts commit
}

// RevertOpts configures reverting file changes.
type RevertOpts struct {
	Paths    []string // empty → revert all
	Callback func(name string, change RevertChange) error
}

// RenameOpts configures renaming a tracked file.
type RenameOpts struct {
	From, To string
	DoFsMove bool // also move on filesystem via Storage
	Callback func(from, to string) error
}

// ChangeEntry describes a single file change in the checkout.
type ChangeEntry struct {
	Name     string
	Change   FileChange
	VFileID  libfossil.FslID
	IsExec   bool
	IsLink   bool
	OrigName string // non-empty if renamed
}

// ChangeVisitor is called for each changed file during VisitChanges.
type ChangeVisitor func(entry ChangeEntry) error
