package fsltype

import "testing"

func TestHTTPize(t *testing.T) {
	if got, want := HTTPize("mo ved&/x.$~-_Az9é"), "mo+ved%26%2Fx.$~-_Az9%C3%A9"; got != want {
		t.Errorf("HTTPize = %q, want %q", got, want)
	}
}

func TestHTMLize(t *testing.T) {
	if got, want := HTMLize(`<a href="x">'&'</a>`), "&lt;a href=&quot;x&quot;&gt;&#39;&amp;&#39;&lt;/a&gt;"; got != want {
		t.Errorf("HTMLize = %q, want %q", got, want)
	}
}
