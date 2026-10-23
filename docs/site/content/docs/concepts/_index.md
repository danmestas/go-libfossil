---
title: Concepts
weight: 20
---

How a Fossil repository is put together, and how go-libfossil implements it. Each page names the Internet-Draft in [`conformance/rfcs/`](https://github.com/danmestas/go-libfossil/tree/main/conformance/rfcs) that specifies the behaviour.

- [Architecture](/docs/concepts/architecture/): packages, drivers, observers and the simulator.
- [Artifacts](/docs/concepts/artifacts/): the text manifests that make up history, and how they are named.
- [Repository state](/docs/concepts/repository-state/): the tables derived from the artifacts.
- [Sync](/docs/concepts/sync/): the card protocol that clones, pulls and pushes.
- [Sub-manifests](/docs/concepts/sub-manifests/): check-ins written as trees instead of flat file lists.
- [Sparse cones](/docs/concepts/sparse-cones/): holding part of a repository by path.
- [Horizons](/docs/concepts/horizons/): holding part of a repository by age.
- [Signing](/docs/concepts/signing/): clear-signed check-ins and signed horizon seeds.
