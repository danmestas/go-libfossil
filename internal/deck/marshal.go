package deck

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

func (d *Deck) Marshal() ([]byte, error) {
	if d == nil {
		panic("deck.Marshal: d must not be nil")
	}
	// §4.7.16: a tag name that is entirely hexadecimal after the sign is
	// ambiguous with an artifact hash, so the parser rejects it. Refusing to
	// emit one keeps the codec symmetric -- without this check a caller can
	// build and store an artifact this repository can no longer read back.
	// Canonical fossil never produces one either: it prefixes branch and
	// user tags with "sym-".
	for _, tag := range d.T {
		if isHexToken(tag.Name) {
			return nil, fmt.Errorf("T-card name %q must not be entirely hexadecimal", tag.Name)
		}
	}
	var b strings.Builder
	marshalCards(&b, d)
	body := b.String()
	zHash := computeZ([]byte(body))
	return []byte(fmt.Sprintf("%sZ %s\n", body, zHash)), nil
}

// marshalCards writes all cards to the builder in strict ASCII order
// (A B C D E F G H I J K L M N P Q R S T U W Z), in contiguous groups so each
// writer stays small.
func marshalCards(b *strings.Builder, d *Deck) {
	if b == nil {
		panic("deck.marshalCards: b must not be nil")
	}
	if d == nil {
		panic("deck.marshalCards: d must not be nil")
	}
	marshalCardsABCDE(b, d)
	marshalCardF(b, d)
	marshalCardsGHIJKL(b, d)
	marshalCardsMNPQR(b, d)
	marshalCardS(b, d)
	marshalCardsTUW(b, d)
}

// marshalCardsABCDE writes the A, B, C, D and E cards.
func marshalCardsABCDE(b *strings.Builder, d *Deck) {
	if d.A != nil {
		b.WriteString("A ")
		b.WriteString(FossilEncode(d.A.Filename))
		b.WriteString(" ")
		// §4.3.1/§4.7.2: the A target is the one escape-decoded hash position,
		// so it is escape-encoded on the way out (mirror of parseACard).
		b.WriteString(FossilEncode(d.A.Target))
		if d.A.Source != "" {
			b.WriteString(" ")
			b.WriteString(d.A.Source)
		}
		b.WriteString("\n")
	}

	if d.B != "" {
		fmt.Fprintf(b, "B %s\n", d.B)
	}

	// §4.2: a present-but-empty comment re-emits as "C " + LF, so the
	// artifact round-trips. A caller that never set a comment leaves C empty
	// and cPresent false, and no C card is written.
	if d.C != "" || d.cPresent {
		fmt.Fprintf(b, "C %s\n", FossilEncode(d.C))
	}

	if !d.D.IsZero() {
		// §4.6.4: the D card is written to at least millisecond precision (its
		// writer form), extended to keep whatever finer fraction the parser
		// read, so a sub-millisecond timestamp round-trips.
		fmt.Fprintf(b, "D %s\n", marshalTimestamp(d.D, 3))
	}

	if d.E != nil {
		// The E card's writer form is second precision; a finer fraction the
		// parser read is kept so it round-trips.
		fmt.Fprintf(b, "E %s %s\n", marshalTimestamp(d.E.Date, 0), d.E.UUID)
	}
}

// marshalCardF writes the F (file) cards, sorted by decoded filename. A
// sub-manifest's filenames take the canonical encoding of
// draft-fossil-submanifests-00 §6.2; every other artifact's take §4.3's.
func marshalCardF(b *strings.Builder, d *Deck) {
	if len(d.F) == 0 {
		return
	}
	encode := FossilEncode
	if d.Type == Submanifest {
		encode = CanonicalEncode
	}
	sorted := make([]FileCard, len(d.F))
	copy(sorted, d.F)
	sort.Slice(sorted, func(i, j int) bool { return Compare(sorted[i].Name, sorted[j].Name) < 0 })
	for _, f := range sorted {
		writeFileCard(b, f, encode)
	}
}

// writeFileCard writes one F card, its filename and old name through encode.
func writeFileCard(b *strings.Builder, f FileCard, encode func(string) string) {
	b.WriteString("F ")
	b.WriteString(encode(f.Name))
	if f.UUID != "" {
		b.WriteString(" ")
		b.WriteString(f.UUID)
		// Canonical Fossil forces a "w" permission placeholder on a
		// rename card whose perm would otherwise be empty, so the
		// prior-name field keeps its 4th positional slot rather than
		// being misread as the permission (src/checkin.c:1999).
		perm := f.Perm
		if perm == "" && f.OldName != "" {
			perm = "w"
		}
		if perm != "" {
			b.WriteString(" ")
			b.WriteString(perm)
		}
		// The prior name is positional and only meaningful after a
		// UUID; a deletion card (no UUID) must never carry one, or
		// the old name would be misread as the hash.
		if f.OldName != "" {
			b.WriteString(" ")
			b.WriteString(encode(f.OldName))
		}
	}
	b.WriteString("\n")
}

// marshalCardsGHIJKL writes the G, H, I, J, K and L cards.
func marshalCardsGHIJKL(b *strings.Builder, d *Deck) {
	if d.G != "" {
		fmt.Fprintf(b, "G %s\n", d.G)
	}
	if d.H != "" {
		fmt.Fprintf(b, "H %s\n", FossilEncode(d.H))
	}
	if d.I != "" {
		fmt.Fprintf(b, "I %s\n", d.I)
	}

	if len(d.J) > 0 {
		// §4.5.2 requires a strictly ascending J run, which the parser
		// enforces. Callers build TicketField slices in whatever order
		// suits them, so wire order is the marshaller's to impose — the
		// same responsibility it already takes for F, M and T.
		sorted := make([]TicketField, len(d.J))
		copy(sorted, d.J)
		sort.Slice(sorted, func(i, j int) bool { return Compare(sorted[i].Name, sorted[j].Name) < 0 })
		for _, j := range sorted {
			// §4.7.8 mirror of the parse direction: the name is written
			// verbatim, the value is escape-encoded.
			if j.Value != "" {
				fmt.Fprintf(b, "J %s %s\n", j.Name, FossilEncode(j.Value))
			} else {
				fmt.Fprintf(b, "J %s\n", j.Name)
			}
		}
	}

	if d.K != "" {
		// §4.7.9: a parser may accept an uppercase ticket ID, a writer emits
		// lowercase.
		fmt.Fprintf(b, "K %s\n", strings.ToLower(d.K))
	}
	if d.L != "" {
		fmt.Fprintf(b, "L %s\n", FossilEncode(d.L))
	}
}

// marshalCardsMNPQR writes the M, N, P, Q and R cards.
func marshalCardsMNPQR(b *strings.Builder, d *Deck) {
	if len(d.M) > 0 {
		sorted := make([]string, len(d.M))
		copy(sorted, d.M)
		sort.Slice(sorted, func(i, j int) bool { return Compare(sorted[i], sorted[j]) < 0 })
		for _, m := range sorted {
			fmt.Fprintf(b, "M %s\n", m)
		}
	}

	if d.N != "" || d.nPresent {
		// §4.3.1: the N mimetype is escape-decoded on parse, so encode it here;
		// a present-but-empty N re-emits as "N " + LF.
		fmt.Fprintf(b, "N %s\n", FossilEncode(d.N))
	}

	if len(d.P) > 0 {
		fmt.Fprintf(b, "P %s\n", strings.Join(d.P, " "))
	} else if d.pPresent {
		// §4.7.13: a bare P card (the initial empty baseline) re-emits as "P"
		// + LF, with no trailing space.
		b.WriteString("P\n")
	}

	for _, q := range d.Q {
		prefix := "+"
		if q.IsBackout {
			prefix = "-"
		}
		if q.Baseline != "" {
			fmt.Fprintf(b, "Q %s%s %s\n", prefix, q.Target, q.Baseline)
		} else {
			fmt.Fprintf(b, "Q %s%s\n", prefix, q.Target)
		}
	}

	if d.R != "" {
		// §6.3.4: the checksum is always rendered lowercase, though the
		// parse grammar is case-insensitive.
		fmt.Fprintf(b, "R %s\n", strings.ToLower(d.R))
	}
}

// marshalCardS writes the S cards of a tree manifest or interior sub-manifest
// (draft-fossil-submanifests-00 §3.6), sorted by decoded filename, under the
// canonical encoding of §6.2.
func marshalCardS(b *strings.Builder, d *Deck) {
	if len(d.S) == 0 {
		return
	}
	sorted := make([]SubmanifestCard, len(d.S))
	copy(sorted, d.S)
	sort.Slice(sorted, func(i, j int) bool { return Compare(sorted[i].Name, sorted[j].Name) < 0 })
	for _, sc := range sorted {
		fmt.Fprintf(b, "S %s %s\n", CanonicalEncode(sc.Name), sc.UUID)
	}
}

// MarshalLeaf serializes a leaf sub-manifest (draft-fossil-submanifests-00
// §6.2 step 2): files, in canonical filename order, as F cards and a Z card.
// Every file must carry a hash, since a sub-manifest holds no delete marker
// (§4.4).
func MarshalLeaf(files []FileCard) []byte {
	if len(files) == 0 {
		panic("deck.MarshalLeaf: files must not be empty")
	}
	for _, f := range files {
		if f.UUID == "" {
			panic("deck.MarshalLeaf: every file must carry a hash")
		}
	}
	return marshalSubmanifest(&Deck{Type: Submanifest, F: files})
}

// MarshalInterior serializes an interior sub-manifest (§6.2 step 3): one S
// card per member, in key order, and a Z card.
func MarshalInterior(refs []SubmanifestCard) []byte {
	if len(refs) == 0 {
		panic("deck.MarshalInterior: refs must not be empty")
	}
	return marshalSubmanifest(&Deck{Type: Submanifest, S: refs})
}

// LeafCard is the F card line a leaf sub-manifest carries for f, its
// terminating newline included: what the cost of §6.3 measures a leaf by.
func LeafCard(f FileCard) string {
	var b strings.Builder
	writeFileCard(&b, f, CanonicalEncode)
	return b.String()
}

func marshalSubmanifest(d *Deck) []byte {
	var b strings.Builder
	marshalCards(&b, d)
	body := b.String()
	return []byte(body + "Z " + computeZ([]byte(body)) + "\n")
}

// marshalCardsTUW writes the T, U and W cards.
func marshalCardsTUW(b *strings.Builder, d *Deck) {
	if len(d.T) > 0 {
		sorted := make([]TagCard, len(d.T))
		copy(sorted, d.T)
		// Two-level (decoded name, target hash) per §4.5.2 and §4.7.16,
		// not a comparison of the concatenated key: concatenating lets the
		// target's leading characters compete with the tail of a longer
		// name, which reorders tags whose names are prefixes of one
		// another into a run the parser rejects. tagOrderKey is the same
		// key the parser's check uses, so the two cannot drift.
		sort.Slice(sorted, func(i, j int) bool {
			if cmp := Compare(tagOrderKey(sorted[i]), tagOrderKey(sorted[j])); cmp != 0 {
				return cmp < 0
			}
			return Compare(sorted[i].UUID, sorted[j].UUID) < 0
		})
		for _, tag := range sorted {
			// §4.7.16: the name and value are escape-encoded (a branch name
			// may hold a space, and the value carries that same name on a
			// `T *branch` card); the target token is written verbatim, never
			// encoded.
			fmt.Fprintf(b, "T %c%s %s", tag.Type, FossilEncode(tag.Name), tag.UUID)
			if tag.Value != "" || tag.valuePresent {
				// A present-but-empty value re-emits as a trailing space.
				fmt.Fprintf(b, " %s", FossilEncode(tag.Value))
			}
			b.WriteString("\n")
		}
	}

	if d.U != nil && *d.U != "" {
		fmt.Fprintf(b, "U %s\n", FossilEncode(*d.U))
	}

	// §4.7.18: a W card whose body the parser read -- even a zero-length one --
	// is non-nil, so it re-emits as "W 0" + LF + LF; a wholly absent W card is
	// nil and writes nothing.
	if d.W != nil {
		fmt.Fprintf(b, "W %d\n%s\n", len(d.W), d.W)
	}
}

// marshalTimestamp renders a D- or E-card time in the writer form
// YYYY-MM-DDTHH:MM:SS with a fractional second of at least minFracDigits
// digits, extended to keep any finer precision the parser read and with
// trailing zeros past minFracDigits removed. minFracDigits is 3 for the D
// card and 0 for the E card, matching their writer forms.
func marshalTimestamp(t time.Time, minFracDigits int) string {
	t = t.UTC()
	base := t.Format("2006-01-02T15:04:05")
	if frac := fractionDigits(t.Nanosecond(), minFracDigits); frac != "" {
		return base + "." + frac
	}
	return base
}

// fractionDigits formats nsec as decimal fraction digits, keeping at least
// minDigits and trimming trailing zeros beyond that.
func fractionDigits(nsec, minDigits int) string {
	digits := fmt.Sprintf("%09d", nsec)
	end := len(digits)
	for end > minDigits && digits[end-1] == '0' {
		end--
	}
	return digits[:end]
}
