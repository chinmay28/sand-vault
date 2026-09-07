package provider

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The fixtures below were produced by Filen's own Go SDK
// (github.com/FilenCloudDienste/filen-sdk-go, package crypto), so a match here
// is conformance with the real client rather than with this file's own
// reading of the protocol.
const (
	filenFixturePassword = "correct horse battery"

	filenFixtureV2Salt        = "0123456789abcdef0123456789abcdef"
	filenFixtureV2MasterRaw   = "35af21b207f7b4d321762abf50f64f70ec9146c53d00f1aa22d22af0c2b4e304"
	filenFixtureV2DerivedPass = "4caec788fe2de77d8416bff74cc67ca4e8ee89e85ed7b2db918d5dbe26042698bfcf31b35b713e34eaba2ba572ea7e212370573d9e399416713787f417838296"
	filenFixtureV2Meta        = "002MyHyNopYY9F2xm7FCJ2omw+cqI9it3ahPbt3ajLdSRxKIgk/5IgcaQ=="

	filenFixtureV3Salt        = "00112233445566778899aabbccddeeff"
	filenFixtureV3KEK         = "bed70e0db101acc7bb23f0ba69c03a2b4328ea81609b141441255f3297de2007"
	filenFixtureV3DerivedPass = "f10ec7038704944a48d16337a0b593d94080d7f4e767f728e1f80cad7ee6690e"
	filenFixtureV3Meta        = "003f8d0b2065c0ca67fcf1083f5wDn3wP17V7aaE5ExDluDPwHvUZBulZRX/6iHumFgsQ=="

	// A file key of 32 printable characters, a chunk sealed under it, and the
	// file's name sealed under the same key in the version 2 format.
	filenFixtureFileKey        = "0123456789abcdef0123456789abcdef"
	filenFixtureData           = "ddd66a975f8cfa8c2d565c3f4292bd980eba483e8e7a65ec2c913b663f972fde2dc18657b8ca3323f6f3da8a21f8f34d4fff0cd3048d7cd6965e3aad"
	filenFixtureDataPlain      = "twelve chunk bytes and then some"
	filenFixtureFileMasterMeta = "002ejGpP0nnkjLjFBP+WZpeLbVXoV6jLdT6oHa4sjZavrD0/W9WVTAwDGhxLxp8"

	filenFixtureName   = "abc-c0000000-p1.sand"
	filenFixtureV2Hash = "a541b0728112f14f1ef8a716921173588f399aed"

	filenFixtureRSA      = "MIIEvgIBADANBgkqhkiG9w0BAQEFAASCBKgwggSkAgEAAoIBAQDF4EeFGaby9/lz6w+tYR/r98CzMkXp/zfwoZnpdqVJwql05DHjeMHtnfjrP4fAyo0/vHubPs0g7e6zn6YoPDNDqGl812M1Eu0xWLOEW3y8eWLA7qFnoWxuMv5YzOzUgjc1Ouv4XSjNo964+GQU64BApRU633m9RjVhCoY1HtX3WWvYFK1zwrqBgOHvDAvtAur7GznFElcre9BX87F757EzowAaHFwz9gvev5RGpVvCMmON00ZIfn4FkItYs6MgFP3Gu0XvyZGcsnDA/xwEbpGkywR6BUP6vWl2YyykPfaSKPwzuGl1QoiJdoMB07BljC8EJZgynNfZkP7/ctIx4TLNAgMBAAECggEAEp2SbOmoZKRCkg6zp16DR8pDlTguDqEFjLxPRAD29NT85zPOo7VJHUcm007jQRZtKmlbgZPrbWUk9z4WPiPHuN4/qlCDE0uoB+Pt445N0ldjHH52rc6oakee2RDSWP21Hutfprpw68O2YNVxaOxs4OgG8sZyaAWSYQJB9P5lJ8NVvmWNYR+mY4nsClxogQj+wbpSx/86DDfUtv6RBpoOohcYvyW0JGYxjgd6iAiOGahTdwDwuLA97dVuevKa2J7eJD50fhu2ZAxSF4fWR3feCLOGN0EFbl+TKiwDqdXNZK4yaims/4OWQkdpc0H9GqLL4AD+dkBFfV/X3UqAeR9VgQKBgQD7t6lY9mIRG9IiMcjrLkPxaTJeM0/pEw9kDGstFgvbd4FNVCgwXaQrDsHRSnHsR4GwukilySBEsTvbIlVDPL8dMOGJpG1lxKpDHiPv/LpodyiolVsUakIQvDfpMPKT+Zc1LYGgnhwadspFsyxQHemBpJNoKUlvUKChKp8YyirXDQKBgQDJPh2R3y4RQUwHVMacZek7RK0w7IuuqdYeUz6budUOokEYYv1bMCMrJbCiGYg8wIhAa9iITlU1fE0JDWd/P4EHIiBf0ttEqa9w+B+LeZyOTPr9YQg+qWpHnDQyiWKgq8+T+sVnXHNw1vU+y4LX0I5qE1KOvIsJdzdbr/t9oUTawQKBgQDdprtjkAmIwTPHUol2tlWzxYtJseti9Jqv4dOabvhf+BqO6lU9SaffFm6LCf/JLKpB4bdI7RMSCfMIInJr85jiboGbf4OpgoTe9zJ0B9ppVMwjruj10B9+tw6Qs75XmQeSFxE3SyK6FvJEb+LMZZqRw0beCMUWVSws3ugbnyIcHQKBgQC5Xs4+ICZ/Hna6Cg0o43cDcS9XcYz5RthE9sklCPiIkk0D+asG5ECA7ibWKk6kJ3VaYf0DEaTLr8QGIqLDQ+vGdlj7626uwN8qYGQuRcdADQjlfQvrLIMJk4lBQ+vltF1xIf3USATOXDNrtGrCAQouC75wXJx2C6qiemheQL78AQKBgFZHIdTa6q9laz7c56+6efen+HecMqRQ03Rv3hsjkyEIM/o3Ps9sZZn36V8bH5y2FgsMUUpILDSO8meepjIsAL2LEAvqJ+Lm+m7DnlrucwIqwUBpIbHb+FUC3vS4bOmBJk09vBYU3+I0smxlCRhfSxC4fh3dvvDgZk7GJuzCmlmB"
	filenFixtureHMACHash = "ff053632ef938f661cdac3b316d70464817b5ab1236c13d71ace392a56f9b861"
)

func TestFilenCryptoMatchesTheOfficialSDK(t *testing.T) {
	master, pass, err := filenDeriveV2(filenFixturePassword, filenFixtureV2Salt)
	if err != nil {
		t.Fatal(err)
	}
	if string(master.raw) != filenFixtureV2MasterRaw {
		t.Errorf("v2 master key = %s", master.raw)
	}
	if pass != filenFixtureV2DerivedPass {
		t.Errorf("v2 derived password = %s", pass)
	}
	if plain, err := master.decryptMeta(filenFixtureV2Meta); err != nil || plain != `{"name":"sand"}` {
		t.Errorf("v2 metadata decrypts to %q, %v", plain, err)
	}
	if plain, err := master.decryptMeta(master.encryptMeta("round trip")); err != nil || plain != "round trip" {
		t.Errorf("v2 round trip = %q, %v", plain, err)
	}

	kek, pass3, err := filenDeriveV3(filenFixturePassword, filenFixtureV3Salt)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(kek.bytes[:]) != filenFixtureV3KEK {
		t.Errorf("v3 KEK = %x", kek.bytes)
	}
	if pass3 != filenFixtureV3DerivedPass {
		t.Errorf("v3 derived password = %s", pass3)
	}
	if plain, err := kek.decryptMeta(filenFixtureV3Meta); err != nil || plain != `{"name":"sand"}` {
		t.Errorf("v3 metadata decrypts to %q, %v", plain, err)
	}

	fileKey, err := filenKeyFromString(filenFixtureFileKey)
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := hex.DecodeString(filenFixtureData)
	if plain, err := fileKey.decryptData(sealed); err != nil || string(plain) != filenFixtureDataPlain {
		t.Errorf("chunk decrypts to %q, %v", plain, err)
	}
	if plain, err := fileKey.decryptData(fileKey.encryptData([]byte("again"))); err != nil || string(plain) != "again" {
		t.Errorf("chunk round trip = %q, %v", plain, err)
	}
	fileMaster, _ := fileKey.asMasterKey()
	if plain, err := fileMaster.decryptMeta(filenFixtureFileMasterMeta); err != nil || plain != filenFixtureName {
		t.Errorf("file name under the file key decrypts to %q, %v", plain, err)
	}

	if got := filenV2Hash(filenFixtureName); got != filenFixtureV2Hash {
		t.Errorf("v2 name hash = %s", got)
	}

	der, _ := base64.StdEncoding.DecodeString(filenFixtureRSA)
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		t.Fatal(err)
	}
	hk, err := filenHMACKey(parsed.(*rsa.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	sess := &filenSession{AuthVersion: 3, hmacKey: hk}
	if got := sess.hashName(filenFixtureName); got != filenFixtureHMACHash {
		t.Errorf("v3 name hash = %s", got)
	}
}

func TestFilenV1MetadataStillReads(t *testing.T) {
	// A "Salted__" AES-256-CBC blob, as Filen's first clients wrote and as
	// `openssl enc -aes-256-cbc -md md5 -pass pass:<key>` still produces.
	master, _ := newFilenMasterKey([]byte("legacy-master-key"))
	salt := []byte("saltsalt")
	key, iv := evpBytesToKey(master.raw, salt)
	plain := []byte(`{"name":"old folder"}`)
	pad := 16 - len(plain)%16
	padded := append(plain, bytes.Repeat([]byte{byte(pad)}, pad)...)
	blob := append([]byte("Salted__"), salt...)
	block, _ := aesCBCEncrypt(key, iv, padded)
	blob = append(blob, block...)
	enc := base64.StdEncoding.EncodeToString(blob)
	if !strings.HasPrefix(enc, "U2FsdGVk") {
		t.Fatalf("fixture does not start the way Filen's v1 metadata does: %s", enc)
	}
	got, err := master.decryptMeta(enc)
	if err != nil || got != string(plain) {
		t.Errorf("v1 metadata decrypts to %q, %v", got, err)
	}
}

// aesCBCEncrypt is the encrypting half of the v1 scheme, which the backend
// only ever decrypts; the test needs it to build a fixture.
func aesCBCEncrypt(key, iv, padded []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return out, nil
}

// --- Stub --------------------------------------------------------------------

// filenStub is enough of Filen's three hosts to drive the backend: sign-in,
// the account's keys, encrypted folder listings, chunk storage, and the
// upload registration that dedupes on the hashed name. It never decrypts
// anything, exactly as the real service cannot.
type filenStub struct {
	t  *testing.T
	mu sync.Mutex

	authVersion int
	salt        string
	password    string // the derived password sign-in expects
	twoFactor   string // required code, "" for none
	apiKey      string
	baseFolder  string
	masterKeys  string // v2: the encrypted "a|b" list the server hands back
	dek         string // v3: the encrypted DEK
	privateKey  string // encrypted PKCS8 base64
	publicKey   string

	folders map[string]filenStubFolder
	files   map[string]*filenStubFile
	trashed map[string]bool
	logins  int
}

type filenStubFolder struct{ parent, name string }

type filenStubFile struct {
	parent, metadata, nameHashed string
	name, size, mime             string
	version                      int
	chunks                       map[int][]byte
}

func (s *filenStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := r.URL.Path
	if strings.HasPrefix(path, "/egest/") {
		s.serveEgest(w, r)
		return
	}
	if !strings.HasPrefix(path, "/v3/auth/info") && path != "/v3/login" {
		if r.Header.Get("Authorization") != "Bearer "+s.apiKey {
			s.reply(w, false, "unauthorized", "api_key_not_found", nil)
			return
		}
	}
	if path == "/ingest/v3/upload" {
		// The body is the chunk itself, not JSON.
		s.serveUpload(w, r)
		return
	}
	var body map[string]any
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&body)
	}
	str := func(k string) string {
		if v, ok := body[k]; ok {
			return fmt.Sprint(v)
		}
		return ""
	}

	switch path {
	case "/v3/auth/info":
		s.reply(w, true, "", "", map[string]any{"authVersion": s.authVersion, "salt": s.salt})
	case "/v3/login":
		s.logins++
		if str("password") != s.password {
			s.reply(w, false, "Invalid email or password.", "invalid_credentials", nil)
			return
		}
		if s.twoFactor != "" && str("twoFactorCode") != s.twoFactor {
			s.reply(w, false, "Please enter your Two Factor Authentication code.", "enter_2fa", nil)
			return
		}
		s.reply(w, true, "", "", map[string]any{"apiKey": s.apiKey})
	case "/v3/user/masterKeys":
		s.reply(w, true, "", "", map[string]any{"keys": s.masterKeys})
	case "/v3/user/dek":
		s.reply(w, true, "", "", map[string]any{"dek": s.dek})
	case "/v3/user/keyPair/info":
		s.reply(w, true, "", "", map[string]any{"privateKey": s.privateKey, "publicKey": s.publicKey})
	case "/v3/user/baseFolder":
		s.reply(w, true, "", "", map[string]any{"uuid": s.baseFolder})
	case "/v3/user/info":
		s.reply(w, true, "", "", map[string]any{"email": "alice@example.test", "maxStorage": 1000, "storageUsed": 250})
	case "/v3/dir/content":
		s.serveDirContent(w, str("uuid"))
	case "/v3/dir/create":
		id := str("uuid")
		s.folders[id] = filenStubFolder{parent: str("parent"), name: str("name")}
		s.reply(w, true, "", "", map[string]any{"uuid": id})
	case "/v3/upload/done", "/v3/upload/empty":
		s.serveUploadDone(w, path, body, str)
	case "/v3/file/trash":
		if _, ok := s.files[str("uuid")]; !ok {
			s.reply(w, false, "File not found.", "file_not_found", nil)
			return
		}
		s.trashed[str("uuid")] = true
		s.reply(w, true, "", "", nil)
	case "/v3/file/delete/permanent":
		if _, ok := s.files[str("uuid")]; !ok {
			s.reply(w, false, "File not found.", "file_not_found", nil)
			return
		}
		delete(s.files, str("uuid"))
		delete(s.trashed, str("uuid"))
		s.reply(w, true, "", "", nil)
	default:
		s.t.Errorf("unexpected Filen request %s %s", r.Method, path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *filenStub) reply(w http.ResponseWriter, ok bool, message, code string, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": ok, "message": message, "code": code, "data": data})
}

func (s *filenStub) serveDirContent(w http.ResponseWriter, dir string) {
	uploads := []map[string]any{}
	for id, f := range s.files {
		if f.parent != dir || s.trashed[id] {
			continue
		}
		size := 0
		for _, c := range f.chunks {
			size += len(c)
		}
		uploads = append(uploads, map[string]any{
			"uuid": id, "metadata": f.metadata, "chunks": len(f.chunks), "size": size,
			"bucket": "bucket-1", "region": "eu-1", "parent": dir, "version": f.version,
		})
	}
	folders := []map[string]any{}
	for id, f := range s.folders {
		if f.parent == dir {
			folders = append(folders, map[string]any{"uuid": id, "name": f.name, "parent": dir})
		}
	}
	s.reply(w, true, "", "", map[string]any{"uploads": uploads, "folders": folders})
}

func (s *filenStub) serveUpload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	data, _ := io.ReadAll(r.Body)
	sum := sha512.Sum512(data)
	if q.Get("hash") != hex.EncodeToString(sum[:]) {
		s.reply(w, false, "Hash mismatch.", "invalid_hash", nil)
		return
	}
	if q.Get("uploadKey") == "" || q.Get("parent") == "" {
		s.reply(w, false, "Missing parameters.", "invalid_params", nil)
		return
	}
	idx, _ := strconv.Atoi(q.Get("index"))
	f, ok := s.files[q.Get("uuid")]
	if !ok {
		f = &filenStubFile{parent: q.Get("parent"), chunks: map[int][]byte{}}
		s.files[q.Get("uuid")] = f
	}
	f.chunks[idx] = data
	s.reply(w, true, "", "", map[string]any{"bucket": "bucket-1", "region": "eu-1"})
}

func (s *filenStub) serveUploadDone(w http.ResponseWriter, path string, body map[string]any, str func(string) string) {
	id := str("uuid")
	f, ok := s.files[id]
	if path == "/v3/upload/empty" {
		if ok {
			s.reply(w, false, "Chunks were uploaded for an empty file.", "invalid_params", nil)
			return
		}
		f = &filenStubFile{parent: str("parent"), chunks: map[int][]byte{}}
		s.files[id] = f
	} else if !ok {
		s.reply(w, false, "No chunks uploaded.", "file_not_found", nil)
		return
	} else if want, _ := body["chunks"].(float64); int(want) != len(f.chunks) {
		s.reply(w, false, fmt.Sprintf("Declared %v chunks, received %d.", want, len(f.chunks)), "invalid_params", nil)
		return
	}
	for _, field := range []string{"name", "nameHashed", "size", "mime", "metadata", "version"} {
		if str(field) == "" {
			s.reply(w, false, "Missing "+field, "invalid_params", nil)
			return
		}
	}
	// One file per hashed name in a folder: the registration replaces any
	// other, which is how an overwrite works on the real service.
	for other, of := range s.files {
		if other != id && of.parent == f.parent && of.nameHashed == str("nameHashed") {
			delete(s.files, other)
		}
	}
	f.metadata, f.nameHashed = str("metadata"), str("nameHashed")
	f.name, f.size, f.mime = str("name"), str("size"), str("mime")
	f.version, _ = strconv.Atoi(str("version"))
	s.reply(w, true, "", "", map[string]any{"chunks": len(f.chunks), "size": 0})
}

func (s *filenStub) serveEgest(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+s.apiKey {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/egest/"), "/")
	if len(parts) != 4 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f, ok := s.files[parts[2]]
	idx, _ := strconv.Atoi(parts[3])
	if !ok || s.trashed[parts[2]] {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	chunk, ok := f.chunks[idx]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Write(chunk)
}

// newFilenStub builds an account of the given authentication version whose
// keys are sealed the way the real service seals them, using this file's
// own primitives — which the known-answer test above holds to the SDK.
func newFilenStub(t *testing.T, authVersion int) *filenStub {
	s := &filenStub{
		t: t, authVersion: authVersion, apiKey: "api-key-1", baseFolder: "base-folder",
		folders: map[string]filenStubFolder{}, files: map[string]*filenStubFile{}, trashed: map[string]bool{},
	}
	private, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(private)
	pub, _ := x509.MarshalPKIXPublicKey(&private.PublicKey)
	s.publicKey = base64.StdEncoding.EncodeToString(pub)

	switch authVersion {
	case 2:
		s.salt = filenFixtureV2Salt
		master, pass, _ := filenDeriveV2(filenFixturePassword, s.salt)
		s.password = pass
		// The account has had one earlier password; the old key is still on
		// file so old metadata can be read.
		s.masterKeys = master.encryptMeta(string(master.raw) + "|" + "oldkeyoldkeyoldkeyoldkeyoldkeyoldkeyoldkeyoldkeyoldkeyoldkeyolde")
		s.privateKey = master.encryptMeta(base64.StdEncoding.EncodeToString(der))
	case 3:
		s.salt = filenFixtureV3Salt
		kek, pass, _ := filenDeriveV3(filenFixturePassword, s.salt)
		s.password = pass
		var raw [32]byte
		rand.Read(raw[:])
		dek, _ := newFilenKey(raw)
		s.dek = kek.encryptMeta(hex.EncodeToString(raw[:]))
		s.privateKey = dek.encryptMeta(base64.StdEncoding.EncodeToString(der))
	}
	return s
}

func newTestFilen(t *testing.T, authVersion int, options map[string]string) (*filenProvider, *filenStub, *[]map[string]string) {
	stub := newFilenStub(t, authVersion)
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)

	restore := []string{filenGateway, filenIngest, filenEgest}
	filenGateway, filenIngest, filenEgest = server.URL, server.URL+"/ingest", server.URL+"/egest"
	t.Cleanup(func() { filenGateway, filenIngest, filenEgest = restore[0], restore[1], restore[2] })

	opts := map[string]string{"email": "alice@example.test", "password": filenFixturePassword, "folder": "sand"}
	for k, v := range options {
		opts[k] = v
	}
	p, err := New(Config{Kind: KindFilen, Name: "filen", Options: opts})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var rotations []map[string]string
	p.(CredentialRotator).OnCredentialChange(func(m map[string]string) { rotations = append(rotations, m) })
	return p.(*filenProvider), stub, &rotations
}

func filenRoundTrip(t *testing.T, authVersion int) {
	p, stub, rotations := newTestFilen(t, authVersion, nil)
	ctx := context.Background()

	if err := p.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if account, err := p.Account(ctx); err != nil || account != "alice@example.test" {
		t.Errorf("Account = %q, %v", account, err)
	}
	if len(*rotations) != 1 || (*rotations)[0]["session"] == "" || (*rotations)[0]["two_factor_code"] != "" {
		t.Fatalf("the sign-in was not written back as a session: %v", *rotations)
	}
	if stub.logins != 1 {
		t.Errorf("signed in %d times, want once", stub.logins)
	}

	payload := []byte("encrypted shard bytes")
	key := "abc-c0000000-p1.sand"
	if err := p.Put(ctx, key, payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if len(stub.folders) != 1 {
		t.Fatalf("shard folder was not created: %v", stub.folders)
	}
	got, err := p.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("Get returned %q, want %q", got, payload)
	}

	// Nothing the server holds may say what the file is called or contain
	// its bytes.
	for _, f := range stub.files {
		for _, field := range []string{f.metadata, f.name, f.nameHashed, f.size, f.mime} {
			if strings.Contains(field, key) || strings.Contains(field, "sand") {
				t.Errorf("a file record leaked a plaintext name: %q", field)
			}
		}
		for _, c := range f.chunks {
			if bytes.Contains(c, payload) {
				t.Error("a stored chunk holds the plaintext part")
			}
		}
	}
	for _, f := range stub.folders {
		if strings.Contains(f.name, "sand") {
			t.Errorf("the folder record leaked its plaintext name: %q", f.name)
		}
	}

	if err := p.Put(ctx, key, []byte("second")); err != nil {
		t.Fatalf("overwrite Put: %v", err)
	}
	if got, _ := p.Get(ctx, key); string(got) != "second" {
		t.Errorf("Get after overwrite = %q", got)
	}
	if len(stub.files) != 1 {
		t.Errorf("an overwrite left %d files, want 1", len(stub.files))
	}
	info, err := p.Stat(ctx, key)
	if err != nil || info.Size != int64(len("second")) {
		t.Errorf("Stat = %+v, %v", info, err)
	}

	for _, k := range []string{"abc-c0000000-p2.sand", "def-c0000000-p1.sand", "manifest.sand"} {
		if err := p.Put(ctx, k, []byte(k)); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}
	objects, err := p.List(ctx, "abc")
	if err != nil || len(objects) != 2 {
		t.Errorf("List(abc) = %+v, %v; want two", objects, err)
	}
	if all, _ := p.List(ctx, ""); len(all) != 4 {
		t.Errorf("List of everything found %d, want 4", len(all))
	}

	usage, err := p.Usage(ctx)
	if err != nil || usage.Total != 1000 || usage.Used != 250 {
		t.Errorf("Usage = %+v, %v", usage, err)
	}

	if err := p.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := p.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
	if err := p.Delete(ctx, key); err != nil {
		t.Errorf("second Delete: %v", err)
	}
	if len(stub.trashed) != 0 {
		t.Errorf("%d files were left in the trash", len(stub.trashed))
	}
}

func TestFilenRoundTripV3(t *testing.T) { filenRoundTrip(t, 3) }
func TestFilenRoundTripV2(t *testing.T) { filenRoundTrip(t, 2) }

func TestFilenLargeShardGoesUpInChunks(t *testing.T) {
	p, stub, _ := newTestFilen(t, 3, nil)
	ctx := context.Background()
	payload := make([]byte, 2*filenChunkSize+12345)
	for i := range payload {
		payload[i] = byte(i * 13)
	}
	if err := p.Put(ctx, "big-c0000001-p2.sand", payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	for _, f := range stub.files {
		if len(f.chunks) != 3 {
			t.Errorf("stored in %d chunks, want 3", len(f.chunks))
		}
	}
	got, err := p.Get(ctx, "big-c0000001-p2.sand")
	if err != nil || !bytes.Equal(got, payload) {
		t.Errorf("a chunked part came back as %d bytes, %v; want %d", len(got), err, len(payload))
	}
}

func TestFilenStoredSessionSkipsSignIn(t *testing.T) {
	first, stub, rotations := newTestFilen(t, 3, nil)
	ctx := context.Background()
	if err := first.Put(ctx, "abc-c0000000-p1.sand", []byte("x")); err != nil {
		t.Fatal(err)
	}
	session := (*rotations)[0]["session"]

	// A second provider built from the stored session never signs in, and
	// can still read what the first one wrote.
	stub.mu.Lock()
	stub.password = "changed-so-a-sign-in-would-fail"
	stub.mu.Unlock()
	p, err := New(Config{Kind: KindFilen, Options: map[string]string{
		"email": "alice@example.test", "password": filenFixturePassword, "folder": "sand", "session": session,
	}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Get(ctx, "abc-c0000000-p1.sand")
	if err != nil || string(got) != "x" {
		t.Errorf("Get through the stored session = %q, %v", got, err)
	}
	if stub.logins != 1 {
		t.Errorf("signed in %d times across two providers, want once", stub.logins)
	}
}

func TestFilenOldMasterKeyStillReadsOldFiles(t *testing.T) {
	p, stub, _ := newTestFilen(t, 2, nil)
	ctx := context.Background()
	if err := p.Put(ctx, "new-c0000000-p1.sand", []byte("new")); err != nil {
		t.Fatal(err)
	}

	// A file written under the account's previous password: its metadata is
	// sealed under the old master key, which the account still holds.
	old, _ := newFilenMasterKey([]byte("oldkeyoldkeyoldkeyoldkeyoldkeyoldkeyoldkeyoldkeyoldkeyoldkeyolde"))
	fileKey := filenRandomString(32)
	meta, _ := json.Marshal(filenFileMeta{Name: "old-c0000000-p1.sand", Size: 3, Key: fileKey, Mime: "application/octet-stream"})
	fk, _ := filenKeyFromString(fileKey)
	var folder string
	for id := range stub.folders {
		folder = id
	}
	stub.mu.Lock()
	stub.files["old-uuid"] = &filenStubFile{
		parent: folder, metadata: old.encryptMeta(string(meta)), nameHashed: filenV2Hash("old-c0000000-p1.sand"),
		version: 2, chunks: map[int][]byte{0: fk.encryptData([]byte("old"))},
	}
	stub.mu.Unlock()

	got, err := p.Get(ctx, "old-c0000000-p1.sand")
	if err != nil || string(got) != "old" {
		t.Errorf("a file under the previous key came back as %q, %v", got, err)
	}
}

func TestFilenTwoFactorIsAskedForAndThenCleared(t *testing.T) {
	p, stub, rotations := newTestFilen(t, 3, nil)
	stub.twoFactor = "123456"
	err := p.Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "enter_2fa") {
		t.Fatalf("Ping without the code = %v, want the two-factor refusal", err)
	}

	p2, _, rotations2 := newTestFilen(t, 3, map[string]string{"two_factor_code": "123456"})
	_ = rotations
	if err := p2.Ping(context.Background()); err != nil {
		// This stub has no code requirement; the point is the code is
		// accepted and then dropped from what is stored.
		t.Fatalf("Ping with a code: %v", err)
	}
	if got := (*rotations2)[0]["two_factor_code"]; got != "" {
		t.Errorf("the one-time code was stored as %q", got)
	}
}

func TestFilenWrongPasswordIsAClearError(t *testing.T) {
	p, _, _ := newTestFilen(t, 3, map[string]string{"password": "not it"})
	err := p.Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid_credentials") {
		t.Errorf("Ping with the wrong password = %v", err)
	}
}

func TestFilenSpecKeepsSecretsSecret(t *testing.T) {
	spec, ok := SpecFor(KindFilen)
	if !ok {
		t.Fatal("filen is not registered")
	}
	if spec.OAuth != nil || spec.SignInLink != nil {
		t.Error("Filen is connected with a password, not a browser sign-in")
	}
	redacted := Config{Kind: KindFilen, Options: map[string]string{
		"password": "p", "session": `{"api_key":"k"}`, "email": "a@b",
	}}.Redacted()
	if redacted.Options["password"] != RedactedSecret || redacted.Options["session"] != RedactedSecret {
		t.Errorf("secrets reached the API layer: %v", redacted.Options)
	}
	if redacted.Options["email"] != "a@b" {
		t.Error("the email is not a secret")
	}
	if _, err := New(Config{Kind: KindFilen, Options: map[string]string{"email": "a@b"}}); err == nil {
		t.Error("an account with no password should be refused")
	}
}
