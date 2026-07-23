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
}

func (c *LoginCard) Type() CardType { return CardLogin }

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
}

func (c *ErrorCard) Type() CardType { return CardError }

// MessageCard carries an informational message.
type MessageCard struct {
	Message string // plain text (defossilized)
}

func (c *MessageCard) Type() CardType { return CardMessage }

// UnknownCard captures any unrecognized card command.
type UnknownCard struct {
	Command string
	Args    []string
}

func (c *UnknownCard) Type() CardType { return CardUnknown }
