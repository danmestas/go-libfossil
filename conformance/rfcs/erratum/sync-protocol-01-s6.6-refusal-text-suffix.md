# Erratum: draft-fossil-sync-protocol-01 §6.6

| | |
|---|---|
| **Document** | `draft-fossil-sync-protocol-01` |
| **Section** | 6.6, Capability Model (the text of a refusal) |
| **Type** | Technical (a refusal can carry more text than the draft gives) |
| **Status** | Applied in `draft-fossil-sync-protocol-01` (2026-10-16) |
| **Reported** | 2026-10-16 |
| **Submitter** | Dan Mestas |
| **Found via** | Rechecking the §6.6 refusal forms against Fossil's source; confirmed by running Fossil 2.28 |

## 1. Summary

§6.6 and the sections it points to give each refusal as an exact card:
`error not authorized to write`, `message pull only - not authorized to
push`, `error not authorized to push configuration`, `error not authorized
to sync private content`. Fossil appends ` because the 'localauth' setting
is enabled` to every one of them when it refuses a loopback request that
its `localauth` setting requires to log in. A client that matches the whole
text misses the refusal. Only §10.1's autopush rule, which matches
`%not authorized to write%`, already allows for it.

## 2. The text in error

Page 22 (§6.6):

> Otherwise the server resets accumulated output and emits error not
> authorized to write.

The draft says nothing about further text after any refusal.

## 3. What Fossil implements

`src/xfer.c`:

```
static char *whyNotAuth(void){
  if( g.useLocalauth && db_get_boolean("localauth",0)!=0 ){
    return "\\sbecause\\sthe\\s'localauth'\\ssetting\\sis\\senabled";
  }
  return "";
}
```

Every refusal in `page_xfer()` is written as, for example,

```
@ error not\sauthorized\sto\swrite%s(whyNotAuth())
```

and the same call follows the `pull only` message and the read, clone,
configuration and private-content refusals.

Checked with Fossil 2.28 [52445a27f1]: a repository with `localauth` set to
1, served by `fossil server --localauth`, and an anonymous `push` card sent
from 127.0.0.1. The reply is

```
error not\sauthorized\sto\swrite\sbecause\sthe\s'localauth'\ssetting\sis\senabled
```

## 4. Corrected text

Add to §6.6, after its last paragraph:

> A server MAY append further words to the text of any refusal this
> document gives in the form "not authorized to ...", including the
> message form above. Fossil appends " because the 'localauth' setting is
> enabled" when it refuses a request from the loopback address because
> that setting requires such requests to log in. A client MUST recognize a
> refusal by the text this document gives at the start of the decoded
> text, not by the whole text.

## 5. Observable consequence of the uncorrected text

A client that compares the whole decoded text treats a refused push from a
local `fossil server --localauth` as an unknown error, or does not
recognize the `pull only` message, and reports or continues differently
from Fossil's client.

## 6. Implementations

zit's client does not match refusal text, and its server never appends to
it. No change is needed.
