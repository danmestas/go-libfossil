package xfer_test

import (
	"fmt"

	"github.com/danmestas/libfossil/internal/xfer"
)

func ExampleMessage_Encode() {
	msg := &xfer.Message{
		Cards: []xfer.Card{
			&xfer.PushCard{ProjectCode: "abc123"},
			&xfer.PullCard{ProjectCode: "abc123", ServerCode: "def456"},
			&xfer.IGotCard{UUID: "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d"},
		},
	}

	wire, err := msg.Encode()
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	// Wire format is zlib-compressed xfer cards.
	fmt.Printf("compressed: %v\n", len(wire) > 0)

	// Decode round-trips back to a Message.
	decoded, err := xfer.Decode(wire)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("cards: %d\n", len(decoded.Cards))
	// Output:
	// compressed: true
	// cards: 3
}
