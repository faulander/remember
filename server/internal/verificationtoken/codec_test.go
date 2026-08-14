package verificationtoken

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
)

func TestCodecBindsCiphertextToUserAndRejectsTampering(t *testing.T) {
	t.Parallel()
	codec, err := NewCodec(bytes.Repeat([]byte{1}, KeySize))
	if err != nil {
		t.Fatal(err)
	}
	userID, otherUserID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	plaintext := bytes.Repeat([]byte{2}, 32)
	nonce, ciphertext, err := codec.Seal(bytes.NewReader(bytes.Repeat([]byte{3}, 32)), userID, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := codec.Open(userID, nonce, ciphertext)
	if err != nil || !bytes.Equal(opened, plaintext) {
		t.Fatalf("Open() matches=%v err=%v", bytes.Equal(opened, plaintext), err)
	}
	if _, err := codec.Open(otherUserID, nonce, ciphertext); err == nil {
		t.Fatal("Open() accepted ciphertext rebound to another user")
	}
	tampered := append([]byte(nil), ciphertext...)
	tampered[len(tampered)-1] ^= 1
	if _, err := codec.Open(userID, nonce, tampered); err == nil {
		t.Fatal("Open() accepted tampered ciphertext")
	}
}
