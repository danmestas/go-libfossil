---
title: Embedding the CLI
weight: 65
---

An application that keeps version history in a Fossil repository can run the `libfossil` binary as a child process: no checkout, no shadow working tree. Four commands make up the interface; their output and exit statuses are stable.

## Commands

```
libfossil -R repo.fossil repo snapshot -m MSG [--user U] --path notes/x.md [--stdin] [--json]
libfossil -R repo.fossil repo log      --path notes/x.md [--json] [-n N]
libfossil -R repo.fossil repo cat      --at VERSION notes/x.md
libfossil -R repo.fossil repo diff     [--json] A B [PATH]
```

**snapshot** commits one file's bytes onto trunk and carries every other file forward. The bytes come from stdin with `--stdin`, otherwise from `--path` read on disk relative to the working directory. `--path` must be a name fossil accepts for a new file: a clean relative path (`notes/x.md`; not `/x`, `../x`, `./x`, `a//b`, `a/`, `a\b`) in valid UTF-8, with no control character (a byte below 0x20), and not one of the names fossil reserves for its own files (`_FOSSIL_` or `.fslckout`, in any case, with or without a `-wal`, `-shm` or `-journal` suffix, at the top or as the last element of the path). A rejected path exits 1 and says why. When the bytes equal the file at the trunk tip, nothing is committed and the existing tip is reported. The first snapshot into an empty repository becomes the root check-in. The check-in time is the current time.

**log** lists the check-ins that changed the path, newest first, as `fossil finfo` does. `-n` caps the count; the default is all.

**cat** writes the file's bytes as VERSION holds them, as `fossil cat -r` does. VERSION is anything `repo resolve` accepts: `tip`, `trunk`, a branch, a uuid or a unique prefix of one. Without `--at`, `repo cat` still outputs an artifact by uuid.

**diff** with two versions prints the unified diff between them for PATH, or for every file that differs when PATH is omitted. With one or no version it compares the working directory, as before.

## Output

Every JSON document is a single line on stdout. Fields are never renamed or removed; new fields may be added, so parse tolerantly. Times are RFC 3339 in UTC with milliseconds, such as `2026-10-19T08:30:00.123Z`. A uuid is the repository's hash of the artifact (40 hex characters for SHA1, 64 for SHA3-256).

`snapshot` prints the check-in uuid and a newline. With `--json`:

```json
{"uuid": "9f2c...", "changed": true}
```

`changed` is false when the bytes already matched the tip and `uuid` is that tip.

`log --json` prints an array, newest first:

```json
[{"uuid": "9f2c...", "time": "2026-10-19T08:30:00.123Z", "user": "dan",
  "comment": "edit", "action": "modified", "file_uuid": "ab12...", "prev_name": ""}]
```

`action` is `added`, `modified`, `deleted` or `renamed`. `file_uuid` is absent for `deleted`; `prev_name` is present only for `renamed`. Without `--json`, each line is `UUID10  TIME  USER  ACTION  COMMENT`.

`diff --json A B [PATH]` prints an array, empty when nothing differs:

```json
[{"name": "notes/x.md", "unified": "--- a/notes/x.md\n+++ b/notes/x.md\n@@ ..."}]
```

## Exit statuses

| Status | Meaning |
| --- | --- |
| 0 | Success, including a `snapshot` that changed nothing and a `diff` with no differences. |
| 1 | Any other failure: a bad `--path`, an unreadable repository, an ambiguous version prefix, an I/O error. The message is on stderr. |
| 2 | Not found: the version does not resolve, the file is not in that version (`cat`), or the path has no history (`log`). Nothing is written to stdout. |
| 80 | Usage error: an unknown command or flag, or a missing required flag or argument. The command-line parser sets this status and writes the error and the command's usage to stderr; stdout stays empty. |

These are the statuses of the `libfossil` binary itself. `go run ./cmd/libfossil` reports any non-zero status as 1, so measure them on a built binary. A combination the parser accepts but the command refuses, such as `diff --json` without two versions, exits 1.

## Concurrency

Processes may snapshot into one repository at the same time. Writers serialize on SQLite's write lock, which waits up to the busy timeout (5 seconds) before failing. A snapshot also checks, inside its write transaction, that no check-in arrived since it read the trunk tip, and starts over if one did, so concurrent snapshots form one linear trunk and none loses another's files. Keep the repository outside any synced folder, and open it from one machine at a time.
