package cone

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

const (
	hashSHA1 = "0123456789abcdef0123456789abcdef01234567"
	hashSHA3 = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

func mustHorizon(t *testing.T, hash string, r HorizonRule, d HorizonDepth) Horizon {
	t.Helper()
	h, err := NewHorizon(hash, r, d)
	if err != nil {
		t.Fatalf("NewHorizon: %v", err)
	}
	return h
}

func mustNewWith(t *testing.T, include, exclude []string, h Horizon) *Cone {
	t.Helper()
	c, err := NewWithHorizon(include, exclude, h)
	if err != nil {
		t.Fatalf("NewWithHorizon: %v", err)
	}
	return c
}

// cat joins byte pieces, so an expected form reads as the format does.
func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func TestHorizonSerializeExactBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    *Cone
		want []byte
	}{
		{
			"horizon only, descendants, content, sha1",
			mustNewWith(t, nil, nil, mustHorizon(t, hashSHA1, RuleDescendants, DepthContent)),
			cat([]byte{0x00, 0x03, 0x00, 0x00, 0x00, 40}, []byte(hashSHA1)),
		},
		{
			"include, leaves, structural, sha1",
			mustNewWith(t, []string{"src"}, nil, mustHorizon(t, hashSHA1, RuleLeaves, DepthStructural)),
			cat([]byte{0x00, 0x03, 0x01, 0x01, 0x01, 40}, []byte(hashSHA1),
				[]byte{0x00, 0x03, 0x00}, []byte("src")),
		},
		{
			"include and exclude, descendants, structural, sha3",
			mustNewWith(t, []string{"a"}, []string{"a/b"}, mustHorizon(t, hashSHA3, RuleDescendants, DepthStructural)),
			cat([]byte{0x00, 0x03, 0x02, 0x00, 0x01, 64}, []byte(hashSHA3),
				[]byte{0x00, 0x01, 0x00}, []byte("a"), []byte{0x01, 0x03, 0x00}, []byte("a/b")),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.Serialize(); !bytes.Equal(got, tc.want) {
				t.Fatalf("Serialize = % x\nwant        % x", got, tc.want)
			}
			back, err := Parse(tc.want)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if !back.Equal(tc.c) {
				t.Fatalf("Parse(%q) = %v, want %v", tc.want, back, tc.c)
			}
		})
	}
}

func TestHorizonRoundTrip(t *testing.T) {
	for _, hs := range []string{hashSHA1, hashSHA3} {
		for _, r := range []HorizonRule{RuleDescendants, RuleLeaves} {
			for _, d := range []HorizonDepth{DepthContent, DepthStructural} {
				h := mustHorizon(t, hs, r, d)
				for _, c := range []*Cone{
					mustNewWith(t, nil, nil, h),
					mustNewWith(t, []string{"docs", "src"}, nil, h),
					mustNewWith(t, []string{"src"}, []string{"src/vendor", "src/gen"}, h),
				} {
					back, err := Parse(c.Serialize())
					if err != nil {
						t.Fatalf("Parse(%v): %v", c, err)
					}
					got, ok := back.Horizon()
					if !ok || got != h || !back.Equal(c) || back.Digest() != c.Digest() {
						t.Fatalf("round trip of %v gave %v", c, back)
					}
				}
			}
		}
	}
}

func TestHorizonParseRejections(t *testing.T) {
	good := cat([]byte{0x00, 0x03, 0x00, 0x00, 0x00, 40}, []byte(hashSHA1))
	if _, err := Parse(good); err != nil {
		t.Fatalf("baseline refused: %v", err)
	}
	mutate := func(f func(b []byte) []byte) []byte { return f(bytes.Clone(good)) }
	for name, data := range map[string][]byte{
		"unknown version 4":     mutate(func(b []byte) []byte { b[1] = 4; return b }),
		"unknown version 255":   mutate(func(b []byte) []byte { b[1] = 255; return b }),
		"unknown rule":          mutate(func(b []byte) []byte { b[3] = 2; return b }),
		"unknown depth":         mutate(func(b []byte) []byte { b[4] = 2; return b }),
		"hash length 39":        mutate(func(b []byte) []byte { b[5] = 39; return b[:len(b)-1] }),
		"hash length 41":        mutate(func(b []byte) []byte { b[5] = 41; return append(b, '0') }),
		"hash length 0":         {0x00, 0x03, 0x00, 0x00, 0x00, 0x00},
		"hash truncated":        good[:len(good)-1],
		"header truncated":      good[:5],
		"non-hex hash":          mutate(func(b []byte) []byte { b[10] = 'g'; return b }),
		"upper-case hash":       mutate(func(b []byte) []byte { b[10] = 'A'; return b }),
		"bytes after horizon":   append(bytes.Clone(good), 0),
		"count without members": mutate(func(b []byte) []byte { b[2] = 1; return b }),
		"count over max":        mutate(func(b []byte) []byte { b[2] = 255; return b }),
		"bare v3 lead-in":       {0x00, 0x03},
	} {
		t.Run(name, func(t *testing.T) {
			if c, err := Parse(data); err == nil {
				t.Fatalf("Parse accepted % x as %v", data, c)
			}
		})
	}
	// Count 0 without a horizon, in each form that could carry it.
	for _, data := range [][]byte{{0x00, 0x02, 0x00}} {
		if _, err := Parse(data); !errors.Is(err, ErrBounds) {
			t.Fatalf("Parse(% x) = %v, want ErrBounds", data, err)
		}
	}
	// A non-canonical member order is refused as in version 2.
	unsorted := cat([]byte{0x00, 0x03, 0x02, 0x00, 0x00, 40}, []byte(hashSHA1),
		[]byte{0x00, 0x01, 0x00}, []byte("b"), []byte{0x00, 0x01, 0x00}, []byte("a"))
	if _, err := Parse(unsorted); err == nil {
		t.Fatal("unsorted members accepted")
	}
}

func TestHorizonValidation(t *testing.T) {
	for _, tc := range []struct {
		hash string
		r    HorizonRule
		d    HorizonDepth
	}{
		{hashSHA1[:39], RuleDescendants, DepthContent},
		{hashSHA1 + "0", RuleDescendants, DepthContent},
		{strings.Repeat("z", 40), RuleDescendants, DepthContent},
		{hashSHA1, 2, DepthContent},
		{hashSHA1, RuleLeaves, 2},
	} {
		if _, err := NewHorizon(tc.hash, tc.r, tc.d); !errors.Is(err, ErrInvalidHorizon) {
			t.Errorf("NewHorizon(%q, %v, %v) = %v, want ErrInvalidHorizon", tc.hash, tc.r, tc.d, err)
		}
	}
	h := mustHorizon(t, strings.ToUpper(hashSHA1), RuleLeaves, DepthStructural)
	if h.Hash() != hashSHA1 || h.Rule() != RuleLeaves || h.Depth() != DepthStructural {
		t.Fatalf("horizon = %v, want lower-cased hash", h)
	}
	if _, err := NewWithHorizon(nil, nil, Horizon{}); !errors.Is(err, ErrInvalidHorizon) {
		t.Fatalf("zero Horizon accepted: %v", err)
	}
	// Excluding from nothing is still a dead exclusion.
	if _, err := NewWithHorizon(nil, []string{"a"}, h); !errors.Is(err, ErrDeadExclusion) {
		t.Fatalf("horizon-only with exclusion: %v", err)
	}
}

func TestHorizonNames(t *testing.T) {
	for name, want := range map[string]HorizonRule{"descendants": RuleDescendants, "leaves": RuleLeaves} {
		got, err := ParseHorizonRule(name)
		if err != nil || got != want || got.String() != name {
			t.Errorf("rule %q = %v, %v", name, got, err)
		}
	}
	for name, want := range map[string]HorizonDepth{"content": DepthContent, "structural": DepthStructural} {
		got, err := ParseHorizonDepth(name)
		if err != nil || got != want || got.String() != name {
			t.Errorf("depth %q = %v, %v", name, got, err)
		}
	}
	if _, err := ParseHorizonRule("Leaves"); !errors.Is(err, ErrInvalidHorizon) {
		t.Errorf("rule parse is by exact name: %v", err)
	}
	if _, err := ParseHorizonDepth("deep"); !errors.Is(err, ErrInvalidHorizon) {
		t.Errorf("depth: %v", err)
	}
}

func TestHorizonOnlyIsPathDense(t *testing.T) {
	c := mustNewWith(t, nil, nil, mustHorizon(t, hashSHA1, RuleDescendants, DepthContent))
	if d := c.Decide("any/path"); !d.Covered || d.Member != "" || d.Excluded() {
		t.Fatalf("Decide = %+v, want covered with no member", d)
	}
	if !c.Covers("x") || !c.IntersectsSubtree("a/b") || !c.MayCover("a", "b") || !c.MayCover("", "") {
		t.Fatal("horizon-only cone must behave as everything covered")
	}
	if len(c.Include()) != 0 || len(c.Exclude()) != 0 {
		t.Fatal("horizon-only cone has path members")
	}
}

func TestHorizonPreservedByAmendmentsAndEqual(t *testing.T) {
	h := mustHorizon(t, hashSHA1, RuleLeaves, DepthContent)
	other := mustHorizon(t, hashSHA1, RuleDescendants, DepthContent)
	c := mustNewWith(t, []string{"src"}, nil, h)

	with, err := c.With([]string{"docs"}, []string{"src/vendor"})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := with.Horizon(); !ok || got != h {
		t.Fatalf("With lost the horizon: %v", with)
	}
	without, err := with.Without([]string{"src", "src/vendor"})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := without.Horizon(); !ok || got != h || without.Include()[0] != "docs" {
		t.Fatalf("Without lost the horizon: %v", without)
	}
	// Removing the last inclusion leaves a horizon-only cone, not an error.
	bare, err := c.Without([]string{"src"})
	if err != nil || !bare.pathDense() {
		t.Fatalf("Without last inclusion = %v, %v", bare, err)
	}

	plain := mustNew(t, []string{"src"}, nil)
	if c.Equal(plain) || plain.Equal(c) {
		t.Fatal("a cone with a horizon equals one without")
	}
	if c.Equal(mustNewWith(t, []string{"src"}, nil, other)) {
		t.Fatal("cones with different rules are equal")
	}
	if !c.Equal(mustNewWith(t, []string{"src"}, nil, h)) {
		t.Fatal("identical cones are unequal")
	}
	if _, ok := plain.Horizon(); ok {
		t.Fatal("plain cone reports a horizon")
	}
	if got := c.String(); !strings.Contains(got, hashSHA1) || !strings.Contains(got, "src") {
		t.Fatalf("String = %q", got)
	}
}

func TestHorizonChangesDigestAndKeepsOldForms(t *testing.T) {
	plain := mustNew(t, []string{"src"}, nil)
	withH := mustNewWith(t, []string{"src"}, nil, mustHorizon(t, hashSHA1, RuleDescendants, DepthContent))
	if plain.Digest() == withH.Digest() {
		t.Fatal("horizon did not change the digest")
	}
	if got, want := plain.Serialize(), cat([]byte{1, 3, 0}, []byte("src")); !bytes.Equal(got, want) {
		t.Fatalf("v1 form = % x", got)
	}
	ex := mustNew(t, []string{"a"}, []string{"a/b"})
	want := cat([]byte{0, 2, 2, 0, 1, 0}, []byte("a"), []byte{1, 3, 0}, []byte("a/b"))
	if got := ex.Serialize(); !bytes.Equal(got, want) {
		t.Fatalf("v2 form = % x", got)
	}
}
