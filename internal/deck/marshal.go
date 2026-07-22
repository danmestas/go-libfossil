package deck

import (
	"fmt"
	"sort"
	"strings"
)

func (d *Deck) Marshal() ([]byte, error) {
	if d == nil {
		panic("deck.Marshal: d must not be nil")
	}
	var b strings.Builder
	marshalCards(&b, d)
	body := b.String()
	zHash := computeZ([]byte(body))
	return []byte(fmt.Sprintf("%sZ %s\n", body, zHash)), nil
}

// marshalCards writes all cards in ASCII order to the builder.
func marshalCards(b *strings.Builder, d *Deck) {
	if b == nil {
		panic("deck.marshalCards: b must not be nil")
	}
	if d == nil {
		panic("deck.marshalCards: d must not be nil")
	}

	// Cards in strict ASCII order: A B C D E F G H I J K L M N P Q R T U W Z

	if d.A != nil {
		b.WriteString("A ")
		b.WriteString(FossilEncode(d.A.Filename))
		b.WriteString(" ")
		b.WriteString(d.A.Target)
		if d.A.Source != "" {
			b.WriteString(" ")
			b.WriteString(d.A.Source)
		}
		b.WriteString("\n")
	}

	if d.B != "" {
		fmt.Fprintf(b, "B %s\n", d.B)
	}

	if d.C != "" {
		fmt.Fprintf(b, "C %s\n", FossilEncode(d.C))
	}

	if !d.D.IsZero() {
		fmt.Fprintf(b, "D %s\n", d.D.UTC().Format("2006-01-02T15:04:05.000"))
	}

	if d.E != nil {
		fmt.Fprintf(b, "E %s %s\n", d.E.Date.UTC().Format("2006-01-02T15:04:05"), d.E.UUID)
	}

	if len(d.F) > 0 {
		sorted := make([]FileCard, len(d.F))
		copy(sorted, d.F)
		sort.Slice(sorted, func(i, j int) bool { return Compare(sorted[i].Name, sorted[j].Name) < 0 })
		for _, f := range sorted {
			b.WriteString("F ")
			b.WriteString(FossilEncode(f.Name))
			if f.UUID != "" {
				b.WriteString(" ")
				b.WriteString(f.UUID)
				if f.Perm != "" {
					b.WriteString(" ")
					b.WriteString(f.Perm)
				}
			}
			if f.OldName != "" {
				b.WriteString(" ")
				b.WriteString(FossilEncode(f.OldName))
			}
			b.WriteString("\n")
		}
	}

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
		fmt.Fprintf(b, "K %s\n", d.K)
	}
	if d.L != "" {
		fmt.Fprintf(b, "L %s\n", FossilEncode(d.L))
	}

	if len(d.M) > 0 {
		sorted := make([]string, len(d.M))
		copy(sorted, d.M)
		sort.Slice(sorted, func(i, j int) bool { return Compare(sorted[i], sorted[j]) < 0 })
		for _, m := range sorted {
			fmt.Fprintf(b, "M %s\n", m)
		}
	}

	if d.N != "" {
		fmt.Fprintf(b, "N %s\n", d.N)
	}

	if len(d.P) > 0 {
		fmt.Fprintf(b, "P %s\n", strings.Join(d.P, " "))
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
		fmt.Fprintf(b, "R %s\n", d.R)
	}

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
			// §4.7.16: the name is escape-encoded (a branch name may hold a
			// space); the target token is written verbatim, never encoded.
			fmt.Fprintf(b, "T %c%s %s", tag.Type, FossilEncode(tag.Name), tag.UUID)
			if tag.Value != "" {
				fmt.Fprintf(b, " %s", tag.Value)
			}
			b.WriteString("\n")
		}
	}

	if d.U != nil && *d.U != "" {
		fmt.Fprintf(b, "U %s\n", FossilEncode(*d.U))
	}

	if len(d.W) > 0 {
		fmt.Fprintf(b, "W %d\n%s\n", len(d.W), d.W)
	}
}
