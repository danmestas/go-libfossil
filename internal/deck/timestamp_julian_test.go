package deck

import "testing"

// TestScanJulianNumber pins the §4.6.4 form 5 lexical grammar, including the
// spellings strconv.ParseFloat accepts but SQLite's julianday() does not.
func TestScanJulianNumber(t *testing.T) {
	for _, s := range []string{"1", "+1", "-1", ".5", "1.", "1.5", "2.45e6", "1E-3", "1e+3"} {
		if !scanJulianNumber(s) {
			t.Errorf("scanJulianNumber(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", ".", "+", "e5", "1e", "1e+",
		"0x10", "inf", "nan", "1_0", "1.5.2", " 1", "1 "} {
		if scanJulianNumber(s) {
			t.Errorf("scanJulianNumber(%q) = true, want false", s)
		}
	}
}

// TestParseJulianTimestampRange checks the bound is applied after rounding to
// the millisecond and that huge exponents cannot overflow the conversion.
func TestParseJulianTimestampRange(t *testing.T) {
	for _, s := range []string{"1e-10", "5373484.5", "1e300", "0", "-1", "+0.0",
		"0.00000000001", "5373484.499999995"} {
		if _, matched, err := parseJulianTimestamp(s); !matched || err == nil {
			t.Errorf("parseJulianTimestamp(%q): matched=%v err=%v, want rejection", s, matched, err)
		}
	}
	if _, _, err := parseJulianTimestamp("+2461042.5"); err != nil {
		t.Errorf("signed Julian day rejected: %v", err)
	}
	// 5.8e-9 days rounds to one millisecond, the smallest accepted value.
	if _, _, err := parseJulianTimestamp("5.8e-9"); err != nil {
		t.Errorf("one-millisecond Julian day rejected: %v", err)
	}
}
