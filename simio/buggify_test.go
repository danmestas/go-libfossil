package simio

import "testing"

func TestBuggifyDisabledByDefault(t *testing.T) {
	// Should never fire when not enabled.
	for range 1000 {
		if Buggify(1.0) {
			t.Fatal("Buggify fired when not enabled")
		}
	}
}

func TestBuggifyEnabledFires(t *testing.T) {
	EnableBuggify(42)
	defer DisableBuggify()

	// With probability 1.0, should always fire.
	if !Buggify(1.0) {
		t.Fatal("Buggify(1.0) should always fire when enabled")
	}

	// With probability 0.0, should never fire.
	if Buggify(0.0) {
		t.Fatal("Buggify(0.0) should never fire")
	}
}

func TestBuggifyDeterministic(t *testing.T) {
	run := func(seed int64) int {
		EnableBuggify(seed)
		defer DisableBuggify()

		count := 0
		for range 1000 {
			if Buggify(0.5) {
				count++
			}
		}
		return count
	}

	c1 := run(99)
	c2 := run(99)
	if c1 != c2 {
		t.Fatalf("non-deterministic: %d vs %d", c1, c2)
	}

	// Different seed should produce different count (with high probability).
	c3 := run(7)
	if c1 == c3 {
		t.Logf("WARNING: different seeds produced same count %d (unlikely)", c1)
	}
}

func TestBuggifyDisableStops(t *testing.T) {
	EnableBuggify(1)
	if !Buggify(1.0) {
		t.Fatal("should fire when enabled")
	}

	DisableBuggify()
	if Buggify(1.0) {
		t.Fatal("should not fire after disable")
	}
}

func TestSuspendBuggifyRestores(t *testing.T) {
	EnableBuggify(1)
	defer DisableBuggify()
	resume := SuspendBuggify()
	inner := SuspendBuggify()
	if Buggify(1.0) {
		t.Fatal("should not fire while suspended")
	}
	inner()
	if Buggify(1.0) {
		t.Fatal("an inner resume must not end the outer suspension")
	}
	resume()
	if !Buggify(1.0) {
		t.Fatal("should fire again after resume")
	}
}
