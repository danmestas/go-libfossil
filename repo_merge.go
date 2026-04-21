package libfossil

import (
	"bytes"
	"errors"
	"fmt"
	"sort"

	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/internal/content"
	"github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/internal/manifest"
	"github.com/danmestas/libfossil/internal/merge"
)

// MergeOpts configures a merge operation.
type MergeOpts struct {
	Strategy string
	Dir      string
}

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

// FindCommonAncestor finds the nearest common ancestor of two checkins.
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

	ancestor, err := r.FindCommonAncestor(srcTip, dstTip)
	if err != nil {
		return 0, "", fmt.Errorf("libfossil: merge: %w", err)
	}

	srcFiles, err := loadFileset(r, srcTip)
	if err != nil {
		return 0, "", fmt.Errorf("libfossil: merge: src fileset: %w", err)
	}
	dstFiles, err := loadFileset(r, dstTip)
	if err != nil {
		return 0, "", fmt.Errorf("libfossil: merge: dst fileset: %w", err)
	}
	baseFiles, err := loadFileset(r, ancestor)
	if err != nil {
		return 0, "", fmt.Errorf("libfossil: merge: ancestor fileset: %w", err)
	}

	allNames := make(map[string]struct{}, len(srcFiles)+len(dstFiles))
	for n := range srcFiles {
		allNames[n] = struct{}{}
	}
	for n := range dstFiles {
		allNames[n] = struct{}{}
	}

	strat := &merge.ThreeWayText{}
	var conflictFiles []string
	var merged []FileToCommit

	for name := range allNames {
		local, localOK := dstFiles[name]
		remote, remoteOK := srcFiles[name]
		base, baseOK := baseFiles[name]

		switch {
		case !localOK && !remoteOK:
			// Shouldn't happen (not in union).
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
			merged = append(merged, FileToCommit{Name: name, Content: res.Content, Perm: perm})
		}
	}

	if len(conflictFiles) > 0 {
		sort.Strings(conflictFiles)
		return 0, "", &MergeConflictError{Files: conflictFiles}
	}

	sort.Slice(merged, func(i, j int) bool { return merged[i].Name < merged[j].Name })

	return r.Commit(CommitOpts{
		Files:        merged,
		Comment:      message,
		User:         user,
		ParentID:     dstTip,
		MergeParents: []int64{srcTip},
	})
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
		brid, ok := blob.Exists(r.DB(), e.UUID)
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
