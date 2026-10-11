package auth

import "testing"

func TestHasCapability(t *testing.T) {
	tests := []struct {
		caps     string
		required byte
		want     bool
	}{
		{"oi", 'o', true},
		{"oi", 'i', true},
		{"oi", 'g', false},
		{"", 'o', false},
		{"cghijknorswy", 's', true},
		{"cghijknorswy", 'a', false},
	}
	for _, tt := range tests {
		got := HasCapability(tt.caps, tt.required)
		if got != tt.want {
			t.Errorf("HasCapability(%q, %q) = %v, want %v", tt.caps, tt.required, got, tt.want)
		}
	}
}

func TestCanPush(t *testing.T) {
	if !CanPush("oi") { t.Error("CanPush(oi) should be true") }
	if CanPush("o") { t.Error("CanPush(o) should be false") }
}

func TestCanPull(t *testing.T) {
	if !CanPull("oi") { t.Error("CanPull(oi) should be true") }
	if CanPull("i") { t.Error("CanPull(i) should be false") }
}

func TestCanClone(t *testing.T) {
	if !CanClone("g") { t.Error("CanClone(g) should be true") }
	if CanClone("oi") { t.Error("CanClone(oi) should be false") }
}

func TestCanPushUV(t *testing.T) {
	if !CanPushUV("y") { t.Error("CanPushUV(y) should be true") }
	if CanPushUV("oi") { t.Error("CanPushUV(oi) should be false") }
}

func TestCanSyncPrivate(t *testing.T) {
	if !CanSyncPrivate("x") { t.Error("CanSyncPrivate(x) should be true") }
	if CanSyncPrivate("oi") { t.Error("CanSyncPrivate(oi) should be false") }
	if CanSyncPrivate("as") { t.Error("CanSyncPrivate(as) should be false — x must be explicit") }
}

func TestFossilImpliedCapabilities(t *testing.T) {
	tests := []struct {
		caps                  string
		read, admin, readAddr bool
	}{
		{"", false, false, false},
		{"g", false, false, false},
		{"o", true, false, false},
		{"i", true, false, false},
		{"e", false, false, true},
		{"a", true, true, true},
		{"s", true, true, true},
	}
	for _, tt := range tests {
		if got := CanRead(tt.caps); got != tt.read {
			t.Errorf("CanRead(%q) = %v, want %v", tt.caps, got, tt.read)
		}
		if got := IsAdmin(tt.caps); got != tt.admin {
			t.Errorf("IsAdmin(%q) = %v, want %v", tt.caps, got, tt.admin)
		}
	}
}
