package content

import "testing"

func TestExpand_NilQuerier(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil querier")
		}
	}()
	Expand(nil, 1)
}

func TestVerify_NilQuerier(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil querier")
		}
	}()
	Verify(nil, 1)
}

func TestIsPhantom_NilQuerier(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil querier")
		}
	}()
	IsPhantom(nil, 1)
}
