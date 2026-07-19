package cli

// RepoCmd is the top-level command group for repository operations.
// Embed this in your CLI struct alongside Globals for a complete fossil CLI.
type RepoCmd struct {
	New          RepoNewCmd          `cmd:"" help:"Create a new repository"`
	Clone        RepoCloneCmd        `cmd:"" help:"Clone a remote repository"`
	Ci           RepoCiCmd           `cmd:"" help:"Checkin file changes"`
	Co           RepoCoCmd           `cmd:"" help:"Checkout a version"`
	Ls           RepoLsCmd           `cmd:"" help:"List files in a version"`
	Timeline     RepoTimelineCmd     `cmd:"" help:"Show repository history"`
	Cat          RepoCatCmd          `cmd:"" help:"Output artifact content"`
	Info         RepoInfoCmd         `cmd:"" help:"Repository statistics"`
	Hash         RepoHashCmd         `cmd:"" help:"Hash files (SHA1 or SHA3)"`
	Delta        RepoDeltaCmd        `cmd:"" help:"Delta create/apply operations"`
	Config       RepoConfigCmd       `cmd:"" help:"Repository configuration"`
	Query        RepoQueryCmd        `cmd:"" help:"Execute SQL against repository"`
	Verify       RepoVerifyCmd       `cmd:"" help:"Verify repository integrity"`
	Resolve      RepoResolveCmd      `cmd:"" help:"Resolve symbolic name to UUID"`
	Extract      RepoExtractCmd      `cmd:"" help:"Extract files from a version"`
	Wiki         RepoWikiCmd         `cmd:"" help:"Wiki page operations"`
	Tag          RepoTagCmd          `cmd:"" help:"Tag operations"`
	Open         RepoOpenCmd         `cmd:"" help:"Open a checkout in a directory"`
	Status       RepoStatusCmd       `cmd:"" help:"Show working directory changes"`
	Add          RepoAddCmd          `cmd:"" help:"Stage files for addition"`
	Rm           RepoRmCmd           `cmd:"" help:"Stage files for removal"`
	Rename       RepoRenameCmd       `cmd:"" help:"Rename a tracked file"`
	Revert       RepoRevertCmd       `cmd:"" help:"Undo staging changes"`
	Diff         RepoDiffCmd         `cmd:"" help:"Show changes vs a version"`
	Merge        RepoMergeCmd        `cmd:"" help:"Merge a divergent version"`
	Conflicts    RepoConflictsCmd    `cmd:"" help:"List/manage unresolved conflicts"`
	MarkResolved RepoMergeResolveCmd `cmd:"" name:"mark-resolved" help:"Mark a conflict as resolved"`
	Undo         RepoUndoCmd         `cmd:"" help:"Undo last operation"`
	Redo         RepoRedoCmd         `cmd:"" help:"Redo undone operation"`
	Stash        RepoStashCmd        `cmd:"" help:"Stash working changes"`
	Bisect       RepoBisectCmd       `cmd:"" help:"Binary search for bugs"`
	Annotate     RepoAnnotateCmd     `cmd:"" help:"Annotate file lines with version history"`
	Blame        RepoBlameCmd        `cmd:"" help:"Alias for annotate"`
	Branch       RepoBranchCmd       `cmd:"" help:"Branch operations"`
	UV           RepoUVCmd           `cmd:"" name:"uv" help:"Unversioned file operations"`
	Schema       RepoSchemaCmd       `cmd:"" help:"Synced table schema operations"`
	User         RepoUserCmd         `cmd:"" help:"User management"`
	Invite       RepoInviteCmd       `cmd:"" help:"Generate invite token for a user"`
	Serve        RepoServeCmd        `cmd:"" help:"Serve repository over HTTP for sync/clone"`
}
