package crypto

import (
	"bytes"
	"crypto/rand"
	"testing"
)

// ---------------------------------------------------------------------------
// Argon2id Key Derivation Tests
// ---------------------------------------------------------------------------

func TestDeriveKey_Deterministic(t *testing.T) {
	params := DefaultArgon2Params()
	salt := []byte("0123456789abcdef") // 16 bytes

	key1 := DeriveKey("password123", salt, params)
	key2 := DeriveKey("password123", salt, params)

	if !bytes.Equal(key1, key2) {
		t.Fatal("same password + salt should produce identical keys")
	}
	if len(key1) != 32 {
		t.Fatalf("expected 32-byte key, got %d", len(key1))
	}
}

func TestDeriveKey_DifferentPasswords(t *testing.T) {
	params := DefaultArgon2Params()
	salt := []byte("0123456789abcdef")

	key1 := DeriveKey("password1", salt, params)
	key2 := DeriveKey("password2", salt, params)

	if bytes.Equal(key1, key2) {
		t.Fatal("different passwords should produce different keys")
	}
}

func TestDeriveKey_DifferentSalts(t *testing.T) {
	params := DefaultArgon2Params()
	salt1 := []byte("0123456789abcdef")
	salt2 := []byte("fedcba9876543210")

	key1 := DeriveKey("password", salt1, params)
	key2 := DeriveKey("password", salt2, params)

	if bytes.Equal(key1, key2) {
		t.Fatal("different salts should produce different keys")
	}
}

func TestDeriveKey_EmptyPassword(t *testing.T) {
	params := DefaultArgon2Params()
	salt := []byte("0123456789abcdef")

	key := DeriveKey("", salt, params)
	if len(key) != 32 {
		t.Fatalf("expected 32-byte key even for empty password, got %d", len(key))
	}
}

func TestDeriveKey_UnicodePassword(t *testing.T) {
	params := DefaultArgon2Params()
	salt := []byte("0123456789abcdef")

	key1 := DeriveKey("пароль🔑密码", salt, params)
	key2 := DeriveKey("пароль🔑密码", salt, params)

	if !bytes.Equal(key1, key2) {
		t.Fatal("unicode password should be deterministic")
	}
	if len(key1) != 32 {
		t.Fatalf("expected 32-byte key, got %d", len(key1))
	}
}

// ---------------------------------------------------------------------------
// Salt Generation Tests
// ---------------------------------------------------------------------------

func TestGenerateSalt_Length(t *testing.T) {
	salt, err := GenerateSalt(16)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(salt) != 16 {
		t.Fatalf("expected 16-byte salt, got %d", len(salt))
	}
}

func TestGenerateSalt_Uniqueness(t *testing.T) {
	salt1, _ := GenerateSalt(16)
	salt2, _ := GenerateSalt(16)

	if bytes.Equal(salt1, salt2) {
		t.Fatal("two random salts should not be equal")
	}
}

// ---------------------------------------------------------------------------
// Nonce Generation Tests
// ---------------------------------------------------------------------------

func TestGenerateNonce_Length(t *testing.T) {
	nonce, err := GenerateNonce()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nonce) != NonceSize {
		t.Fatalf("expected %d-byte nonce, got %d", NonceSize, len(nonce))
	}
}

func TestGenerateNonce_Uniqueness(t *testing.T) {
	nonce1, _ := GenerateNonce()
	nonce2, _ := GenerateNonce()

	if bytes.Equal(nonce1, nonce2) {
		t.Fatal("two random nonces should not be equal")
	}
}

// ---------------------------------------------------------------------------
// AES-256-GCM Encryption/Decryption Tests
// ---------------------------------------------------------------------------

func makeTestKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func makeTestNonce(t *testing.T) []byte {
	t.Helper()
	nonce, err := GenerateNonce()
	if err != nil {
		t.Fatal(err)
	}
	return nonce
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	key := makeTestKey(t)
	nonce := makeTestNonce(t)
	plaintext := []byte("Hello, SAND project!")

	ciphertext, err := Encrypt(key, nonce, plaintext, nil)
	if err != nil {
		t.Fatalf("encryption failed: %v", err)
	}

	decrypted, err := Decrypt(key, nonce, ciphertext, nil)
	if err != nil {
		t.Fatalf("decryption failed: %v", err)
	}

	if !bytes.Equal(plaintext, decrypted) {
		t.Fatal("decrypted text does not match original")
	}
}

func TestEncryptDecrypt_WithAssociatedData(t *testing.T) {
	key := makeTestKey(t)
	nonce := makeTestNonce(t)
	plaintext := []byte("secret data")
	ad := []byte("header metadata binding")

	ciphertext, err := Encrypt(key, nonce, plaintext, ad)
	if err != nil {
		t.Fatalf("encryption failed: %v", err)
	}

	// Decrypt with correct AD
	decrypted, err := Decrypt(key, nonce, ciphertext, ad)
	if err != nil {
		t.Fatalf("decryption with correct AD failed: %v", err)
	}
	if !bytes.Equal(plaintext, decrypted) {
		t.Fatal("decrypted text does not match original")
	}

	// Decrypt with wrong AD should fail
	_, err = Decrypt(key, nonce, ciphertext, []byte("wrong AD"))
	if err == nil {
		t.Fatal("decryption with wrong AD should fail")
	}

	// Decrypt with no AD should fail
	_, err = Decrypt(key, nonce, ciphertext, nil)
	if err == nil {
		t.Fatal("decryption with missing AD should fail")
	}
}

func TestEncrypt_CiphertextDiffersFromPlaintext(t *testing.T) {
	key := makeTestKey(t)
	nonce := makeTestNonce(t)
	plaintext := []byte("this is plaintext that must not appear in ciphertext")

	ciphertext, err := Encrypt(key, nonce, plaintext, nil)
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(plaintext, ciphertext) {
		t.Fatal("ciphertext should differ from plaintext")
	}

	// Ciphertext should be larger (GCM tag is 16 bytes)
	if len(ciphertext) != len(plaintext)+16 {
		t.Fatalf("expected ciphertext length %d, got %d", len(plaintext)+16, len(ciphertext))
	}
}

func TestDecrypt_WrongKey(t *testing.T) {
	key1 := makeTestKey(t)
	key2 := makeTestKey(t)
	nonce := makeTestNonce(t)
	plaintext := []byte("secret")

	ciphertext, _ := Encrypt(key1, nonce, plaintext, nil)

	_, err := Decrypt(key2, nonce, ciphertext, nil)
	if err == nil {
		t.Fatal("decryption with wrong key should fail")
	}
}

func TestDecrypt_WrongNonce(t *testing.T) {
	key := makeTestKey(t)
	nonce1 := makeTestNonce(t)
	nonce2 := makeTestNonce(t)
	plaintext := []byte("secret")

	ciphertext, _ := Encrypt(key, nonce1, plaintext, nil)

	_, err := Decrypt(key, nonce2, ciphertext, nil)
	if err == nil {
		t.Fatal("decryption with wrong nonce should fail")
	}
}

func TestDecrypt_TamperedCiphertext(t *testing.T) {
	key := makeTestKey(t)
	nonce := makeTestNonce(t)
	plaintext := []byte("important data")

	ciphertext, _ := Encrypt(key, nonce, plaintext, nil)

	// Flip a bit in the ciphertext
	tampered := make([]byte, len(ciphertext))
	copy(tampered, ciphertext)
	tampered[0] ^= 0x01

	_, err := Decrypt(key, nonce, tampered, nil)
	if err == nil {
		t.Fatal("decryption of tampered ciphertext should fail")
	}
}

func TestDecrypt_TruncatedCiphertext(t *testing.T) {
	key := makeTestKey(t)
	nonce := makeTestNonce(t)
	plaintext := []byte("some data here")

	ciphertext, _ := Encrypt(key, nonce, plaintext, nil)

	// Truncate — remove last byte (corrupts GCM tag)
	truncated := ciphertext[:len(ciphertext)-1]

	_, err := Decrypt(key, nonce, truncated, nil)
	if err == nil {
		t.Fatal("decryption of truncated ciphertext should fail")
	}
}

func TestEncrypt_EmptyPlaintext(t *testing.T) {
	key := makeTestKey(t)
	nonce := makeTestNonce(t)

	ciphertext, err := Encrypt(key, nonce, []byte{}, nil)
	if err != nil {
		t.Fatalf("encrypting empty plaintext should work: %v", err)
	}

	decrypted, err := Decrypt(key, nonce, ciphertext, nil)
	if err != nil {
		t.Fatalf("decrypting empty ciphertext should work: %v", err)
	}
	if len(decrypted) != 0 {
		t.Fatal("decrypted empty plaintext should be empty")
	}
}

func TestEncrypt_LargePayload(t *testing.T) {
	key := makeTestKey(t)
	nonce := makeTestNonce(t)

	// 1MB payload
	plaintext := make([]byte, 1<<20)
	if _, err := rand.Read(plaintext); err != nil {
		t.Fatal(err)
	}

	ciphertext, err := Encrypt(key, nonce, plaintext, nil)
	if err != nil {
		t.Fatal(err)
	}

	decrypted, err := Decrypt(key, nonce, ciphertext, nil)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(plaintext, decrypted) {
		t.Fatal("large payload round-trip failed")
	}
}

func TestEncrypt_InvalidKeySize(t *testing.T) {
	nonce := makeTestNonce(t)

	for _, size := range []int{0, 1, 15, 16, 24, 31, 33, 64} {
		key := make([]byte, size)
		_, err := Encrypt(key, nonce, []byte("test"), nil)
		if err == nil {
			t.Fatalf("should reject key of size %d", size)
		}
	}
}

func TestEncrypt_InvalidNonceSize(t *testing.T) {
	key := makeTestKey(t)

	for _, size := range []int{0, 1, 8, 11, 13, 16} {
		nonce := make([]byte, size)
		_, err := Encrypt(key, nonce, []byte("test"), nil)
		if err == nil {
			t.Fatalf("should reject nonce of size %d", size)
		}
	}
}

func TestEncrypt_UniqueNoncesProduceDifferentCiphertext(t *testing.T) {
	key := makeTestKey(t)
	plaintext := []byte("same plaintext both times")

	nonce1 := makeTestNonce(t)
	nonce2 := makeTestNonce(t)

	ct1, _ := Encrypt(key, nonce1, plaintext, nil)
	ct2, _ := Encrypt(key, nonce2, plaintext, nil)

	if bytes.Equal(ct1, ct2) {
		t.Fatal("different nonces should produce different ciphertexts")
	}
}

// ---------------------------------------------------------------------------
// ZeroBytes Tests
// ---------------------------------------------------------------------------

func TestZeroBytes(t *testing.T) {
	data := []byte{0xFF, 0xAB, 0xCD, 0xEF}
	ZeroBytes(data)

	for i, b := range data {
		if b != 0 {
			t.Fatalf("byte %d not zeroed: got %x", i, b)
		}
	}
}

func TestZeroBytes_Empty(t *testing.T) {
	// Should not panic
	ZeroBytes([]byte{})
	ZeroBytes(nil)
}

// ---------------------------------------------------------------------------
// Integration: Full Key Derivation + Encrypt/Decrypt
// ---------------------------------------------------------------------------

func TestFullFlow_DeriveAndEncryptDecrypt(t *testing.T) {
	params := DefaultArgon2Params()
	salt, _ := GenerateSalt(params.SaltLen)
	password := "my-secure-password"

	key := DeriveKey(password, salt, params)
	defer ZeroBytes(key)

	nonce := makeTestNonce(t)
	plaintext := []byte("classified document contents")
	ad := []byte("part1-header")

	ciphertext, err := Encrypt(key, nonce, plaintext, ad)
	if err != nil {
		t.Fatal(err)
	}

	// Re-derive key from same password + salt
	key2 := DeriveKey(password, salt, params)
	defer ZeroBytes(key2)

	decrypted, err := Decrypt(key2, nonce, ciphertext, ad)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(plaintext, decrypted) {
		t.Fatal("full flow round-trip failed")
	}
}

func TestFullFlow_WrongPassword(t *testing.T) {
	params := DefaultArgon2Params()
	salt, _ := GenerateSalt(params.SaltLen)

	key := DeriveKey("correct-password", salt, params)
	nonce := makeTestNonce(t)
	plaintext := []byte("secret")

	ciphertext, _ := Encrypt(key, nonce, plaintext, nil)

	wrongKey := DeriveKey("wrong-password", salt, params)
	_, err := Decrypt(wrongKey, nonce, ciphertext, nil)
	if err == nil {
		t.Fatal("wrong password should fail decryption")
	}
}
