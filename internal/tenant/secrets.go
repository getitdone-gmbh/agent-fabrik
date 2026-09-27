package tenant

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Box encrypts tenant secrets with AES-256-GCM. The tenant and secret name
// are bound as associated data, so ciphertexts cannot be moved between
// tenants or names.
type Box struct{ aead cipher.AEAD }

// NewBox parses a base64 (std or URL) 32-byte key. An empty key creates a
// random, process-local key (secrets are lost on restart).
func NewBox(key string) (*Box, bool, error) {
	var raw []byte
	ephemeral := false
	if key == "" {
		raw = make([]byte, 32)
		_, _ = rand.Read(raw)
		ephemeral = true
	} else {
		var err error
		k := strings.TrimSpace(key)
		raw, err = base64.StdEncoding.DecodeString(k)
		if err != nil {
			raw, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(k, "="))
		}
		if err != nil || len(raw) != 32 {
			return nil, false, errors.New("tenancy.secret_key must be 32 bytes, base64 encoded (openssl rand -base64 32)")
		}
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, false, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, false, err
	}
	return &Box{aead: aead}, ephemeral, nil
}

func aad(tenant, name string) []byte { return []byte(tenant + "\x00" + name) }

// Seal encrypts a secret value.
func (b *Box) Seal(tenant, name string, plaintext []byte) []byte {
	nonce := make([]byte, b.aead.NonceSize())
	_, _ = rand.Read(nonce)
	return b.aead.Seal(nonce, nonce, plaintext, aad(tenant, name))
}

// Open decrypts a secret value.
func (b *Box) Open(tenant, name string, ct []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(ct) < n {
		return nil, errors.New("ciphertext too short")
	}
	pt, err := b.aead.Open(nil, ct[:n], ct[n:], aad(tenant, name))
	if err != nil {
		return nil, fmt.Errorf("secret %q cannot be decrypted (wrong tenancy.secret_key?)", name)
	}
	return pt, nil
}
