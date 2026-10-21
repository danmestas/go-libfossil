package cone

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/hash"
)

func mustNew(t *testing.T, include, exclude []string) *Cone {
	t.Helper()
	c, err := New(include, exclude)
	if err != nil {
		t.Fatalf("New(%q, %q): %v", include, exclude, err)
	}
	return c
}

func TestNewNormalizesSortsAndValidates(t *testing.T) {
	c := mustNew(t, []string{"src/vendor", "src", "docs/rfcs", "src/vendor/deep"}, nil)
	if got := strings.Join(c.Include(), ","); got != "docs/rfcs,src" {
		t.Fatalf("Include = %q, want docs/rfcs,src", got)
	}
	if len(c.Exclude()) != 0 {
		t.Fatalf("Exclude = %q, want none", c.Exclude())
	}

	for _, tc := range []struct {
		include, exclude []string
		want             error
	}{
		{nil, nil, ErrBounds},
		{[]string{"a//b"}, nil, ErrInvalidMember},
		{[]string{"../out"}, nil, ErrInvalidMember},
		{[]string{"./a"}, nil, ErrInvalidMember},
		{[]string{"a/"}, nil, ErrInvalidMember},
		{[]string{"/abs"}, nil, ErrInvalidMember},
		{[]string{"a\\b"}, nil, ErrInvalidMember},
		{[]string{""}, nil, ErrInvalidMember},
		{[]string{"bad\xffutf8"}, nil, ErrInvalidMember},
		{[]string{".fslckout"}, nil, ErrReservedMember},
		{[]string{"sub/_FOSSIL_"}, nil, ErrReservedMember},
		{[]string{"src"}, []string{"src//v"}, ErrInvalidMember},
	} {
		if _, err := New(tc.include, tc.exclude); !errors.Is(err, tc.want) {
			t.Errorf("New(%q, %q) = %v, want %v", tc.include, tc.exclude, err, tc.want)
		}
	}
}

func TestNewBounds(t *testing.T) {
	names := make([]string, MaxMembers+1)
	for i := range names {
		names[i] = fmt.Sprintf("d%d", i)
	}
	if _, err := New(names[:MaxMembers], nil); err != nil {
		t.Fatalf("%d members: %v", MaxMembers, err)
	}
	if _, err := New(names, nil); !errors.Is(err, ErrBounds) {
		t.Fatalf("%d members: %v, want ErrBounds", len(names), err)
	}
	// The cap is joint across polarities.
	if _, err := New(names[:1], names[1:]); !errors.Is(err, ErrBounds) {
		t.Fatalf("joint cap: %v, want ErrBounds", err)
	}
	long := strings.Repeat("a", MaxMemberLen+1)
	if _, err := New([]string{long}, nil); !errors.Is(err, ErrInvalidMember) {
		t.Fatalf("long member: %v, want ErrInvalidMember", err)
	}
	if _, err := New([]string{long[:MaxMemberLen]}, nil); err != nil {
		t.Fatalf("member at the cap: %v", err)
	}
}

func TestNewExclusionNormalization(t *testing.T) {
	c := mustNew(t, []string{"src", "src/vendor/keep"}, []string{"src/vendor", "src/vendor/junk"})
	// src/vendor/junk is redundant (its nearest covering member is an
	// exclusion too); src/vendor/keep is a re-inclusion and survives.
	if got := strings.Join(c.Include(), ","); got != "src,src/vendor/keep" {
		t.Fatalf("Include = %q", got)
	}
	if got := strings.Join(c.Exclude(), ","); got != "src/vendor" {
		t.Fatalf("Exclude = %q", got)
	}

	for _, tc := range []struct {
		include, exclude []string
		want             error
	}{
		{[]string{"src"}, []string{"tools"}, ErrDeadExclusion},
		{[]string{"src"}, []string{"src"}, ErrContradiction},
		{nil, []string{"src/vendor"}, ErrBounds},
		// Covered by an exclusion, so a redundant exclusion, never dead.
		{[]string{"src", "src/a/b"}, []string{"src/a", "src/a/b/c"}, nil},
	} {
		if _, err := New(tc.include, tc.exclude); !errors.Is(err, tc.want) {
			t.Errorf("New(%q, %q) = %v, want %v", tc.include, tc.exclude, err, tc.want)
		}
	}

	// A dash-sibling sorts between a member and its subtree ('-' < '/') and
	// must not sever the ancestor chain for later entries.
	dashed := mustNew(t, []string{"src", "src-other"}, []string{"src/vendor"})
	if len(dashed.Include()) != 2 || len(dashed.Exclude()) != 1 {
		t.Fatalf("dashed = %q / %q", dashed.Include(), dashed.Exclude())
	}
	if dashed.Covers("src/vendor/x") || !dashed.Covers("src-other/y") {
		t.Fatal("dash-sibling broke the exclusion")
	}
	// Exact duplicates collapse.
	dup := mustNew(t, []string{"a", "a"}, []string{"a/b", "a/b"})
	if len(dup.Include()) != 1 || len(dup.Exclude()) != 1 {
		t.Fatalf("dup = %q / %q", dup.Include(), dup.Exclude())
	}
}

func TestCoversBoundaries(t *testing.T) {
	c := mustNew(t, []string{"src", "docs/rfcs"}, nil)
	for path, want := range map[string]bool{
		"src":                 true,
		"src/cone.go":         true,
		"docs/rfcs/draft.txt": true,
		"src2/file":           false,
		"srcx":                false,
		"docs":                false,
		"docs/plans/x":        false,
		"src.txt":             false, // '.' sorts before '/'
	} {
		if got := c.Covers(path); got != want {
			t.Errorf("Covers(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestCoversLongestMatchAcrossPolarities(t *testing.T) {
	c := mustNew(t, []string{"src", "src/vendor/keep"}, []string{"src/vendor"})
	for path, want := range map[string]bool{
		"src/a.go":                 true,
		"src/vendor":               false,
		"src/vendor/lib.c":         false,
		"src/vendor/keep":          true,
		"src/vendor/keep/deep.txt": true,
		"src/vendorx":              true,
		"src/vendor.txt":           true,
		"README":                   false,
	} {
		if got := c.Covers(path); got != want {
			t.Errorf("Covers(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestDecideNamesTheDecidingMember(t *testing.T) {
	c := mustNew(t, []string{"src", "src/vendor/keep"}, []string{"src/vendor"})
	for _, tc := range []struct {
		path, member string
		covered      bool
	}{
		{"src/a.go", "src", true},
		{"src/vendor/lib.c", "src/vendor", false},
		{"src/vendor/keep/deep.txt", "src/vendor/keep", true},
		{"README", "", false},
	} {
		d := c.Decide(tc.path)
		if d.Covered != tc.covered || d.Member != tc.member {
			t.Errorf("Decide(%q) = %+v, want covered=%v member=%q", tc.path, d, tc.covered, tc.member)
		}
		if d.Covered != c.Covers(tc.path) {
			t.Errorf("Decide and Covers disagree on %q", tc.path)
		}
		if d.Excluded() != (d.Member != "" && !d.Covered) {
			t.Errorf("Decide(%q).Excluded() = %v", tc.path, d.Excluded())
		}
	}
}

func TestIntersectsSubtree(t *testing.T) {
	c := mustNew(t, []string{"src/lib"}, nil)
	for dir, want := range map[string]bool{
		"src/lib": true, "src/lib/deep": true, "src": true, "srcx": false, "tools": false,
	} {
		if got := c.IntersectsSubtree(dir); got != want {
			t.Errorf("IntersectsSubtree(%q) = %v, want %v", dir, got, want)
		}
	}
	nested := mustNew(t, []string{"src", "src/vendor/keep"}, []string{"src/vendor"})
	if !nested.IntersectsSubtree("src/vendor") {
		t.Error("a re-inclusion must keep its excluded ancestor intersecting")
	}
}

func TestMayCover(t *testing.T) {
	c := mustNew(t, []string{"src"}, []string{"src/vendor"})
	for _, tc := range []struct {
		first, limit string
		want         bool
	}{
		{"src/a", "src/z", true},
		{"tools/a", "", false},
		{"README", "docs/z", false},
		{"README", "tools", true},
		{"README", "src", false}, // the limit is exclusive
		{"README", "src.txt", true},
		{"src0", "", false}, // past "src/" + anything
		{"src.txt", "src/a", true},
		{"a", "", true},
	} {
		if got := c.MayCover(tc.first, tc.limit); got != tc.want {
			t.Errorf("MayCover(%q, %q) = %v, want %v", tc.first, tc.limit, got, tc.want)
		}
	}
}

// The serialized forms below are byte-for-byte what zit's cone.zig writes:
// version 1 (count byte, then per member a u16 LE length and the bytes) for a
// cone with no exclusions, and the 0x00-led version 2 form (version byte,
// total count, then per member a polarity flag, u16 LE length and bytes, in
// one sorted order across both polarities) for one with exclusions.
func TestSerializeVectors(t *testing.T) {
	for _, tc := range []struct {
		include, exclude []string
		hex              string
	}{
		{[]string{"src"}, nil, "01" + "0300" + hex.EncodeToString([]byte("src"))},
		{[]string{"b/dir", "a"}, nil, "02" + "0100" + "61" + "0500" + hex.EncodeToString([]byte("b/dir"))},
		{
			[]string{"src", "src/vendor/keep"}, []string{"src/vendor"},
			"00" + "02" + "03" +
				"00" + "0300" + hex.EncodeToString([]byte("src")) +
				"01" + "0a00" + hex.EncodeToString([]byte("src/vendor")) +
				"00" + "0f00" + hex.EncodeToString([]byte("src/vendor/keep")),
		},
	} {
		c := mustNew(t, tc.include, tc.exclude)
		if got := hex.EncodeToString(c.Serialize()); got != tc.hex {
			t.Errorf("Serialize(%q, %q) = %s, want %s", tc.include, tc.exclude, got, tc.hex)
		}
	}
}

func TestParseRoundTripAndFailsClosed(t *testing.T) {
	for _, c := range []*Cone{
		mustNew(t, []string{"b/dir", "a"}, nil),
		mustNew(t, []string{"src", "src/vendor/keep"}, []string{"src/vendor"}),
	} {
		data := c.Serialize()
		back, err := Parse(data)
		if err != nil {
			t.Fatalf("Parse(%x): %v", data, err)
		}
		if !bytes.Equal(back.Serialize(), data) || !back.Equal(c) {
			t.Fatalf("round trip changed %x", data)
		}
		if _, err := Parse(append(append([]byte{}, data...), 'x')); !errors.Is(err, ErrInvalidMember) {
			t.Errorf("residue: %v", err)
		}
		if _, err := Parse(data[:len(data)-1]); !errors.Is(err, ErrInvalidMember) {
			t.Errorf("truncation: %v", err)
		}
	}

	// Not canonical: "a/sub" is covered by "a", so normalization would drop
	// it; stored state fails closed rather than self-repairs.
	forged := []byte{2, 1, 0, 'a', 5, 0, 'a', '/', 's', 'u', 'b'}
	if _, err := Parse(forged); !errors.Is(err, ErrInvalidMember) {
		t.Errorf("non-canonical: %v", err)
	}
	// Not canonical: members out of order.
	if _, err := Parse([]byte{2, 1, 0, 'b', 1, 0, 'a'}); !errors.Is(err, ErrInvalidMember) {
		t.Errorf("unsorted: %v", err)
	}
	for _, tc := range []struct {
		data []byte
		want error
	}{
		{nil, ErrInvalidMember},
		{[]byte{0}, ErrInvalidMember},       // a truncated versioned lead-in
		{[]byte{0, 2, 0}, ErrBounds},        // versioned, zero members
		{[]byte{0, 3, 1}, ErrInvalidMember}, // an unknown version is refused
		{[]byte{0, 2, 1, 2, 1, 0, 'a'}, ErrInvalidMember},
		{[]byte{1, 0, 0}, ErrInvalidMember}, // a zero-length member
	} {
		if _, err := Parse(tc.data); !errors.Is(err, tc.want) {
			t.Errorf("Parse(%x) = %v, want %v", tc.data, err, tc.want)
		}
	}
}

func TestDigestStableUnderPermutation(t *testing.T) {
	forward := mustNew(t, []string{"src", "docs"}, nil)
	backward := mustNew(t, []string{"docs", "src", "src/sub"}, nil)
	other := mustNew(t, []string{"src", "docs/rfcs"}, nil)
	excluded := mustNew(t, []string{"src", "docs"}, []string{"src/vendor"})
	if forward.Digest() != backward.Digest() {
		t.Error("digest depends on input order")
	}
	if forward.Digest() == other.Digest() || forward.Digest() == excluded.Digest() {
		t.Error("digest ignores coverage")
	}
	// SHA3-256 over the canonical form, as zit keys its caches; the
	// conformance suite pins the values against zit itself.
	if got, want := forward.Digest(), hash.SHA3(forward.Serialize()); got != want {
		t.Fatalf("Digest = %s, want %s", got, want)
	}
}

func TestAmendment(t *testing.T) {
	c := mustNew(t, []string{"src"}, []string{"src/vendor"})
	wider, err := c.With([]string{"docs"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(wider.Include(), ","); got != "docs,src" {
		t.Fatalf("With: %q", got)
	}
	replaced := mustNew(t, []string{"docs", "src"}, []string{"src/vendor"})
	if !wider.Equal(replaced) {
		t.Fatal("incremental and wholesale amendment disagree")
	}
	narrower, err := wider.Without([]string{"src/vendor", "docs"})
	if err != nil {
		t.Fatal(err)
	}
	if !narrower.Equal(mustNew(t, []string{"src"}, nil)) {
		t.Fatalf("Without: %q / %q", narrower.Include(), narrower.Exclude())
	}
	if _, err := wider.Without([]string{"src"}); !errors.Is(err, ErrDeadExclusion) {
		t.Fatalf("removing the inclusion over an exclusion: %v", err)
	}
	if _, err := c.Without([]string{"src"}); !errors.Is(err, ErrBounds) {
		t.Fatalf("removing every inclusion: %v", err)
	}
	// As in zit, a path that is not a member removes nothing.
	same, err := c.Without([]string{"tools"})
	if err != nil || !same.Equal(c) {
		t.Fatalf("removing a non-member: %v", err)
	}
}

func TestStringNamesTheCone(t *testing.T) {
	c := mustNew(t, []string{"src", "src/vendor/keep"}, []string{"src/vendor"})
	if got := c.String(); got != "src, !src/vendor, src/vendor/keep" {
		t.Fatalf("String = %q", got)
	}
}
