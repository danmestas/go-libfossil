package libfossil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/danmestas/go-libfossil/internal/checkout"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/merge"
)

// MergeResult describes the outcome of a merge.
type MergeResult struct {
	Clean     bool
	Conflicts []MergeConflict
}

// MergeConflict describes a conflict region in a file.
type MergeConflict struct {
	Name      string
	StartLine int
	EndLine   int
}

// Fork describes a divergence point between two branches.
type Fork struct {
	Ancestor  int64
	LocalTip  int64
	RemoteTip int64
}

// ErrMergeConflict is returned (wrapped in a MergeConflictError) when Merge
// detects any unresolved three-way conflicts. Callers can check with
// errors.Is(err, ErrMergeConflict) without caring about the file list.
var ErrMergeConflict = errors.New("libfossil: merge has conflicts")

// MergeConflictError reports which files had unresolved merge conflicts.
// Files is sorted alphabetically for deterministic output.
type MergeConflictError struct {
	Files []string
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("libfossil: merge has conflicts in %d file(s): %v", len(e.Files), e.Files)
}

func (e *MergeConflictError) Is(target error) bool {
	return target == ErrMergeConflict
}

// FindCommonAncestor finds the most recent common ancestor of two checkins,
// the merge base fossil's merge-base command names.
func (r *Repo) FindCommonAncestor(ridA, ridB int64) (int64, error) {
	ancestor, err := merge.FindCommonAncestor(r.inner, fsltype.FslID(ridA), fsltype.FslID(ridB))
	if err != nil {
		return 0, fmt.Errorf("libfossil: find common ancestor: %w", err)
	}
	return int64(ancestor), nil
}

// DetectForks finds divergent branches in the repository.
func (r *Repo) DetectForks() ([]Fork, error) {
	forks, err := merge.DetectForks(r.inner)
	if err != nil {
		return nil, fmt.Errorf("libfossil: detect forks: %w", err)
	}
	result := make([]Fork, len(forks))
	for i, f := range forks {
		result[i] = Fork{
			Ancestor:  int64(f.Ancestor),
			LocalTip:  int64(f.LocalTip),
			RemoteTip: int64(f.RemoteTip),
		}
	}
	return result, nil
}

// ListConflictForks returns filenames with unresolved conflict-fork entries.
func (r *Repo) ListConflictForks() ([]string, error) {
	names, err := merge.ListConflictForks(r.inner)
	if err != nil {
		return nil, fmt.Errorf("libfossil: list conflict forks: %w", err)
	}
	return names, nil
}

// ResolveConflictFork marks a conflict-fork entry as resolved.
func (r *Repo) ResolveConflictFork(filename string) error {
	return merge.ResolveConflictFork(r.inner, filename)
}

// BranchTip returns the RID of the most recent checkin on the named branch.
// Resolves via the 'branch' propagating tag: the tip is the checkin with the
// latest event.mtime whose tagxref still has that branch value active.
// Returns an error if no such branch exists in the repository.
func (r *Repo) BranchTip(name string) (int64, error) {
	if name == "" {
		return 0, fmt.Errorf("libfossil: branch tip: name is required")
	}
	var rid int64
	// Secondary sort on rid breaks ties when two checkins on the same branch
	// land in the same julian-day bucket — without it, the SQLite engine is
	// free to return either row and BranchTip becomes flaky for rapid commits.
	err := r.DB().QueryRow(`
		SELECT tagxref.rid FROM tagxref
		JOIN tag ON tagxref.tagid=tag.tagid
		JOIN event ON event.objid=tagxref.rid
		WHERE tag.tagname='branch' AND tagxref.value=? AND tagxref.tagtype>0
		ORDER BY event.mtime DESC, tagxref.rid DESC LIMIT 1`,
		name,
	).Scan(&rid)
	if err != nil {
		return 0, fmt.Errorf("libfossil: branch tip %q: %w", name, err)
	}
	return rid, nil
}

// BranchOpenLeaf names one open leaf of the named branch (repo-state-00
// Section 9): a check-in whose active branch tag is name, that is a leaf and
// that is not closed. When the branch has several, the one with the greatest
// event.mtime is returned, as the draft requires of any report that names a
// single open leaf. open is false, with a nil error, when the branch has no
// open leaf: it is not open. An empty name is an error, as in BranchTip; the
// main branch is named by CurrentTrunk and MainBranch.
//
// Unlike BranchTip, which returns the branch's newest check-in whether or
// not it is a leaf or closed, this is fossil's `branch info` answer. fossil's
// query (branch_is_open, src/branch.c) takes whichever open leaf it meets
// first; this follows the draft's greatest-mtime rule.
func (r *Repo) BranchOpenLeaf(name string) (rid int64, open bool, err error) {
	if name == "" {
		return 0, false, fmt.Errorf("libfossil: branch open leaf: name is required")
	}
	leaf, open, err := checkout.BranchOpenLeaf(r.inner, name)
	if err != nil {
		return 0, false, fmt.Errorf("libfossil: branch open leaf %q: %w", name, err)
	}
	return int64(leaf), open, nil
}

// Merge performs a three-way merge of srcBranch into dstBranch and creates a
// merge commit whose primary parent is dstBranch's tip and whose secondary
// parent is srcBranch's tip. Returns (rid, uuid) of the new commit on success.
//
// Conflict policy: any unresolved conflict in any file aborts the whole
// operation with a *MergeConflictError — no commit is written. Callers should
// check errors.Is(err, ErrMergeConflict) to detect this case.
//
// File handling:
//   - Present on both sides: three-way merge against the common ancestor.
//   - Present on only one side (new file): kept as-is.
//   - In ancestor, missing on one side, unchanged on the other: agreed deletion.
//   - In ancestor, missing on one side, modified on the other: modify/delete conflict.
//
// Files are matched across the three versions following renames, as fossil's
// merge matches them: a file srcBranch renamed and dstBranch did not takes
// srcBranch's name, and the commit records the rename; a file dstBranch
// renamed keeps its name.
func (r *Repo) Merge(srcBranch, dstBranch, message, user string) (int64, string, error) {
	if srcBranch == "" || dstBranch == "" {
		return 0, "", fmt.Errorf("libfossil: merge: src and dst branches are required")
	}
	if user == "" {
		return 0, "", fmt.Errorf("libfossil: merge: user is required")
	}
	if srcBranch == dstBranch {
		return 0, "", fmt.Errorf("libfossil: merge: src and dst branches are the same (%q)", srcBranch)
	}

	srcTip, err := r.BranchTip(srcBranch)
	if err != nil {
		return 0, "", fmt.Errorf("libfossil: merge: src branch %q: %w", srcBranch, err)
	}
	dstTip, err := r.BranchTip(dstBranch)
	if err != nil {
		return 0, "", fmt.Errorf("libfossil: merge: dst branch %q: %w", dstBranch, err)
	}
	if srcTip == dstTip {
		return 0, "", fmt.Errorf("libfossil: merge: branches %q and %q point to same checkin (rid=%d)", srcBranch, dstBranch, srcTip)
	}

	// fossil's merge: the merged-in version is the primary (merge.c).
	base, err := merge.FindPivot(context.Background(), r.inner, merge.PivotQuery{
		Primary: fsltype.FslID(srcTip), Secondaries: []fsltype.FslID{fsltype.FslID(dstTip)}})
	if err != nil && !errors.Is(err, merge.ErrNoCommonAncestor) {
		return 0, "", fmt.Errorf("libfossil: merge: %w", err)
	}
	ancestor := base.RID
	// draft-fossil-horizon-00 §5.5: never merge against a base the store
	// does not hold, nor one found by a walk that crossed the horizon, and
	// name the horizon when that is why.
	if cerr := content.MergeBaseCrossing(r.inner.DB(), fsltype.FslID(srcTip), ancestor, base.Unheld); cerr != nil {
		return 0, "", fmt.Errorf("libfossil: merge: %w", cerr)
	}
	// Nor over one it found while either side's ancestry crosses the
	// horizon: only the origin can name that base, and Merge has no peer.
	sides := []fsltype.FslID{fsltype.FslID(srcTip), fsltype.FslID(dstTip)}
	if cerr := content.AncestryCrossing(r.inner.DB(), sides[0], sides); cerr != nil {
		return 0, "", fmt.Errorf("libfossil: merge: %w; Repo.Merge cannot ask the origin: "+
			"merge in a checkout with Checkout.MergeContext and a Peer", cerr)
	}
	if err != nil {
		return 0, "", fmt.Errorf("libfossil: merge: find common ancestor: %w", err)
	}

	srcFiles, err := loadFileset(r, srcTip)
	if err != nil {
		return 0, "", fmt.Errorf("libfossil: merge: src fileset: %w", err)
	}
	dstFiles, err := loadFileset(r, dstTip)
	if err != nil {
		return 0, "", fmt.Errorf("libfossil: merge: dst fileset: %w", err)
	}
	baseFiles, err := loadFileset(r, int64(ancestor))
	if err != nil {
		return 0, "", fmt.Errorf("libfossil: merge: ancestor fileset: %w", err)
	}

	// Files are matched across the versions following renames, as fossil's
	// merge matches them; dst is the checkout's version.
	dstNames := make([]merge.LocalFile, 0, len(dstFiles))
	for _, n := range sortedFileNames(dstFiles) {
		dstNames = append(dstNames, merge.LocalFile{Name: n})
	}
	matches, err := merge.FollowRenames(context.Background(), r.inner, merge.RenameQuery{
		V: fsltype.FslID(dstTip), P: ancestor, M: fsltype.FslID(srcTip), Local: dstNames,
		Pivot: sortedFileNames(baseFiles), Merged: sortedFileNames(srcFiles),
	})
	if err != nil {
		return 0, "", fmt.Errorf("libfossil: merge: %w", err)
	}

	strat := &merge.ThreeWayText{}
	var conflictFiles []string
	var merged []FileToCommit

	for _, m := range matches {
		var name string
		var local mergeFileEntry
		localOK := m.Local >= 0
		if localOK {
			name = dstNames[m.Local].Name
			local = dstFiles[name]
		}
		remote, remoteOK := srcFiles[m.Merged]
		base, baseOK := baseFiles[m.Pivot]
		if !localOK {
			name = m.Merged
		}
		// Renamed on src only: the result takes src's name (fnp=fn,
		// fnm!=fnp in merge_cmd), and the commit records the rename.
		oldName := ""
		if localOK && remoteOK && baseOK && m.Pivot == name && m.Merged != name {
			name, oldName = m.Merged, name
		}

		switch {
		case !localOK && !remoteOK:
			// In the base alone: deleted on both sides.
			continue

		case localOK && !remoteOK:
			if !baseOK {
				// Added on dst only.
				merged = append(merged, FileToCommit{Name: name, Content: local.content, Perm: local.perm})
				continue
			}
			if bytes.Equal(base.content, local.content) {
				// dst untouched, src deleted — agree to delete.
				continue
			}
			conflictFiles = append(conflictFiles, name)

		case !localOK && remoteOK:
			if !baseOK {
				// Added on src only.
				merged = append(merged, FileToCommit{Name: name, Content: remote.content, Perm: remote.perm})
				continue
			}
			if bytes.Equal(base.content, remote.content) {
				// src untouched, dst deleted — agree to delete.
				continue
			}
			conflictFiles = append(conflictFiles, name)

		default:
			// Both sides present.
			basePayload := []byte{}
			if baseOK {
				basePayload = base.content
			}
			res, err := strat.Merge(basePayload, local.content, remote.content)
			if err != nil {
				return 0, "", fmt.Errorf("libfossil: merge %s: %w", name, err)
			}
			if !res.Clean {
				conflictFiles = append(conflictFiles, name)
				continue
			}
			perm := local.perm
			if perm == "" {
				perm = remote.perm
			}
			merged = append(merged, FileToCommit{Name: name, Content: res.Content, Perm: perm, OldName: oldName})
		}
	}

	if len(conflictFiles) > 0 {
		sort.Strings(conflictFiles)
		return 0, "", &MergeConflictError{Files: conflictFiles}
	}

	sort.Slice(merged, func(i, j int) bool { return merged[i].Name < merged[j].Name })

	// merged is the whole tree: a file dst has and the merge drops stays
	// dropped.
	return r.Commit(CommitOpts{
		Files:           merged,
		Comment:         message,
		User:            user,
		ParentID:        dstTip,
		MergeParents:    []int64{srcTip},
		PartialManifest: true,
	})
}

// sortedFileNames is a fileset's names, sorted.
func sortedFileNames(files map[string]mergeFileEntry) []string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// mergeFileEntry caches the expanded bytes and perm of one file in a fileset.
type mergeFileEntry struct {
	perm    string
	content []byte
}

// loadFileset materializes every file in the checkin into memory. Used by
// Merge so the three-way loop can do pure map lookups against src, dst, and
// ancestor without re-querying the blob store per file.
func loadFileset(r *Repo, rid int64) (map[string]mergeFileEntry, error) {
	if rid <= 0 {
		return map[string]mergeFileEntry{}, nil
	}
	entries, err := manifest.ListFiles(r.inner, fsltype.FslID(rid))
	if err != nil {
		return nil, err
	}
	out := make(map[string]mergeFileEntry, len(entries))
	for _, e := range entries {
		brid, ok := content.AvailableByUUID(r.DB(), e.UUID)
		if !ok {
			return nil, fmt.Errorf("blob not found for uuid %s", e.UUID)
		}
		data, err := content.Expand(r.DB(), brid)
		if err != nil {
			return nil, fmt.Errorf("expand blob %s: %w", e.UUID, err)
		}
		out[e.Name] = mergeFileEntry{perm: e.Perm, content: data}
	}
	return out, nil
}
