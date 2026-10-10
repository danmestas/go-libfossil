// Package conformance checks go-libfossil against the Fossil Internet-Drafts
// in rfcs/, requirement by requirement.
//
// The package holds no library code; everything is in its tests. Each draft
// has a requirements table in requirements/, written from the draft text, and
// a test file whose cases name the requirement they exercise. Coverage is
// derived from the cases, never recorded by hand: a requirement with no case
// is a GAP, one whose cases all conform is COVERED (or PARTIAL when its table
// row says what the cases leave out), and one with a case that is known to
// fail is FAILING until the issue that case names is fixed.
//
// What counts as correct, in order:
//
//  1. The draft text, with the errata in rfcs/erratum.
//  2. fossil(1), run on the same input wherever the draft is silent or an
//     erratum is unsettled. A case that expects something fossil(1) does not
//     do must say why.
//  3. zit and libfossil, whose own suites inform which cases to write.
//
// A case that fails is marked with the issue tracking the fix and is skipped;
// once it passes, it fails until the mark is removed, so the list of known
// failures cannot go stale. Set CONFORMANCE_REPORT to a file path to have the
// coverage test write a Markdown report there.
package conformance
