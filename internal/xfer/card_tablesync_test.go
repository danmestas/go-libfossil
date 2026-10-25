package xfer

import (
	"bufio"
	"bytes"
	"testing"
)

func TestSchemaCardType(t *testing.T) {
	c := &SchemaCard{Table: "peer_registry", Version: 1, Hash: "abc", MTime: 100, Content: []byte(`{}`)}
	if c.Type() != CardSchema {
		t.Fatalf("got %v, want CardSchema", c.Type())
	}
}

func TestXIGotCardType(t *testing.T) {
	c := &XIGotCard{Table: "peer_registry", PKHash: "abc", MTime: 100}
	if c.Type() != CardXIGot {
		t.Fatalf("got %v, want CardXIGot", c.Type())
	}
}

func TestXGimmeCardType(t *testing.T) {
	c := &XGimmeCard{Table: "peer_registry", PKHash: "abc"}
	if c.Type() != CardXGimme {
		t.Fatalf("got %v, want CardXGimme", c.Type())
	}
}

func TestXRowCardType(t *testing.T) {
	c := &XRowCard{Table: "peer_registry", PKHash: "abc", MTime: 100, Content: []byte(`{}`)}
	if c.Type() != CardXRow {
		t.Fatalf("got %v, want CardXRow", c.Type())
	}
}

func TestSchemaCard_RoundTrip(t *testing.T) {
	original := &SchemaCard{
		Table: "peer_registry", Version: 1, Hash: "abc123",
		MTime:   1711300000,
		Content: []byte(`{"columns":[{"name":"id","type":"text","pk":true}],"conflict":"self-write"}`),
	}
	var buf bytes.Buffer
	if err := EncodeCard(&buf, original); err != nil {
		t.Fatalf("encode: %v", err)
	}
	r := bufio.NewReader(&buf)
	got, err := DecodeCard(r)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	sc, ok := got.(*SchemaCard)
	if !ok {
		t.Fatalf("got %T, want *SchemaCard", got)
	}
	if sc.Table != "peer_registry" || sc.Version != 1 || sc.Hash != "abc123" || sc.MTime != 1711300000 {
		t.Fatalf("header mismatch: %+v", sc)
	}
	if !bytes.Equal(sc.Content, original.Content) {
		t.Fatalf("content mismatch: got %q", sc.Content)
	}
}

func TestXIGotCard_RoundTrip(t *testing.T) {
	original := &XIGotCard{Table: "peer_registry", PKHash: "abc123", MTime: 1711300000}
	var buf bytes.Buffer
	if err := EncodeCard(&buf, original); err != nil {
		t.Fatalf("encode: %v", err)
	}
	r := bufio.NewReader(&buf)
	got, err := DecodeCard(r)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	xig, ok := got.(*XIGotCard)
	if !ok {
		t.Fatalf("got %T, want *XIGotCard", got)
	}
	if xig.Table != "peer_registry" || xig.PKHash != "abc123" || xig.MTime != 1711300000 {
		t.Fatalf("mismatch: %+v", xig)
	}
}

func TestXGimmeCard_RoundTrip(t *testing.T) {
	original := &XGimmeCard{Table: "peer_registry", PKHash: "abc123"}
	var buf bytes.Buffer
	if err := EncodeCard(&buf, original); err != nil {
		t.Fatalf("encode: %v", err)
	}
	r := bufio.NewReader(&buf)
	got, err := DecodeCard(r)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	xg, ok := got.(*XGimmeCard)
	if !ok {
		t.Fatalf("got %T, want *XGimmeCard", got)
	}
	if xg.Table != "peer_registry" || xg.PKHash != "abc123" {
		t.Fatalf("mismatch: %+v", xg)
	}
}

func TestXDeleteCardType(t *testing.T) {
	c := &XDeleteCard{Table: "devices", PKHash: "abc123", MTime: 2000, PKData: []byte(`{"device_id":"d1"}`)}
	if c.Type() != CardXDelete {
		t.Fatalf("got %v, want CardXDelete", c.Type())
	}
}

func TestXDeleteCard_RoundTrip(t *testing.T) {
	original := &XDeleteCard{
		Table:  "devices",
		PKHash: "abc123def456",
		MTime:  1711300000,
		PKData: []byte(`{"device_id":"d1"}`),
	}
	var buf bytes.Buffer
	if err := EncodeCard(&buf, original); err != nil {
		t.Fatalf("encode: %v", err)
	}
	r := bufio.NewReader(&buf)
	got, err := DecodeCard(r)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	xd, ok := got.(*XDeleteCard)
	if !ok {
		t.Fatalf("got %T, want *XDeleteCard", got)
	}
	if xd.Table != original.Table || xd.PKHash != original.PKHash || xd.MTime != original.MTime {
		t.Errorf("fields mismatch: got %+v", xd)
	}
	if string(xd.PKData) != string(original.PKData) {
		t.Errorf("PKData = %q, want %q", xd.PKData, original.PKData)
	}
}

func FuzzParseXDelete(f *testing.F) {
	f.Add("devices abc123 1711300000 18\n{\"device_id\":\"d1\"}\n")
	f.Add("t a 0 0\n\n")
	f.Add("")
	f.Fuzz(func(t *testing.T, input string) {
		r := bufio.NewReader(bytes.NewReader([]byte("xdelete " + input)))
		_, _ = DecodeCard(r) // must not panic
	})
}

func TestXRowCard_RoundTrip(t *testing.T) {
	original := &XRowCard{
		Table: "peer_registry", PKHash: "abc123", MTime: 1711300000,
		Content: []byte(`{"peer_id":"leaf-01","version":"0.5.0"}`),
	}
	var buf bytes.Buffer
	if err := EncodeCard(&buf, original); err != nil {
		t.Fatalf("encode: %v", err)
	}
	r := bufio.NewReader(&buf)
	got, err := DecodeCard(r)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	xr, ok := got.(*XRowCard)
	if !ok {
		t.Fatalf("got %T, want *XRowCard", got)
	}
	if xr.Table != "peer_registry" || xr.PKHash != "abc123" || xr.MTime != 1711300000 {
		t.Fatalf("header mismatch: %+v", xr)
	}
	if !bytes.Equal(xr.Content, original.Content) {
		t.Fatalf("content mismatch: got %q", xr.Content)
	}
}
