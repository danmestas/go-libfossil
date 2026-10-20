// Package xfer implements Fossil's sync protocol card codec.
// It encodes and decodes the line-oriented "xfer card" format used by
// Fossil's /xfer HTTP endpoint, with NO database dependency.
package xfer

// CardType enumerates the 24 card types in Fossil's sync protocol.
type CardType int

const (
	CardFile       CardType = iota // 0  — file
	CardCFile                      // 1  — cfile (compressed file)
	CardIGot                       // 2  — igot
	CardGimme                      // 3  — gimme
	CardLogin                      // 4  — login
	CardPush                       // 5  — push
	CardPull                       // 6  — pull
	CardCookie                     // 7  — cookie
	CardClone                      // 8  — clone
	CardCloneSeqNo                 // 9  — clone_seqno (undocumented)
	CardConfig                     // 10 — config
	CardReqConfig                  // 11 — reqconfig
	CardPrivate                    // 12 — private
	CardUVFile                     // 13 — uvfile
	CardUVGimme                    // 14 — uvgimme
	CardUVIGot                     // 15 — uvigot
	CardPragma                     // 16 — pragma
	CardError                      // 17 — error
	CardMessage                    // 18 — message
	CardUnknown                    // 19 — unrecognized card
	CardSchema                     // 20 — schema (table sync)
	CardXIGot                      // 21 — xigot (table sync row announcement)
	CardXGimme                     // 22 — xgimme (table sync row request)
	CardXRow                       // 23 — xrow (table sync row payload)
	CardXDelete                    // 24 — xdelete (table sync row deletion)
	CardComment                    // 25 — "# ..." comment line
	CardBadCommand                 // 26 — a line no card rule accepts
	CardBadFile                    // 27 — a file/cfile/uvfile line that cannot be framed
)

// Card is the interface implemented by every xfer card type.
type Card interface {
	Type() CardType
}

// FileCard represents a "file" card carrying an artifact payload.
type FileCard struct {
	UUID     string
	DeltaSrc string // empty when not a delta
	Content  []byte
}

func (c *FileCard) Type() CardType { return CardFile }

// CFileCard represents a "cfile" card (compressed file with uncompressed size).
type CFileCard struct {
	UUID     string
	DeltaSrc string
	USize    int
	Content  []byte

	// StoredBlob is the received card's payload re-expressed in Fossil's
	// on-disk blob format ([4-byte big-endian uncompressed size][zlib
	// data]), suitable for writing to the blob table's content column
	// verbatim. It is derived by decode-time bookkeeping only -- never by
	// recompressing Content -- so a receiver that persists StoredBlob
	// reproduces the sender's stored bytes exactly, rather than the bytes
	// of a fresh, independently-parameterized zlib pass. Always non-nil
	// after a successful decode.
	StoredBlob []byte
}

func (c *CFileCard) Type() CardType { return CardCFile }

// IGotCard represents an "igot" card — the sender has this artifact.
type IGotCard struct {
	UUID      string
	IsPrivate bool // true when second arg is "1"
}

func (c *IGotCard) Type() CardType { return CardIGot }

// GimmeCard represents a "gimme" card — the sender wants this artifact.
type GimmeCard struct {
	UUID string
}

func (c *GimmeCard) Type() CardType { return CardGimme }

// LoginCard represents a "login" card for authentication.
// User is stored as plain text (Fossil-decoded).
type LoginCard struct {
	User      string // plain text (defossilized)
	Nonce     string
	Signature string

	// tailSHA1 is the SHA-1 of the message bytes that followed this card's
	// line, recorded by DecodeUncompressed. Empty for a card built in memory.
	tailSHA1 string
}

func (c *LoginCard) Type() CardType { return CardLogin }

// NonceCoversTail reports whether Nonce is the SHA-1 of the decompressed bytes
// that followed this card's line in the message it was decoded from: "every
// decompressed body byte after the body login line's LF through body end"
// (draft §6.2, src/xfer.c check_tail_hash). The signature covers only the
// nonce, so this check is what binds a login to its body; without it a
// captured card authenticates any body. A card that was not decoded has no
// tail and covers nothing.
func (c *LoginCard) NonceCoversTail() bool {
	return c.tailSHA1 != "" && c.Nonce == c.tailSHA1
}

// CommentCard represents a "# ..." line. A receiver ignores it — canonical's
// card loop skips any line starting with '#', as does this package's decoder,
// so a comment never survives a round trip.
//
// It carries no protocol meaning but it is not inert: a login card's nonce
// hashes every byte that follows it, and the server re-hashes those bytes to
// validate it (src/xfer.c check_tail_hash). The comment is where a client puts
// the randomness that makes each nonce, and so each signature, unique — which
// only works if the comment is actually transmitted, not merely hashed.
type CommentCard struct {
	Text string
}

func (c *CommentCard) Type() CardType { return CardComment }

// PushCard represents a "push" card sent by the client to begin a push.
type PushCard struct {
	ServerCode  string
	ProjectCode string
}

func (c *PushCard) Type() CardType { return CardPush }

// PullCard represents a "pull" card sent by the client to begin a pull.
type PullCard struct {
	ServerCode  string
	ProjectCode string
}

func (c *PullCard) Type() CardType { return CardPull }

// CookieCard carries a session cookie value.
type CookieCard struct {
	Value string
}

func (c *CookieCard) Type() CardType { return CardCookie }

// CloneCard represents a "clone" card. Version and SeqNo are zero for a
// bare "clone" request.
//
// SeqNoIsDecimal records the SHAPE of the SEQNO token, not its presence:
// §8.1 keys its fatal on a digit-only SEQNO (`decimal = 1*DIGIT`) and a
// parsed int cannot carry that. "clone 3 -1" therefore has a SEQNO token
// but is not decimal, and "clone" and "clone 3 0" both parse to SeqNo 0
// while only the latter is fatal.
type CloneCard struct {
	Version        int
	SeqNo          int
	SeqNoIsDecimal bool
}

func (c *CloneCard) Type() CardType { return CardClone }

// CloneSeqNoCard represents a "clone_seqno" card.
type CloneSeqNoCard struct {
	SeqNo int
	Raw   string // line as received, LF kept; empty for a built card
}

func (c *CloneSeqNoCard) Type() CardType { return CardCloneSeqNo }

// ConfigCard represents a "config" card carrying configuration data.
type ConfigCard struct {
	Name    string
	Content []byte
}

func (c *ConfigCard) Type() CardType { return CardConfig }

// ReqConfigCard requests a named configuration section.
type ReqConfigCard struct {
	Name string
}

func (c *ReqConfigCard) Type() CardType { return CardReqConfig }

// PrivateCard signals that subsequent igot cards are private.
type PrivateCard struct{}

func (c *PrivateCard) Type() CardType { return CardPrivate }

// UV file flag constants.
const (
	UVFlagDeletion       = 0x0001 // File is a deletion tombstone.
	UVFlagContentOmitted = 0x0004 // Content not included in this card.
	UVFlagNoPayload      = UVFlagDeletion | UVFlagContentOmitted // Mask: no payload expected.
)

// UVFileCard represents an "uvfile" card for unversioned file content.
type UVFileCard struct {
	Name    string
	MTime   int64
	Hash    string
	Size    int
	Flags   int
	Content []byte
}

func (c *UVFileCard) Type() CardType { return CardUVFile }

// UVGimmeCard requests an unversioned file by name.
type UVGimmeCard struct {
	Name string
}

func (c *UVGimmeCard) Type() CardType { return CardUVGimme }

// UVIGotCard announces possession of an unversioned file.
type UVIGotCard struct {
	Name  string
	MTime int64
	Hash  string
	Size  int
}

func (c *UVIGotCard) Type() CardType { return CardUVIGot }

// PragmaCard represents a "pragma" card for protocol negotiation.
type PragmaCard struct {
	Name   string
	Values []string
}

func (c *PragmaCard) Type() CardType { return CardPragma }

// ErrorCard carries an error message from the server.
type ErrorCard struct {
	Message string // plain text (defossilized)
	Raw     string // line as received, LF kept; empty for a built card
}

func (c *ErrorCard) Type() CardType { return CardError }

// MessageCard carries an informational message.
type MessageCard struct {
	Message string // plain text (defossilized)
	Raw     string // line as received, LF kept; empty for a built card
}

func (c *MessageCard) Type() CardType { return CardMessage }

// UnknownCard captures any unrecognized card command.
type UnknownCard struct {
	Command string
	Args    []string
	Raw     string // line as received, LF kept; empty for a built card
}

func (c *UnknownCard) Type() CardType { return CardUnknown }

// BadCommandCard is a line the decoder could not read as any known card: an
// unrecognized operator, or a known one whose arguments break its rule (a
// bare pragma, a gimme with a surplus token, a config with a non-decimal
// size). It carries no payload, so the decoder reads on past it.
//
// §5.2, src/xfer.c page_xfer's final "else": a server resets its reply,
// answers "error bad\scommand:\s<LINE>" and keeps parsing; a client ends the
// reply-card loop. Line is the raw line the decoder read, its terminating LF
// kept, so the error quotes it exactly as fossil's %F(blob_str(&xfer.line)).
type BadCommandCard struct {
	Line string
}

func (c *BadCommandCard) Type() CardType { return CardBadCommand }

// BadFileCard is a file, cfile or uvfile line whose metadata the decoder
// cannot frame a payload from — a bad arity, hash, size or flags — or whose
// payload the body ends inside (sync-protocol-00 Section 5.1). Either way no
// later card can be located, so the decoder yields this card and stops.
//
// src/xfer.c xfer_accept_file / _compressed_file / _unversioned_file: the
// receiver writes Message into its error, and page_xfer resets the reply,
// emits that error and breaks out of the card loop. Message is the handler's
// own wording ("malformed file line", "malformed cfile line", "malformed
// uvfile line"), which the draft fossilizes and fossil(1) URL-encodes.
type BadFileCard struct {
	Message string
}

func (c *BadFileCard) Type() CardType { return CardBadFile }
