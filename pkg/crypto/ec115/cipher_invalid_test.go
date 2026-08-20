package ec115

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"testing"
)

func TestDecryptRejectsShortCiphertextWithoutPanic(t *testing.T) {
	c := &EcdhCipher{key: make([]byte, aes.BlockSize), iv: make([]byte, aes.BlockSize)}
	if _, err := c.Decrypt([]byte{1, 2, 3}); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("expected invalid ciphertext error, got %v", err)
	}
}

func TestDecryptRejectsImpossibleCompressedLengthWithoutPanic(t *testing.T) {
	key := make([]byte, aes.BlockSize)
	iv := make([]byte, aes.BlockSize)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	plain := make([]byte, aes.BlockSize)
	plain[0] = 0xcf
	plain[1] = 0xcf // 0xcfcf = 53199, larger than this one-block frame.
	encrypted := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(encrypted, plain)

	c := &EcdhCipher{key: key, iv: iv}
	_, err = c.Decrypt(encrypted)
	if !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("expected invalid ciphertext error, got %v", err)
	}
}
