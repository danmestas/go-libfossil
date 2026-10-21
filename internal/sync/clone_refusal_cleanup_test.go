package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// A refusal still owes Fossil's operation-less second round. Its reply must
// never turn that failed clone into a success or introduce more content.
func TestCloneRefusalCleanupIgnoresPayload(t *testing.T) {
	for _, mode := range []string{"dense", "sparse"} {
		t.Run(mode, func(t *testing.T) {
			opts := CloneOpts{}
			if mode == "sparse" {
				c, err := cone.New([]string{"src"}, nil)
				if err != nil {
					t.Fatal(err)
				}
				opts.StoreCone = c
			}
			calls := 0
			peer := refusalCleanupPeer(t, &calls)
			path := filepath.Join(t.TempDir(), "refused.fossil")
			r, result, err := Clone(context.Background(), path, peer, opts)
			if r != nil {
				r.Close()
				t.Fatal("refused clone returned a repository")
			}
			if err == nil || !strings.Contains(err.Error(), "origin refuses clone") {
				t.Fatalf("lost refusal: %v", err)
			}
			if calls != 2 || result == nil || result.BlobsRecvd != 0 {
				t.Fatalf("refusal calls=%d result=%+v, want cleanup and no content", calls, result)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("refused clone left database: %v", err)
			}
		})
	}
}

func refusalCleanupPeer(t *testing.T, calls *int) Transport {
	t.Helper()
	return &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		*calls++
		if *calls == 1 {
			return &xfer.Message{Cards: []xfer.Card{
				&xfer.ErrorCard{Message: "origin refuses clone"},
			}}
		}
		if *calls != 2 {
			t.Fatalf("clone continued after refusal cleanup: %d exchanges", *calls)
		}
		for _, card := range req.Cards {
			switch card.(type) {
			case *xfer.CloneCard, *xfer.PullCard, *xfer.PushCard, *xfer.GimmeCard:
				t.Fatalf("refusal cleanup requested content: %T", card)
			}
		}
		data := []byte("unsolicited cleanup payload")
		return &xfer.Message{Cards: []xfer.Card{
			&xfer.PushCard{ServerCode: strings.Repeat("a", 40), ProjectCode: strings.Repeat("b", 40)},
			&xfer.FileCard{UUID: hash.SHA3(data), Content: data},
			&xfer.CloneSeqNoCard{SeqNo: 0},
		}}
	}}
}
