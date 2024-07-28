package aes

import "fmt"

const (
	mahavakyas   = "Prajnanam Brahma, Ayam Atma Brahma, Tat Tvam Asi, Aham Brahmasmi"
	batmanBegins = "Training is nothing! Will is everything! The will to act!"
)

func getAdditionalData() []byte {
	return []byte(mahavakyas)
}

func getKeyBuffer(passphrase string) []byte {
	keyBuffer := make([]byte, 32)
	passphraseBuffer := []byte(passphrase)
	copy(keyBuffer, passphraseBuffer)
	fmt.Printf("initial key buffer: %x\n", keyBuffer)
	copy(keyBuffer[len(passphraseBuffer):], []byte(batmanBegins))
	fmt.Printf("final key buffer: %x\n", keyBuffer)
	return keyBuffer
}
