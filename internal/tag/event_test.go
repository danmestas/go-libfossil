package tag

import (
	"testing"

	"github.com/danmestas/go-libfossil/internal/deck"
)

// The expected comments are the ones fossil 2.28 wrote for the same cards.
const (
	ciA = "de9059422df041e4bd1c5c9f5c06c1e5b4a2b1a3"
	ciB = "7b35f1e935e516b7aa0d8a2c4c1e7e0f6a8b9c0d"
)

func TestControlComment(t *testing.T) {
	tag := func(typ deck.TagType, name, uuid, value string) deck.TagCard {
		return deck.TagCard{Type: typ, Name: name, UUID: uuid, Value: value}
	}
	cases := []struct {
		name string
		tags []deck.TagCard
		want string
	}{
		{"sym add", []deck.TagCard{tag('+', "sym-rel-1", ciA, "")},
			`Edit [de9059422df041e4|de9059422d]: Add tag "rel-1".`},
		{"sym note escaped", []deck.TagCard{tag('+', "sym-note", ciA, `val "q" <b>`)},
			`Edit [de9059422df041e4|de9059422d]: Add tag "note" with note "val &quot;q&quot; &lt;b&gt;".`},
		{"sym propagating", []deck.TagCard{tag('*', "sym-prop", ciA, "pv")},
			`Edit [de9059422df041e4|de9059422d]: Add propagating tag "prop" with note "pv".`},
		{"sym cancel", []deck.TagCard{tag('-', "sym-rel-1", ciA, "")},
			`Edit [de9059422df041e4|de9059422d]: Cancel tag "rel-1".`},
		{"raw add", []deck.TagCard{tag('+', "plain", ciA, "")},
			`Edit [de9059422df041e4|de9059422d]: Add "plain".`},
		{"raw propagating", []deck.TagCard{tag('*', "hidden", ciA, "")},
			`Edit [de9059422df041e4|de9059422d]: Add propagating "hidden".`},
		{"raw cancel value", []deck.TagCard{tag('-', "zap", ciA, "why")},
			`Edit [de9059422df041e4|de9059422d]: Cancel "zap" with value "why".`},
		{"comment", []deck.TagCard{tag('+', "comment", ciA, "x")},
			`Edit [de9059422df041e4|de9059422d]: Edit check-in comment.`},
		{"date", []deck.TagCard{tag('+', "date", ciA, "2026-01-02 03:04:05")},
			`Edit [de9059422df041e4|de9059422d]: Timestamp 2026-01-02 03:04:05.`},
		{"branch color", []deck.TagCard{tag('*', "bgcolor", ciA, "#eee")},
			`Edit [de9059422df041e4|de9059422d]: Change branch background color to "#eee".`},
		{"cancel color", []deck.TagCard{tag('-', "bgcolor", ciA, "")},
			`Edit [de9059422df041e4|de9059422d]: Cancel background color.`},
		{"closed", []deck.TagCard{tag('-', "closed", ciA, "")},
			`Edit [de9059422df041e4|de9059422d]: Remove the "Closed" mark.`},
		{"sorted by name", []deck.TagCard{
			tag('+', "user", ciA, "carol"), tag('+', "sym-t'9", ciA, ""),
			tag('+', "closed", ciA, ""), tag('+', "bgcolor", ciA, "#a1a")},
			`Edit [de9059422df041e4|de9059422d]: Change background color to "#a1a". Mark "Closed".` +
				` Add tag "t&#39;9". Change user to "carol".`},
		{"branch move, two targets", []deck.TagCard{
			tag('*', "branch", ciB, "x y&z/w"), tag('*', "sym-x y&z/w", ciB, ""),
			tag('+', "sym-one", ciB, ""), tag('+', "sym-one", ciA, "n<1>"),
			tag('-', "sym-trunk", ciB, ""), tag('-', "zap", ciA, "why")},
			`Edit [7b35f1e935e516b7|7b35f1e935]: Move to branch` +
				` [/timeline?r=x+y%26z%2Fw&nd&dp=7b35f1e935e516b7&unhide | x y&amp;z/w].` +
				` Add tag "one". Edit [de9059422df041e4|de9059422d]: Add tag "one" with note "n&lt;1&gt;".` +
				` Cancel "zap" with value "why".`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := controlComment(&deck.Deck{T: c.tags}, 10, 16)
			if got != c.want {
				t.Errorf("comment:\n got %q\nwant %q", got, c.want)
			}
		})
	}
}
