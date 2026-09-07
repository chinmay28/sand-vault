package provider

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/md5"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"
)

func init() {
	Register(Spec{
		Kind:  KindFilen,
		Label: "Filen",
		Description: "A Filen account, reached through Filen's own API. Filen encrypts everything " +
			"end to end, so SAND signs in with your email and password, derives the account's " +
			"keys on this machine, and never sends the password itself. Parts go in one folder.",
		DocsURL: "https://filen.io/",
		Order:   23,
		Fields: []FieldSpec{
			{Key: "email", Label: "Email", Required: true},
			{
				Key:      "password",
				Label:    "Password",
				Secret:   true,
				Required: true,
				Help: "Used on this machine to unlock the account's keys; Filen only ever sees a " +
					"hash derived from it.",
			},
			{
				Key:      "two_factor_code",
				Label:    "Two-factor code",
				Advanced: true,
				Help:     "Only for the first sign-in, if the account has two-factor turned on. Used once and cleared.",
			},
			{
				Key:     "folder",
				Label:   "Folder",
				Default: "sand",
				Help:    "A folder at the top of the account. SAND creates it if it is missing; blank means the root.",
			},
			{
				Key:      "session",
				Label:    "Session",
				Secret:   true,
				Advanced: true,
				Help:     "Filled in by SAND after the first sign-in. Clear it to sign in again.",
			},
		},
	}, newFilenProvider)
}

// Filen's three hosts: the API, the one chunks go up to, and the one they
// come back down from. Variables so tests can point them at a stub.
var (
	filenGateway = "https://gateway.filen.io"
	filenIngest  = "https://ingest.filen.io"
	filenEgest   = "https://egest.filen.io"
)

// filenChunkSize is the plaintext length of every chunk but a file's last.
// Fixed by Filen: every client cuts files the same way, and the download
// address of a chunk is its index.
const filenChunkSize = 1 << 20

// filenProvider stores each shard as a file in a single Filen folder.
//
// Filen is end-to-end encrypted, which makes this backend unlike the others:
// the server never sees a file name, a size or a byte of content in the
// clear. Every name is encrypted under the account's key before it is sent,
// every file gets a key of its own that lives inside its encrypted metadata,
// and a listing is a pile of ciphertext that only the account's keys can turn
// back into names. None of that is for SAND's benefit — the parts are already
// encrypted — but it is the only way to talk to Filen at all.
type filenProvider struct {
	base
	email      string
	password   string
	twoFactor  string
	folderName string

	mu      sync.Mutex
	sess    *filenSession
	folder  string // UUID of the folder shards live in, once resolved
	entries map[string]filenFile
	rotate  func(map[string]string)
}

// filenFile is what SAND keeps about one stored file between listings.
type filenFile struct {
	uuid    string
	size    int64
	chunks  int64
	bucket  string
	region  string
	version int
	key     filenKey
}

func newFilenProvider(cfg Config) (Provider, error) {
	p := &filenProvider{
		base:       base{cfg: cfg},
		email:      strings.TrimSpace(cfg.Option("email")),
		password:   cfg.Option("password"),
		twoFactor:  strings.TrimSpace(cfg.Option("two_factor_code")),
		folderName: strings.Trim(strings.TrimSpace(cfg.Option("folder")), "/"),
		entries:    map[string]filenFile{},
	}
	if raw := strings.TrimSpace(cfg.Option("session")); raw != "" {
		sess, err := parseFilenSession(raw)
		if err != nil {
			return nil, fmt.Errorf("filen: the stored session is unreadable, clear it to sign in again: %w", err)
		}
		p.sess = sess
	}
	return p, nil
}

// OnCredentialChange registers where a completed sign-in is written, so the
// keys derived from the password are kept rather than derived again — a
// derivation Filen deliberately makes slow.
func (p *filenProvider) OnCredentialChange(fn func(map[string]string)) {
	p.mu.Lock()
	p.rotate = fn
	p.mu.Unlock()
}

// --- Keys ------------------------------------------------------------------

// filenKey is a 256-bit AES-GCM key: the account's key-encryption and
// data-encryption keys on a version 3 account, and every file's own key.
type filenKey struct {
	bytes [32]byte
	aead  cipher.AEAD
}

func newFilenKey(raw [32]byte) (filenKey, error) {
	block, err := aes.NewCipher(raw[:])
	if err != nil {
		return filenKey{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return filenKey{}, err
	}
	return filenKey{bytes: raw, aead: aead}, nil
}

// filenKeyFromString reads a file key as it appears in metadata: 32 raw
// characters on a version 1 or 2 file, 64 hex characters on version 3.
func filenKeyFromString(s string) (filenKey, error) {
	switch len(s) {
	case 32:
		return newFilenKey([32]byte([]byte(s)))
	case 64:
		raw, err := hex.DecodeString(s)
		if err != nil {
			return filenKey{}, fmt.Errorf("file key is not hex: %w", err)
		}
		return newFilenKey([32]byte(raw))
	}
	return filenKey{}, fmt.Errorf("file key has length %d, want 32 or 64", len(s))
}

// encryptMeta seals a string in Filen's version 3 metadata format.
func (k filenKey) encryptMeta(plain string) string {
	nonce := make([]byte, 12)
	rand.Read(nonce)
	sealed := k.aead.Seal(nil, nonce, []byte(plain), nil)
	return "003" + hex.EncodeToString(nonce) + base64.StdEncoding.EncodeToString(sealed)
}

func (k filenKey) decryptMeta(enc string) (string, error) {
	if len(enc) < 27 || enc[:3] != "003" {
		return "", fmt.Errorf("not version 3 metadata")
	}
	nonce, err := hex.DecodeString(enc[3:27])
	if err != nil {
		return "", err
	}
	sealed, err := base64.StdEncoding.DecodeString(enc[27:])
	if err != nil {
		return "", err
	}
	plain, err := k.aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// encryptData seals one chunk: a fresh 12-byte nonce, then the ciphertext
// with its tag.
func (k filenKey) encryptData(plain []byte) []byte {
	out := make([]byte, 12, 12+len(plain)+k.aead.Overhead())
	rand.Read(out)
	return k.aead.Seal(out, out[:12], plain, nil)
}

func (k filenKey) decryptData(sealed []byte) ([]byte, error) {
	if len(sealed) < 12 {
		return nil, fmt.Errorf("chunk too short to hold a nonce")
	}
	return k.aead.Open(nil, sealed[:12], sealed[12:], nil)
}

// asMasterKey is the same 32 bytes used the way a version 2 master key is —
// Filen encrypts a file's own name and size under its file key in that
// older format, whatever the account's version.
func (k filenKey) asMasterKey() (filenMasterKey, error) {
	return newFilenMasterKey(k.bytes[:])
}

// filenMasterKey is a version 1 or 2 account key: a string of bytes the
// real AES key is derived from with a single PBKDF2 round.
type filenMasterKey struct {
	raw  []byte
	aead cipher.AEAD
}

func newFilenMasterKey(raw []byte) (filenMasterKey, error) {
	derived, err := pbkdf2.Key(sha512.New, string(raw), raw, 1, 32)
	if err != nil {
		return filenMasterKey{}, err
	}
	block, err := aes.NewCipher(derived)
	if err != nil {
		return filenMasterKey{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return filenMasterKey{}, err
	}
	return filenMasterKey{raw: raw, aead: aead}, nil
}

// encryptMeta seals a string in Filen's version 2 metadata format, whose
// nonce is twelve printable characters rather than twelve random bytes.
func (k filenMasterKey) encryptMeta(plain string) string {
	nonce := []byte(filenRandomString(12))
	sealed := k.aead.Seal(nil, nonce, []byte(plain), nil)
	return "002" + string(nonce) + base64.StdEncoding.EncodeToString(sealed)
}

func (k filenMasterKey) decryptMeta(enc string) (string, error) {
	switch {
	case strings.HasPrefix(enc, "U2FsdGVk"):
		return k.decryptMetaV1(enc)
	case len(enc) >= 15 && enc[:3] == "002":
		sealed, err := base64.StdEncoding.DecodeString(enc[15:])
		if err != nil {
			return "", err
		}
		plain, err := k.aead.Open(nil, []byte(enc[3:15]), sealed, nil)
		if err != nil {
			return "", err
		}
		return string(plain), nil
	}
	return "", fmt.Errorf("not version 1 or 2 metadata")
}

// decryptMetaV1 reads the format Filen's first clients wrote: OpenSSL's
// "Salted__" AES-256-CBC with an MD5 key schedule. Old folders in an old
// account still carry names in it, so a listing has to read it even though
// nothing writes it any more.
func (k filenMasterKey) decryptMetaV1(enc string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	if len(raw) < 32 || len(raw)%aes.BlockSize != 0 {
		return "", fmt.Errorf("malformed version 1 metadata")
	}
	key, iv := evpBytesToKey(k.raw, raw[8:16])
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	plain := make([]byte, len(raw)-16)
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, raw[16:])
	pad := int(plain[len(plain)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(plain) {
		return "", fmt.Errorf("malformed version 1 padding")
	}
	return string(plain[:len(plain)-pad]), nil
}

// evpBytesToKey is OpenSSL's EVP_BytesToKey with MD5 and one round,
// stretched to a 32-byte key and a 16-byte IV.
func evpBytesToKey(password, salt []byte) (key, iv []byte) {
	var out []byte
	var prev []byte
	for len(out) < 48 {
		h := md5.New()
		h.Write(prev)
		h.Write(password)
		h.Write(salt)
		prev = h.Sum(nil)
		out = append(out, prev...)
	}
	return out[:32], out[32:48]
}

// filenRandomString is Filen's alphanumeric random string, used where the
// protocol wants printable randomness: version 2 nonces, file keys, and
// upload keys.
func filenRandomString(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	out := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range out {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		out[i] = alphabet[idx.Int64()]
	}
	return string(out)
}

// filenDeriveV2 turns a password and the account's salt into the version 2
// master key and the password hash Filen is shown at sign-in. The two come
// out of one 512-bit PBKDF2 stretch: the first half is the key and stays
// here, the second half is hashed once more and sent.
func filenDeriveV2(password, salt string) (filenMasterKey, string, error) {
	stretched, err := pbkdf2.Key(sha512.New, password, []byte(salt), 200000, 64)
	if err != nil {
		return filenMasterKey{}, "", err
	}
	derived := hex.EncodeToString(stretched)
	master, err := newFilenMasterKey([]byte(derived[:64]))
	if err != nil {
		return filenMasterKey{}, "", err
	}
	sum := sha512.Sum512([]byte(derived[64:]))
	return master, hex.EncodeToString(sum[:]), nil
}

// filenDeriveV3 is the version 3 equivalent: Argon2id instead of PBKDF2,
// producing the key-encryption key and the password hash.
func filenDeriveV3(password, salt string) (filenKey, string, error) {
	saltBytes, err := hex.DecodeString(salt)
	if err != nil {
		return filenKey{}, "", fmt.Errorf("salt is not hex: %w", err)
	}
	derived := hex.EncodeToString(argon2.IDKey([]byte(password), saltBytes, 3, 65536, 4, 64))
	raw, err := hex.DecodeString(derived[:64])
	if err != nil {
		return filenKey{}, "", err
	}
	kek, err := newFilenKey([32]byte(raw))
	if err != nil {
		return filenKey{}, "", err
	}
	return kek, derived[64:], nil
}

// filenV2Hash is how version 1 and 2 accounts hash a name for lookup:
// SHA-1 over the hex of SHA-512.
func filenV2Hash(name string) string {
	inner := sha512.Sum512([]byte(name))
	outer := sha1.Sum([]byte(hex.EncodeToString(inner[:])))
	return hex.EncodeToString(outer[:])
}

// filenHMACKey derives the name-hashing key a version 3 account uses from
// its RSA private key.
func filenHMACKey(private *rsa.PrivateKey) ([32]byte, error) {
	var out [32]byte
	key, err := hkdf.Key(sha256.New, private.D.Bytes(), nil, "hmac-sha256-key", 32)
	if err != nil {
		return out, err
	}
	copy(out[:], key)
	return out, nil
}

// --- Session ---------------------------------------------------------------

// filenSession is everything a signed-in account needs to talk to Filen
// without the password: the API key and the keys the password unlocked. It
// is what the "session" option stores, encrypted in the vault.
type filenSession struct {
	APIKey      string `json:"api_key"`
	AuthVersion int    `json:"auth_version"`
	BaseFolder  string `json:"base_folder"`

	// Keys holds the version 1/2 master keys, newest first, or on a version
	// 3 account the single data-encryption key as hex.
	Keys []string `json:"keys"`

	// HMACKey is the version 3 name-hashing key as hex; empty on version 2.
	HMACKey string `json:"hmac_key,omitempty"`

	masterKeys []filenMasterKey
	dek        filenKey
	hmacKey    [32]byte
}

func parseFilenSession(raw string) (*filenSession, error) {
	var s filenSession
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, err
	}
	if err := s.prepare(); err != nil {
		return nil, err
	}
	return &s, nil
}

// prepare builds the ciphers from the stored key strings.
func (s *filenSession) prepare() error {
	if s.APIKey == "" || s.BaseFolder == "" || len(s.Keys) == 0 {
		return fmt.Errorf("session is incomplete")
	}
	switch s.AuthVersion {
	case 1, 2:
		s.masterKeys = s.masterKeys[:0]
		for _, k := range s.Keys {
			mk, err := newFilenMasterKey([]byte(k))
			if err != nil {
				return err
			}
			s.masterKeys = append(s.masterKeys, mk)
		}
	case 3:
		dek, err := filenKeyFromString(s.Keys[0])
		if err != nil {
			return fmt.Errorf("data-encryption key: %w", err)
		}
		s.dek = dek
		hk, err := hex.DecodeString(s.HMACKey)
		if err != nil || len(hk) != 32 {
			return fmt.Errorf("name-hashing key is malformed")
		}
		copy(s.hmacKey[:], hk)
	default:
		return fmt.Errorf("unknown authentication version %d", s.AuthVersion)
	}
	return nil
}

func (s *filenSession) String() string {
	out, _ := json.Marshal(s)
	return string(out)
}

// encryptMeta seals metadata under the account's current key.
func (s *filenSession) encryptMeta(plain string) string {
	if s.AuthVersion == 3 {
		return s.dek.encryptMeta(plain)
	}
	return s.masterKeys[0].encryptMeta(plain)
}

// decryptMeta opens metadata written under any key the account has had.
func (s *filenSession) decryptMeta(enc string) (string, error) {
	if len(enc) >= 3 && enc[:3] == "003" {
		if s.AuthVersion != 3 {
			return "", fmt.Errorf("version 3 metadata on an account without a data-encryption key")
		}
		return s.dek.decryptMeta(enc)
	}
	var last error
	for _, mk := range s.masterKeys {
		plain, err := mk.decryptMeta(enc)
		if err == nil {
			return plain, nil
		}
		last = err
	}
	if last == nil {
		last = fmt.Errorf("no key to read it with")
	}
	return "", last
}

// hashName is the lookup hash Filen keeps beside every encrypted name.
func (s *filenSession) hashName(name string) string {
	name = strings.ToLower(name)
	if s.AuthVersion == 3 {
		mac := hmac.New(sha256.New, s.hmacKey[:])
		mac.Write([]byte(name))
		return hex.EncodeToString(mac.Sum(nil))
	}
	return filenV2Hash(name)
}

// fileVersion is the encryption version new files are written with.
func (s *filenSession) fileVersion() int {
	if s.AuthVersion == 3 {
		return 3
	}
	return 2
}

// newFileKey makes a key for one new file, in the form the account's
// version stores it: 32 printable characters on version 2, 32 random bytes
// on version 3.
func (s *filenSession) newFileKey() (filenKey, string, error) {
	if s.AuthVersion == 3 {
		var raw [32]byte
		rand.Read(raw[:])
		k, err := newFilenKey(raw)
		return k, hex.EncodeToString(raw[:]), err
	}
	str := filenRandomString(32)
	k, err := newFilenKey([32]byte([]byte(str)))
	return k, str, err
}

// --- API -------------------------------------------------------------------

// filenResponse is the envelope every gateway answer comes in.
type filenResponse struct {
	Status  bool            `json:"status"`
	Message string          `json:"message"`
	Code    string          `json:"code"`
	Data    json.RawMessage `json:"data"`
}

// filenError is a request Filen answered but refused.
type filenError struct {
	op      string
	code    string
	message string
}

func (e *filenError) Error() string {
	return fmt.Sprintf("%s: %s (%s)", e.op, e.message, e.code)
}

// filenCall sends a JSON request to the gateway and decodes the data field
// into out. apiKey may be empty for the two endpoints that need none.
func filenCall(ctx context.Context, apiKey, method, path string, payload, out any) error {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, filenGateway+path, body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return filenDo(req, apiKey, "filen "+path, out)
}

func filenDo(req *http.Request, apiKey, op string, out any) error {
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	defer drainAndClose(resp)
	if !isSuccess(resp.StatusCode) {
		return httpError(op, resp)
	}
	raw, err := readAllBody(resp)
	if err != nil {
		return err
	}
	var envelope filenResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("%s: parsing response: %w", op, err)
	}
	if !envelope.Status {
		return &filenError{op: op, code: envelope.Code, message: envelope.Message}
	}
	if out == nil {
		return nil
	}
	if len(envelope.Data) == 0 {
		return fmt.Errorf("%s: response carries no data", op)
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("%s: parsing response data: %w", op, err)
	}
	return nil
}

// session returns the signed-in session, signing in the first time.
func (p *filenProvider) session(ctx context.Context) (*filenSession, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sess != nil {
		return p.sess, nil
	}
	sess, err := p.signIn(ctx)
	if err != nil {
		return nil, err
	}
	p.sess = sess
	if p.rotate != nil {
		// The one-time code is spent; storing it would only make the next
		// sign-in fail with a stale one.
		p.rotate(map[string]string{"session": sess.String(), "two_factor_code": ""})
	}
	return sess, nil
}

// signIn derives the account's keys from the password, trades the derived
// hash for an API key, and fetches the keys the account holds.
func (p *filenProvider) signIn(ctx context.Context) (*filenSession, error) {
	var info struct {
		AuthVersion int    `json:"authVersion"`
		Salt        string `json:"salt"`
	}
	if err := filenCall(ctx, "", http.MethodPost, "/v3/auth/info", map[string]string{"email": p.email}, &info); err != nil {
		return nil, err
	}

	sess := &filenSession{AuthVersion: info.AuthVersion}
	var derivedPassword string
	var v2 filenMasterKey
	var kek filenKey
	switch info.AuthVersion {
	case 2:
		var err error
		v2, derivedPassword, err = filenDeriveV2(p.password, info.Salt)
		if err != nil {
			return nil, err
		}
	case 3:
		var err error
		kek, derivedPassword, err = filenDeriveV3(p.password, info.Salt)
		if err != nil {
			return nil, err
		}
	case 1:
		return nil, fmt.Errorf("filen: this account still uses Filen's original sign-in scheme, which SAND " +
			"does not implement; changing the account's password in Filen moves it to the current one")
	default:
		return nil, fmt.Errorf("filen: unknown authentication version %d", info.AuthVersion)
	}

	twoFactor := p.twoFactor
	if twoFactor == "" {
		// What Filen's own clients send when the account has no second
		// factor: the field is required, the value is a placeholder.
		twoFactor = "XXXXXX"
	}
	var login struct {
		APIKey string `json:"apiKey"`
	}
	err := filenCall(ctx, "", http.MethodPost, "/v3/login", map[string]any{
		"email":         p.email,
		"password":      derivedPassword,
		"twoFactorCode": twoFactor,
		"authVersion":   info.AuthVersion,
	}, &login)
	if err != nil {
		return nil, fmt.Errorf("filen sign-in: %w", err)
	}
	sess.APIKey = login.APIKey

	switch info.AuthVersion {
	case 2:
		// The account may have had several passwords, and files encrypted
		// under an old one are still encrypted under an old one. The server
		// hands back every key it holds, sealed under the current one.
		var keys struct {
			Keys string `json:"keys"`
		}
		err := filenCall(ctx, sess.APIKey, http.MethodPost, "/v3/user/masterKeys",
			map[string]string{"masterKeys": v2.encryptMeta(string(v2.raw))}, &keys)
		if err != nil {
			return nil, err
		}
		joined, err := v2.decryptMeta(keys.Keys)
		if err != nil {
			return nil, fmt.Errorf("filen: the password does not open this account's keys: %w", err)
		}
		sess.Keys = []string{string(v2.raw)}
		for _, k := range strings.Split(joined, "|") {
			if k != "" && k != string(v2.raw) {
				sess.Keys = append(sess.Keys, k)
			}
		}
	case 3:
		var dek struct {
			DEK string `json:"dek"`
		}
		if err := filenCall(ctx, sess.APIKey, http.MethodGet, "/v3/user/dek", nil, &dek); err != nil {
			return nil, err
		}
		plain, err := kek.decryptMeta(dek.DEK)
		if err != nil {
			return nil, fmt.Errorf("filen: the password does not open this account's keys: %w", err)
		}
		sess.Keys = []string{plain}
		if err := sess.prepareKeysOnly(); err != nil {
			return nil, err
		}

		// Version 3 hashes names with a key derived from the account's RSA
		// private key, which is itself stored encrypted under the DEK.
		var pair struct {
			PrivateKey string `json:"privateKey"`
		}
		if err := filenCall(ctx, sess.APIKey, http.MethodGet, "/v3/user/keyPair/info", nil, &pair); err != nil {
			return nil, err
		}
		pemless, err := sess.dek.decryptMeta(pair.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("filen: opening the account's key pair: %w", err)
		}
		der, err := base64.StdEncoding.DecodeString(pemless)
		if err != nil {
			return nil, fmt.Errorf("filen: the private key is not base64: %w", err)
		}
		parsed, err := x509.ParsePKCS8PrivateKey(der)
		if err != nil {
			return nil, fmt.Errorf("filen: parsing the private key: %w", err)
		}
		private, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("filen: the account's private key is not RSA")
		}
		hk, err := filenHMACKey(private)
		if err != nil {
			return nil, err
		}
		sess.HMACKey = hex.EncodeToString(hk[:])
	}

	var root struct {
		UUID string `json:"uuid"`
	}
	if err := filenCall(ctx, sess.APIKey, http.MethodGet, "/v3/user/baseFolder", nil, &root); err != nil {
		return nil, err
	}
	sess.BaseFolder = root.UUID
	if err := sess.prepare(); err != nil {
		return nil, err
	}
	return sess, nil
}

// prepareKeysOnly builds the data-encryption key before the rest of the
// session is known, since decrypting the key pair needs it.
func (s *filenSession) prepareKeysOnly() error {
	dek, err := filenKeyFromString(s.Keys[0])
	if err != nil {
		return fmt.Errorf("data-encryption key: %w", err)
	}
	s.dek = dek
	return nil
}

// --- Folder and listing ----------------------------------------------------

// filenDirContent is a folder listing as the server returns it: encrypted
// names, encrypted metadata, and the plaintext facts about where the bytes
// are.
type filenDirContent struct {
	Uploads []struct {
		UUID     string `json:"uuid"`
		Metadata string `json:"metadata"`
		Chunks   int64  `json:"chunks"`
		Size     int64  `json:"size"`
		Bucket   string `json:"bucket"`
		Region   string `json:"region"`
		Parent   string `json:"parent"`
		Version  int    `json:"version"`
	} `json:"uploads"`
	Folders []struct {
		UUID string `json:"uuid"`
		Name string `json:"name"` // encrypted metadata, despite the field name
	} `json:"folders"`
}

// filenFileMeta is the JSON inside a file's encrypted metadata.
type filenFileMeta struct {
	Name         string `json:"name"`
	Size         int64  `json:"size"`
	Mime         string `json:"mime"`
	Key          string `json:"key"`
	LastModified int64  `json:"lastModified"`
	Created      int64  `json:"creation"`
	Hash         string `json:"blake3"`
}

func (p *filenProvider) listDir(ctx context.Context, sess *filenSession, dir string) (filenDirContent, error) {
	var content filenDirContent
	err := filenCall(ctx, sess.APIKey, http.MethodPost, "/v3/dir/content", map[string]string{"uuid": dir}, &content)
	return content, err
}

// shardFolder resolves — creating if necessary — the folder shards live in.
func (p *filenProvider) shardFolder(ctx context.Context, sess *filenSession) (string, error) {
	if p.folderName == "" {
		return sess.BaseFolder, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.folder != "" {
		return p.folder, nil
	}

	content, err := p.listDir(ctx, sess, sess.BaseFolder)
	if err != nil {
		return "", err
	}
	for _, f := range content.Folders {
		plain, err := sess.decryptMeta(f.Name)
		if err != nil {
			continue // a folder under a key this session does not have
		}
		var meta struct {
			Name string `json:"name"`
		}
		if json.Unmarshal([]byte(plain), &meta) == nil && meta.Name == p.folderName {
			p.folder = f.UUID
			return p.folder, nil
		}
	}

	metaJSON, _ := json.Marshal(map[string]any{"name": p.folderName, "creation": time.Now().UnixMilli()})
	var created struct {
		UUID string `json:"uuid"`
	}
	err = filenCall(ctx, sess.APIKey, http.MethodPost, "/v3/dir/create", map[string]string{
		"uuid":       uuid.NewString(),
		"name":       sess.encryptMeta(string(metaJSON)),
		"nameHashed": sess.hashName(p.folderName),
		"parent":     sess.BaseFolder,
	}, &created)
	if err != nil {
		return "", err
	}
	if created.UUID == "" {
		return "", fmt.Errorf("filen create folder: no UUID in the response")
	}
	p.folder = created.UUID
	return p.folder, nil
}

// refresh lists the shard folder and replaces what is known about the files
// in it, answering with the entry for key if there is one. Names the
// session cannot decrypt are skipped rather than fatal: a folder shared
// into the account carries names under someone else's key.
func (p *filenProvider) refresh(ctx context.Context, key string) (filenFile, bool, error) {
	sess, err := p.session(ctx)
	if err != nil {
		return filenFile{}, false, err
	}
	folder, err := p.shardFolder(ctx, sess)
	if err != nil {
		return filenFile{}, false, err
	}
	content, err := p.listDir(ctx, sess, folder)
	if err != nil {
		return filenFile{}, false, err
	}

	entries := make(map[string]filenFile, len(content.Uploads))
	for _, u := range content.Uploads {
		plain, err := sess.decryptMeta(u.Metadata)
		if err != nil {
			continue
		}
		var meta filenFileMeta
		if err := json.Unmarshal([]byte(plain), &meta); err != nil || meta.Name == "" {
			continue
		}
		fileKey, err := filenKeyFromString(meta.Key)
		if err != nil {
			continue
		}
		entries[meta.Name] = filenFile{
			uuid: u.UUID, size: meta.Size, chunks: u.Chunks,
			bucket: u.Bucket, region: u.Region, version: u.Version, key: fileKey,
		}
	}

	p.mu.Lock()
	p.entries = entries
	entry, ok := entries[key]
	p.mu.Unlock()
	return entry, ok, nil
}

func (p *filenProvider) entryFor(ctx context.Context, key string) (filenFile, error) {
	p.mu.Lock()
	entry, ok := p.entries[key]
	p.mu.Unlock()
	if ok {
		return entry, nil
	}
	entry, ok, err := p.refresh(ctx, key)
	if err != nil {
		return filenFile{}, err
	}
	if !ok {
		return filenFile{}, ErrNotFound
	}
	return entry, nil
}

func (p *filenProvider) forget(key string) {
	p.mu.Lock()
	delete(p.entries, key)
	p.mu.Unlock()
}

// --- The six methods -------------------------------------------------------

// Put encrypts the shard chunk by chunk under a key of its own, sends the
// chunks up, then registers the file with its encrypted metadata. A file
// already under the name is replaced by the registration: Filen keeps one
// file per hashed name in a folder.
func (p *filenProvider) Put(ctx context.Context, key string, data []byte) error {
	sess, err := p.session(ctx)
	if err != nil {
		return err
	}
	folder, err := p.shardFolder(ctx, sess)
	if err != nil {
		return err
	}
	previous, err := p.entryFor(ctx, key)
	if err != nil && err != ErrNotFound {
		return err
	}

	fileKey, keyString, err := sess.newFileKey()
	if err != nil {
		return err
	}
	fileUUID := uuid.NewString()
	uploadKey := filenRandomString(32)

	var bucket, region string
	chunks := (int64(len(data)) + filenChunkSize - 1) / filenChunkSize
	for i := int64(0); i < chunks; i++ {
		start := i * filenChunkSize
		end := min(start+filenChunkSize, int64(len(data)))
		sealed := fileKey.encryptData(data[start:end])
		sum := sha512.Sum512(sealed)

		params := url.Values{
			"uuid":      {fileUUID},
			"index":     {strconv.FormatInt(i, 10)},
			"parent":    {folder},
			"uploadKey": {uploadKey},
			"hash":      {hex.EncodeToString(sum[:])},
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			filenIngest+"/v3/upload?"+params.Encode(), bytes.NewReader(sealed))
		if err != nil {
			return err
		}
		req.ContentLength = int64(len(sealed))
		var placed struct {
			Bucket string `json:"bucket"`
			Region string `json:"region"`
		}
		if err := filenDo(req, sess.APIKey, "filen upload", &placed); err != nil {
			return fmt.Errorf("chunk %d of %d: %w", i+1, chunks, err)
		}
		if bucket == "" {
			bucket, region = placed.Bucket, placed.Region
		}
	}

	now := time.Now().UnixMilli()
	metaJSON, err := json.Marshal(filenFileMeta{
		Name: key, Size: int64(len(data)), Mime: "application/octet-stream",
		Key: keyString, LastModified: now, Created: now,
	})
	if err != nil {
		return err
	}
	// The name, size and type Filen shows in a listing are sealed under the
	// file's own key in the version 2 format, whatever the account's
	// version; the metadata blob is sealed under the account's key.
	fileMaster, err := fileKey.asMasterKey()
	if err != nil {
		return err
	}
	done := map[string]any{
		"uuid":       fileUUID,
		"name":       fileMaster.encryptMeta(key),
		"nameHashed": sess.hashName(key),
		"size":       fileMaster.encryptMeta(strconv.Itoa(len(data))),
		"parent":     folder,
		"mime":       fileMaster.encryptMeta("application/octet-stream"),
		"metadata":   sess.encryptMeta(string(metaJSON)),
		"version":    sess.fileVersion(),
	}
	endpoint := "/v3/upload/empty"
	if len(data) > 0 {
		endpoint = "/v3/upload/done"
		done["chunks"] = chunks
		done["rm"] = filenRandomString(32)
		done["uploadKey"] = uploadKey
	}
	if err := filenCall(ctx, sess.APIKey, http.MethodPost, endpoint, done, nil); err != nil {
		return err
	}

	p.mu.Lock()
	p.entries[key] = filenFile{
		uuid: fileUUID, size: int64(len(data)), chunks: chunks,
		bucket: bucket, region: region, version: sess.fileVersion(), key: fileKey,
	}
	p.mu.Unlock()

	// The registration replaced the old file under this name; what is left
	// of it is erased so it stops counting against the account.
	if previous.uuid != "" && previous.uuid != fileUUID {
		if err := p.erase(ctx, sess, previous.uuid); err != nil {
			return fmt.Errorf("filen upload: erasing the old copy: %w", err)
		}
	}
	return nil
}

func (p *filenProvider) Get(ctx context.Context, key string) ([]byte, error) {
	sess, err := p.session(ctx)
	if err != nil {
		return nil, err
	}
	entry, err := p.entryFor(ctx, key)
	if err != nil {
		return nil, err
	}
	if entry.version == 1 {
		return nil, fmt.Errorf("filen: %s was written in Filen's original file format, which SAND does not read", key)
	}

	out := make([]byte, 0, entry.size)
	for i := int64(0); i < entry.chunks; i++ {
		addr := fmt.Sprintf("%s/%s/%s/%s/%d", filenEgest, entry.region, entry.bucket, entry.uuid, i)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+sess.APIKey)
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("filen download: %w", err)
		}
		if resp.StatusCode == http.StatusNotFound {
			drainAndClose(resp)
			p.forget(key)
			return nil, ErrNotFound
		}
		if !isSuccess(resp.StatusCode) {
			err := httpError("filen download", resp)
			drainAndClose(resp)
			return nil, err
		}
		sealed, err := readAllBody(resp)
		drainAndClose(resp)
		if err != nil {
			return nil, err
		}
		plain, err := entry.key.decryptData(sealed)
		if err != nil {
			return nil, fmt.Errorf("filen download: chunk %d of %s does not decrypt: %w", i, key, err)
		}
		out = append(out, plain...)
	}
	if int64(len(out)) != entry.size {
		return nil, fmt.Errorf("filen download: %s came back as %d bytes, its metadata says %d", key, len(out), entry.size)
	}
	return out, nil
}

// Stat answers from a fresh listing, since it is what the health check asks.
func (p *filenProvider) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	entry, found, err := p.refresh(ctx, key)
	if err != nil {
		return ObjectInfo{}, err
	}
	if !found {
		return ObjectInfo{}, ErrNotFound
	}
	return ObjectInfo{Key: key, Size: entry.size}, nil
}

func (p *filenProvider) Delete(ctx context.Context, key string) error {
	sess, err := p.session(ctx)
	if err != nil {
		return err
	}
	entry, err := p.entryFor(ctx, key)
	if err == ErrNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	p.forget(key)
	return p.erase(ctx, sess, entry.uuid)
}

// erase removes a file for good: into the trash, then out of it. A file in
// Filen's trash still counts against the account, and nobody is going to
// recognise an encrypted part there and restore it.
func (p *filenProvider) erase(ctx context.Context, sess *filenSession, fileUUID string) error {
	err := filenCall(ctx, sess.APIKey, http.MethodPost, "/v3/file/trash", map[string]string{"uuid": fileUUID}, nil)
	if err != nil && !filenGone(err) {
		return err
	}
	err = filenCall(ctx, sess.APIKey, http.MethodPost, "/v3/file/delete/permanent", map[string]string{"uuid": fileUUID}, nil)
	if err != nil && !filenGone(err) {
		return err
	}
	return nil
}

// filenGone reports whether an error says the file was already not there.
func filenGone(err error) bool {
	var fe *filenError
	if errors.As(err, &fe) {
		return strings.Contains(fe.code, "not_found") || strings.Contains(strings.ToLower(fe.message), "not found")
	}
	return false
}

func (p *filenProvider) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if _, _, err := p.refresh(ctx, ""); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []ObjectInfo
	for name, entry := range p.entries {
		if strings.HasPrefix(name, prefix) {
			out = append(out, ObjectInfo{Key: name, Size: entry.size})
		}
	}
	return out, nil
}

// filenUserInfo is what /v3/user/info reports.
type filenUserInfo struct {
	Email       string `json:"email"`
	MaxStorage  int64  `json:"maxStorage"`
	StorageUsed int64  `json:"storageUsed"`
}

func (p *filenProvider) userInfo(ctx context.Context) (filenUserInfo, error) {
	sess, err := p.session(ctx)
	if err != nil {
		return filenUserInfo{}, err
	}
	var info filenUserInfo
	err = filenCall(ctx, sess.APIKey, http.MethodGet, "/v3/user/info", nil, &info)
	return info, err
}

func (p *filenProvider) Ping(ctx context.Context) error {
	if _, err := p.userInfo(ctx); err != nil {
		return fmt.Errorf("cannot reach Filen: %w", err)
	}
	return nil
}

func (p *filenProvider) Usage(ctx context.Context) (Usage, error) {
	info, err := p.userInfo(ctx)
	if err != nil {
		return Usage{}, err
	}
	return Usage{Used: info.StorageUsed, Total: info.MaxStorage}, nil
}

// Account reports the signed-in email, used to label the connection.
func (p *filenProvider) Account(ctx context.Context) (string, error) {
	info, err := p.userInfo(ctx)
	if err != nil {
		return "", err
	}
	if info.Email != "" {
		return info.Email, nil
	}
	return p.email, nil
}
