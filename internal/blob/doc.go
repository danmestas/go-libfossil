// Package blob handles content-addressed blob storage in Fossil
// repository databases.
//
// Fossil's blob format is a 4-byte big-endian uncompressed-size prefix
// followed by zlib-compressed data. [Compress] and [Decompress] handle
// this encoding transparently.
//
// [Store] compresses content, computes its SHA1 hash, and inserts it
// into the blob table. [Load] retrieves and decompresses a blob by RID.
// [StoreDeltaRaw] stores content that arrived over the wire already
// delta-encoded, without expanding it. [EncodeForStorage] is the shared
// decision point for whether a receive path can skip [Compress] entirely
// and write already-encoded wire bytes straight to blob.content.
//
// This package does not decide what gets delta-encoded. Deltifying an
// artifact that is already stored means rewriting an existing row, not
// inserting a new one, and the decision needs to expand content, which
// this package cannot do without importing internal/content. Both live
// in content.Deltify, which is the only place the policy is stated.
// [StoreDelta] inserts a new row as a delta and has no callers outside
// tests; it is not the commit path's primitive.
package blob
