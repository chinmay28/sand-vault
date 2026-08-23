package sftp

import (
	"context"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/sftp/sftptest"
	"golang.org/x/crypto/ssh"
)

// The whole point of generating the pair here is that the two halves are of
// each other, so the test that matters is the one where the public half is
// installed on a server and the private half signs in with it. Anything less
// tests that a key parses, which is not the claim being made.
func TestGeneratedKeySignsInWithItsOwnPublicHalf(t *testing.T) {
	pair, err := GenerateKeyPair("")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	installed, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(pair.PublicKey))
	if err != nil {
		t.Fatalf("the public half does not parse as an authorized_keys line: %v", err)
	}
	if comment != KeyComment {
		t.Fatalf("comment %q, want %q", comment, KeyComment)
	}
	if got := Fingerprint(installed); got != pair.Fingerprint {
		t.Fatalf("fingerprint %q does not match the public half %q", pair.Fingerprint, got)
	}

	server := sftptest.NewServer(t, t.TempDir())
	server.Authorized = installed

	client, err := Dial(context.Background(), dialTest(t, server, pair.PrivateKey, ""))
	if err != nil {
		t.Fatalf("a generated key would not sign in with its own public half: %v", err)
	}
	client.Close()
}

// The private half is written in the format everything else here reads, and
// with no passphrase — see the comment on KeyPair for why not.
func TestGeneratedPrivateKeyIsAnUnencryptedOpenSSHKey(t *testing.T) {
	pair, err := GenerateKeyPair("")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if !strings.HasPrefix(pair.PrivateKey, "-----BEGIN OPENSSH PRIVATE KEY-----") {
		t.Fatalf("private key does not start with the OpenSSH header:\n%s", pair.PrivateKey)
	}
	if _, err := parseKey(pair.PrivateKey, ""); err != nil {
		t.Fatalf("parsing a generated key with no passphrase: %v", err)
	}
	if !strings.HasPrefix(pair.PublicKey, "ssh-ed25519 ") {
		t.Fatalf("public key %q is not ed25519", pair.PublicKey)
	}
}

// The public half is copied out of a box and into a shell, so it is one line
// with nothing on either end of it.
func TestGeneratedPublicKeyIsOneCleanLine(t *testing.T) {
	pair, err := GenerateKeyPair("")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if strings.ContainsAny(pair.PublicKey, "\r\n") {
		t.Fatalf("public key has a line break in it: %q", pair.PublicKey)
	}
	if pair.PublicKey != strings.TrimSpace(pair.PublicKey) {
		t.Fatalf("public key has whitespace on an end: %q", pair.PublicKey)
	}
}

// Two calls must not agree, which is the one failure mode of a key generator
// that a round-trip test would not notice.
func TestGenerateKeyPairIsNotDeterministic(t *testing.T) {
	first, err := GenerateKeyPair("")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	second, err := GenerateKeyPair("")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if first.Fingerprint == second.Fingerprint {
		t.Fatal("two generated keys have the same fingerprint")
	}
}

func TestGenerateKeyPairTakesAComment(t *testing.T) {
	pair, err := GenerateKeyPair("sand@the-nas")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.HasSuffix(pair.PublicKey, " sand@the-nas") {
		t.Fatalf("comment did not reach the authorized_keys line: %q", pair.PublicKey)
	}
}

// A comment lands in a file parsed by whitespace, so a line break in one would
// write a second entry rather than a funny-looking first one.
func TestGenerateKeyPairRefusesACommentWithALineBreak(t *testing.T) {
	if _, err := GenerateKeyPair("sand\nssh-ed25519 AAAA…"); err == nil {
		t.Fatal("a comment with a newline in it was accepted")
	}
}
