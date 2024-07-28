package main

import (
	"flag"
	"fmt"
	"sand/aes"
	"slices"
)

func main() {

	text := flag.String("text", "Hello, world of secrets!", "Text that you want to encrypt/decrypt")
	passphrase := flag.String("passphrase", "7N#x*Ge8or!f#N", "Passphrase to use for encryption")
	action := flag.String("action", "both", "Whether to encrypt or decrpyt")

	flag.Parse()

	if *text == "" {
		fmt.Println("No plain text provided for encryption")
		return
	}
	if !slices.Contains([]string{"encrypt", "decrypt", "both"}, *action) {
		fmt.Printf("Action %s not recognized. Specify one of encrypt or decrypt\n", *action)
		return
	}

	data := []byte(*text)
	switch *action {
	case "encrypt":
		cipherBytes, err := aes.EncryptBytes(data, *passphrase)
		if err != nil {
			panic(err.Error())
		}
		fmt.Printf("encrypted : %x\n", cipherBytes)
	case "decrypt":
		plainBytes, err := aes.DecryptBytes(data, *passphrase)
		if err != nil {
			panic(err.Error())
		}
		fmt.Printf("decrypted : %s\n", plainBytes)
	default:
		cipherBytes, err := aes.EncryptBytes(data, *passphrase)
		if err != nil {
			panic(err.Error())
		}
		fmt.Printf("encrypted : %x\n", cipherBytes)

		plainBytes, err := aes.DecryptBytes(cipherBytes, *passphrase)
		if err != nil {
			panic(err.Error())
		}
		fmt.Printf("decrypted : %s\n", plainBytes)
	}
}
