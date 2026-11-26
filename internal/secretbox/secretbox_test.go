package secretbox

import (
	"errors"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	b, err := New([]byte("0123456789abcdef-key-material\n"))
	if err != nil {
		t.Fatal(err)
	}
	ct := b.Seal("hunter2", "repo:1:TOKEN")
	if strings.Contains(ct, "hunter2") || !strings.HasPrefix(ct, "v1:"+b.ID()+":") {
		t.Fatalf("bad ciphertext %q", ct)
	}
	if ct == b.Seal("hunter2", "repo:1:TOKEN") {
		t.Fatal("nonce reuse: identical ciphertexts")
	}
	pt, err := b.Open(ct, "repo:1:TOKEN")
	if err != nil || pt != "hunter2" {
		t.Fatalf("open = %q, %v", pt, err)
	}
	if _, err := b.Open(ct, "repo:2:TOKEN"); err == nil {
		t.Fatal("ciphertext opened under different additional data")
	}
	// Trailing whitespace in key files is ignored.
	b2, _ := New([]byte("0123456789abcdef-key-material"))
	if b2.ID() != b.ID() {
		t.Fatal("whitespace changed the key")
	}
	other, _ := New([]byte("a-completely-different-key"))
	if _, err := other.Open(ct, "repo:1:TOKEN"); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("want ErrWrongKey, got %v", err)
	}
	if _, err := New([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
}
