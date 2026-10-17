# Erratum: draft-fossil-artifact-format-01 §4.7.3

| | |
|---|---|
| **Document** | `draft-fossil-artifact-format-01` |
| **Section** | 4.7.3, B Card (a delta manifest whose baseline is itself a delta) |
| **Type** | Technical (the named delta's delete markers are not covered; Fossil's handling beyond the file listing is not recorded) |
| **Status** | Applied in `draft-fossil-artifact-format-01` (2026-10-14) |
| **Reported** | 2026-10-14 |
| **Submitter** | Dan Mestas |
| **Found via** | A review of the drafts from libfossil; confirmed by running Fossil 2.28 and reading its source |
| **Amends** | `artifact-format-00-s4.7.3-delta-of-delta.md` (2026-10-11) |

## 1. Summary

The earlier erratum made §4.7.3 say that Fossil accepts a delta manifest
whose `B` card names another delta manifest, computes its file set one level
deep, and reports no error. That is what Fossil's file listing does. Two
things are missing.

1. The named delta may carry delete markers (`F` cards with no hash). In the
   one-level merge they arrive on the baseline side of the walk, where §8.2's
   steps never expect a hashless entry. §8.2 does require consumers to filter
   entries without a hash, but the text only names the delta's own tail.
2. Fossil is not consistent on such an artifact beyond the listing. Its
   archive commands crash on the named delta's delete markers, and the
   file-change rows it records when crosslinking the check-in do not
   describe the change from the parent. The draft's "no error is reported"
   reads as if Fossil handled the artifact cleanly.

## 2. The text in error

Page 22 (§4.7.3):

> Fossil does not enforce this on read: it parses and cross-links a delta
> manifest whose baseline is itself a delta and computes its file set from
> that delta's own F cards and the named artifact's own F cards only.
> Files the named artifact inherits from its own baseline are absent from
> the result, and no error is reported. A reader MUST accept such an
> artifact and MUST compute its file set in the same way (Section 8.2);
> rejecting it, or resolving it recursively, diverges from Fossil.

## 3. What Fossil implements

`manifest_file_next()` in `src/manifest.c` merges a delta's `F` cards with
its baseline's. Entries taken from the baseline side are returned as they
are, with no test for a missing hash, because a baseline manifest never
carries a delete marker. When the baseline is itself a delta, its delete
markers come back as file entries with a null hash. `fossil ls` skips them.
`zip.c:811` and the matching loops in `tar.c` and the sqlar writer pass the
null hash to `uuid_to_rid()` and crash. Fossil check-in 4886b7de6d
(2026-10-14), after the 2.28 release, makes those loops skip entries
without a hash; no release carries it yet.

Checked with Fossil 2.28 [52445a27f1]. Each repository was built by
committing check-ins with `fossil commit`, adding one hand-written check-in
C2 (`B` and `P` both naming the delta check-in C1, one `F` card for `b`),
and running `fossil reconstruct`. `fossil rebuild` gives the same rows.

Case 1: A holds `a b c`; C1 is a delta that edits `a`.

| Command on C2 | Result |
|---|---|
| `fossil ls -r C2` | `a b` (one level: `c` is dropped) |
| `fossil zip`, `tarball` | archive of `a b`, exit 0 |
| mlink rows for C2 | `b` changed; `a` changed from itself to itself; no row for `c` |

Case 2: A holds `a b c`; C1 is a delta that deletes `c` (`F c`, no hash).

| Command on C2 | Result |
|---|---|
| `fossil ls -r C2` | `b` |
| `fossil zip`, `tarball`, `sqlar` | segmentation fault, exit 139 |
| mlink rows for C2 | `b` changed; `c` changed from nothing to nothing; no row for `a` |

In both cases the parent's file set differs from C2's by files that the
mlink rows do not mention, and the rows that are written record no change.

zit trunk, given the same repositories through `import-fossil`, lists
the same file sets, writes the archives with the same files, and does not
crash, because it filters hashless entries as §8.2 requires.

## 4. Corrected text

Replace the paragraph quoted in Section 2 with:

> Fossil does not enforce this on read: it parses and cross-links a delta
> manifest whose baseline is itself a delta and computes its file set from
> that delta's own F cards and the named artifact's own F cards only.
> Files the named artifact inherits from its own baseline are absent from
> the result, and the file listing reports no error. A reader MUST accept
> such an artifact and MUST compute its file set in the same way
> (Section 8.2); rejecting it, or resolving it recursively, diverges from
> Fossil. The named artifact's delete markers enter that walk on the
> baseline side, and like the delta's own they add no file: a consumer
> MUST filter out every merged entry without a hash.
>
> This paragraph is informative. Fossil 2.28 is not consistent on such an
> artifact beyond its file listing. Its zip, tarball, and sqlar commands
> do not filter the named artifact's delete markers and terminate with a
> segmentation fault when it carries one; Fossil check-in 4886b7de6d
> (2026-10-14), on the development line after 2.28, filters them. The
> file-change records it writes when it cross-links the check-in do not
> describe the difference between the parent's file set and the
> check-in's: files the one-level rule drops get no record, while
> unchanged files and delete markers can be recorded as changes. This
> document does not define those records for such a check-in.

## 5. Observable consequence of the uncorrected text

A reader that applies §8.2's steps literally to the baseline side can treat a
hashless baseline entry as a file and fail to look it up, which is Fossil's
crash. A reader that compares itself against Fossil's file-change history for
such a check-in will find differences the draft does not explain.

## 6. Implementations

zit already filters hashless entries from both sides of the merge. The
change for an implementation that does not is to drop every merged entry
without a hash before using the file set.
