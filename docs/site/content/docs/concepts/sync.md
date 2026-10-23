---
title: Sync
weight: 40
---

Two repositories exchange artifacts with Fossil's sync protocol: text cards carried in HTTP POST bodies, one request and one reply per round. go-libfossil implements both ends, and talks to stock fossil, zit and other go-libfossil peers. The governing text is `draft-fossil-sync-protocol-00`.

## Messages and cards

A message is a list of cards, one per line: a card name, then space-separated arguments, and for content cards a byte payload after the line (§5). Unknown pragmas are ignored, so a peer can add new ones without breaking old ones (§3.5).

The client POSTs each request to the repository URL (§4). The body is a 4-byte big-endian length followed by a zlib stream (§4.1). A clone reply may be sent uncompressed, because its `cfile` payloads are already compressed.

The cards that move artifacts:

| Card | Meaning | Draft |
|---|---|---|
| `igot HASH` | I hold this artifact. `igot HASH 1` marks it private. | §8.6 |
| `gimme HASH` | Send me this artifact. | §8.6 |
| `file HASH SIZE` | The artifact's bytes, or a delta against another artifact (`file HASH DELTASRC SIZE`). Used by push and pull. | §7.1 |
| `cfile HASH USIZE CSIZE` | The artifact as the sender stores it, compressed, sent verbatim. Used by clone. | §7.2 |
| `push` / `pull` | The operation, with the server and project codes. | §3.2 |
| `clone 3 SEQNO` / `clone_seqno N` | The clone loop's position. | §8.2 |
| `login USER NONCE SIGNATURE` | Authentication. | §6.1 |
| `pragma NAME ...` | Version negotiation and extensions. | §9 |
| `error` / `message` | Errors and notices. | §10 |

Unversioned files travel as `uvigot`, `uvgimme` and `uvfile` (§7.3, §8.5.1). Configuration travels as `reqconfig` and `config` (§9.3).

## Rounds

A session is a loop of rounds. Each round the client says what it holds (`igot`) and what it wants (`gimme`), and sends the content the server asked for in the last reply. The server answers in kind. A `gimme` is never sent for a shunned artifact or one the peer has said it lacks (§8.8).

The client decides when to stop. It issues another round only while the last round made progress: it stored content and still has phantoms (artifacts it knows by name but does not hold), it queued content to send, or one of the unversioned-file or clone conditions holds (§3.7). Phantoms alone do not keep the loop running. The server sends no completion card.

## Clone, pull, push

- **Clone** starts with `clone 3 1`. The server sends its stored blobs as `cfile` cards in batches and a `clone_seqno` saying where to continue; zero ends the transfer and one cleanup round follows (§8.1 to §8.4). Artifacts the rounds leave missing stay phantoms, as with fossil's clone, unless `CloneOpts.PullPhantoms` is set.
- **Pull** sends `pull` each round and receives what it lacks.
- **Push** sends `push` each round and answers the server's `gimme` cards with `file` cards.
- **Sync** sends both.

After artifacts arrive they are [crosslinked](/docs/concepts/repository-state/), so branches, tags and leaves follow from the artifacts each side holds, not from anything sent on the wire (§8.6.1).

## Login

A named user signs each request (§6). `NONCE` is the SHA-1 of the request body after the login line. The shared secret is `SHA1(project-code "/" login "/" password)`, and `SIGNATURE` is `SHA1(NONCE || shared-secret)`. The first clone round carries no login. A request with no login runs as `nobody`. Once the server announces a version that reads it, the login moves to the `x-f-l-c` cookie (§6.5).

The server checks the user's capabilities from its `user` table (§6.6). In a new repository `nobody` may clone and pull but not push; grant more with `Repo.SetCaps`.

## The server is stateless

Each request is handled on its own (§8.9). The server keeps no session between rounds, so any number of clients can sync against one repository, and a server can sit behind any HTTP stack.

```go
repo, err := libfossil.Open("repo.fossil")
if err != nil {
    log.Fatal(err)
}
defer repo.Close()

mux := http.NewServeMux()
mux.HandleFunc("/", repo.XferHandler())
mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {})
log.Fatal(http.ListenAndServe(":8080", mux))
```

- `Repo.XferHandler()` returns an `http.HandlerFunc` that decodes a request, handles it and encodes the reply. A non-POST request gets a plain page.
- `Repo.ServeHTTP(ctx, addr)` runs a server on `addr` until `ctx` is cancelled. `libfossil repo serve --addr 127.0.0.1:8080` does the same from the command line.
- `Repo.HandleSync(ctx, payload)` and `HandleSyncWithOpts` take and return raw message bytes, for an embedder with its own transport. Over HTTP, pass the `x-f-l-c` cookie in `HandleOpts.LoginCookie` and the client address in `HandleOpts.RemoteAddr`; `XferHandler` and `ServeHTTP` fill both themselves.

## The client

```go
t := libfossil.NewHTTPTransport("https://example.com/repo")

repo, res, err := libfossil.Clone(ctx, "repo.fossil", t, libfossil.CloneOpts{
    User: "alice", Password: "secret",
})

sr, err := repo.Sync(ctx, t, libfossil.SyncOpts{
    Push: true, Pull: true, User: "alice", Password: "secret",
})
```

`Transport` is one method, `RoundTrip(ctx, payload) ([]byte, error)`, so any byte pipe works. `NewHTTPTransport` takes `WithHTTPClient` for a custom `http.Client`. `Repo.Pull(ctx, url, PullOpts{})` is a pull-only shortcut over HTTP.

`SyncOpts` also turns on unversioned files (`UV`), private artifacts (`Private`, §7.5), fossil's `--share-links` (`ShareLinks`, §9.5) and go-libfossil's own table sync (`XTableSync`). `SyncResult` reports rounds, files and bytes each way, and errors.

The `libfossil` binary clones (`repo clone`) and serves (`repo serve`). Pull, push and sync are library calls.

## Check-in lock

A client that is about to commit can ask the server to hold the parent with `pragma ci-lock` (§9.4). The server records a `ci-lock-<parent hash>` row in its `config` table and reports a conflicting holder with `ci-lock-fail`. A lock goes stale after the repository's `lock-timeout` setting, 60 seconds when unset (`DefaultCkinLockTimeout`). Fossil uses the same row, so a repository served by both shares one lock table.

## Shunned artifacts

A shunned artifact is never requested, and one that arrives is dropped rather than stored. The shun list itself can travel as the `shun` configuration group (§9.3).

## Extensions

Pragmas carry the extensions. Each is ignored by a peer that does not know it.

- [Sparse cones](/docs/concepts/sparse-cones/): `pragma cone`, answered by `cone-served` (`draft-fossil-sparse-cone-00` §6).
- [Horizons](/docs/concepts/horizons/): `horizon-served`, the seed and the cursor (`draft-fossil-horizon-00` §6).
- [Sub-manifests](/docs/concepts/sub-manifests/): `send-submanifests` and `avoid-submanifests` (`draft-fossil-submanifests-00` §7.2).

## Where the code is

- **Client** (`internal/sync/client.go`, `clone.go`, `session.go`): drives the rounds. Supports clone, pull, push and sync, plus unversioned files and config table sync (`client_tablesync.go`).
- **Server** (`internal/sync/handler.go`, `serve_http.go`, `handler_uv.go`, `handler_tablesync.go`): one request in, one reply out.
- **Cards** (`internal/xfer`): encoding and decoding of messages and cards.
- **Check-in lock** (`internal/sync/ckin_lock.go`).
- **Table sync deletes** (`internal/sync/xdelete.go`): an `xdelete` card removes a synced table row, or leaves a tombstone where the row is absent.
- **Public entry points**: `Clone` in `fossil.go`; `Repo.Sync`, `HandleSync`, `XferHandler` and `ServeHTTP` in `repo_sync.go`; `Repo.Pull` in `repo_pull.go`; `NewHTTPTransport` in `transport.go`.
