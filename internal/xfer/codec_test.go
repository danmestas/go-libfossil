package xfer

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"testing"
)

// helper: encode a card, then decode it back, returning the decoded card.
func roundTrip(t *testing.T, c Card) Card {
	t.Helper()
	var buf bytes.Buffer
	if err := EncodeCard(&buf, c); err != nil {
		t.Fatalf("EncodeCard(%T): %v", c, err)
	}
	r := bufio.NewReader(&buf)
	got, err := DecodeCard(r)
	if err != nil {
		t.Fatalf("DecodeCard after encode(%T): %v (wire: %q)", c, err, buf.String())
	}
	return got
}

// --- Task 2: Simple (non-payload) card round-trips ---

func TestRoundTrip_IGot(t *testing.T) {
	c := &IGotCard{UUID: "abc123def456"}
	got := roundTrip(t, c).(*IGotCard)
	if got.UUID != c.UUID {
		t.Errorf("UUID = %q, want %q", got.UUID, c.UUID)
	}
	if got.IsPrivate {
		t.Error("IsPrivate should be false")
	}
}

func TestRoundTrip_IGotPrivate(t *testing.T) {
	c := &IGotCard{UUID: "abc123def456", IsPrivate: true}
	got := roundTrip(t, c).(*IGotCard)
	if got.UUID != c.UUID {
		t.Errorf("UUID = %q, want %q", got.UUID, c.UUID)
	}
	if !got.IsPrivate {
		t.Error("IsPrivate should be true")
	}
}

func TestRoundTrip_Gimme(t *testing.T) {
	c := &GimmeCard{UUID: "deadbeef0123"}
	got := roundTrip(t, c).(*GimmeCard)
	if got.UUID != c.UUID {
		t.Errorf("UUID = %q, want %q", got.UUID, c.UUID)
	}
}

func TestRoundTrip_Push(t *testing.T) {
	// Both codes present: "push <project-code> <server-code>\n"
	c := &PushCard{ProjectCode: "proj1", ServerCode: "srv1"}
	got := roundTrip(t, c).(*PushCard)
	if got.ProjectCode != c.ProjectCode || got.ServerCode != c.ServerCode {
		t.Errorf("Push = %+v, want %+v", got, c)
	}
}

func TestRoundTrip_Pull(t *testing.T) {
	// Both codes required on pull: "pull <project-code> <server-code>\n"
	c := &PullCard{ProjectCode: "proj2", ServerCode: "srv2"}
	got := roundTrip(t, c).(*PullCard)
	if got.ProjectCode != c.ProjectCode || got.ServerCode != c.ServerCode {
		t.Errorf("Pull = %+v, want %+v", got, c)
	}
}

func TestRoundTrip_Cookie(t *testing.T) {
	c := &CookieCard{Value: "session-abc-123"}
	got := roundTrip(t, c).(*CookieCard)
	if got.Value != c.Value {
		t.Errorf("Value = %q, want %q", got.Value, c.Value)
	}
}

func TestRoundTrip_ReqConfig(t *testing.T) {
	c := &ReqConfigCard{Name: "css"}
	got := roundTrip(t, c).(*ReqConfigCard)
	if got.Name != c.Name {
		t.Errorf("Name = %q, want %q", got.Name, c.Name)
	}
}

func TestRoundTrip_Private(t *testing.T) {
	c := &PrivateCard{}
	got := roundTrip(t, c).(*PrivateCard)
	if got.Type() != CardPrivate {
		t.Error("wrong type")
	}
}

func TestRoundTrip_CloneNoArgs(t *testing.T) {
	c := &CloneCard{} // Version=0, SeqNo=0 -> "clone\n"
	got := roundTrip(t, c).(*CloneCard)
	if got.Version != 0 || got.SeqNo != 0 {
		t.Errorf("Clone = %+v, want {0 0}", got)
	}
}

func TestRoundTrip_CloneWithArgs(t *testing.T) {
	c := &CloneCard{Version: 3, SeqNo: 42}
	got := roundTrip(t, c).(*CloneCard)
	if got.Version != 3 || got.SeqNo != 42 {
		t.Errorf("Clone = {%d %d}, want {3 42}", got.Version, got.SeqNo)
	}
}

func TestRoundTrip_CloneSeqNo(t *testing.T) {
	c := &CloneSeqNoCard{SeqNo: 99}
	got := roundTrip(t, c).(*CloneSeqNoCard)
	if got.SeqNo != 99 {
		t.Errorf("SeqNo = %d, want 99", got.SeqNo)
	}
}

func TestRoundTrip_UVGimme(t *testing.T) {
	c := &UVGimmeCard{Name: "data/config.json"}
	got := roundTrip(t, c).(*UVGimmeCard)
	if got.Name != c.Name {
		t.Errorf("Name = %q, want %q", got.Name, c.Name)
	}
}

func TestRoundTrip_PragmaNoValues(t *testing.T) {
	c := &PragmaCard{Name: "sym-pressure"}
	got := roundTrip(t, c).(*PragmaCard)
	if got.Name != c.Name {
		t.Errorf("Name = %q, want %q", got.Name, c.Name)
	}
	if len(got.Values) != 0 {
		t.Errorf("Values = %v, want empty", got.Values)
	}
}

func TestRoundTrip_PragmaWithValues(t *testing.T) {
	c := &PragmaCard{Name: "sym-pressure", Values: []string{"100", "200"}}
	got := roundTrip(t, c).(*PragmaCard)
	if got.Name != c.Name {
		t.Errorf("Name = %q, want %q", got.Name, c.Name)
	}
	if len(got.Values) != 2 || got.Values[0] != "100" || got.Values[1] != "200" {
		t.Errorf("Values = %v, want [100 200]", got.Values)
	}
}

// --- Comment/empty line skipping and unknown cards ---

func TestDecode_SkipCommentAndEmptyLines(t *testing.T) {
	input := "# this is a comment\n\n# another comment\ngimme abc123\n"
	r := bufio.NewReader(strings.NewReader(input))
	card, err := DecodeCard(r)
	if err != nil {
		t.Fatalf("DecodeCard: %v", err)
	}
	g, ok := card.(*GimmeCard)
	if !ok {
		t.Fatalf("got %T, want *GimmeCard", card)
	}
	if g.UUID != "abc123" {
		t.Errorf("UUID = %q, want %q", g.UUID, "abc123")
	}
}

func TestDecode_UnknownCard(t *testing.T) {
	input := "foobar arg1 arg2\n"
	r := bufio.NewReader(strings.NewReader(input))
	card, err := DecodeCard(r)
	if err != nil {
		t.Fatalf("DecodeCard: %v", err)
	}
	unk, ok := card.(*UnknownCard)
	if !ok {
		t.Fatalf("got %T, want *UnknownCard", card)
	}
	if unk.Command != "foobar" {
		t.Errorf("Command = %q, want %q", unk.Command, "foobar")
	}
	if len(unk.Args) != 2 || unk.Args[0] != "arg1" || unk.Args[1] != "arg2" {
		t.Errorf("Args = %v, want [arg1 arg2]", unk.Args)
	}
	if unk.Type() != CardUnknown {
		t.Errorf("Type() = %d, want %d", unk.Type(), CardUnknown)
	}
}

func TestDecode_EOF(t *testing.T) {
	r := bufio.NewReader(strings.NewReader(""))
	_, err := DecodeCard(r)
	if err != io.EOF {
		t.Errorf("got err = %v, want io.EOF", err)
	}
}

func TestDecode_CommentOnlyEOF(t *testing.T) {
	input := "# only comments\n# no cards\n"
	r := bufio.NewReader(strings.NewReader(input))
	_, err := DecodeCard(r)
	if err != io.EOF {
		t.Errorf("got err = %v, want io.EOF", err)
	}
}

func TestDecode_MultipleCards(t *testing.T) {
	input := "gimme aaa\nprivate\nigot bbb\n"
	r := bufio.NewReader(strings.NewReader(input))

	c1, err := DecodeCard(r)
	if err != nil {
		t.Fatalf("card 1: %v", err)
	}
	if _, ok := c1.(*GimmeCard); !ok {
		t.Fatalf("card 1: got %T, want *GimmeCard", c1)
	}

	c2, err := DecodeCard(r)
	if err != nil {
		t.Fatalf("card 2: %v", err)
	}
	if _, ok := c2.(*PrivateCard); !ok {
		t.Fatalf("card 2: got %T, want *PrivateCard", c2)
	}

	c3, err := DecodeCard(r)
	if err != nil {
		t.Fatalf("card 3: %v", err)
	}
	if _, ok := c3.(*IGotCard); !ok {
		t.Fatalf("card 3: got %T, want *IGotCard", c3)
	}

	_, err = DecodeCard(r)
	if err != io.EOF {
		t.Errorf("expected io.EOF after all cards, got %v", err)
	}
}

// --- Payload card arg validation (formerly stubs) ---

func TestDecode_FileBadArgCount(t *testing.T) {
	// "file" with only 1 arg should fail (needs 2 or 3)
	r := bufio.NewReader(strings.NewReader("file abc123\n"))
	_, err := DecodeCard(r)
	if err == nil {
		t.Error("expected error for file with 1 arg")
	}
}

func TestDecode_CFileBadArgCount(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("cfile abc123 100\n"))
	_, err := DecodeCard(r)
	if err == nil {
		t.Error("expected error for cfile with 2 args")
	}
}

func TestDecode_ConfigBadArgCount(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("config\n"))
	_, err := DecodeCard(r)
	if err == nil {
		t.Error("expected error for config with 0 args")
	}
}

func TestDecode_UVFileBadArgCount(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("uvfile name\n"))
	_, err := DecodeCard(r)
	if err == nil {
		t.Error("expected error for uvfile with 1 arg")
	}
}

// --- Arg count validation ---

func TestDecode_IGotBadArgCount(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("igot\n"))
	_, err := DecodeCard(r)
	if err == nil {
		t.Error("expected error for igot with 0 args")
	}
}

// TestDecode_PushOneArg verifies that "push <project-code>" (1 arg) decodes
// correctly — arg[0] is the project code, not the server code.
func TestDecode_PushOneArg(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("push only-one\n"))
	card, err := DecodeCard(r)
	if err != nil {
		t.Fatalf("push with 1 arg should succeed: %v", err)
	}
	push := card.(*PushCard)
	if push.ProjectCode != "only-one" {
		t.Errorf("ProjectCode = %q, want %q", push.ProjectCode, "only-one")
	}
	if push.ServerCode != "" {
		t.Errorf("ServerCode should be empty, got %q", push.ServerCode)
	}
}

// TestEncode_PushEmptyCodes verifies that encoding a PushCard with empty
// ProjectCode panics — bare "push\n" is rejected by the upstream Fossil C
// server, so we prevent it at encode time.
func TestEncode_PushEmptyCodes(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic when encoding PushCard with empty ProjectCode")
		}
	}()
	var buf bytes.Buffer
	_ = EncodeCard(&buf, &PushCard{})
}

// TestEncode_PullEmptyCodes verifies that encoding a PullCard with empty
// codes panics — both project-code and server-code are required by the
// upstream Fossil C xfer parser.
func TestEncode_PullEmptyCodes(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic when encoding PullCard with empty codes")
		}
	}()
	var buf bytes.Buffer
	_ = EncodeCard(&buf, &PullCard{})
}

// TestEncode_PullMissingServerCode verifies that a PullCard with ProjectCode
// but no ServerCode panics — Fossil C requires both args on pull.
func TestEncode_PullMissingServerCode(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic when encoding PullCard with empty ServerCode")
		}
	}()
	var buf bytes.Buffer
	_ = EncodeCard(&buf, &PullCard{ProjectCode: "proj1"})
}

// TestDecode_PushZeroArgs verifies that a bare "push\n" (0 args) is now
// rejected by the decoder — it was previously accepted as a Go-only extension
// that masked the encoder bug.
func TestDecode_PushZeroArgs(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("push\n"))
	_, err := DecodeCard(r)
	if err == nil {
		t.Error("expected error for bare push with 0 args")
	}
}

// TestDecode_PullOneArg verifies that a single-arg "pull <code>\n" is
// rejected — Fossil C requires both project-code and server-code on pull.
func TestDecode_PullOneArg(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("pull only-one\n"))
	_, err := DecodeCard(r)
	if err == nil {
		t.Error("expected error for pull with 1 arg (requires 2)")
	}
}

// TestDecode_PullZeroArgs verifies that a bare "pull\n" is rejected.
func TestDecode_PullZeroArgs(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("pull\n"))
	_, err := DecodeCard(r)
	if err == nil {
		t.Error("expected error for bare pull with 0 args")
	}
}

// TestRoundTrip_PushProjectCodeOnly verifies that a PushCard with only
// ProjectCode set (no ServerCode) round-trips correctly.
// Wire form: "push <project-code>\n"
func TestRoundTrip_PushProjectCodeOnly(t *testing.T) {
	c := &PushCard{ProjectCode: "proj123"}
	var buf bytes.Buffer
	if err := EncodeCard(&buf, c); err != nil {
		t.Fatalf("EncodeCard: %v", err)
	}
	if buf.String() != "push proj123\n" {
		t.Errorf("wire form = %q, want %q", buf.String(), "push proj123\n")
	}
	r := bufio.NewReader(&buf)
	got, err := DecodeCard(r)
	if err != nil {
		t.Fatalf("DecodeCard: %v", err)
	}
	push := got.(*PushCard)
	if push.ProjectCode != "proj123" || push.ServerCode != "" {
		t.Errorf("decoded = %+v, want ProjectCode=proj123, ServerCode=\"\"", push)
	}
}

// TestWireFormat_PushBothCodes verifies the Fossil-wire byte order for push:
// "push <project-code> <server-code>\n" — project first, server second.
// This is the format a real Fossil C client emits on the second+ round when
// both codes are known.
func TestWireFormat_PushBothCodes(t *testing.T) {
	c := &PushCard{ProjectCode: "proj1", ServerCode: "srv1"}
	var buf bytes.Buffer
	if err := EncodeCard(&buf, c); err != nil {
		t.Fatalf("EncodeCard: %v", err)
	}
	const want = "push proj1 srv1\n"
	if buf.String() != want {
		t.Errorf("wire form = %q, want %q", buf.String(), want)
	}
}

// TestWireFormat_PullBothCodes verifies the Fossil-wire byte order for pull:
// "pull <project-code> <server-code>\n" — project first, server second.
func TestWireFormat_PullBothCodes(t *testing.T) {
	c := &PullCard{ProjectCode: "proj1", ServerCode: "srv1"}
	var buf bytes.Buffer
	if err := EncodeCard(&buf, c); err != nil {
		t.Fatalf("EncodeCard: %v", err)
	}
	const want = "pull proj1 srv1\n"
	if buf.String() != want {
		t.Errorf("wire form = %q, want %q", buf.String(), want)
	}
}

func TestDecode_CloneBadArgCount(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("clone 3\n"))
	_, err := DecodeCard(r)
	if err == nil {
		t.Error("expected error for clone with 1 arg")
	}
}

// TestDecode_CloneSeqNoNonDecimal pins §3.2 and §8.2: NEXT is recorded only
// when it is decimal (`decimal = 1*DIGIT`), and a visible but non-decimal
// NEXT is a receiver-tolerance exception that leaves the recorded sequence
// unchanged without stopping later reply cards. Erroring here would abort a
// clone the spec expects to continue, so the card must decode to something
// no consumer will record — never a CloneSeqNoCard.
func TestDecode_CloneSeqNoNonDecimal(t *testing.T) {
	for _, arg := range []string{"notanumber", "-1", "+3", "3x"} {
		r := bufio.NewReader(strings.NewReader("clone_seqno " + arg + "\nprivate\n"))
		card, err := DecodeCard(r)
		if err != nil {
			t.Fatalf("clone_seqno %q: unexpected error %v", arg, err)
		}
		if _, isSeq := card.(*CloneSeqNoCard); isSeq {
			t.Errorf("clone_seqno %q decoded to CloneSeqNoCard; must not be recorded", arg)
		}

		// Parsing must continue into the following card.
		next, err := DecodeCard(r)
		if err != nil {
			t.Fatalf("clone_seqno %q: parsing stopped after non-decimal NEXT: %v", arg, err)
		}
		if _, ok := next.(*PrivateCard); !ok {
			t.Errorf("clone_seqno %q: next card = %T, want *PrivateCard", arg, next)
		}
	}
}

// TestDecode_CloneSeqNoDecimalRecorded is the positive half: a digit-only
// NEXT is recorded.
func TestDecode_CloneSeqNoDecimalRecorded(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("clone_seqno 0\n"))
	card, err := DecodeCard(r)
	if err != nil {
		t.Fatalf("DecodeCard: %v", err)
	}
	seq, ok := card.(*CloneSeqNoCard)
	if !ok {
		t.Fatalf("card = %T, want *CloneSeqNoCard", card)
	}
	if seq.SeqNo != 0 {
		t.Errorf("SeqNo = %d, want 0", seq.SeqNo)
	}
}

// TestDecode_CloneSeqNoPresence pins that the decoder distinguishes a bare
// `clone` from `clone VERSION SEQNO`, which §8.1 treats differently: the
// parsed SeqNo is 0 either way, so only the presence flag separates the
// non-fatal case from the fatal one.
func TestDecode_CloneSeqNoPresence(t *testing.T) {
	tests := []struct {
		line     string
		wantHas  bool
		wantSeq  int
		wantVers int
	}{
		{"clone\n", false, 0, 0},
		{"clone 3 0\n", true, 0, 3},
		{"clone 3 5\n", true, 5, 3},
		{"clone 3 -1\n", false, -1, 3}, // not digit-only: §8.1 fatal withheld
	}
	for _, tt := range tests {
		card, err := DecodeCard(bufio.NewReader(strings.NewReader(tt.line)))
		if err != nil {
			t.Fatalf("%q: %v", tt.line, err)
		}
		c, ok := card.(*CloneCard)
		if !ok {
			t.Fatalf("%q: card = %T, want *CloneCard", tt.line, card)
		}
		if c.SeqNoIsDecimal != tt.wantHas || c.SeqNo != tt.wantSeq || c.Version != tt.wantVers {
			t.Errorf("%q: got SeqNoIsDecimal=%v SeqNo=%d Version=%d, want %v/%d/%d",
				tt.line, c.SeqNoIsDecimal, c.SeqNo, c.Version, tt.wantHas, tt.wantSeq, tt.wantVers)
		}
	}
}

// TestDecode_CloneUnparseableToken pins issue #95: a VERSION or SEQNO token
// that strconv.Atoi cannot parse at all (not just one that fails the §8.1
// digit-only rule) must not fail the whole message. parseClone degrades the
// same way parseCloneSeqNo does for clone_seqno -- carrying the raw tokens
// forward as an UnknownCard -- so a later card in the same message still
// decodes rather than the whole message erroring out on the first Atoi call.
func TestDecode_CloneUnparseableToken(t *testing.T) {
	tests := []struct {
		name string
		line string
	}{
		{"non-numeric version", "clone abc 5\n"},
		{"non-numeric seqno", "clone 3 abc\n"},
		{"both non-numeric", "clone abc xyz\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := bufio.NewReader(strings.NewReader(tt.line + "private\n"))
			card, err := DecodeCard(r)
			if err != nil {
				t.Fatalf("%q: unexpected error %v", tt.line, err)
			}
			uc, ok := card.(*UnknownCard)
			if !ok {
				t.Fatalf("%q: card = %T, want *UnknownCard", tt.line, card)
			}
			if uc.Command != "clone" {
				t.Errorf("%q: Command = %q, want %q", tt.line, uc.Command, "clone")
			}

			// Parsing must continue into the following card.
			next, err := DecodeCard(r)
			if err != nil {
				t.Fatalf("%q: parsing stopped after unparseable clone card: %v", tt.line, err)
			}
			if _, ok := next.(*PrivateCard); !ok {
				t.Errorf("%q: next card = %T, want *PrivateCard", tt.line, next)
			}
		})
	}
}

// --- Wire format verification ---

func TestEncode_WireFormat_IGot(t *testing.T) {
	var buf bytes.Buffer
	EncodeCard(&buf, &IGotCard{UUID: "abc"})
	if buf.String() != "igot abc\n" {
		t.Errorf("wire = %q, want %q", buf.String(), "igot abc\n")
	}
}

func TestEncode_WireFormat_IGotPrivate(t *testing.T) {
	var buf bytes.Buffer
	EncodeCard(&buf, &IGotCard{UUID: "abc", IsPrivate: true})
	if buf.String() != "igot abc 1\n" {
		t.Errorf("wire = %q, want %q", buf.String(), "igot abc 1\n")
	}
}

func TestEncode_WireFormat_Private(t *testing.T) {
	var buf bytes.Buffer
	EncodeCard(&buf, &PrivateCard{})
	if buf.String() != "private\n" {
		t.Errorf("wire = %q, want %q", buf.String(), "private\n")
	}
}

func TestEncode_WireFormat_CloneNoArgs(t *testing.T) {
	var buf bytes.Buffer
	EncodeCard(&buf, &CloneCard{})
	if buf.String() != "clone\n" {
		t.Errorf("wire = %q, want %q", buf.String(), "clone\n")
	}
}

func TestEncode_WireFormat_CloneWithArgs(t *testing.T) {
	var buf bytes.Buffer
	EncodeCard(&buf, &CloneCard{Version: 3, SeqNo: 42})
	if buf.String() != "clone 3 42\n" {
		t.Errorf("wire = %q, want %q", buf.String(), "clone 3 42\n")
	}
}

// --- Task 3: Fossil-encoded cards ---

func TestRoundTrip_Login(t *testing.T) {
	c := &LoginCard{User: "test user", Nonce: "nonce123", Signature: "sig456"}
	got := roundTrip(t, c).(*LoginCard)
	if got.User != "test user" {
		t.Errorf("User = %q, want %q", got.User, "test user")
	}
	if got.Nonce != "nonce123" || got.Signature != "sig456" {
		t.Errorf("Login = %+v, want %+v", got, c)
	}
}

func TestRoundTrip_LoginFossilEncoding(t *testing.T) {
	// Verify the wire format uses Fossil encoding for the user field
	c := &LoginCard{User: "test user", Nonce: "n", Signature: "s"}
	var buf bytes.Buffer
	if err := EncodeCard(&buf, c); err != nil {
		t.Fatal(err)
	}
	wire := buf.String()
	if wire != "login test\\suser n s\n" {
		t.Errorf("wire = %q, want %q", wire, "login test\\suser n s\n")
	}
}

func TestRoundTrip_ErrorMessage(t *testing.T) {
	c := &ErrorCard{Message: "not authorized to write"}
	got := roundTrip(t, c).(*ErrorCard)
	if got.Message != "not authorized to write" {
		t.Errorf("Message = %q, want %q", got.Message, "not authorized to write")
	}
}

func TestRoundTrip_ErrorFossilEncoding(t *testing.T) {
	c := &ErrorCard{Message: "not authorized to write"}
	var buf bytes.Buffer
	if err := EncodeCard(&buf, c); err != nil {
		t.Fatal(err)
	}
	wire := buf.String()
	if wire != "error not\\sauthorized\\sto\\swrite\n" {
		t.Errorf("wire = %q, want %q", wire, "error not\\sauthorized\\sto\\swrite\n")
	}
}

func TestRoundTrip_Message(t *testing.T) {
	c := &MessageCard{Message: "clone in progress"}
	got := roundTrip(t, c).(*MessageCard)
	if got.Message != "clone in progress" {
		t.Errorf("Message = %q, want %q", got.Message, "clone in progress")
	}
}

func TestRoundTrip_MessageFossilEncoding(t *testing.T) {
	c := &MessageCard{Message: "clone in progress"}
	var buf bytes.Buffer
	if err := EncodeCard(&buf, c); err != nil {
		t.Fatal(err)
	}
	wire := buf.String()
	if wire != "message clone\\sin\\sprogress\n" {
		t.Errorf("wire = %q, want %q", wire, "message clone\\sin\\sprogress\n")
	}
}

func TestRoundTrip_UVIGot(t *testing.T) {
	c := &UVIGotCard{
		Name:  "data/config.json",
		MTime: 1700000000,
		Hash:  "abc123def456",
		Size:  4096,
	}
	got := roundTrip(t, c).(*UVIGotCard)
	if got.Name != c.Name {
		t.Errorf("Name = %q, want %q", got.Name, c.Name)
	}
	if got.MTime != c.MTime {
		t.Errorf("MTime = %d, want %d", got.MTime, c.MTime)
	}
	if got.Hash != c.Hash {
		t.Errorf("Hash = %q, want %q", got.Hash, c.Hash)
	}
	if got.Size != c.Size {
		t.Errorf("Size = %d, want %d", got.Size, c.Size)
	}
}

func TestEncode_WireFormat_UVIGot(t *testing.T) {
	c := &UVIGotCard{Name: "f.txt", MTime: 100, Hash: "abc", Size: 42}
	var buf bytes.Buffer
	EncodeCard(&buf, c)
	if buf.String() != "uvigot f.txt 100 abc 42\n" {
		t.Errorf("wire = %q, want %q", buf.String(), "uvigot f.txt 100 abc 42\n")
	}
}

func TestDecode_UVIGotBadArgCount(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("uvigot only-name\n"))
	_, err := DecodeCard(r)
	if err == nil {
		t.Error("expected error for uvigot with 1 arg")
	}
}

func TestDecode_UVIGotBadMTime(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("uvigot name notnum hash 42\n"))
	_, err := DecodeCard(r)
	if err == nil {
		t.Error("expected error for uvigot with non-numeric mtime")
	}
}

// Test that UnknownCard round-trips through encode
func TestRoundTrip_UnknownCard(t *testing.T) {
	c := &UnknownCard{Command: "newcmd", Args: []string{"x", "y"}}
	got := roundTrip(t, c).(*UnknownCard)
	if got.Command != "newcmd" {
		t.Errorf("Command = %q, want %q", got.Command, "newcmd")
	}
	if len(got.Args) != 2 || got.Args[0] != "x" || got.Args[1] != "y" {
		t.Errorf("Args = %v, want [x y]", got.Args)
	}
}

// Test Fossil encoding with special characters: backslash and newline
func TestRoundTrip_ErrorWithBackslash(t *testing.T) {
	c := &ErrorCard{Message: "path\\to\\file"}
	got := roundTrip(t, c).(*ErrorCard)
	if got.Message != c.Message {
		t.Errorf("Message = %q, want %q", got.Message, c.Message)
	}
}

func TestRoundTrip_MessageWithNewline(t *testing.T) {
	c := &MessageCard{Message: "line1\nline2"}
	got := roundTrip(t, c).(*MessageCard)
	if got.Message != c.Message {
		t.Errorf("Message = %q, want %q", got.Message, c.Message)
	}
}

// --- Task 3 additional edge-case tests ---

// Verify that login with spaces in user survives a full encode->wire->decode cycle
func TestRoundTrip_LoginSpacesInUser(t *testing.T) {
	c := &LoginCard{User: "john doe", Nonce: "aaa", Signature: "bbb"}
	var buf bytes.Buffer
	if err := EncodeCard(&buf, c); err != nil {
		t.Fatal(err)
	}
	wire := buf.String()
	// Wire must contain the Fossil-encoded form, NOT raw spaces
	if !strings.Contains(wire, `john\sdoe`) {
		t.Errorf("wire %q does not contain fossil-encoded user", wire)
	}
	// Decode must recover the original plain-text user
	r := bufio.NewReader(strings.NewReader(wire))
	got, err := DecodeCard(r)
	if err != nil {
		t.Fatal(err)
	}
	login := got.(*LoginCard)
	if login.User != "john doe" {
		t.Errorf("User = %q, want %q", login.User, "john doe")
	}
}

func TestDecode_LoginBadArgCount(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("login onlyuser\n"))
	_, err := DecodeCard(r)
	if err == nil {
		t.Error("expected error for login with 1 arg")
	}
}

func TestDecode_ErrorBadArgCount(t *testing.T) {
	// error takes exactly 1 Fossil-encoded token
	r := bufio.NewReader(strings.NewReader("error\n"))
	_, err := DecodeCard(r)
	if err == nil {
		t.Error("expected error for error with 0 args")
	}
}

func TestDecode_MessageBadArgCount(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("message\n"))
	_, err := DecodeCard(r)
	if err == nil {
		t.Error("expected error for message with 0 args")
	}
}

func TestDecode_PragmaBadArgCount(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("pragma\n"))
	_, err := DecodeCard(r)
	if err == nil {
		t.Error("expected error for pragma with 0 args")
	}
}

// UVIGot with large int64 MTime
func TestRoundTrip_UVIGotLargeMTime(t *testing.T) {
	c := &UVIGotCard{Name: "big.bin", MTime: 9999999999, Hash: "deadbeef", Size: 0}
	got := roundTrip(t, c).(*UVIGotCard)
	if got.MTime != 9999999999 {
		t.Errorf("MTime = %d, want 9999999999", got.MTime)
	}
}

// --- Task 4: FileCard ---

func TestRoundTrip_File(t *testing.T) {
	c := &FileCard{UUID: "abc123def456", Content: []byte("hello world")}
	got := roundTrip(t, c).(*FileCard)
	if got.UUID != c.UUID {
		t.Errorf("UUID = %q, want %q", got.UUID, c.UUID)
	}
	if got.DeltaSrc != "" {
		t.Errorf("DeltaSrc = %q, want empty", got.DeltaSrc)
	}
	if !bytes.Equal(got.Content, c.Content) {
		t.Errorf("Content = %q, want %q", got.Content, c.Content)
	}
}

func TestRoundTrip_FileWithDeltaSrc(t *testing.T) {
	c := &FileCard{UUID: "abc123", DeltaSrc: "def456", Content: []byte("delta payload")}
	got := roundTrip(t, c).(*FileCard)
	if got.UUID != c.UUID {
		t.Errorf("UUID = %q, want %q", got.UUID, c.UUID)
	}
	if got.DeltaSrc != c.DeltaSrc {
		t.Errorf("DeltaSrc = %q, want %q", got.DeltaSrc, c.DeltaSrc)
	}
	if !bytes.Equal(got.Content, c.Content) {
		t.Errorf("Content = %q, want %q", got.Content, c.Content)
	}
}

func TestRoundTrip_FileEmptyContent(t *testing.T) {
	c := &FileCard{UUID: "abc123", Content: []byte{}}
	got := roundTrip(t, c).(*FileCard)
	if got.UUID != c.UUID {
		t.Errorf("UUID = %q, want %q", got.UUID, c.UUID)
	}
	if len(got.Content) != 0 {
		t.Errorf("Content length = %d, want 0", len(got.Content))
	}
}

func TestEncode_WireFormat_FileNoTrailingNewline(t *testing.T) {
	c := &FileCard{UUID: "abc123", Content: []byte("data")}
	var buf bytes.Buffer
	if err := EncodeCard(&buf, c); err != nil {
		t.Fatal(err)
	}
	wire := buf.Bytes()
	// Should be: "file abc123 4\ndata" — no trailing \n
	expected := []byte("file abc123 4\ndata")
	if !bytes.Equal(wire, expected) {
		t.Errorf("wire = %q, want %q", wire, expected)
	}
	// Verify no trailing newline
	if len(wire) > 0 && wire[len(wire)-1] == '\n' {
		t.Error("file card wire should NOT end with \\n")
	}
}

func TestDecode_FileTruncatedPayload(t *testing.T) {
	// Header says 100 bytes but only 5 bytes follow
	input := "file abc123 100\nhello"
	r := bufio.NewReader(strings.NewReader(input))
	_, err := DecodeCard(r)
	if err == nil {
		t.Error("expected error for truncated file payload")
	}
}

// --- Task 5: CFileCard ---

func TestRoundTrip_CFile(t *testing.T) {
	c := &CFileCard{UUID: "abc123def456", Content: []byte("hello compressed world")}
	got := roundTrip(t, c).(*CFileCard)
	if got.UUID != c.UUID {
		t.Errorf("UUID = %q, want %q", got.UUID, c.UUID)
	}
	if got.DeltaSrc != "" {
		t.Errorf("DeltaSrc = %q, want empty", got.DeltaSrc)
	}
	if !bytes.Equal(got.Content, c.Content) {
		t.Errorf("Content = %q, want %q", got.Content, c.Content)
	}
}

func TestRoundTrip_CFileWithDeltaSrc(t *testing.T) {
	c := &CFileCard{UUID: "abc123", DeltaSrc: "def456", Content: []byte("delta compressed payload")}
	got := roundTrip(t, c).(*CFileCard)
	if got.UUID != c.UUID {
		t.Errorf("UUID = %q, want %q", got.UUID, c.UUID)
	}
	if got.DeltaSrc != c.DeltaSrc {
		t.Errorf("DeltaSrc = %q, want %q", got.DeltaSrc, c.DeltaSrc)
	}
	if !bytes.Equal(got.Content, c.Content) {
		t.Errorf("Content = %q, want %q", got.Content, c.Content)
	}
}

func TestRoundTrip_CFileLargeContent(t *testing.T) {
	// 100KB of repeated data — should compress well
	content := bytes.Repeat([]byte("ABCDEFGHIJ"), 10240) // 100KB
	c := &CFileCard{UUID: "large123", Content: content}
	got := roundTrip(t, c).(*CFileCard)
	if !bytes.Equal(got.Content, content) {
		t.Errorf("large content mismatch: got %d bytes, want %d", len(got.Content), len(content))
	}
}

// TestDecodeCFile_StoredBlobPrefixedWirePreservedVerbatim is the regression
// test for issue #112: when a cfile card's wire payload already carries
// Fossil's on-disk blob format ([4-byte BE size][zlib data]) -- exactly
// what send_compressed_file transmits, since it reads a blob.content column
// straight off disk -- StoredBlob must be those exact bytes, not a
// recompression of the decompressed content. This is the case that fires
// against a real Fossil server.
func TestDecodeCFile_StoredBlobPrefixedWirePreservedVerbatim(t *testing.T) {
	plain := []byte("some artifact content, long enough that zlib actually does something")

	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	if _, err := zw.Write(plain); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}

	// Fossil's on-disk blob format: 4-byte BE uncompressed-size prefix,
	// then the zlib stream, exactly as send_compressed_file would send it.
	wireBlob := make([]byte, 0, 4+zbuf.Len())
	sizeHdr := make([]byte, 4)
	binary.BigEndian.PutUint32(sizeHdr, uint32(len(plain)))
	wireBlob = append(wireBlob, sizeHdr...)
	wireBlob = append(wireBlob, zbuf.Bytes()...)

	wire := fmt.Sprintf("cfile aabbcc %d %d\n", len(plain), len(wireBlob))
	r := bufio.NewReader(strings.NewReader(wire + string(wireBlob)))
	card, err := DecodeCard(r)
	if err != nil {
		t.Fatalf("DecodeCard: %v", err)
	}
	c, ok := card.(*CFileCard)
	if !ok {
		t.Fatalf("expected *CFileCard, got %T", card)
	}
	if !bytes.Equal(c.Content, plain) {
		t.Fatalf("Content = %q, want %q", c.Content, plain)
	}
	if !bytes.Equal(c.StoredBlob, wireBlob) {
		t.Fatalf("StoredBlob was re-encoded instead of preserved verbatim:\n  got  %x\n  want %x",
			c.StoredBlob, wireBlob)
	}
}

// TestDecodeCFile_StoredBlobPlainZlibGetsPrefixWithoutRecompression covers
// the other wire form: our own encoder sends plain zlib with no size
// prefix. StoredBlob must still be built without a second zlib pass --
// only the 4-byte length header is bookkeeping added on top of the exact
// zlib bytes that were received.
func TestDecodeCFile_StoredBlobPlainZlibGetsPrefixWithoutRecompression(t *testing.T) {
	plain := []byte("other artifact content, also long enough for zlib to matter here")

	c := &CFileCard{UUID: "abc123", Content: plain}
	var buf bytes.Buffer
	if err := EncodeCard(&buf, c); err != nil {
		t.Fatalf("EncodeCard: %v", err)
	}

	r := bufio.NewReader(&buf)
	got, err := DecodeCard(r)
	if err != nil {
		t.Fatalf("DecodeCard: %v", err)
	}
	gotCFile, ok := got.(*CFileCard)
	if !ok {
		t.Fatalf("expected *CFileCard, got %T", got)
	}
	if len(gotCFile.StoredBlob) < 4 {
		t.Fatalf("StoredBlob too short to carry a size prefix: %d bytes", len(gotCFile.StoredBlob))
	}
	gotUsize := binary.BigEndian.Uint32(gotCFile.StoredBlob[:4])
	if int(gotUsize) != len(plain) {
		t.Fatalf("StoredBlob size prefix = %d, want %d", gotUsize, len(plain))
	}
	// Decompressing StoredBlob (skipping the prefix we just checked) must
	// reproduce plain exactly.
	zr, err := zlib.NewReader(bytes.NewReader(gotCFile.StoredBlob[4:]))
	if err != nil {
		t.Fatalf("zlib.NewReader(StoredBlob[4:]): %v", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("zlib read: %v", err)
	}
	if !bytes.Equal(out, plain) {
		t.Fatalf("StoredBlob decompressed = %q, want %q", out, plain)
	}
}

func TestEncode_CFileCompression(t *testing.T) {
	// Verify that compression actually reduces size for compressible data
	content := bytes.Repeat([]byte("AAAA"), 1000)
	c := &CFileCard{UUID: "abc123", Content: content}
	var buf bytes.Buffer
	if err := EncodeCard(&buf, c); err != nil {
		t.Fatal(err)
	}
	wireLen := buf.Len()
	// The wire format includes the header line + compressed data.
	// For 4000 bytes of "AAAA", compressed should be much smaller.
	if wireLen >= len(content) {
		t.Errorf("compressed wire length %d should be less than content length %d", wireLen, len(content))
	}
}

func TestDecode_CFileUSizeMismatch(t *testing.T) {
	// Manually construct a cfile with wrong usize
	content := []byte("hello")
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	zw.Write(content)
	zw.Close()
	// Claim usize is 999 but actual decompressed size is 5
	wire := fmt.Sprintf("cfile abc123 999 %d\n", zbuf.Len())
	input := append([]byte(wire), zbuf.Bytes()...)
	r := bufio.NewReader(bytes.NewReader(input))
	_, err := DecodeCard(r)
	if err == nil {
		t.Error("expected error for cfile usize mismatch")
	}
}

// --- Task 6: ConfigCard ---

func TestRoundTrip_Config(t *testing.T) {
	c := &ConfigCard{Name: "css", Content: []byte("body { color: red; }")}
	got := roundTrip(t, c).(*ConfigCard)
	if got.Name != c.Name {
		t.Errorf("Name = %q, want %q", got.Name, c.Name)
	}
	if !bytes.Equal(got.Content, c.Content) {
		t.Errorf("Content = %q, want %q", got.Content, c.Content)
	}
}

func TestEncode_WireFormat_ConfigTrailingNewline(t *testing.T) {
	c := &ConfigCard{Name: "css", Content: []byte("data")}
	var buf bytes.Buffer
	if err := EncodeCard(&buf, c); err != nil {
		t.Fatal(err)
	}
	wire := buf.Bytes()
	// Should be: "config css 4\ndata\n" — WITH trailing \n
	expected := []byte("config css 4\ndata\n")
	if !bytes.Equal(wire, expected) {
		t.Errorf("wire = %q, want %q", wire, expected)
	}
	// Verify trailing newline IS present
	if wire[len(wire)-1] != '\n' {
		t.Error("config card wire should end with \\n")
	}
}

// --- Task 7: UVFileCard ---

func TestRoundTrip_UVFile(t *testing.T) {
	c := &UVFileCard{
		Name:    "data/config.json",
		MTime:   1700000000,
		Hash:    "abc123def456",
		Size:    13,
		Flags:   0,
		Content: []byte("hello, world!"),
	}
	got := roundTrip(t, c).(*UVFileCard)
	if got.Name != c.Name {
		t.Errorf("Name = %q, want %q", got.Name, c.Name)
	}
	if got.MTime != c.MTime {
		t.Errorf("MTime = %d, want %d", got.MTime, c.MTime)
	}
	if got.Hash != c.Hash {
		t.Errorf("Hash = %q, want %q", got.Hash, c.Hash)
	}
	if got.Size != c.Size {
		t.Errorf("Size = %d, want %d", got.Size, c.Size)
	}
	if got.Flags != c.Flags {
		t.Errorf("Flags = %d, want %d", got.Flags, c.Flags)
	}
	if !bytes.Equal(got.Content, c.Content) {
		t.Errorf("Content = %q, want %q", got.Content, c.Content)
	}
}

func TestRoundTrip_UVFileDeleted(t *testing.T) {
	c := &UVFileCard{
		Name:  "old-file.txt",
		MTime: 1700000000,
		Hash:  "-",
		Size:  0,
		Flags: 0x0001, // deleted
	}
	got := roundTrip(t, c).(*UVFileCard)
	if got.Flags != 0x0001 {
		t.Errorf("Flags = %d, want 1", got.Flags)
	}
	if got.Hash != "-" {
		t.Errorf("Hash = %q, want %q", got.Hash, "-")
	}
	if len(got.Content) != 0 {
		t.Errorf("Content length = %d, want 0 for deleted file", len(got.Content))
	}
}

func TestRoundTrip_UVFileContentOmitted(t *testing.T) {
	c := &UVFileCard{
		Name:  "large-file.bin",
		MTime: 1700000000,
		Hash:  "abc123",
		Size:  0,
		Flags: 0x0004, // content omitted
	}
	got := roundTrip(t, c).(*UVFileCard)
	if got.Flags != 0x0004 {
		t.Errorf("Flags = %d, want 4", got.Flags)
	}
	if len(got.Content) != 0 {
		t.Errorf("Content length = %d, want 0 for content-omitted file", len(got.Content))
	}
}

// Decode a line without trailing newline (EOF-terminated)
func TestDecode_NoTrailingNewline(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("gimme abc123"))
	card, err := DecodeCard(r)
	if err != nil {
		t.Fatalf("DecodeCard: %v", err)
	}
	g, ok := card.(*GimmeCard)
	if !ok {
		t.Fatalf("got %T, want *GimmeCard", card)
	}
	if g.UUID != "abc123" {
		t.Errorf("UUID = %q, want %q", g.UUID, "abc123")
	}
}

// TestDecodeCFile_FossilBlobFormat verifies that cfile cards using Fossil's
// native blob compression format (4-byte BE size prefix + zlib) are decoded
// correctly. This is what Fossil's send_compressed_file() sends during clone v3,
// reading raw blob content directly from the blob table (xfer.c:657-683).
func TestDecodeCFile_FossilBlobFormat(t *testing.T) {
	content := []byte("hello from fossil blob format")
	uuid := "fossilblob123"

	// Create Fossil blob-format payload: [4-byte BE uncompressed size][zlib data].
	var compressed bytes.Buffer
	var sizePrefix [4]byte
	binary.BigEndian.PutUint32(sizePrefix[:], uint32(len(content)))
	compressed.Write(sizePrefix[:])
	zw := zlib.NewWriter(&compressed)
	zw.Write(content)
	zw.Close()

	// Build raw cfile wire: "cfile UUID USIZE CSIZE\nPAYLOAD"
	csize := compressed.Len()
	line := fmt.Sprintf("cfile %s %d %d\n", uuid, len(content), csize)

	var wire bytes.Buffer
	wire.WriteString(line)
	wire.Write(compressed.Bytes())
	wire.WriteByte('\n')

	r := bufio.NewReader(&wire)
	card, err := DecodeCard(r)
	if err != nil {
		t.Fatalf("DecodeCard: %v", err)
	}
	cf, ok := card.(*CFileCard)
	if !ok {
		t.Fatalf("expected *CFileCard, got %T", card)
	}
	if cf.UUID != uuid {
		t.Errorf("UUID = %q, want %q", cf.UUID, uuid)
	}
	if !bytes.Equal(cf.Content, content) {
		t.Errorf("Content = %q, want %q", cf.Content, content)
	}
}

// --- ci-lock pragma round-trip ---

func TestCkinLockPragma_RoundTrip(t *testing.T) {
	cards := []Card{
		&PragmaCard{Name: "ci-lock", Values: []string{"abc123def456", "client-001"}},
		&PragmaCard{Name: "ci-lock-fail", Values: []string{"alice", "1712000000"}},
	}
	msg := &Message{Cards: cards}
	encoded, err := msg.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(decoded.Cards) != 2 {
		t.Fatalf("got %d cards, want 2", len(decoded.Cards))
	}

	lock := decoded.Cards[0].(*PragmaCard)
	if lock.Name != "ci-lock" || len(lock.Values) != 2 ||
		lock.Values[0] != "abc123def456" || lock.Values[1] != "client-001" {
		t.Fatalf("ci-lock = %+v", lock)
	}

	fail := decoded.Cards[1].(*PragmaCard)
	if fail.Name != "ci-lock-fail" || len(fail.Values) != 2 ||
		fail.Values[0] != "alice" || fail.Values[1] != "1712000000" {
		t.Fatalf("ci-lock-fail = %+v", fail)
	}
}

// TestCloneCardWireRoundTrip pins that every clone wire form re-encodes to
// the bytes it was decoded from. The bare `clone` and `clone 0 0` are
// distinct on the wire but both parse to Version 0 / SeqNo 0, so only
// SeqNoIsDecimal separates them — consuming that field as mere presence
// would collapse `clone 0 0` back into a bare `clone`.
func TestCloneCardWireRoundTrip(t *testing.T) {
	for _, line := range []string{
		"clone\n",
		"clone 0 0\n",
		"clone 3 0\n",
		"clone 3 1\n",
		"clone 3 4096\n",
		"clone 3 -1\n",
		"clone 1 7\n",
	} {
		card, err := DecodeCard(bufio.NewReader(strings.NewReader(line)))
		if err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		var buf bytes.Buffer
		if err := EncodeCard(&buf, card); err != nil {
			t.Fatalf("encode %q: %v", line, err)
		}
		if buf.String() != line {
			t.Errorf("round trip %q -> %q", line, buf.String())
		}
	}
}
