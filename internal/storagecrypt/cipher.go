// Package storagecrypt encrypts stored bot data with an externally managed key.
package storagecrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"os"

	"github.com/mmionya/nyande-bot/resources"
)

type Cipher struct{ aead cipher.AEAD }

func New(key []byte) (*Cipher, error) {
	if len(key) == 0 {
		return nil, nil
	}
	if len(key) != 32 {
		return nil, errors.New(resources.Get("storagecrypt.error.storage_encryption_requires_a_32_byte_key"))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

func ReadKeyFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != 32 || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New(resources.Get("storagecrypt.error.storage_key_must_be_a_regular_32_byte_file"))
	}
	key := make([]byte, 32)
	if _, err := file.ReadAt(key, 0); err != nil {
		return nil, err
	}
	return key, nil
}

// Seal prefixes a fresh random nonce; associatedData binds a ciphertext to its use.
func (c *Cipher) Seal(plaintext, associatedData []byte) []byte {
	return c.aead.Seal(nil, nil, plaintext, associatedData)
}

func (c *Cipher) Open(data, associatedData []byte) ([]byte, error) {
	plaintext, err := c.aead.Open(nil, nil, data, associatedData)
	if err != nil {
		return nil, errors.New(resources.Get("storagecrypt.error.stored_data_authentication_failed_wrong_key_or_damaged_data"))
	}
	return plaintext, nil
}
