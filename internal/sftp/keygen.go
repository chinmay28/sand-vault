package sftp

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Making the key here rather than asking for one.
//
// The connect form used to have exactly one way in: run ssh-keygen somewhere
// else, paste the private half into a textarea, install the public half on the
// server. Every step of that is a step where the wrong half gets pasted, and
// the wrong half is the interesting one to get wrong — a private key pasted
// into a box labelled "public key" on somebody else's site is a bad afternoon.
//
// So SAND offers to make the pair itself. The private half never leaves this
// process: it is generated here, held for as long as it takes to fill in the
// rest of the form, and goes into the encrypted vault when the connection is
// stored. What the browser is shown is the public half, which is a line to
// paste into authorized_keys and is not a secret at all. The direction of the
// paste is reversed, and the half that travels is the one it does not matter
// about.
//
// Pasting your own key still works and is unchanged, because plenty of people
// already have a key for this machine, and a key held in an agent or issued by
// a CA is not something SAND can invent a replacement for.

// KeyComment is what a generated key is tagged with, and is what shows up in
// the authorized_keys line on the server. It says where the key came from to
// somebody reading that file a year later, which is the only job a key comment
// has ever had.
const KeyComment = "sand-vault"

// KeyPair is a freshly made SSH key, both halves.
type KeyPair struct {
	// PrivateKey is an unencrypted OpenSSH private key.
	//
	// Unencrypted deliberately. A passphrase protects a key that sits on a
	// disk somebody else might read; this one goes straight into the vault,
	// and the passphrase would have to be stored beside it in that same vault
	// to be usable by a daemon nobody is sitting in front of. That is a lock
	// with its key taped to it — it would look like protection without being
	// any. The vault's encryption is the protection here, and it is the same
	// protection every other credential in SAND gets.
	PrivateKey string

	// PublicKey is the authorized_keys line, exactly as it should land on the
	// server: type, base64, comment, one line, no trailing newline.
	PublicKey string

	// Fingerprint is the SHA256:… form of the public half, so the key SAND
	// holds can be compared against what `ssh-keygen -lf` prints on the server
	// once it has been installed.
	Fingerprint string
}

// GenerateKeyPair makes an Ed25519 key pair.
//
// Ed25519 and nothing else, with no algorithm picker. Every sshd since OpenSSH
// 6.5 (2014) accepts it, the keys are short enough to read and to paste as one
// line, and there is no key size to get wrong. An RSA option would only ever be
// chosen by somebody who did not need it, and the machine old enough to require
// one is a machine to paste a key into rather than a reason for a dropdown.
func GenerateKeyPair(comment string) (KeyPair, error) {
	comment = strings.TrimSpace(comment)
	if comment == "" {
		comment = KeyComment
	}
	// A comment is appended to a line in a file that is parsed by whitespace,
	// so a newline in one would not be a cosmetic problem: it would be a second
	// entry in authorized_keys. Nothing legitimate needs one.
	if strings.ContainsAny(comment, "\r\n") {
		return KeyPair{}, errors.New("a key comment cannot contain a line break")
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return KeyPair{}, fmt.Errorf("generating a key: %w", err)
	}

	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return KeyPair{}, fmt.Errorf("writing the private key: %w", err)
	}

	signer, err := ssh.NewPublicKey(pub)
	if err != nil {
		return KeyPair{}, fmt.Errorf("writing the public key: %w", err)
	}

	// MarshalAuthorizedKey ends its line with a newline and carries no comment.
	// Both are fixed here rather than left to the caller: the line is going to
	// be shown in a box and copied out of it, and a trailing newline copied
	// into a terminal runs whatever it lands in front of.
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer))) + " " + comment

	return KeyPair{
		PrivateKey:  string(pem.EncodeToMemory(block)),
		PublicKey:   line,
		Fingerprint: Fingerprint(signer),
	}, nil
}
