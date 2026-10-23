---
title: Documentation
cascade:
  type: docs
---

libfossil is a pure-Go library and CLI for reading and writing Fossil SCM repositories. Zero CGo. Static binaries. WASM-ready.

## What you'll find here

- **[Quickstart](./quickstart)** — install the CLI, clone a repo, and read its history from Go in five minutes.
- **[Architecture](./architecture)** — module layout, storage model, driver layer, and the deterministic simulator.
- **[Migration from Fossil](./migration-from-fossil)** — for upstream Fossil users: what's covered, what isn't, and how the CLI shapes differ.
- **[Sparse cones and horizons](./sparse-and-horizons)** — hold part of a repository by path or by age: checkout and store cones, content horizons, library and CLI usage.
- **[Extension Points](./extension-points)** — observer interfaces for telemetry, custom SQLite drivers, and the `db.DB` abstraction.
- **[Production patterns](./production)** — error handling, context, telemetry wiring beyond the quickstart's `log.Fatal` shortcuts.
- **[Testing](./testing)** — running the suite, the dual-driver matrix, and DST simulation with seeds and BUGGIFY.
- **[SDK reference](./reference/sdk)** — auto-generated Go API reference, one page per package.
