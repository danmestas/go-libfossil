package repo

import "testing"

// TestSettingBoolean reads a stored setting as fossil's db_get_boolean does:
// is_truth wins, then is_false, and anything else keeps the default.
func TestSettingBoolean(t *testing.T) {
	for _, tc := range []struct {
		value string
		dflt  bool
		want  bool
	}{
		{"on", false, true}, {"YES", false, true}, {"True", false, true},
		{"1", false, true}, {" 2x", false, true}, {"-1", false, true},
		{"off", true, false}, {"No", true, false}, {"FALSE", true, false},
		{"0", true, false},
		{"00", true, true}, {"00", false, false}, {"", true, true},
		{"junk", true, true}, {"junk", false, false}, {" 0", true, true},
	} {
		if got := SettingBoolean(tc.value, tc.dflt); got != tc.want {
			t.Errorf("SettingBoolean(%q, %t) = %t, want %t", tc.value, tc.dflt, got, tc.want)
		}
	}
}
