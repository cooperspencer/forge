// Package secretbox encrypts CI secret values at rest with AES-256-GCM.
//
// Ciphertexts look like "v1:<key id>:<base64(nonce|sealed)>". The key id is a
// short fingerprint of the key, so the server can tell which values were
// written with a different key (e.g. after the key file was changed) instead
// of failing with an opaque authentication error. Callers pass "additional
// data" that binds a ciphertext to its row (scope + name), so a value copied
// into another repository's row in the database will not decrypt.
package secretbox

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ErrWrongKey means the value was encrypted with a different key.
var ErrWrongKey = errors.New("secret was encrypted with a different key")

type Box struct {
	aead cipher.AEAD
	id   string
}

// New derives an AES-256 key from arbitrary key material (≥16 bytes after
// trimming surrounding whitespace, so `openssl rand -base64 32 > key` works).
func New(material []byte) (*Box, error) {
	material = bytes.TrimSpace(material)
	if len(material) < 16 {
		return nil, fmt.Errorf("secret key too short: need at least 16 bytes, got %d", len(material))
	}
	m := hmac.New(sha256.New, material)
	m.Write([]byte("forge ci secrets v1"))
	key := m.Sum(nil)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(key)
	return &Box{aead: aead, id: hex.EncodeToString(sum[:4])}, nil
}

// ID is the key fingerprint embedded in ciphertexts.
func (b *Box) ID() string { return b.id }

func (b *Box) Seal(plaintext, ad string) string {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	out := b.aead.Seal(nonce, nonce, []byte(plaintext), []byte(ad))
	return "v1:" + b.id + ":" + base64.RawStdEncoding.EncodeToString(out)
}

func (b *Box) Open(ciphertext, ad string) (string, error) {
	kid, err := KeyID(ciphertext)
	if err != nil {
		return "", err
	}
	if kid != b.id {
		return "", fmt.Errorf("%w (key %s, configured key is %s)", ErrWrongKey, kid, b.id)
	}
	raw, err := base64.RawStdEncoding.DecodeString(ciphertext[len("v1:")+len(kid)+1:])
	ns := b.aead.NonceSize()
	if err != nil || len(raw) < ns {
		return "", fmt.Errorf("malformed secret ciphertext")
	}
	pt, err := b.aead.Open(nil, raw[:ns], raw[ns:], []byte(ad))
	if err != nil {
		return "", fmt.Errorf("secret failed to decrypt (corrupted, or moved to another row)")
	}
	return string(pt), nil
}

// KeyID returns the key fingerprint of a ciphertext.
func KeyID(ciphertext string) (string, error) {
	parts := strings.SplitN(ciphertext, ":", 3)
	if len(parts) != 3 || parts[0] != "v1" {
		return "", fmt.Errorf("malformed secret ciphertext")
	}
	return parts[1], nil
}
