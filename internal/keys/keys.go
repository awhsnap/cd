// Package keys implements per-business envelope encryption.
//
// Each business gets its own random 256-bit data key. That key is stored in
// the database only in "wrapped" form — encrypted with the master key, which
// lives in an environment secret and never touches the database or backups.
// Sensitive fields (notes, message bodies) are encrypted with the business's
// data key. Deleting a business erases its wrapped key, which makes all of
// its encrypted data unreadable everywhere, including old backups.
package keys

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
)

var (
	ErrShredded = errors.New("keys: business data key has been destroyed")
	ErrDecrypt  = errors.New("keys: decryption failed")
)

type Keyring struct {
	master cipher.AEAD
}

func New(master []byte) (*Keyring, error) {
	aead, err := newAEAD(master)
	if err != nil {
		return nil, err
	}
	return &Keyring{master: aead}, nil
}

// NewBusinessKey creates a fresh data key and returns it wrapped by the master key.
func (k *Keyring) NewBusinessKey(businessID string) ([]byte, error) {
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return nil, err
	}
	return seal(k.master, dek, []byte("business-key:"+businessID))
}

// Business unwraps a business's data key for use.
func (k *Keyring) Business(businessID string, wrapped []byte) (*Business, error) {
	if len(wrapped) == 0 {
		return nil, ErrShredded
	}
	dek, err := open(k.master, wrapped, []byte("business-key:"+businessID))
	if err != nil {
		return nil, err
	}
	aead, err := newAEAD(dek)
	if err != nil {
		return nil, err
	}
	return &Business{aead: aead, id: businessID}, nil
}

// Business encrypts and decrypts one business's fields. The business ID is
// bound as associated data, so ciphertext copied into another business's rows
// will not decrypt.
type Business struct {
	aead cipher.AEAD
	id   string
}

func (b *Business) Seal(plain string) ([]byte, error) {
	if plain == "" {
		return nil, nil
	}
	return seal(b.aead, []byte(plain), []byte(b.id))
}

func (b *Business) Open(ciphertext []byte) (string, error) {
	if len(ciphertext) == 0 {
		return "", nil
	}
	p, err := open(b.aead, ciphertext, []byte(b.id))
	return string(p), err
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func seal(aead cipher.AEAD, plain, aad []byte) ([]byte, error) {
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plain, aad), nil
}

func open(aead cipher.AEAD, data, aad []byte) ([]byte, error) {
	ns := aead.NonceSize()
	if len(data) < ns {
		return nil, ErrDecrypt
	}
	p, err := aead.Open(nil, data[:ns], data[ns:], aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return p, nil
}
