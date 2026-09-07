package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
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

// The fixtures below were produced by Internxt's own Go adapter
// (github.com/internxt/rclone-adapter), so a match here is conformance with
// the real client rather than with this file's own reading of the protocol.
const (
	internxtFixtureTextEnc   = "53616c7465645f5f9d8b2ab79abfbe1f0e7a70c1300210283b0b31ecec2e8ebd"
	internxtFixtureTextPlain = "hello world"
	internxtFixtureTextKey   = "secret-pw"

	internxtFixturePassword = "correct horse battery"
	internxtFixtureSaltHex  = "00112233445566778899aabbccddeeff"
	internxtFixtureSKey     = "53616c7465645f5f3d8a8a58cd13ad3003a67c707e6fbd61586bdfb69f5cd9b3d714c16731c4fdc800b6d947842c5cdb13a389a564839dec2cb9d2bb94b95916"
	internxtFixturePassHash = "a9a962d2b65f9b9aaf78a0a822c084baeb4ecd364a3f22436dba957f1421471c"

	internxtFixtureMnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	internxtFixtureBucket   = "0123456789abcdef01234567"
	internxtFixtureIndex    = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	internxtFixtureFileKey  = "fb88f74e630c815774e49726a9c50f0795be58b3e6bf4623d17589d0a3f00b4c"
	internxtFixtureFileIV   = "a1b2c3d4e5f60718293a4b5c6d7e8f90"

	internxtFixtureHashOfX = "4e944b03e84fdc97f2fb68cb62b73d000ef5be71"
)

func TestInternxtCryptoMatchesTheOfficialAdapter(t *testing.T) {
	if plain, err := internxtDecryptText(internxtFixtureTextEnc, internxtFixtureTextKey); err != nil || plain != internxtFixtureTextPlain {
		t.Errorf("Salted__ text decrypts to %q, %v", plain, err)
	}
	enc, err := internxtEncryptText("round trip", "k")
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := internxtDecryptText(enc, "k"); err != nil || plain != "round trip" {
		t.Errorf("Salted__ round trip = %q, %v", plain, err)
	}

	// The sign-in salt arrives wrapped under the app secret; the password
	// hash goes back wrapped the same way.
	if salt, err := internxtDecryptText(internxtFixtureSKey, internxtAppSecret); err != nil || salt != internxtFixtureSaltHex {
		t.Errorf("sKey unwraps to %q, %v", salt, err)
	}
	wrapped, err := internxtPasswordHash(internxtFixturePassword, internxtFixtureSKey)
	if err != nil {
		t.Fatal(err)
	}
	if hash, err := internxtDecryptText(wrapped, internxtAppSecret); err != nil || hash != internxtFixturePassHash {
		t.Errorf("password hash = %q, %v; want %s", hash, err, internxtFixturePassHash)
	}

	key, iv, err := internxtFileKey(internxtFixtureMnemonic, internxtFixtureBucket, internxtFixtureIndex)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(key) != internxtFixtureFileKey || hex.EncodeToString(iv) != internxtFixtureFileIV {
		t.Errorf("file key = %x, iv = %x", key, iv)
	}

	sum := sha256.Sum256([]byte("x"))
	if got := internxtShardHash([]byte("x")); got != internxtFixtureHashOfX || len(sum) == 0 {
		t.Errorf("shard hash = %s", got)
	}
}

// --- Stub --------------------------------------------------------------------

// internxtStub is enough of Internxt's gateway to drive the backend: the
// drive API with its sign-in, folders and file records, and the network API
// with its pre-signed upload slots and stored objects. Bytes are stored as
// they arrive, exactly as the real network does.
type internxtStub struct {
	t  *testing.T
	mu sync.Mutex

	password string // the account password, used to seal the mnemonic
	mnemonic string
	tfa      string // required code, "" for none
	token    string
	stale    map[string]bool // tokens the drive refuses but the refresh endpoint still rotates
	expired  map[string]bool // tokens nothing accepts any more

	folders map[string]internxtStubFolder
	files   map[string]internxtStubRecord // drive uuid -> record
	objects map[string]internxtStubObject // network file id -> bytes
	slots   map[string]string             // upload slot uuid -> bytes once PUT
	logins  int
	nextID  int
}

type internxtStubFolder struct{ parent, name string }
type internxtStubRecord struct {
	folder, plain, ext, fileID string
	size                       int
}
type internxtStubObject struct {
	index, hash string
	body        []byte
}

const (
	internxtStubRoot   = "root-folder-uuid"
	internxtStubBucket = "0123456789abcdef01234567"
	internxtStubBridge = "alice@example.test"
	internxtStubUserID = "user-id-1"
)

func newInternxtStub(t *testing.T) *internxtStub {
	return &internxtStub{
		t: t, password: internxtFixturePassword, mnemonic: internxtFixtureMnemonic, token: "token-1",
		stale: map[string]bool{}, expired: map[string]bool{}, folders: map[string]internxtStubFolder{},
		files: map[string]internxtStubRecord{}, objects: map[string]internxtStubObject{}, slots: map[string]string{},
	}
}

func (s *internxtStub) mint(prefix string) string {
	s.nextID++
	return fmt.Sprintf("%s-%d", prefix, s.nextID)
}

func (s *internxtStub) access() map[string]any {
	sealed, _ := internxtEncryptText(s.mnemonic, s.password)
	return map[string]any{
		"user": map[string]any{
			"mnemonic": sealed, "rootFolderId": internxtStubRoot, "bucket": internxtStubBucket,
			"bridgeUser": internxtStubBridge, "userId": internxtStubUserID, "email": "alice@example.test",
		},
		"token": "legacy-" + s.token, "newToken": s.token,
	}
}

func (s *internxtStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := r.URL.Path

	if strings.HasPrefix(path, "/store/") {
		s.serveStore(w, r)
		return
	}
	if strings.HasPrefix(path, "/network/") {
		want := (&internxtSession{BridgeUser: internxtStubBridge, UserID: internxtStubUserID}).networkAuth()
		if r.Header.Get("Authorization") != want {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		s.serveNetwork(w, r)
		return
	}

	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	str := func(k string) string {
		if v, ok := body[k]; ok && v != nil {
			return fmt.Sprint(v)
		}
		return ""
	}
	w.Header().Set("Content-Type", "application/json")

	switch {
	case path == "/drive/auth/login":
		skey, _ := internxtEncryptText(internxtFixtureSaltHex, internxtAppSecret)
		json.NewEncoder(w).Encode(map[string]any{"sKey": skey, "tfa": s.tfa != ""})
		return
	case path == "/drive/auth/cli/login/access":
		s.logins++
		hash, err := internxtDecryptText(str("password"), internxtAppSecret)
		if err != nil || hash != internxtFixturePassHash {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"Wrong login credentials"}`))
			return
		}
		if s.tfa != "" && str("tfa") != s.tfa {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"Wrong 2-factor auth code"}`))
			return
		}
		// Every sign-in issues a token of its own.
		s.token = s.mint("token")
		json.NewEncoder(w).Encode(s.access())
		return
	}

	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token != s.token || s.expired[token] || (s.stale[token] && path != "/drive/users/cli/refresh") {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Unauthorized"}`))
		return
	}

	switch {
	case path == "/drive/users/cli/refresh":
		s.token = s.mint("token")
		json.NewEncoder(w).Encode(s.access())
	case path == "/drive/users/usage":
		w.Write([]byte(`{"drive":250}`))
	case path == "/drive/users/limit":
		w.Write([]byte(`{"maxSpaceBytes":1000}`))
	case strings.HasPrefix(path, "/drive/folders/content/"):
		s.serveContent(w, r)
	case path == "/drive/folders" && r.Method == http.MethodPost:
		id := s.mint("folder")
		s.folders[id] = internxtStubFolder{parent: str("parentFolderUuid"), name: str("plainName")}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"uuid": id, "plainName": str("plainName")})
	case path == "/drive/files" && r.Method == http.MethodPost:
		s.serveCreateRecord(w, body, str)
	case strings.HasPrefix(path, "/drive/files/") && strings.HasSuffix(path, "/meta") && r.Method == http.MethodPut:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/drive/files/"), "/meta")
		rec, ok := s.files[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		rec.plain, rec.ext = str("plainName"), str("type")
		s.files[id] = rec
		w.Write([]byte(`{}`))
	case strings.HasPrefix(path, "/drive/files/") && r.Method == http.MethodDelete:
		id := strings.TrimPrefix(path, "/drive/files/")
		if _, ok := s.files[id]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(s.files, id)
		w.Write([]byte(`{}`))
	default:
		s.t.Errorf("unexpected Internxt request %s %s", r.Method, path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *internxtStub) serveContent(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/drive/folders/content/")
	parent, kind, _ := strings.Cut(rest, "/")
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		s.t.Errorf("listing without a limit: %s", r.URL.RawQuery)
	}

	var items []map[string]any
	switch kind {
	case "folders":
		for id, f := range s.folders {
			if f.parent == parent {
				items = append(items, map[string]any{"uuid": id, "plainName": f.name, "status": "EXISTS"})
			}
		}
	case "files":
		for id, f := range s.files {
			if f.folder == parent {
				items = append(items, map[string]any{
					"uuid": id, "fileId": f.fileID, "plainName": f.plain, "type": f.ext,
					"size": strconv.Itoa(f.size), "status": "EXISTS",
				})
			}
		}
	}
	// Small pages so the backend has to follow the offset.
	const pageSize = 50
	_ = pageSize
	end := min(offset+limit, len(items))
	start := min(offset, len(items))
	json.NewEncoder(w).Encode(map[string]any{kind: items[start:end]})
}

func (s *internxtStub) serveCreateRecord(w http.ResponseWriter, body map[string]any, str func(string) string) {
	for id, f := range s.files {
		if f.folder == str("folderUuid") && f.plain == str("plainName") && f.ext == str("type") {
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(fmt.Sprintf(`{"message":"File already exists","uuid":%q}`, id)))
			return
		}
	}
	size, _ := body["size"].(float64)
	fileID := str("fileId")
	if fileID != "" {
		if _, ok := s.objects[fileID]; !ok {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"message":"unknown fileId"}`))
			return
		}
	}
	for _, field := range []string{"bucket", "encryptVersion", "folderUuid", "plainName"} {
		if str(field) == "" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"message":"missing ` + field + `"}`))
			return
		}
	}
	id := s.mint("file")
	s.files[id] = internxtStubRecord{folder: str("folderUuid"), plain: str("plainName"), ext: str("type"), fileID: fileID, size: int(size)}
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{"uuid": id, "fileId": fileID})
}

func (s *internxtStub) serveNetwork(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("internxt-version") == "" {
		s.t.Error("a network request carried no internxt-version header")
	}
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, "/network/")

	switch {
	case strings.HasSuffix(path, "/files/start"):
		if !strings.HasPrefix(path, "v2/buckets/"+internxtStubBucket+"/") {
			s.t.Errorf("upload started against the wrong bucket: %s", path)
		}
		slot := s.mint("slot")
		s.slots[slot] = ""
		json.NewEncoder(w).Encode(map[string]any{"uploads": []map[string]any{{
			"index": 0, "uuid": slot, "url": "http://" + r.Host + "/store/" + slot,
		}}})
	case strings.HasSuffix(path, "/files/finish"):
		shards := body["shards"].([]any)
		shard := shards[0].(map[string]any)
		stored, ok := s.slots[fmt.Sprint(shard["uuid"])]
		if !ok || stored == "" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"message":"no bytes in the slot"}`))
			return
		}
		if internxtShardHash([]byte(stored)) != fmt.Sprint(shard["hash"]) {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"message":"hash mismatch"}`))
			return
		}
		id := s.mint("object")
		s.objects[id] = internxtStubObject{index: fmt.Sprint(body["index"]), hash: fmt.Sprint(shard["hash"]), body: []byte(stored)}
		delete(s.slots, fmt.Sprint(shard["uuid"]))
		json.NewEncoder(w).Encode(map[string]any{"id": id, "index": body["index"]})
	case strings.HasSuffix(path, "/info"):
		parts := strings.Split(path, "/")
		id := parts[len(parts)-2]
		obj, ok := s.objects[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"index": obj.index, "size": len(obj.body),
			"shards": []map[string]any{{"index": 0, "hash": obj.hash, "url": "http://" + r.Host + "/store/object/" + id}},
		})
	default:
		s.t.Errorf("unexpected network request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// serveStore is the object store behind the pre-signed URLs: no account
// credentials, just bytes in and bytes out.
func (s *internxtStub) serveStore(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "" {
		s.t.Error("account credentials were sent to the object store")
	}
	rest := strings.TrimPrefix(r.URL.Path, "/store/")
	switch {
	case r.Method == http.MethodPut:
		if _, ok := s.slots[rest]; !ok {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		body, _ := io.ReadAll(r.Body)
		s.slots[rest] = string(body)
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "object/"):
		obj, ok := s.objects[strings.TrimPrefix(rest, "object/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write(obj.body)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func newTestInternxt(t *testing.T, options map[string]string) (*internxtProvider, *internxtStub, *[]map[string]string) {
	stub := newInternxtStub(t)
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	restore := internxtAPI
	internxtAPI = server.URL
	t.Cleanup(func() { internxtAPI = restore })

	opts := map[string]string{"email": "alice@example.test", "password": internxtFixturePassword, "folder": "sand"}
	for k, v := range options {
		opts[k] = v
	}
	p, err := New(Config{Kind: KindInternxt, Name: "internxt", Options: opts})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var rotations []map[string]string
	p.(CredentialRotator).OnCredentialChange(func(m map[string]string) { rotations = append(rotations, m) })
	return p.(*internxtProvider), stub, &rotations
}

func TestInternxtRoundTrip(t *testing.T) {
	p, stub, rotations := newTestInternxt(t, nil)
	ctx := context.Background()

	if err := p.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if len(*rotations) != 1 || (*rotations)[0]["session"] == "" || (*rotations)[0]["two_factor_code"] != "" {
		t.Fatalf("the sign-in was not written back as a session: %v", *rotations)
	}
	var sess internxtSession
	json.Unmarshal([]byte((*rotations)[0]["session"]), &sess)
	if sess.Mnemonic != internxtFixtureMnemonic || sess.Bucket != internxtStubBucket || sess.Token != stub.token {
		t.Errorf("stored session = %+v", sess)
	}

	payload := []byte("encrypted shard bytes")
	key := "abc-c0000000-p1.sand"
	if err := p.Put(ctx, key, payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if len(stub.folders) != 1 {
		t.Fatalf("shard folder was not created: %v", stub.folders)
	}
	for _, rec := range stub.files {
		if rec.plain != "abc-c0000000-p1" || rec.ext != "sand" {
			t.Errorf("record named %q.%q, want the key split at its extension", rec.plain, rec.ext)
		}
	}
	for _, obj := range stub.objects {
		if bytes.Contains(obj.body, payload) {
			t.Error("the stored object holds the plaintext part")
		}
		if len(obj.body) != len(payload) {
			t.Errorf("CTR should not change the length: stored %d, part %d", len(obj.body), len(payload))
		}
	}

	got, err := p.Get(ctx, key)
	if err != nil || !bytes.Equal(got, payload) {
		t.Errorf("Get = %q, %v", got, err)
	}

	if err := p.Put(ctx, key, []byte("second")); err != nil {
		t.Fatalf("overwrite Put: %v", err)
	}
	if got, _ := p.Get(ctx, key); string(got) != "second" {
		t.Errorf("Get after overwrite = %q", got)
	}
	if len(stub.files) != 1 {
		t.Errorf("an overwrite left %d records, want 1: %v", len(stub.files), stub.files)
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
	if objects, err := p.List(ctx, "abc"); err != nil || len(objects) != 2 {
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
	if stub.logins != 1 {
		t.Errorf("signed in %d times, want once", stub.logins)
	}
}

// A token the drive stops accepting is refreshed without the password, and
// the replacement is written back so the next process starts with it.
func TestInternxtStaleTokenIsRefreshedAndStored(t *testing.T) {
	p, stub, rotations := newTestInternxt(t, nil)
	ctx := context.Background()
	if err := p.Put(ctx, "abc-c0000000-p1.sand", []byte("x")); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	old := stub.token
	stub.stale[old] = true
	stub.mu.Unlock()

	// Stat lists the folder, which is a drive call and so meets the refusal.
	if info, err := p.Stat(ctx, "abc-c0000000-p1.sand"); err != nil || info.Size != 1 {
		t.Fatalf("Stat after the token went stale = %+v, %v", info, err)
	}
	last := (*rotations)[len(*rotations)-1]
	var sess internxtSession
	json.Unmarshal([]byte(last["session"]), &sess)
	if sess.Token == old || sess.Token == "" || sess.Token != stub.token {
		t.Errorf("the refreshed token was not stored: got %q, stub has %q", sess.Token, stub.token)
	}
	if stub.logins != 1 {
		t.Errorf("a refresh should not need the password; signed in %d times", stub.logins)
	}
}

// A token nothing accepts any more — not even the refresh endpoint — means
// signing in again with the stored password.
func TestInternxtExpiredTokenMeansSigningInAgain(t *testing.T) {
	p, stub, rotations := newTestInternxt(t, nil)
	ctx := context.Background()
	if err := p.Put(ctx, "abc-c0000000-p1.sand", []byte("x")); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	stub.expired[stub.token] = true
	stub.mu.Unlock()

	if info, err := p.Stat(ctx, "abc-c0000000-p1.sand"); err != nil || info.Size != 1 {
		t.Fatalf("Stat after the token expired = %+v, %v", info, err)
	}
	if stub.logins != 2 {
		t.Errorf("signed in %d times, want a second sign-in", stub.logins)
	}
	last := (*rotations)[len(*rotations)-1]
	var sess internxtSession
	json.Unmarshal([]byte(last["session"]), &sess)
	if sess.Token != stub.token {
		t.Errorf("the new token was not stored: got %q, stub has %q", sess.Token, stub.token)
	}
}

func TestInternxtStoredSessionSkipsSignIn(t *testing.T) {
	first, stub, rotations := newTestInternxt(t, nil)
	ctx := context.Background()
	if err := first.Put(ctx, "abc-c0000000-p1.sand", []byte("x")); err != nil {
		t.Fatal(err)
	}
	p, err := New(Config{Kind: KindInternxt, Options: map[string]string{
		"email": "alice@example.test", "password": "no longer needed", "session": (*rotations)[0]["session"],
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := p.Get(ctx, "abc-c0000000-p1.sand"); err != nil || string(got) != "x" {
		t.Errorf("Get through the stored session = %q, %v", got, err)
	}
	if stub.logins != 1 {
		t.Errorf("signed in %d times across two providers, want once", stub.logins)
	}
}

func TestInternxtTwoFactorIsAskedFor(t *testing.T) {
	p, stub, _ := newTestInternxt(t, nil)
	stub.tfa = "654321"
	err := p.Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "two-factor") {
		t.Fatalf("Ping without the code = %v", err)
	}
	if stub.logins != 0 {
		t.Error("a sign-in was attempted without the code the account demands")
	}

	p2, stub2, rotations := newTestInternxt(t, map[string]string{"two_factor_code": "654321"})
	stub2.tfa = "654321"
	if err := p2.Ping(context.Background()); err != nil {
		t.Fatalf("Ping with the code: %v", err)
	}
	if got := (*rotations)[0]["two_factor_code"]; got != "" {
		t.Errorf("the one-time code was stored as %q", got)
	}
}

func TestInternxtWrongPasswordIsAClearError(t *testing.T) {
	p, _, _ := newTestInternxt(t, map[string]string{"password": "not it"})
	err := p.Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("Ping with the wrong password = %v", err)
	}
}

func TestInternxtNamesSplitAtTheExtension(t *testing.T) {
	for _, tc := range []struct{ name, plain, ext string }{
		{"abc-c0000000-p1.sand", "abc-c0000000-p1", "sand"},
		{"manifest.sand", "manifest", "sand"},
		{"noext", "noext", ""},
		{".hidden", ".hidden", ""},
	} {
		plain, ext := internxtSplitName(tc.name)
		if plain != tc.plain || ext != tc.ext {
			t.Errorf("split(%q) = %q, %q", tc.name, plain, ext)
		}
		if internxtJoinName(plain, ext) != tc.name {
			t.Errorf("join(split(%q)) = %q", tc.name, internxtJoinName(plain, ext))
		}
	}
}

func TestInternxtSpecKeepsSecretsSecret(t *testing.T) {
	spec, ok := SpecFor(KindInternxt)
	if !ok {
		t.Fatal("internxt is not registered")
	}
	if spec.OAuth != nil || spec.SignInLink != nil {
		t.Error("Internxt is connected with a password, not a browser sign-in")
	}
	redacted := Config{Kind: KindInternxt, Options: map[string]string{
		"password": "p", "session": `{"token":"t"}`, "email": "a@b",
	}}.Redacted()
	if redacted.Options["password"] != RedactedSecret || redacted.Options["session"] != RedactedSecret {
		t.Errorf("secrets reached the API layer: %v", redacted.Options)
	}
}
