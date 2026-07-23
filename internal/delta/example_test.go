package delta_test

import (
	"fmt"

	"github.com/danmestas/go-libfossil/internal/delta"
)

func ExampleCreate() {
	source := []byte("the quick brown fox jumps over the lazy dog\n")
	target := []byte("the quick brown fox leaps over the lazy dog\n")

	d := delta.Create(source, target)

	// Delta is smaller than storing the full target.
	fmt.Printf("delta smaller: %v\n", len(d) < len(target))

	// Apply reconstructs the target from source + delta.
	reconstructed, err := delta.Apply(source, d)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Print(string(reconstructed))
	// Output:
	// delta smaller: true
	// the quick brown fox leaps over the lazy dog
}
