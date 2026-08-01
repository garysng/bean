package secret

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	b, err := NewBox("master-key")
	if err != nil {
		t.Fatal(err)
	}
	plain := "registry-password-123"
	blob, err := b.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	// Ciphertext must not contain the plaintext.
	if bytes.Contains(blob, []byte(plain)) {
		t.Error("plaintext leaked into ciphertext")
	}
	got, err := b.Open(blob)
	if err != nil {
		t.Fatal(err)
	}
	if got != plain {
		t.Errorf("got %q, want %q", got, plain)
	}
}

func TestSealIsNonDeterministic(t *testing.T) {
	b, _ := NewBox("k")
	a, _ := b.Seal("same")
	c, _ := b.Seal("same")
	if bytes.Equal(a, c) {
		t.Error("identical plaintext produced identical ciphertext; nonce not random")
	}
}

func TestOpenWithWrongKeyFails(t *testing.T) {
	b1, _ := NewBox("key-one")
	b2, _ := NewBox("key-two")
	blob, _ := b1.Seal("secret")
	if _, err := b2.Open(blob); !errors.Is(err, ErrDecrypt) {
		t.Errorf("err = %v, want ErrDecrypt", err)
	}
}

func TestOpenRejectsTamperedCiphertext(t *testing.T) {
	b, _ := NewBox("k")
	blob, _ := b.Seal("secret")
	blob[len(blob)-1] ^= 0xff
	if _, err := b.Open(blob); !errors.Is(err, ErrDecrypt) {
		t.Errorf("err = %v, want ErrDecrypt for tampered data", err)
	}
}

func TestOpenRejectsShortInput(t *testing.T) {
	b, _ := NewBox("k")
	if _, err := b.Open([]byte{1, 2}); !errors.Is(err, ErrDecrypt) {
		t.Errorf("err = %v, want ErrDecrypt", err)
	}
}

func TestEmptyKeyDisablesEncryption(t *testing.T) {
	if _, err := NewBox(""); !errors.Is(err, ErrNoKey) {
		t.Errorf("err = %v, want ErrNoKey", err)
	}
	// A nil box must refuse rather than silently pass data through.
	var b *Box
	if _, err := b.Seal("x"); !errors.Is(err, ErrNoKey) {
		t.Errorf("nil Seal err = %v, want ErrNoKey", err)
	}
	if _, err := b.Open([]byte("x")); !errors.Is(err, ErrNoKey) {
		t.Errorf("nil Open err = %v, want ErrNoKey", err)
	}
}

func TestAnyKeyLengthWorks(t *testing.T) {
	for _, k := range []string{"a", strings.Repeat("x", 500), "🔑"} {
		b, err := NewBox(k)
		if err != nil {
			t.Fatalf("key %q: %v", k, err)
		}
		blob, err := b.Seal("v")
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := b.Open(blob); got != "v" {
			t.Errorf("key %q round-trip failed", k)
		}
	}
}

func TestGenerateKey(t *testing.T) {
	k1, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	k2, _ := GenerateKey()
	if k1 == k2 {
		t.Error("generated keys collide")
	}
	if _, err := NewBox(k1); err != nil {
		t.Errorf("generated key unusable: %v", err)
	}
}
