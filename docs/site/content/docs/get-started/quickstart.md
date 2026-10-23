---
title: "Quickstart: the CLI"
linkTitle: Quickstart
weight: 20
aliases: ["/docs/quickstart/"]
---

Create a repository, commit a file, and read the result with stock `fossil`. The `libfossil` command drives the same code as the library, and the file it writes is an ordinary Fossil repository.

## Create a repository and a checkout

```sh
libfossil repo new notes.fossil
mkdir work && cd work
libfossil -R ../notes.fossil repo open .
```

```text
created repository: notes.fossil
opened checkout in . (repo: /home/ana/notes.fossil)
empty repository -- no checkins yet
```

`repo open` writes a `.fslckout` file in the directory. Commands run inside the checkout find the repository through it, so `-R` is only needed to open it.

## Commit

```sh
echo '# notes' > README.md
libfossil repo add README.md
libfossil repo ci -m "first notes" --user ana
```

```text
ADDED    README.md
checkin 542a963405 (rid=2)
```

The check-in name is the hash of the manifest, which includes the time, so yours differs. `--user` sets the name on the check-in; without it the OS user name is used.

## Read the history

```sh
libfossil repo timeline
```

```text
542a963405  2026-10-23 21:00  ana  first notes
```

The repository is a Fossil repository, so the `fossil` binary reads it unchanged:

```sh
fossil timeline -R ../notes.fossil
```

```text
=== 2026-10-23 ===
21:00:14 [542a963405] first notes (user: ana tags: trunk)
+++ no more data (1) +++
```

## Next

- [Embed in Go](/docs/get-started/embed-in-go/): the same steps from a Go program.
- [First sync](/docs/get-started/first-sync/): serve the repository and clone it.
- [CLI reference](/docs/reference/cli/): every command and flag.
- [Migrating from fossil](/docs/guides/migrating-from-fossil/): how the commands map to fossil's.
