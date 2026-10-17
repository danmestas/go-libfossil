# Erratum: draft-fossil-artifact-format-01 §4.4, §4.5.1 and §6.2

| | |
|---|---|
| **Document** | `draft-fossil-artifact-format-01` |
| **Section** | 4.4, Card Ordering ("Nothing follows Z"); 4.5.1, Occurrence Classes; 6.2, Z Card: Artifact Self-Checksum |
| **Type** | Editorial (the reason given for the Z card being last is wrong; the verification states are described by card rather than by byte position) |
| **Status** | Applied in `draft-fossil-artifact-format-01` (2026-10-16) |
| **Reported** | 2026-10-16 |
| **Submitter** | Dan Mestas |
| **Found via** | A reader working from the draft text alone; confirmed by running Fossil 2.28 and reading its source |

## 1. Summary

§4.4 and §4.5.1 say the `Z` card's must-be-last property follows from the
checksum framing, which "fixes the Z card as occupying exactly the final 35
bytes". The framing fixes nothing: it reads the final 35 bytes by position
and treats them as a `Z` card when the first two are `Z` and a space,
whether or not a card line begins there. The `Z` card is last for two other
reasons: the ordering rule admits only another `Z` after a `Z`, and every
artifact type requires a `Z` card (Table 4). Its 32-digit token then puts it
in the final 35 bytes.

§6.2's state table names the absent and mismatch states by whether a `Z`
card is present. Fossil decides them by the bytes at offsets n-35 and n-34,
so an artifact with no `Z` card can reach the mismatch state.

## 2. The text in error

Page 14 (§4.4):

> Z is the lexicographically last defined letter, the ordering rule admits
> no letter after it (the N-after-P exception does not involve Z), and the
> checksum-verification framing fixes the Z card as occupying exactly the
> final 35 bytes of the stripped content: Z, space, 32 hexadecimal digits,
> LF. The Z card's must-be-last property is enforced structurally — by
> ordering plus byte-position framing — rather than by an explicit
> last-card check in the Z card grammar, which validates only token syntax.

Page 15 (§4.5.1):

> No separate rule requires the Z card to be exactly one and last; that
> property follows from the framing.

Page 33 (§6.2, the state table):

> absent: no Z card present, or artifact too short to hold one
> mismatch: Z card present but token differs

## 3. What Fossil implements

`verify_z_card()` in `src/manifest.c`:

```
if( n<35 ) return 0;
if( z[n-35]!='Z' || z[n-34]!=' ' ) return 0;
md5sum_init();
md5sum_step_text(z, n-35);
zHash = md5sum_finish(0);
if( memcmp(&z[n-33], zHash, 32)==0 ){
  return 1;
}else{
  ...
  return 2;
}
```

It looks at two bytes, not at line boundaries. `manifestCardTypes` lists
`Z` among the required cards of all eight types, and the ordering check in
`manifest_parse()` rejects any letter that sorts below the one before it.

Checked with Fossil 2.28 [52445a27f1], `fossil test-parse-manifest`, on a
control artifact with no `Z` card whose last line is a `U` card ending in
`Z` and a 32-character token:

| Last line | Result |
|---|---|
| `U aliceZ 0123456789abcdef0123456789abcdef` | `incorrect Z-card cksum` |
| `U aliceZ` followed by the MD5 of the bytes before that `Z` | `line 3: extra characters at end of card` |
| `U alice` | `Z-card missing in tag` |

The first artifact has no `Z` card and still fails the checksum. All three
are rejected; only the reason differs.

## 4. Corrected text

§4.4, replace the bullet with:

> Nothing follows Z. Z is the lexicographically last defined letter and
> the ordering rule admits no letter after it (the N-after-P exception
> does not involve Z), so only another Z card can follow a Z card. Every
> artifact type requires a Z card (Section 5.2), so the last line of an
> accepted artifact is a Z card, and because its token is exactly 32
> hexadecimal digits it occupies exactly the final 35 bytes of the
> stripped content: Z, space, 32 hexadecimal digits, LF. The must-be-last
> property follows from ordering and the required-card rule, not from an
> explicit last-card check in the Z card grammar, which validates only
> token syntax. Checksum verification (Section 6.2) does not establish it
> either: it reads the final 35 bytes by position, whatever card they
> belong to.

§4.5.1, replace the sentence quoted in Section 2 with:

> No separate rule requires the Z card to be exactly one, and that it is
> last follows from the ordering and required-card rules (Section 4.4).

§6.2, replace the absent and mismatch conditions with:

> absent: offsets n-35 and n-34 do not hold Z and a space, or artifact too
> short to hold one
> mismatch: offsets n-35 and n-34 hold Z and a space, but the token differs

and add after the paragraph that follows the table:

> The state is decided by the bytes at those offsets, not by where card
> lines begin: an artifact with no Z card whose last line ends in Z, a
> space and 32 further bytes before its LF takes the match or mismatch
> state rather than absent. Such an artifact is rejected either way, by
> the mismatch here or by the card rules, so only the reported reason
> differs.

## 5. Observable consequence of the uncorrected text

A reader that trusts the framing to keep the `Z` card last can skip the
ordering check after a `Z` card, or the required-card check, and accept an
artifact Fossil rejects. A reader that decides the verification state by
looking for a `Z` card line reports a different reason than Fossil for an
artifact whose last line merely ends in `Z` and 32 characters.

## 6. Implementations

zit checks the same two bytes at the same offsets, applies the ordering
rule after a `Z` card, and requires a `Z` card for every type. No change is
needed.
