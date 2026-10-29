package attachstub

import (
	"strings"
	"testing"
)

const sum = "4f9a2c1d0b3e5f6a7b8c9d0e1f2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c"

// The exact bytes Quarry's internal/attach Stub.Format writes for the RFC
// 0016 example, typed out rather than produced by Format, so a drift in
// either direction fails here.
const quarryStub = "quarry-attachment 1\n" +
	"sha256 " + sum + "\n" +
	"size 18349022\n" +
	"type video/mp4\n" +
	"name screen recording.mp4\n"

func TestFormatIsQuarrysBytes(t *testing.T) {
	ref := Ref{SHA256: sum, Size: 18349022, Type: "video/mp4", Name: "screen recording.mp4"}
	raw, err := Format(ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != quarryStub {
		t.Fatalf("Format = %q, want %q", raw, quarryStub)
	}
	got, ok := Parse(raw)
	if !ok || got != ref {
		t.Fatalf("Parse(Format) = %+v, %v", got, ok)
	}
	if got, ok := Parse([]byte(strings.TrimSuffix(quarryStub, "\n"))); !ok || got != ref {
		t.Fatalf("Parse without the trailing newline = %+v, %v", got, ok)
	}
}

// Parse refuses every drift Quarry's parser refuses, so the two never
// disagree about which blobs are stubs.
func TestParseIsStrict(t *testing.T) {
	good := quarryStub
	bad := map[string]string{
		"no header":     strings.Replace(good, "quarry-attachment 1", "quarry-attachment 0", 1),
		"header only":   "quarry-attachment 1\n",
		"bare header":   "quarry-attachment 1",
		"short sha":     strings.Replace(good, sum, sum[:63], 1),
		"upper sha":     strings.Replace(good, sum, strings.ToUpper(sum), 1),
		"size words":    strings.Replace(good, "size 18349022", "size twelve", 1),
		"size sign":     strings.Replace(good, "size 18349022", "size +18349022", 1),
		"size negative": strings.Replace(good, "size 18349022", "size -1", 1),
		"size zero pad": strings.Replace(good, "size 18349022", "size 018349022", 1),
		"unknown key":   good + "url http://x\n",
		"missing name":  strings.Replace(good, "name screen recording.mp4\n", "", 1),
		"twice":         strings.Replace(good, "type video/mp4\n", "type video/mp4\ntype video/mp4\n", 1),
		"empty value":   strings.Replace(good, "type video/mp4", "type ", 1),
		"no value":      strings.Replace(good, "type video/mp4", "type", 1),
		"two newlines":  good + "\n",
		"crlf":          strings.ReplaceAll(good, "\n", "\r\n"),
		"too long":      good + strings.Repeat("x", MaxBytes),
		"empty":         "",
		"a file":        "%PDF-1.7\n",
	}
	for name, raw := range bad {
		if _, ok := Parse([]byte(raw)); ok {
			t.Errorf("%s parsed as a stub", name)
		}
	}
}

// Format refuses a reference Parse would not read back, so nothing written
// here turns into a plain file the moment it is listed.
func TestFormatRefusesWhatParseWouldNotRead(t *testing.T) {
	ok := Ref{SHA256: sum, Size: 0, Type: "text/plain", Name: "a.txt"}
	if _, err := Format(ok); err != nil {
		t.Fatalf("zero-size ref refused: %v", err)
	}
	bad := map[string]Ref{
		"short sha":      {SHA256: sum[:63], Size: 1, Type: "a/b", Name: "n"},
		"upper sha":      {SHA256: strings.ToUpper(sum), Size: 1, Type: "a/b", Name: "n"},
		"negative size":  {SHA256: sum, Size: -1, Type: "a/b", Name: "n"},
		"empty type":     {SHA256: sum, Size: 1, Type: "", Name: "n"},
		"empty name":     {SHA256: sum, Size: 1, Type: "a/b", Name: ""},
		"newline type":   {SHA256: sum, Size: 1, Type: "a/b\nurl x", Name: "n"},
		"newline name":   {SHA256: sum, Size: 1, Type: "a/b", Name: "n\nurl x"},
		"cr name":        {SHA256: sum, Size: 1, Type: "a/b", Name: "n\r"},
		"oversized stub": {SHA256: sum, Size: 1, Type: "a/b", Name: strings.Repeat("n", MaxBytes)},
	}
	for name, ref := range bad {
		if _, err := Format(ref); err == nil {
			t.Errorf("%s formatted", name)
		}
	}
}
