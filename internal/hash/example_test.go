package hash_test

import (
	"fmt"

	"github.com/danmestas/go-libfossil/internal/hash"
)

func ExampleSHA1() {
	uuid := hash.SHA1([]byte("hello"))
	fmt.Println(uuid)
	fmt.Println(hash.IsValidHash(uuid))
	// Output:
	// aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d
	// true
}

func ExampleSHA3() {
	uuid := hash.SHA3([]byte("hello"))
	fmt.Println(len(uuid)) // 64 hex chars = SHA3-256
	fmt.Println(hash.IsValidHash(uuid))
	// Output:
	// 64
	// true
}
