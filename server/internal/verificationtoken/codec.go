// Package verificationtoken seals short-lived email verification secrets for the durable outbox.
package verificationtoken

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"io"

	"github.com/google/uuid"
)

const (
	KeySize              = 32
	associatedDataDomain = "remember:email-verification-outbox:v1\x00"
	associatedDataSize   = len(associatedDataDomain) + 16
)

type Codec struct {
	aead cipher.AEAD
}

func NewCodec(key []byte) (*Codec, error) {
	if len(key) != KeySize {
		return nil, errors.New("invalid email verification token key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("open email verification token cipher")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("open email verification token seal")
	}
	return &Codec{aead: aead}, nil
}

func (c *Codec) Seal(random io.Reader, userID uuid.UUID, plaintext []byte) (nonce, ciphertext []byte, err error) {
	if c == nil || random == nil || userID == uuid.Nil || len(plaintext) == 0 {
		return nil, nil, errors.New("invalid email verification token seal input")
	}
	nonce = make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(random, nonce); err != nil {
		return nil, nil, errors.New("generate email verification token nonce")
	}
	aad := associatedData(userID)
	return nonce, c.aead.Seal(nil, nonce, plaintext, aad[:]), nil
}

func (c *Codec) Open(userID uuid.UUID, nonce, ciphertext []byte) ([]byte, error) {
	if c == nil || userID == uuid.Nil || len(nonce) != c.aead.NonceSize() || len(ciphertext) <= c.aead.Overhead() {
		return nil, errors.New("invalid sealed email verification token")
	}
	aad := associatedData(userID)
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, aad[:])
	if err != nil {
		return nil, errors.New("open sealed email verification token")
	}
	return plaintext, nil
}

func associatedData(userID uuid.UUID) [associatedDataSize]byte {
	var result [associatedDataSize]byte
	copy(result[:], associatedDataDomain)
	copy(result[len(associatedDataDomain):], userID[:])
	return result
}
