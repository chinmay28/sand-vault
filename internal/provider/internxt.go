package provider

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ripemd160"
)

func init() {
	Register(Spec{
		Kind:  KindInternxt,
		Label: "Internxt Drive",
		Description: "An Internxt Drive account, reached through Internxt's own API. Internxt encrypts " +
			"everything end to end, so SAND signs in with your email and password, unlocks the " +
			"account's recovery phrase on this machine, and encrypts each part under a key derived " +
			"from it. Parts go in one folder.",
		DocsURL: "https://internxt.com/drive",
		Order:   24,
		Fields: []FieldSpec{
			{Key: "email", Label: "Email", Required: true},
			{
				Key:      "password",
				Label:    "Password",
				Secret:   true,
				Required: true,
				Help:     "Used on this machine to unlock the account's keys; Internxt only ever sees a hash derived from it.",
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
				Help:    "A folder at the top of the drive. SAND creates it if it is missing; blank means the root.",
			},
			{
				Key:      "session",
				Label:    "Session",
				Secret:   true,
				Advanced: true,
				Help:     "Filled in by SAND after the first sign-in. Clear it to sign in again.",
			},
		},
	}, newInternxtProvider)
}

// internxtAPI fronts both halves of Internxt: the drive API that keeps the
// folders and file records, and the network API that stores the bytes. A
// variable so tests can point the backend at a stub.
var internxtAPI = "https://gateway.internxt.com"

// internxtAppSecret is the fixed key Internxt's clients use to wrap the
// password hash on its way to the server. It is in every client, so it is
// not a secret in any useful sense — it is part of the wire format.
const internxtAppSecret = "6KYQBP847D4ATSFA"

// internxtProvider stores each shard as a file in a single Internxt folder.
//
// Internxt is end-to-end encrypted, but the shape differs from Filen's. The
// account's secret is a recovery phrase — a BIP-39 mnemonic — and every file
// key is derived from it, the account's bucket, and a random index stored
// beside the file, so nothing about a file has to be kept but the index the
// server already holds. File names, on the other hand, are stored in the
// clear as "plainName" and "type", which is why a listing here needs no
// decryption at all.
type internxtProvider struct {
	base
	email      string
	password   string
	twoFactor  string
	folderName string

	mu      sync.Mutex
	sess    *internxtSession
	folder  string
	entries map[string]internxtFile
	rotate  func(map[string]string)
}

// internxtFile is what SAND keeps about one stored file between listings.
type internxtFile struct {
	uuid   string // the drive record, which deletes and renames address
	fileID string // the network object, which downloads address
	size   int64
}

// internxtSession is what a signed-in account needs to work without the
// password. It is what the "session" option stores, encrypted in the vault.
type internxtSession struct {
	Token      string `json:"token"`
	Mnemonic   string `json:"mnemonic"`
	Bucket     string `json:"bucket"`
	RootFolder string `json:"root_folder"`
	BridgeUser string `json:"bridge_user"`
	UserID     string `json:"user_id"`
}

func (s *internxtSession) String() string {
	out, _ := json.Marshal(s)
	return string(out)
}

func (s *internxtSession) complete() bool {
	return s.Token != "" && s.Mnemonic != "" && s.Bucket != "" &&
		s.RootFolder != "" && s.BridgeUser != "" && s.UserID != ""
}

// networkAuth is the Basic credential the network API wants: the bridge user
// and a hash of the user ID.
func (s *internxtSession) networkAuth() string {
	sum := sha256.Sum256([]byte(s.UserID))
	creds := s.BridgeUser + ":" + hex.EncodeToString(sum[:])
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(creds))
}

func newInternxtProvider(cfg Config) (Provider, error) {
	p := &internxtProvider{
		base:       base{cfg: cfg},
		email:      strings.TrimSpace(cfg.Option("email")),
		password:   cfg.Option("password"),
		twoFactor:  strings.TrimSpace(cfg.Option("two_factor_code")),
		folderName: strings.Trim(strings.TrimSpace(cfg.Option("folder")), "/"),
		entries:    map[string]internxtFile{},
	}
	if raw := strings.TrimSpace(cfg.Option("session")); raw != "" {
		var sess internxtSession
		if err := json.Unmarshal([]byte(raw), &sess); err != nil || !sess.complete() {
			return nil, fmt.Errorf("internxt: the stored session is unreadable, clear it to sign in again")
		}
		p.sess = &sess
	}
	return p, nil
}

// OnCredentialChange registers where the session is written. Internxt
// rotates its token on every refresh, so the sink is used for more than the
// first sign-in.
func (p *internxtProvider) OnCredentialChange(fn func(map[string]string)) {
	p.mu.Lock()
	p.rotate = fn
	p.mu.Unlock()
}

// --- Crypto ----------------------------------------------------------------

// internxtEncryptText is the OpenSSL-style AES-256-CBC Internxt's clients use
// for small strings: "Salted__", eight bytes of salt, then the ciphertext,
// the whole thing hex-encoded, with the key and IV stretched from the secret
// by MD5.
func internxtEncryptText(plain, secret string) (string, error) {
	salt := make([]byte, 8)
	rand.Read(salt)
	key, iv := evpBytesToKey([]byte(secret), salt)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append([]byte(plain), bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, 16+len(padded))
	copy(out, "Salted__")
	copy(out[8:], salt)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out[16:], padded)
	return hex.EncodeToString(out), nil
}

func internxtDecryptText(encHex, secret string) (string, error) {
	raw, err := hex.DecodeString(encHex)
	if err != nil {
		return "", fmt.Errorf("not hex: %w", err)
	}
	if len(raw) < 32 || string(raw[:8]) != "Salted__" || len(raw[16:])%aes.BlockSize != 0 {
		return "", fmt.Errorf("not in the Salted__ format")
	}
	key, iv := evpBytesToKey([]byte(secret), raw[8:16])
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	plain := make([]byte, len(raw)-16)
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, raw[16:])
	pad := int(plain[len(plain)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(plain) {
		return "", fmt.Errorf("bad padding")
	}
	for _, b := range plain[len(plain)-pad:] {
		if int(b) != pad {
			return "", fmt.Errorf("bad padding")
		}
	}
	return string(plain[:len(plain)-pad]), nil
}

// internxtPasswordHash is what Internxt is shown at sign-in in place of the
// password: a PBKDF2 hash of it, wrapped under the app secret.
func internxtPasswordHash(password, wrappedSalt string) (string, error) {
	saltHex, err := internxtDecryptText(wrappedSalt, internxtAppSecret)
	if err != nil {
		return "", fmt.Errorf("unwrapping the sign-in salt: %w", err)
	}
	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		return "", fmt.Errorf("sign-in salt is not hex: %w", err)
	}
	hash, err := pbkdf2.Key(sha1.New, password, salt, 10000, 32)
	if err != nil {
		return "", err
	}
	return internxtEncryptText(hex.EncodeToString(hash), internxtAppSecret)
}

// internxtFileKey derives the AES-256-CTR key and IV for one file from the
// account's recovery phrase, its bucket, and the file's index.
func internxtFileKey(mnemonic, bucketHex, indexHex string) (key, iv []byte, err error) {
	bucket, err := hex.DecodeString(bucketHex)
	if err != nil {
		return nil, nil, fmt.Errorf("bucket id is not hex: %w", err)
	}
	index, err := hex.DecodeString(indexHex)
	if err != nil || len(index) < 16 {
		return nil, nil, fmt.Errorf("file index is not a hex string of at least 16 bytes")
	}
	// BIP-39: the phrase is the PBKDF2 password and "mnemonic" the salt.
	// Internxt's phrases are English words, which normalisation leaves
	// alone, so the NFKD step the standard asks for is a no-op here.
	seed, err := pbkdf2.Key(sha512.New, mnemonic, []byte("mnemonic"), 2048, 64)
	if err != nil {
		return nil, nil, err
	}
	bucketKey := sha512.Sum512(append(seed, bucket...))
	fileKey := sha512.Sum512(append(bucketKey[:32:32], index...))
	return fileKey[:32], index[:16], nil
}

// internxtShardHash is the checksum the network keeps for a stored object:
// RIPEMD-160 over SHA-256 of the encrypted bytes.
func internxtShardHash(encrypted []byte) string {
	sum := sha256.Sum256(encrypted)
	h := ripemd160.New()
	h.Write(sum[:])
	return hex.EncodeToString(h.Sum(nil))
}

// internxtCTR applies AES-256-CTR, which is its own inverse.
func internxtCTR(key, iv, data []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	cipher.NewCTR(block, iv).XORKeyStream(out, data)
	return out, nil
}

// internxtSplitName is how Internxt stores a file name: the extension in a
// field of its own.
func internxtSplitName(name string) (plain, ext string) {
	idx := strings.LastIndex(name, ".")
	if idx <= 0 {
		return name, ""
	}
	return name[:idx], name[idx+1:]
}

func internxtJoinName(plain, ext string) string {
	if ext == "" {
		return plain
	}
	return plain + "." + ext
}

// --- API -------------------------------------------------------------------

// internxtError is a request Internxt answered but refused.
type internxtError struct {
	status  int
	op      string
	message string
}

func (e *internxtError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("%s: HTTP %d", e.op, e.status)
	}
	return fmt.Sprintf("%s: %s (HTTP %d)", e.op, e.message, e.status)
}

func internxtStatus(err error) int {
	var ie *internxtError
	if errors.As(err, &ie) {
		return ie.status
	}
	return 0
}

// internxtRequest sends one request with the given Authorization value and
// decodes a JSON answer into out. Both halves of the API share the shape.
func internxtRequest(ctx context.Context, op, method, rawURL, auth string, payload, out any) error {
	var body *bytes.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	} else {
		body = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("internxt-version", "1.0")
	req.Header.Set("internxt-client", "sand-vault")
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	defer drainAndClose(resp)
	raw, err := readAllBody(resp)
	if err != nil {
		return err
	}
	if !isSuccess(resp.StatusCode) {
		var failure struct {
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		_ = json.Unmarshal(raw, &failure)
		message := failure.Message
		if message == "" {
			message = failure.Error
		}
		if message == "" {
			message = strings.TrimSpace(string(raw))
			if len(message) > 200 {
				message = message[:200]
			}
		}
		return &internxtError{status: resp.StatusCode, op: op, message: message}
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: parsing response: %w", op, err)
	}
	return nil
}

// internxtAccess is what a sign-in or a refresh answers with.
type internxtAccess struct {
	User struct {
		Mnemonic     string `json:"mnemonic"`
		RootFolderID string `json:"rootFolderId"`
		Bucket       string `json:"bucket"`
		BridgeUser   string `json:"bridgeUser"`
		UserID       string `json:"userId"`
		Email        string `json:"email"`
	} `json:"user"`
	Token    string `json:"token"`
	NewToken string `json:"newToken"`
}

// session returns the signed-in session, signing in the first time.
func (p *internxtProvider) session(ctx context.Context) (*internxtSession, error) {
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
	p.persistLocked()
	return sess, nil
}

// persistLocked writes the session back to the vault. Called with p.mu held.
func (p *internxtProvider) persistLocked() {
	if p.rotate != nil && p.sess != nil {
		p.rotate(map[string]string{"session": p.sess.String(), "two_factor_code": ""})
	}
}

func (p *internxtProvider) signIn(ctx context.Context) (*internxtSession, error) {
	var login struct {
		SKey string `json:"sKey"`
		TFA  bool   `json:"tfa"`
	}
	err := internxtRequest(ctx, "internxt sign-in", http.MethodPost, internxtAPI+"/drive/auth/login", "",
		map[string]string{"email": p.email}, &login)
	if err != nil {
		return nil, err
	}
	if login.TFA && p.twoFactor == "" {
		return nil, fmt.Errorf("internxt sign-in: this account has two-factor authentication on; " +
			"add the current code under the account's two-factor setting and connect again")
	}
	hash, err := internxtPasswordHash(p.password, login.SKey)
	if err != nil {
		return nil, fmt.Errorf("internxt sign-in: %w", err)
	}

	access, err := p.access(ctx, hash)
	if err != nil {
		return nil, err
	}
	return p.sessionFromAccess(access)
}

func (p *internxtProvider) access(ctx context.Context, hash string) (internxtAccess, error) {
	payload := map[string]string{"email": p.email, "password": hash}
	if p.twoFactor != "" {
		payload["tfa"] = p.twoFactor
	}
	var access internxtAccess
	err := internxtRequest(ctx, "internxt sign-in", http.MethodPost, internxtAPI+"/drive/auth/cli/login/access", "",
		payload, &access)
	return access, err
}

// sessionFromAccess unlocks the recovery phrase — stored on the server
// encrypted under the password — and keeps what the rest of the API needs.
func (p *internxtProvider) sessionFromAccess(access internxtAccess) (*internxtSession, error) {
	mnemonic, err := internxtDecryptText(access.User.Mnemonic, p.password)
	if err != nil {
		return nil, fmt.Errorf("internxt sign-in: the password does not unlock this account's recovery phrase: %w", err)
	}
	words := strings.Fields(mnemonic)
	if len(words) != 12 && len(words) != 24 {
		return nil, fmt.Errorf("internxt sign-in: the unlocked recovery phrase has %d words, want 12 or 24", len(words))
	}
	token := access.NewToken
	if token == "" {
		token = access.Token
	}
	sess := &internxtSession{
		Token:      token,
		Mnemonic:   strings.Join(words, " "),
		Bucket:     access.User.Bucket,
		RootFolder: access.User.RootFolderID,
		BridgeUser: access.User.BridgeUser,
		UserID:     access.User.UserID,
	}
	if !sess.complete() {
		return nil, fmt.Errorf("internxt sign-in: the account answered without a bucket, a root folder or a network user")
	}
	return sess, nil
}

// drive sends a request to the drive API with the bearer token, refreshing
// the token and retrying once when it has expired.
func (p *internxtProvider) drive(ctx context.Context, op, method, path string, payload, out any) error {
	sess, err := p.session(ctx)
	if err != nil {
		return err
	}
	err = internxtRequest(ctx, op, method, internxtAPI+"/drive"+path, "Bearer "+sess.Token, payload, out)
	if internxtStatus(err) != http.StatusUnauthorized {
		return err
	}
	if err := p.renew(ctx); err != nil {
		return err
	}
	p.mu.Lock()
	token := p.sess.Token
	p.mu.Unlock()
	return internxtRequest(ctx, op, method, internxtAPI+"/drive"+path, "Bearer "+token, payload, out)
}

// renew replaces an expired token: from the refresh endpoint if it still
// accepts the old one, and by signing in again with the password if not.
func (p *internxtProvider) renew(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var access internxtAccess
	err := internxtRequest(ctx, "internxt refresh", http.MethodGet, internxtAPI+"/drive/users/cli/refresh",
		"Bearer "+p.sess.Token, nil, &access)
	switch {
	case err == nil && access.NewToken != "":
		p.sess.Token = access.NewToken
	case internxtStatus(err) == http.StatusUnauthorized || err == nil:
		// The two-factor code that let the first sign-in through is long
		// spent, so this works only on an account without one.
		saved := p.twoFactor
		p.twoFactor = ""
		fresh, signErr := p.signIn(ctx)
		p.twoFactor = saved
		if signErr != nil {
			return fmt.Errorf("internxt: the session has expired and signing in again failed: %w", signErr)
		}
		p.sess = fresh
	default:
		return err
	}
	p.persistLocked()
	return nil
}

// network sends a request to the network API, which authenticates with the
// bridge credential rather than the token and so never needs renewing.
func (p *internxtProvider) network(ctx context.Context, op, method, path string, payload, out any) error {
	sess, err := p.session(ctx)
	if err != nil {
		return err
	}
	return internxtRequest(ctx, op, method, internxtAPI+"/network"+path, sess.networkAuth(), payload, out)
}

// --- Folder and listing ----------------------------------------------------

// internxtItem is a file or folder as the drive API lists it.
type internxtItem struct {
	UUID      string      `json:"uuid"`
	FileID    string      `json:"fileId"`
	PlainName string      `json:"plainName"`
	Type      string      `json:"type"`
	Size      json.Number `json:"size"`
	Status    string      `json:"status"`
}

// listContent pages through everything of one kind — "files" or "folders"
// — directly inside a folder.
func (p *internxtProvider) listContent(ctx context.Context, folder, kind string) ([]internxtItem, error) {
	const pageSize = 50
	var out []internxtItem
	for offset := 0; ; offset += pageSize {
		params := url.Values{
			"offset": {strconv.Itoa(offset)},
			"limit":  {strconv.Itoa(pageSize)},
			"sort":   {"plainName"},
			"order":  {"ASC"},
		}
		var page map[string][]internxtItem
		err := p.drive(ctx, "internxt list", http.MethodGet,
			"/folders/content/"+url.PathEscape(folder)+"/"+kind+"?"+params.Encode(), nil, &page)
		if err != nil {
			return nil, err
		}
		items := page[kind]
		for _, item := range items {
			if item.Status == "" || item.Status == "EXISTS" {
				out = append(out, item)
			}
		}
		if len(items) < pageSize {
			return out, nil
		}
	}
}

// shardFolder resolves — creating if necessary — the folder shards live in.
func (p *internxtProvider) shardFolder(ctx context.Context) (string, error) {
	sess, err := p.session(ctx)
	if err != nil {
		return "", err
	}
	if p.folderName == "" {
		return sess.RootFolder, nil
	}
	p.mu.Lock()
	cached := p.folder
	p.mu.Unlock()
	if cached != "" {
		return cached, nil
	}

	folders, err := p.listContent(ctx, sess.RootFolder, "folders")
	if err != nil {
		return "", err
	}
	for _, f := range folders {
		if f.PlainName == p.folderName {
			p.mu.Lock()
			p.folder = f.UUID
			p.mu.Unlock()
			return f.UUID, nil
		}
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	var created struct {
		UUID string `json:"uuid"`
	}
	err = p.drive(ctx, "internxt create folder", http.MethodPost, "/folders", map[string]string{
		"plainName":        p.folderName,
		"parentFolderUuid": sess.RootFolder,
		"creationTime":     now,
		"modificationTime": now,
	}, &created)
	if err != nil {
		return "", err
	}
	if created.UUID == "" {
		return "", fmt.Errorf("internxt create folder: no UUID in the response")
	}
	p.mu.Lock()
	p.folder = created.UUID
	p.mu.Unlock()
	return created.UUID, nil
}

// refresh lists the shard folder and replaces what is known about the files
// in it, answering with the entry for key if there is one.
func (p *internxtProvider) refresh(ctx context.Context, key string) (internxtFile, bool, error) {
	folder, err := p.shardFolder(ctx)
	if err != nil {
		return internxtFile{}, false, err
	}
	files, err := p.listContent(ctx, folder, "files")
	if err != nil {
		return internxtFile{}, false, err
	}
	entries := make(map[string]internxtFile, len(files))
	for _, f := range files {
		size, _ := f.Size.Int64()
		entries[internxtJoinName(f.PlainName, f.Type)] = internxtFile{uuid: f.UUID, fileID: f.FileID, size: size}
	}
	p.mu.Lock()
	p.entries = entries
	entry, ok := entries[key]
	p.mu.Unlock()
	return entry, ok, nil
}

func (p *internxtProvider) entryFor(ctx context.Context, key string) (internxtFile, error) {
	p.mu.Lock()
	entry, ok := p.entries[key]
	p.mu.Unlock()
	if ok {
		return entry, nil
	}
	entry, ok, err := p.refresh(ctx, key)
	if err != nil {
		return internxtFile{}, err
	}
	if !ok {
		return internxtFile{}, ErrNotFound
	}
	return entry, nil
}

func (p *internxtProvider) forget(key string) {
	p.mu.Lock()
	delete(p.entries, key)
	p.mu.Unlock()
}

// --- The six methods -------------------------------------------------------

// Put encrypts the shard under a key derived from the recovery phrase and a
// fresh index, stores the bytes on the network, then records the file in the
// drive. Internxt refuses two files of one name in a folder, so a file
// already under the name is renamed aside first and erased once the new
// record is in — or renamed back if it is not.
func (p *internxtProvider) Put(ctx context.Context, key string, data []byte) error {
	sess, err := p.session(ctx)
	if err != nil {
		return err
	}
	folder, err := p.shardFolder(ctx)
	if err != nil {
		return err
	}
	previous, err := p.entryFor(ctx, key)
	if err != nil && err != ErrNotFound {
		return err
	}
	plain, ext := internxtSplitName(key)

	var fileID string
	if len(data) > 0 {
		fileID, err = p.store(ctx, sess, data)
		if err != nil {
			return err
		}
	}

	if previous.uuid != "" {
		aside := plain + ".replaced-" + filenRandomString(8)
		err := p.drive(ctx, "internxt rename", http.MethodPut, "/files/"+url.PathEscape(previous.uuid)+"/meta",
			map[string]string{"plainName": aside, "type": ext}, nil)
		if err != nil {
			return fmt.Errorf("internxt upload: setting the old copy aside: %w", err)
		}
	}

	now := time.Now().UTC()
	record := map[string]any{
		"name":             plain,
		"plainName":        plain,
		"type":             ext,
		"bucket":           sess.Bucket,
		"encryptVersion":   "03-aes",
		"folderUuid":       folder,
		"size":             len(data),
		"creationTime":     now,
		"date":             now,
		"modificationTime": now,
	}
	if fileID != "" {
		record["fileId"] = fileID
	}
	var created struct {
		UUID   string `json:"uuid"`
		FileID string `json:"fileId"`
	}
	if err := p.drive(ctx, "internxt upload", http.MethodPost, "/files", record, &created); err != nil {
		if previous.uuid != "" {
			// Best effort: the upload has failed already, and the old
			// part is better under its own name than under the aside one.
			_ = p.drive(ctx, "internxt rename", http.MethodPut, "/files/"+url.PathEscape(previous.uuid)+"/meta",
				map[string]string{"plainName": plain, "type": ext}, nil)
		}
		return err
	}
	if created.UUID == "" {
		return fmt.Errorf("internxt upload: no UUID in the response")
	}
	p.mu.Lock()
	p.entries[key] = internxtFile{uuid: created.UUID, fileID: fileID, size: int64(len(data))}
	p.mu.Unlock()

	if previous.uuid != "" {
		if err := p.deleteRecord(ctx, previous.uuid); err != nil {
			return fmt.Errorf("internxt upload: erasing the old copy: %w", err)
		}
	}
	return nil
}

// store encrypts and uploads one object to the network, answering with the
// network's ID for it.
func (p *internxtProvider) store(ctx context.Context, sess *internxtSession, data []byte) (string, error) {
	index := make([]byte, 32)
	rand.Read(index)
	indexHex := hex.EncodeToString(index)
	key, iv, err := internxtFileKey(sess.Mnemonic, sess.Bucket, indexHex)
	if err != nil {
		return "", err
	}
	sealed, err := internxtCTR(key, iv, data)
	if err != nil {
		return "", err
	}

	var started struct {
		Uploads []struct {
			UUID string   `json:"uuid"`
			URL  string   `json:"url"`
			URLs []string `json:"urls"`
		} `json:"uploads"`
	}
	err = p.network(ctx, "internxt upload", http.MethodPost,
		"/v2/buckets/"+url.PathEscape(sess.Bucket)+"/files/start?multiparts=1",
		map[string]any{"uploads": []map[string]any{{"index": 0, "size": len(sealed)}}}, &started)
	if err != nil {
		return "", err
	}
	if len(started.Uploads) == 0 {
		return "", fmt.Errorf("internxt upload: the network offered nowhere to put the bytes")
	}
	target := started.Uploads[0].URL
	if len(started.Uploads[0].URLs) > 0 {
		target = started.Uploads[0].URLs[0]
	}

	// The upload address is pre-signed and lives on the object store behind
	// Internxt, which knows nothing of the account's credentials.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(sealed))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(sealed))
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("internxt upload: %w", err)
	}
	drainAndClose(resp)
	if !isSuccess(resp.StatusCode) {
		return "", fmt.Errorf("internxt upload: the object store answered %s", resp.Status)
	}

	var finished struct {
		ID string `json:"id"`
	}
	err = p.network(ctx, "internxt upload", http.MethodPost,
		"/v2/buckets/"+url.PathEscape(sess.Bucket)+"/files/finish",
		map[string]any{
			"index":  indexHex,
			"shards": []map[string]string{{"hash": internxtShardHash(sealed), "uuid": started.Uploads[0].UUID}},
		}, &finished)
	if err != nil {
		return "", err
	}
	if finished.ID == "" {
		return "", fmt.Errorf("internxt upload: the network did not name the stored object")
	}
	return finished.ID, nil
}

func (p *internxtProvider) Get(ctx context.Context, key string) ([]byte, error) {
	sess, err := p.session(ctx)
	if err != nil {
		return nil, err
	}
	entry, err := p.entryFor(ctx, key)
	if err != nil {
		return nil, err
	}
	if entry.size == 0 || entry.fileID == "" {
		return []byte{}, nil
	}

	var info struct {
		Index  string `json:"index"`
		Size   int64  `json:"size"`
		Shards []struct {
			Hash string `json:"hash"`
			URL  string `json:"url"`
		} `json:"shards"`
	}
	err = p.network(ctx, "internxt download", http.MethodGet,
		"/buckets/"+url.PathEscape(sess.Bucket)+"/files/"+url.PathEscape(entry.fileID)+"/info", nil, &info)
	if internxtStatus(err) == http.StatusNotFound {
		p.forget(key)
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(info.Shards) == 0 {
		return nil, fmt.Errorf("internxt download: the network holds no bytes for %s", key)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.Shards[0].URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("internxt download: %w", err)
	}
	defer drainAndClose(resp)
	if resp.StatusCode == http.StatusNotFound {
		p.forget(key)
		return nil, ErrNotFound
	}
	if !isSuccess(resp.StatusCode) {
		return nil, httpError("internxt download", resp)
	}
	sealed, err := readAllBody(resp)
	if err != nil {
		return nil, err
	}
	if got := internxtShardHash(sealed); got != info.Shards[0].Hash {
		return nil, fmt.Errorf("internxt download: %s came back with checksum %s, the network recorded %s", key, got, info.Shards[0].Hash)
	}
	fileKey, iv, err := internxtFileKey(sess.Mnemonic, sess.Bucket, info.Index)
	if err != nil {
		return nil, err
	}
	plain, err := internxtCTR(fileKey, iv, sealed)
	if err != nil {
		return nil, err
	}
	if int64(len(plain)) != entry.size {
		return nil, fmt.Errorf("internxt download: %s came back as %d bytes, its record says %d", key, len(plain), entry.size)
	}
	return plain, nil
}

// Stat answers from a fresh listing, since it is what the health check asks.
func (p *internxtProvider) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	entry, found, err := p.refresh(ctx, key)
	if err != nil {
		return ObjectInfo{}, err
	}
	if !found {
		return ObjectInfo{}, ErrNotFound
	}
	return ObjectInfo{Key: key, Size: entry.size}, nil
}

func (p *internxtProvider) Delete(ctx context.Context, key string) error {
	entry, err := p.entryFor(ctx, key)
	if err == ErrNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	p.forget(key)
	return p.deleteRecord(ctx, entry.uuid)
}

// deleteRecord removes a file from the drive. Internxt's delete is final —
// there is no trash step to take afterwards.
func (p *internxtProvider) deleteRecord(ctx context.Context, fileUUID string) error {
	err := p.drive(ctx, "internxt delete", http.MethodDelete, "/files/"+url.PathEscape(fileUUID), nil, nil)
	if internxtStatus(err) == http.StatusNotFound {
		return nil
	}
	return err
}

func (p *internxtProvider) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
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

func (p *internxtProvider) Ping(ctx context.Context) error {
	if _, err := p.Usage(ctx); err != nil {
		return fmt.Errorf("cannot reach Internxt: %w", err)
	}
	return nil
}

func (p *internxtProvider) Usage(ctx context.Context) (Usage, error) {
	var used struct {
		Drive int64 `json:"drive"`
	}
	if err := p.drive(ctx, "internxt usage", http.MethodGet, "/users/usage", nil, &used); err != nil {
		return Usage{}, err
	}
	var limit struct {
		MaxSpaceBytes int64 `json:"maxSpaceBytes"`
	}
	if err := p.drive(ctx, "internxt usage", http.MethodGet, "/users/limit", nil, &limit); err != nil {
		return Usage{}, err
	}
	return Usage{Used: used.Drive, Total: limit.MaxSpaceBytes}, nil
}

// Account reports the signed-in email, used to label the connection.
func (p *internxtProvider) Account(ctx context.Context) (string, error) {
	return p.email, nil
}
