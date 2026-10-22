package horizon

// HeldSetWithin is HeldSet under a caller's visited-node bound. The tests
// are an external package because internal/content, which classifies by
// the held set, sits between this package and the manifest writer they use.
var HeldSetWithin = heldSet
