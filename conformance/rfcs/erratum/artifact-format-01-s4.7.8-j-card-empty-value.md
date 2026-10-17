# Erratum: draft-fossil-artifact-format-01 §4.7.8

| | |
|---|---|
| **Document** | `draft-fossil-artifact-format-01` |
| **Section** | 4.7.8, J Card (an absent value and a present empty value) |
| **Type** | Technical (whether a reader keeps the two forms apart is left open) |
| **Status** | Applied in `draft-fossil-artifact-format-01` (2026-10-16) |
| **Reported** | 2026-10-16 |
| **Submitter** | Dan Mestas |
| **Found via** | A reader working from the draft text alone; confirmed by reading Fossil's source |

## 1. Summary

§4.7.8 says an absent `J` value "defaults to the empty string, not null",
and §4.2 makes a present empty final token decode to the empty string too.
The draft does not say whether a reader may still record the two forms
differently, for example as "unset" and "set to empty". Fossil does not:
both reach ticket processing as the same empty string. Fossil also writes
the present empty form itself, so a reader meets it in real repositories.

## 2. The text in error

Page 24 (§4.7.8):

> The value is escape-decoded; the field name is stored verbatim,
> including any leading +. An absent value defaults to the empty string,
> not null.

## 3. What Fossil implements

`manifest_parse()` in `src/manifest.c`, case `'J'`:

```
zName = next_token(&x,0);
zValue = next_token(&x,0);
if( zName==0 ) SYNTAX("name missing from J-card");
if( zValue==0 ) zValue = "";
defossilize(zValue);
```

A present empty token returns a pointer to an empty string, and an absent
one is replaced by `""`; nothing records which it was. `ticket_insert()` in
`src/tkt.c` then writes the value into the ticket tables the same way for
both.

When a ticket field is changed to the empty string, `submitTicketCmd()` in
`src/tkt.c` writes

```
blob_appendf(&tktchng, "J %s %#F\n", aField[i].zName, nValue, zValue);
```

with `nValue` 0, which is `J`, SP, the field name, SP, LF: the present
empty form.

## 4. Corrected text

Replace the sentence "An absent value defaults to the empty string, not
null." with:

> An absent value and a present empty one (J, SP, fieldname, SP, LF) both
> decode to the empty string, not null, and a reader MUST NOT distinguish
> them; Fossil writes the present empty form when a ticket field is set to
> the empty string.

## 5. Observable consequence of the uncorrected text

A reader that keeps the forms apart can treat `J name` as "leave the field
alone" and `J name ` as "clear it", or the reverse, and build ticket state
that differs from Fossil's for the same artifacts.

## 6. Implementations

zit's `J` card parser returns the same empty value for both forms. No
change is needed.
