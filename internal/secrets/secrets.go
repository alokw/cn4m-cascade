// Package secrets encrypts target credentials at rest.
//
// SPEC.md §5 requires credentials to be "encrypted at rest with a key derived
// from an ENCRYPTION_KEY env var". The spec does not pin the construction, so:
// AES-256-GCM with the key derived via HKDF-SHA256, and a fresh random nonce
// per encryption stored alongside the ciphertext.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// hkdfInfo is versioned so a future scheme change can be distinguished.
// hkdfInfo is the HKDF info string the credential key is derived from.
//
// **Deliberately not renamed with the rest of the project (2026-09-06).** It is
// never displayed anywhere, so a new name buys nothing — and changing it
// changes the derived key, which makes every stored target password
// undecryptable. Nothing would fail at build, lint or test time; targets would
// simply start failing to mount with a decryption error, at which point the
// plaintext is gone for good.
//
// Leaving it alone is also what lets an existing database survive being
// renamed on disk. If it ever must change, it needs a "-v2" suffix and a
// re-encryption migration, which is what the "-v1" was always for.
const hkdfInfo = "smbsync-target-creds-v1"

// ErrUndecryptable means the stored ciphertext could not be opened with the
// current key. Overwhelmingly this means ENCRYPTION_KEY changed. Callers
// surface it as a per-target problem rather than failing the whole process,
// so one unreadable credential cannot take the server down.
var ErrUndecryptable = errors.New("credentials cannot be decrypted — ENCRYPTION_KEY may have changed since they were saved")

// Box encrypts and decrypts short secrets.
type Box struct {
	aead cipher.AEAD
}

// NewBox derives an AES-256 key from the raw ENCRYPTION_KEY value.
func NewBox(encryptionKey string) (*Box, error) {
	if encryptionKey == "" {
		return nil, errors.New("encryption key is empty")
	}
	// No salt: the derived key must be reproducible across restarts from the
	// env var alone, and there is nowhere to persist a salt before the
	// database itself is readable.
	key, err := hkdf.Key(sha256.New, []byte(encryptionKey), nil, hkdfInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("deriving encryption key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("initialising cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialising GCM: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Encrypt returns base64(nonce || ciphertext). The empty string encrypts to
// the empty string so that credential-less (guest) targets stay legible in
// the database.
func (b *Box) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generating nonce: %w", err)
	}
	sealed := b.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt. It returns ErrUndecryptable for anything that
// does not open cleanly; the error deliberately carries no detail about the
// ciphertext.
func (b *Box) Decrypt(encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", ErrUndecryptable
	}
	if len(raw) < b.aead.NonceSize() {
		return "", ErrUndecryptable
	}
	nonce, ct := raw[:b.aead.NonceSize()], raw[b.aead.NonceSize():]
	plain, err := b.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", ErrUndecryptable
	}
	return string(plain), nil
}
