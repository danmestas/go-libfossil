package conformance

import (
	"fmt"
	"sort"
	"strings"
)

// Scripts are built from these helpers. Every artifact is dated within the
// minute after 2026-01-01T00:00:00, at the second its case gives it, so
// artifacts order by explicit times and equal times appear only where a
// case wants them. Derived rows print a time as "@S", that second.

// rsTime is the D-card timestamp for second s.
func rsTime(s int) string {
	return fmt.Sprintf("2026-01-01T00:%02d:%02d", s/60, s%60)
}

// checkin is a check-in with no files. tags are T-card bodies, such as
// "*branch * trunk"; they are sorted, as the T-card rules require.
func checkin(label string, s int, parents []string, tags ...string) rsArt {
	var b strings.Builder
	b.WriteString("C " + label + "\n")
	b.WriteString("D " + rsTime(s) + "\n")
	if len(parents) > 0 {
		b.WriteString("P " + strings.Join(parents, " ") + "\n")
	}
	sorted := append([]string(nil), tags...)
	sort.Strings(sorted)
	for _, tag := range sorted {
		b.WriteString("T " + tag + "\n")
	}
	b.WriteString("U u\n")
	return rsArt{label: label, body: b.String()}
}

// control is a control artifact carrying tags, sorted by their template
// text; a case whose cards name the same tag twice writes its body itself.
func control(label string, s int, tags ...string) rsArt {
	sorted := append([]string(nil), tags...)
	sort.Strings(sorted)
	var b strings.Builder
	b.WriteString("D " + rsTime(s) + "\n")
	for _, tag := range sorted {
		b.WriteString("T " + tag + "\n")
	}
	b.WriteString("U u\n")
	return rsArt{label: label, body: b.String()}
}

// technote is an event artifact dated s, with the given T-cards.
func technote(label string, s int, tags ...string) rsArt {
	var b strings.Builder
	b.WriteString("C " + label + "\n")
	b.WriteString("D " + rsTime(s) + "\n")
	b.WriteString("E " + rsTime(s) + " 0123456789abcdef0123456789abcdef01234567\n")
	for _, tag := range tags {
		b.WriteString("T " + tag + "\n")
	}
	b.WriteString("U u\nW 4\nnote\n")
	return rsArt{label: label, body: b.String()}
}

// literal is an artifact whose body the case writes card by card.
func literal(label, body string) rsArt { return rsArt{label: label, body: body} }

// file is the content of a file a check-in names.
func file(label, content string) rsArt { return rsArt{label: label, body: content, file: true} }

// of names earlier artifacts for a P card.
func of(labels ...string) []string {
	refs := make([]string, len(labels))
	for i, label := range labels {
		refs[i] = "{" + label + "}"
	}
	return refs
}

// rsRoot are the tags every scripted root carries: without them `fossil
// rebuild` tags the root itself, storing artifacts the script does not hold.
var rsRoot = []string{"*branch * trunk", "*sym-trunk *"}

// rsFeat moves a check-in onto branch feat.
var rsFeat = []string{"*branch * feat", "*sym-feat *", "-sym-trunk *"}

// chain is c1, the root, and c2, c3 and c4, each the primary child of the
// one before, at seconds 1 to 4.
func chain() []rsArt {
	return []rsArt{
		checkin("c1", 1, nil, rsRoot...),
		checkin("c2", 2, of("c1")),
		checkin("c3", 3, of("c2")),
		checkin("c4", 4, of("c3")),
	}
}

// merged is c1 with two children: c2 on trunk and b1 on feat; m, on
// trunk, has primary parent c2 and merge parent b1.
func merged() []rsArt {
	return []rsArt{
		checkin("c1", 1, nil, rsRoot...),
		checkin("c2", 2, of("c1")),
		checkin("b1", 3, of("c1"), rsFeat...),
		checkin("m", 4, of("c2", "b1")),
	}
}

// branched is c1 with b1 on feat, b2 after b1, and c2 on trunk.
func branched() []rsArt {
	return []rsArt{
		checkin("c1", 1, nil, rsRoot...),
		checkin("b1", 2, of("c1"), rsFeat...),
		checkin("b2", 3, of("b1")),
		checkin("c2", 4, of("c1")),
	}
}

// with appends artifacts to a copy of a script.
func with(base []rsArt, more ...rsArt) []rsArt {
	return append(append([]rsArt(nil), base...), more...)
}

// rows prefixes each row with its table, for want and ignoring.
func rows(table string, rs ...string) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = table + ": " + r
	}
	return out
}

// join concatenates row lists.
func join(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

// chainPlink and chainLeaf are what chain derives.
var (
	chainPlink = rows("plink", "c1>c2 1 @2 -", "c2>c3 1 @3 -", "c3>c4 1 @4 -")
	chainLeaf  = rows("leaf", "c4")
)
