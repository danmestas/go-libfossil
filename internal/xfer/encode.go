package xfer

import (
	"bytes"
	"compress/zlib"
	"fmt"

	"github.com/danmestas/libfossil/internal/deck"
)

// EncodeCard writes the wire-format representation of a Card to w.
func EncodeCard(w *bytes.Buffer, c Card) error {
	if w == nil {
		panic("xfer.EncodeCard: w must not be nil")
	}
	if c == nil {
		panic("xfer.EncodeCard: c must not be nil")
	}
	switch v := c.(type) {
	case *IGotCard:
		return encodeIGot(w, v)
	case *GimmeCard:
		return encodeGimme(w, v)
	case *PushCard:
		return encodePush(w, v)
	case *PullCard:
		return encodePull(w, v)
	case *CookieCard:
		return encodeCookie(w, v)
	case *ReqConfigCard:
		return encodeReqConfig(w, v)
	case *PrivateCard:
		return encodePrivate(w)
	case *CloneCard:
		return encodeClone(w, v)
	case *CloneSeqNoCard:
		return encodeCloneSeqNo(w, v)
	case *UVGimmeCard:
		return encodeUVGimme(w, v)
	case *PragmaCard:
		return encodePragma(w, v)
	case *LoginCard:
		return encodeLogin(w, v)
	case *ErrorCard:
		return encodeError(w, v)
	case *MessageCard:
		return encodeMessage(w, v)
	case *UVIGotCard:
		return encodeUVIGot(w, v)
	case *FileCard:
		return encodeFile(w, v)
	case *CFileCard:
		return encodeCFile(w, v)
	case *ConfigCard:
		return encodeConfig(w, v)
	case *UVFileCard:
		return encodeUVFile(w, v)
	case *SchemaCard:
		return encodeSchema(w, v)
	case *XIGotCard:
		return encodeXIGot(w, v)
	case *XGimmeCard:
		return encodeXGimme(w, v)
	case *XRowCard:
		return encodeXRow(w, v)
	case *XDeleteCard:
		return encodeXDelete(w, v)
	case *UnknownCard:
		return encodeUnknown(w, v)
	default:
		return fmt.Errorf("xfer: cannot encode %T", c)
	}
}

func encodeIGot(w *bytes.Buffer, c *IGotCard) error {
	w.WriteString("igot ")
	w.WriteString(c.UUID)
	if c.IsPrivate {
		w.WriteString(" 1")
	}
	w.WriteByte('\n')
	return nil
}

func encodeGimme(w *bytes.Buffer, c *GimmeCard) error {
	w.WriteString("gimme ")
	w.WriteString(c.UUID)
	w.WriteByte('\n')
	return nil
}

// encodePush writes "push <project-code> [<server-code>]\n".
//
// The Fossil C xfer parser (src/xfer.c xfer_push_card) requires at least the
// project code; a bare "push\n" is rejected by real Fossil servers. The
// project code must always be present — callers must populate PushCard.ProjectCode
// before encoding. ServerCode is optional (omitted on the first round when no
// prior session exists).
//
// Wire-format arg order matches Fossil C: project-code first, server-code second.
func encodePush(w *bytes.Buffer, c *PushCard) error {
	if c.ProjectCode == "" {
		panic("xfer: encodePush: ProjectCode must not be empty")
	}
	w.WriteString("push ")
	w.WriteString(c.ProjectCode)
	if c.ServerCode != "" {
		w.WriteByte(' ')
		w.WriteString(c.ServerCode)
	}
	w.WriteByte('\n')
	return nil
}

// encodePull writes "pull <project-code> <server-code>\n".
//
// The Fossil C xfer parser requires both args on pull — a bare "pull\n" or
// single-arg "pull <code>\n" is rejected. Both ProjectCode and ServerCode must
// be non-empty before encoding.
//
// Wire-format arg order matches Fossil C: project-code first, server-code second.
func encodePull(w *bytes.Buffer, c *PullCard) error {
	if c.ProjectCode == "" {
		panic("xfer: encodePull: ProjectCode must not be empty")
	}
	if c.ServerCode == "" {
		panic("xfer: encodePull: ServerCode must not be empty")
	}
	w.WriteString("pull ")
	w.WriteString(c.ProjectCode)
	w.WriteByte(' ')
	w.WriteString(c.ServerCode)
	w.WriteByte('\n')
	return nil
}

func encodeCookie(w *bytes.Buffer, c *CookieCard) error {
	w.WriteString("cookie ")
	w.WriteString(c.Value)
	w.WriteByte('\n')
	return nil
}

func encodeReqConfig(w *bytes.Buffer, c *ReqConfigCard) error {
	w.WriteString("reqconfig ")
	w.WriteString(c.Name)
	w.WriteByte('\n')
	return nil
}

func encodePrivate(w *bytes.Buffer) error {
	w.WriteString("private\n")
	return nil
}

func encodeClone(w *bytes.Buffer, c *CloneCard) error {
	if c.Version == 0 && c.SeqNo == 0 {
		w.WriteString("clone\n")
	} else {
		fmt.Fprintf(w, "clone %d %d\n", c.Version, c.SeqNo)
	}
	return nil
}

func encodeCloneSeqNo(w *bytes.Buffer, c *CloneSeqNoCard) error {
	fmt.Fprintf(w, "clone_seqno %d\n", c.SeqNo)
	return nil
}

func encodeUVGimme(w *bytes.Buffer, c *UVGimmeCard) error {
	w.WriteString("uvgimme ")
	w.WriteString(c.Name)
	w.WriteByte('\n')
	return nil
}

func encodePragma(w *bytes.Buffer, c *PragmaCard) error {
	w.WriteString("pragma ")
	w.WriteString(c.Name)
	for _, v := range c.Values {
		w.WriteByte(' ')
		w.WriteString(v)
	}
	w.WriteByte('\n')
	return nil
}

func encodeLogin(w *bytes.Buffer, c *LoginCard) error {
	w.WriteString("login ")
	w.WriteString(deck.FossilEncode(c.User))
	w.WriteByte(' ')
	w.WriteString(c.Nonce)
	w.WriteByte(' ')
	w.WriteString(c.Signature)
	w.WriteByte('\n')
	return nil
}

func encodeError(w *bytes.Buffer, c *ErrorCard) error {
	w.WriteString("error ")
	w.WriteString(deck.FossilEncode(c.Message))
	w.WriteByte('\n')
	return nil
}

func encodeMessage(w *bytes.Buffer, c *MessageCard) error {
	w.WriteString("message ")
	w.WriteString(deck.FossilEncode(c.Message))
	w.WriteByte('\n')
	return nil
}

func encodeUVIGot(w *bytes.Buffer, c *UVIGotCard) error {
	fmt.Fprintf(w, "uvigot %s %d %s %d\n", c.Name, c.MTime, c.Hash, c.Size)
	return nil
}

func encodeUnknown(w *bytes.Buffer, c *UnknownCard) error {
	w.WriteString(c.Command)
	for _, a := range c.Args {
		w.WriteByte(' ')
		w.WriteString(a)
	}
	w.WriteByte('\n')
	return nil
}

// --- Payload card encoders ---

// encodeFile writes: file UUID SIZE \n CONTENT  (no trailing \n)
// or: file UUID DELTASRC SIZE \n CONTENT  (delta variant)
func encodeFile(w *bytes.Buffer, c *FileCard) error {
	w.WriteString("file ")
	w.WriteString(c.UUID)
	if c.DeltaSrc != "" {
		w.WriteByte(' ')
		w.WriteString(c.DeltaSrc)
	}
	fmt.Fprintf(w, " %d\n", len(c.Content))
	w.Write(c.Content)
	// NO trailing newline
	return nil
}

// encodeCFile writes: cfile UUID USIZE CSIZE \n ZCONTENT  (no trailing \n)
// or: cfile UUID DELTASRC USIZE CSIZE \n ZCONTENT  (delta variant)
func encodeCFile(w *bytes.Buffer, c *CFileCard) error {
	// Compress content with zlib
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	if _, err := zw.Write(c.Content); err != nil {
		return fmt.Errorf("xfer: cfile zlib write: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("xfer: cfile zlib close: %w", err)
	}
	w.WriteString("cfile ")
	w.WriteString(c.UUID)
	if c.DeltaSrc != "" {
		w.WriteByte(' ')
		w.WriteString(c.DeltaSrc)
	}
	fmt.Fprintf(w, " %d %d\n", len(c.Content), zbuf.Len())
	w.Write(zbuf.Bytes())
	// NO trailing newline
	return nil
}

// encodeConfig writes: config NAME SIZE \n CONTENT \n
// Note the trailing \n after CONTENT.
func encodeConfig(w *bytes.Buffer, c *ConfigCard) error {
	fmt.Fprintf(w, "config %s %d\n", c.Name, len(c.Content))
	w.Write(c.Content)
	w.WriteByte('\n') // trailing newline
	return nil
}

// encodeUVFile writes: uvfile NAME MTIME HASH SIZE FLAGS \n CONTENT
// When deleted (0x0001) or content-omitted (0x0004), no payload follows.
func encodeUVFile(w *bytes.Buffer, c *UVFileCard) error {
	fmt.Fprintf(w, "uvfile %s %d %s %d %d\n", c.Name, c.MTime, c.Hash, c.Size, c.Flags)
	if !uvFileOmitsContent(c.Flags) {
		w.Write(c.Content)
	}
	// NO trailing newline
	return nil
}

func encodeSchema(w *bytes.Buffer, c *SchemaCard) error {
	fmt.Fprintf(w, "schema %s %d %s %d %d\n", c.Table, c.Version, c.Hash, c.MTime, len(c.Content))
	w.Write(c.Content)
	w.WriteByte('\n')
	return nil
}

func encodeXIGot(w *bytes.Buffer, c *XIGotCard) error {
	if c.Table == "" {
		panic("encodeXIGot: Table must not be empty")
	}
	if c.PKHash == "" {
		panic("encodeXIGot: PKHash must not be empty")
	}
	fmt.Fprintf(w, "xigot %s %s %d\n", c.Table, c.PKHash, c.MTime)
	return nil
}

func encodeXGimme(w *bytes.Buffer, c *XGimmeCard) error {
	if c.Table == "" {
		panic("encodeXGimme: Table must not be empty")
	}
	if c.PKHash == "" {
		panic("encodeXGimme: PKHash must not be empty")
	}
	fmt.Fprintf(w, "xgimme %s %s\n", c.Table, c.PKHash)
	return nil
}

func encodeXRow(w *bytes.Buffer, c *XRowCard) error {
	if c.Table == "" {
		panic("encodeXRow: Table must not be empty")
	}
	if c.PKHash == "" {
		panic("encodeXRow: PKHash must not be empty")
	}
	fmt.Fprintf(w, "xrow %s %s %d %d\n", c.Table, c.PKHash, c.MTime, len(c.Content))
	w.Write(c.Content)
	w.WriteByte('\n')
	return nil
}

func encodeXDelete(w *bytes.Buffer, c *XDeleteCard) error {
	if c.Table == "" {
		panic("encodeXDelete: Table must not be empty")
	}
	if c.PKHash == "" {
		panic("encodeXDelete: PKHash must not be empty")
	}
	fmt.Fprintf(w, "xdelete %s %s %d %d\n", c.Table, c.PKHash, c.MTime, len(c.PKData))
	w.Write(c.PKData)
	w.WriteByte('\n')
	return nil
}

