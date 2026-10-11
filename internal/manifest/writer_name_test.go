package manifest

import "testing"

// A writer may not create a filename that a parser would nonetheless accept
// (section 4.6.1 of the artifact format).
func TestCheckWriterName(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{"", false}, // OldName's "no rename"
		{"dir/a b.txt", false},
		{"é/ü.txt", false},
		{"a\\b.txt", true},
		{"a\xff.txt", true},
		{"a\xed\xa0\x80.txt", true}, // UTF-16 surrogate U+D800
		{"a\xc0\x80.txt", true},     // overlong NUL
	}
	for _, tt := range tests {
		if err := checkWriterName(tt.name); (err != nil) != tt.wantErr {
			t.Errorf("checkWriterName(%q) = %v, wantErr %v", tt.name, err, tt.wantErr)
		}
	}
}
