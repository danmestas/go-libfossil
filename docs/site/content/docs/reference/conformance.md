---
title: Conformance
weight: 40
layout: conformance
description: Every requirement of every draft go-libfossil implements, with its status, from the conformance test suite.
---

go-libfossil is checked against the Internet-Drafts that specify Fossil's formats and protocol, and the drafts for the extensions it shares with zit. The drafts are pinned in [`conformance/rfcs/`](https://github.com/danmestas/go-libfossil/tree/main/conformance/rfcs). Each has a table of requirements in [`conformance/requirements/`](https://github.com/danmestas/go-libfossil/tree/main/conformance/requirements), written from the draft text, and test cases that name the requirement they exercise.

`TestCoverage` derives each requirement's status from its cases; nothing is recorded by hand:

- **covered**: every case conforms.
- **partial**: the cases conform, and the table row says what they leave out (shown under the requirement).
- **deviation**: a case documents a deliberate difference, with its reason.
- **failing**: a case is known to fail and names the issue tracking the fix. It is skipped, and fails once it passes until the mark is removed.
- **gap**: no case exercises the requirement.

What counts as correct, in order: the draft text; then fossil itself, run on the same input, where the draft is silent. The suites of zit and libfossil inform which cases to write. The tables below are generated from a run of the suite (`make docs-gen`), and CI fails when they are out of date. To reproduce them:

```sh
CONFORMANCE_REPORT=report.md go test ./conformance -run '^TestCoverage$' -count=1
```
