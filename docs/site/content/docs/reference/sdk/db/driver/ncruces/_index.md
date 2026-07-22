---
title: db/driver/ncruces
weight: 20
---

Wasm-friendly SQLite driver based on `ncruces/go-sqlite3`.

```go
import _ "github.com/danmestas/go-libfossil/db/driver/ncruces"
```

Importing this package registers an `ncruces` driver (driver name `"sqlite3"`) with libfossil's `db` registry. The package exports no public API of its own — register-on-import is the contract.

On `js/wasm` targets the package additionally imports `go-sqlite3-opfs`, routing the OPFS virtual filesystem through the same driver registration so browser-side builds get persistent storage automatically.

Use this driver when you need a smaller Wasm footprint than `modernc` provides, or for build configurations where `ncruces`'s tradeoffs are preferred.

See [db](../..) for the registry interface.
