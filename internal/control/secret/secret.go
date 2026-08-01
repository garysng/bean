// Package secret encrypts values the control plane must persist but should
// never expose: registry passwords today, and any future credential.
//
// The threat this addresses is a database dump: SQLite files get copied
// into backups, dev machines and support tickets. Encrypting at rest means
// that copy alone cannot pull a customer's private images. It does not
// defend against an attacker who already has the process memory or the
// master key — that is what the key's own storage is for.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// ErrNoKey reports that encryption was requested without a master key.
var ErrNoKey = errors.New("no master key configured")

// ErrDecrypt reports ciphertext that does not decrypt with this key, which
// usually means the master key changed.
var ErrDecrypt = errors.New("decrypt failed (wrong key or corrupt data)")

// Box seals and opens secrets with AES-256-GCM.
type Box struct {
	aead cipher.AEAD
}

// NewBox derives a key from the supplied material. Any length is accepted
// and hashed to 32 bytes, so operators can use a passphrase or raw bytes;
// an empty key disables encryption and every call returns ErrNoKey.
func NewBox(keyMaterial string) (*Box, error) {
	if keyMaterial == "" {
		return nil, ErrNoKey
	}
	sum := sha256.Sum256([]byte(keyMaterial))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext. The nonce is prepended to the ciphertext so a
// caller only has to store one blob.
func (b *Box) Seal(plaintext string) ([]byte, error) {
	if b == nil || b.aead == nil {
		return nil, ErrNoKey
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	return b.aead.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// Open decrypts a blob produced by Seal.
func (b *Box) Open(blob []byte) (string, error) {
	if b == nil || b.aead == nil {
		return "", ErrNoKey
	}
	ns := b.aead.NonceSize()
	if len(blob) < ns {
		return "", ErrDecrypt
	}
	plaintext, err := b.aead.Open(nil, blob[:ns], blob[ns:], nil)
	if err != nil {
		return "", ErrDecrypt
	}
	return string(plaintext), nil
}

// GenerateKey returns a base64 master key, for operators bootstrapping a
// deployment that has no key yet.
func GenerateKey() (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}
