package sync

import (
	"context"
	"testing"

	"github.com/danmestas/go-libfossil/internal/auth"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// signedRequest returns the request a client logged in as user would send:
// cards behind a login card whose nonce covers their wire bytes, decoded the
// way the server decodes a request, so the login card carries its tail.
func signedRequest(
	t *testing.T, user, password, projectCode string, cards ...xfer.Card,
) *xfer.Message {
	t.Helper()
	payload, err := encodeCards(cards)
	if err != nil {
		t.Fatalf("encodeCards: %v", err)
	}
	return wireRequest(t, computeLogin(user, password, projectCode, payload), cards...)
}

// wireRequest puts login ahead of cards, encodes the message and decodes it
// again, as the request arrives at a server.
func wireRequest(t *testing.T, login *xfer.LoginCard, cards ...xfer.Card) *xfer.Message {
	t.Helper()
	all := append([]xfer.Card{login}, cards...)
	wire, err := (&xfer.Message{Cards: all}).Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	msg, err := xfer.Decode(wire, xfer.ContentTypeCompressed)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return msg
}

// loginRepo returns a repo whose nobody may only pull and whose "alice" may
// only push, so each test can tell which capabilities a request ran with.
func loginRepo(t *testing.T) (*repo.Repo, string) {
	t.Helper()
	r := setupSyncTestRepo(t)
	pc := testProjectCode(t, r.DB())
	if _, err := r.DB().Exec("UPDATE user SET cap='o' WHERE login='nobody'"); err != nil {
		t.Fatalf("set nobody caps: %v", err)
	}
	if err := auth.CreateUser(r.DB(), pc, "alice", "secret", "i"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return r, pc
}

// requireLoginRefused asserts that resp is the whole of a refused login: the
// one error card fossil sends (src/xfer.c page_xfer resets the reply and
// emits "error login\sfailed"), and nothing processed after it.
func requireLoginRefused(t *testing.T, resp *xfer.Message, want string) {
	t.Helper()
	if len(resp.Cards) != 1 {
		t.Fatalf("reply has %d cards, want only the error: %#v", len(resp.Cards), resp.Cards)
	}
	ec, ok := resp.Cards[0].(*xfer.ErrorCard)
	if !ok || ec.Message != want {
		t.Fatalf("reply = %#v, want error %q", resp.Cards[0], want)
	}
}

// A login card captured from one request must not authenticate another body.
// The nonce is the SHA-1 of the body that followed the login line (§6.2), and
// the server has to check it, or the signature proves nothing about the body.
func TestHandleReplayedLoginRefused(t *testing.T) {
	r, pc := loginRepo(t)
	captured := signedRequest(t, "alice", "secret", pc,
		&xfer.PullCard{ServerCode: "sc", ProjectCode: pc},
	).Cards[0].(*xfer.LoginCard)

	data := []byte("pushed under a replayed login")
	replay := wireRequest(t, captured,
		&xfer.PushCard{ServerCode: "sc", ProjectCode: pc},
		&xfer.FileCard{UUID: hash.SHA1(data), Content: data},
	)
	resp, err := HandleSync(context.Background(), r, replay)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	requireLoginRefused(t, resp, "login failed")
	if _, ok := blob.Exists(r.DB(), hash.SHA1(data)); ok {
		t.Fatal("a replayed login card stored a pushed file")
	}
}

// A login card that was never decoded from a request has no body tail, so it
// cannot vouch for one.
func TestHandleUndecodedLoginRefused(t *testing.T) {
	r, pc := loginRepo(t)
	payload := []byte("anything")
	req := &xfer.Message{Cards: []xfer.Card{
		computeLogin("alice", "secret", pc, payload),
		&xfer.PullCard{ServerCode: "sc", ProjectCode: pc},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	requireLoginRefused(t, resp, "login failed")
}

// A failed login ends the exchange: fossil does not carry on as nobody.
func TestHandleFailedLoginIsFatal(t *testing.T) {
	r, pc := loginRepo(t)
	storeTestBlob(t, r, []byte("visible to nobody"))
	req := signedRequest(t, "alice", "wrong", pc,
		&xfer.PullCard{ServerCode: "sc", ProjectCode: pc},
	)
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	requireLoginRefused(t, resp, "login failed")
}

// A successful login adds the user's capabilities to nobody's; it does not
// replace them (src/xfer.c check_login calls login_set_capabilities, which
// only sets flags). alice may push, nobody may pull, so alice may do both.
func TestHandleLoginAddsToNobodyCaps(t *testing.T) {
	r, pc := loginRepo(t)
	storeTestBlob(t, r, []byte("already on the server"))
	data := []byte("pushed by alice")
	req := signedRequest(t, "alice", "secret", pc,
		&xfer.PushCard{ServerCode: "sc", ProjectCode: pc},
		&xfer.PullCard{ServerCode: "sc", ProjectCode: pc},
		&xfer.FileCard{UUID: hash.SHA1(data), Content: data},
	)
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	if errs := findCards[*xfer.ErrorCard](resp); len(errs) > 0 {
		t.Fatalf("unexpected error: %s", errs[0].Message)
	}
	if _, ok := blob.Exists(r.DB(), hash.SHA1(data)); !ok {
		t.Fatal("alice's push was not stored")
	}
	if len(findCards[*xfer.IGotCard](resp)) == 0 {
		t.Fatal("alice lost nobody's pull capability by logging in")
	}
}

// One login per request (§6.1). A second card is fatal even when both verify.
func TestHandleMultipleLoginCardsRefused(t *testing.T) {
	r, pc := loginRepo(t)
	pull := &xfer.PullCard{ServerCode: "sc", ProjectCode: pc}
	payload, err := encodeCards([]xfer.Card{pull})
	if err != nil {
		t.Fatalf("encodeCards: %v", err)
	}
	second := computeLogin("alice", "secret", pc, payload)
	req := signedRequest(t, "alice", "secret", pc, second, pull)
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	requireLoginRefused(t, resp, "multiple login cards")
}

// Anybody may sync as nobody or anonymous (src/xfer.c check_login): the login
// succeeds without a password and leaves nobody's capabilities in force.
func TestHandleAnonymousLoginKeepsNobodyCaps(t *testing.T) {
	r, pc := loginRepo(t)
	storeTestBlob(t, r, []byte("visible to nobody"))
	req := signedRequest(t, "anonymous", "", pc,
		&xfer.PullCard{ServerCode: "sc", ProjectCode: pc},
	)
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	if errs := findCards[*xfer.ErrorCard](resp); len(errs) > 0 {
		t.Fatalf("unexpected error: %s", errs[0].Message)
	}
	if len(findCards[*xfer.IGotCard](resp)) == 0 {
		t.Fatal("an anonymous login lost nobody's pull capability")
	}
}

// A user row with no password cannot be logged into (src/xfer.c check_login
// requires length(pw)>0): its signature would be SHA1(nonce) alone, which
// anyone can compute.
func TestHandleEmptyPasswordLoginRefused(t *testing.T) {
	r, pc := loginRepo(t)
	if _, err := r.DB().Exec("UPDATE user SET pw='' WHERE login='alice'"); err != nil {
		t.Fatalf("clear pw: %v", err)
	}
	pull := &xfer.PullCard{ServerCode: "sc", ProjectCode: pc}
	payload, err := encodeCards([]xfer.Card{pull})
	if err != nil {
		t.Fatalf("encodeCards: %v", err)
	}
	nonce := sha1Hex(payload)
	login := &xfer.LoginCard{User: "alice", Nonce: nonce, Signature: sha1Hex([]byte(nonce))}
	resp, err := HandleSync(context.Background(), r, wireRequest(t, login, pull))
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	requireLoginRefused(t, resp, "login failed")
}
