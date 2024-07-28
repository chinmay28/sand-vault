package aes

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"io"
)

func getAESGCM(
	passphrase string,
) (cipher.AEAD, error) {
	cipherBlock, err := aes.NewCipher(getKeyBuffer(passphrase))
	if err != nil {
		return nil, fmt.Errorf("could not create cipher block: %w", err)
	}
	// https://en.wikipedia.org/wiki/Galois/Counter_Mode
	// https://golang.org/pkg/crypto/cipher/#NewGCM
	aesGCM, err := cipher.NewGCM(cipherBlock)
	if err != nil {
		return nil, fmt.Errorf("could not create new GCM: %w", err)
	}
	return aesGCM, nil
}

// EncryptBytes encrypts a given slice of bytes using AES
// Note: Saves nonce as a prefix in encrypted data
func EncryptBytes(data []byte, passphrase string) ([]byte, error) {
	aesGCM, err := getAESGCM(passphrase)
	if err != nil {
		return nil, fmt.Errorf("could not get GCM for passphrase %s: %w", passphrase, err)
	}
	nonceBuffer := make([]byte, aesGCM.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonceBuffer); err != nil {
		return nil, fmt.Errorf("could not create nounce from GCM: %w", err)
	}
	cipherData := aesGCM.Seal(nonceBuffer, nonceBuffer, data, getAdditionalData())
	return cipherData, nil
}

// Decrypt decrypts the given slice of bytes containing the AES ciphertext
func DecryptBytes(data []byte, passphrase string) ([]byte, error) {
	aesGCM, err := getAESGCM(passphrase)
	if err != nil {
		return nil, fmt.Errorf("could not get GCM for passphrase %s: %w", passphrase, err)
	}
	nonceSize := aesGCM.NonceSize()
	nonceBuffer, cipherBuffer := data[:nonceSize], data[nonceSize:]
	decryptedData, err := aesGCM.Open(nil, nonceBuffer, cipherBuffer, getAdditionalData())
	if err != nil {
		return nil, fmt.Errorf("could not decrypt data with %s: %w", passphrase, err)
	}
	return decryptedData, nil
}
