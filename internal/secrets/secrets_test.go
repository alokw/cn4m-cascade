package secrets

import (
	"errors"
	"strings"
	"testing"
)

func TestBoxRoundTrip(t *testing.T) {
	box, err := NewBox("a-sufficiently-long-test-key")
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}

	tests := []struct {
		name      string
		plaintext string
	}{
		{"empty", ""},
		{"ascii", "hunter2"},
		{"unicode", "pässwörd-🔐"},
		{"long", strings.Repeat("x", 4096)},
		{"whitespace", "  spaces  and\ttabs\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ct, err := box.Encrypt(tt.plaintext)
			if err != nil {
				t.Fatalf("Encrypt: %v", err)
			}
			if tt.plaintext != "" && strings.Contains(ct, tt.plaintext) {
				t.Fatalf("ciphertext contains the plaintext")
			}
			got, err := box.Decrypt(ct)
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if got != tt.plaintext {
				t.Fatalf("round trip = %q, want %q", got, tt.plaintext)
			}
		})
	}
}

func TestEncryptIsNondeterministic(t *testing.T) {
	box, err := NewBox("a-sufficiently-long-test-key")
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	first, err := box.Encrypt("same-input")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	second, err := box.Encrypt("same-input")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if first == second {
		t.Fatal("two encryptions of the same plaintext are identical: nonce is not random")
	}
}

func TestDecryptWithWrongKey(t *testing.T) {
	original, err := NewBox("the-original-encryption-key")
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	rotated, err := NewBox("a-different-encryption-key!")
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	ct, err := original.Encrypt("hunter2")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := rotated.Decrypt(ct); !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("Decrypt with rotated key = %v, want ErrUndecryptable", err)
	}
}

func TestDecryptGarbage(t *testing.T) {
	box, err := NewBox("a-sufficiently-long-test-key")
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	for _, bad := range []string{"not base64 at all!!", "c2hvcnQ=", "AAAA"} {
		if _, err := box.Decrypt(bad); !errors.Is(err, ErrUndecryptable) {
			t.Fatalf("Decrypt(%q) = %v, want ErrUndecryptable", bad, err)
		}
	}
}
