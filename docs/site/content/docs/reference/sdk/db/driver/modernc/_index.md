---
title: db/driver/modernc
weight: 10
---

Pure-Go SQLite driver, the libfossil default.

```go
import _ "github.com/danmestas/go-libfossil/db/driver/modernc"
```

Importing this package registers a `modernc.org/sqlite` driver (driver name `"sqlite"`) with libfossil's `db` registry. The package exports no public API of its own — register-on-import is the contract.

Use this driver when you want a static, CGo-free build with no native dependencies.

See [db](../..) for the registry interface.
