package merge_test

import (
	"fmt"

	"github.com/danmestas/go-libfossil/internal/merge"
)

func ExampleThreeWayText() {
	base := []byte("line 1\nline 2\nline 3\n")
	local := []byte("line 1\nlocal change\nline 3\n")
	remote := []byte("line 1\nline 2\nline 3\nremote addition\n")

	result, err := (&merge.ThreeWayText{}).Merge(base, local, remote)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("clean:", result.Clean)
	fmt.Print(string(result.Content))
	// Output:
	// clean: true
	// line 1
	// local change
	// line 3
	// remote addition
}
