package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"

	sandsftp "github.com/chinmay28/sand-vault/internal/sftp"
)

func init() {
	Register(Spec{
		Kind:  KindSFTP,
		Label: "SSH / SFTP",
		Description: "Any machine you have an SSH login on — a VPS, a NAS, a storage box at a " +
			"hosting company. Nothing has to be installed on the far end: if you can sftp to it, " +
			"SAND can hold parts on it.",
		DocsURL: "https://github.com/chinmay28/sand-vault/blob/main/docs/sftp.md",
		// Between the services you paste credentials for and the local folder:
		// it is a credential form like theirs, and like the local folder it
		// names no service.
		Order: 30,
		Covers: []Service{
			{Name: "A VPS at Hetzner, OVH, DigitalOcean, Vultr, Linode", Hint: "the address you already ssh to"},
			{Name: "Hetzner Storage Box", Hint: "u123456.your-storagebox.de, port 23"},
			{Name: "rsync.net", Hint: "<user>.rsync.net"},
			{Name: "BorgBase, Cloudsmith and other SSH storage", Hint: "the host the service gives you"},
			{Name: "A Synology, QNAP or TrueNAS box", Hint: "the NAS on your own network"},
			{Name: "A Raspberry Pi in somebody else's house", Hint: "the far leg that is genuinely far"},
		},
		Presets: []Preset{
			{
				Key:   "storagebox",
				Label: "Hetzner Storage Box",
				Help:  "Storage Boxes serve SFTP on port 23, not 22. Enable SSH support in the Robot console first.",
				Values: map[string]string{
					"port": "23",
					"path": "sand",
				},
			},
			{
				Key:    "rsyncnet",
				Label:  "rsync.net",
				Help:   "Your username is also the first part of the hostname.",
				Values: map[string]string{"port": "22", "path": "sand"},
			},
		},
		Fields: []FieldSpec{
			{
				Key:         "host",
				Label:       "Host",
				Placeholder: "vps.example.com",
				Required:    true,
			},
			{Key: "port", Label: "Port", Placeholder: "22", Default: "22"},
			{Key: "username", Label: "Username", Placeholder: "sand", Required: true},
			{
				Key:   "private_key",
				Label: "Private key",
				Help: "Have SAND make a key pair and install the public half on the server, or paste an " +
					"OpenSSH private key you already have — the whole file, including the BEGIN and END lines.",
				Secret:    true,
				Multiline: true,
				SSHKey:    true,
			},
			{
				Key:   "passphrase",
				Label: "Key passphrase",
				Help: "Only for a key you pasted that is encrypted — one SAND generated has no passphrase. " +
					"Held in memory while the vault is open and never written outside it.",
				Secret: true,
			},
			{
				Key:      "password",
				Label:    "Password",
				Help:     "Instead of a key, for a box whose web console will not let you install one. A key is better in every other case.",
				Secret:   true,
				Advanced: true,
			},
			{
				Key:      "path",
				Label:    "Folder",
				Help:     "Where parts go on the far end, relative to where SFTP drops you. Created if it does not exist.",
				Default:  "sand",
				Required: true,
			},
			{
				Key:   "host_key",
				Label: "Host key fingerprint",
				Help: "Left empty, the first connection learns it and every one after that requires it. " +
					"Fill it in to check the very first connection too: run " +
					"ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub on the server and paste the SHA256:… part.",
				Advanced: true,
			},
		},
	}, newSFTPProvider)
}

// sftpProvider stores each shard as a file on a machine reached over SSH.
//
// The interesting part is not the file operations, which are the same as the
// local backend's — it is that a connection is worth keeping. A scatter writes
// every shard of a chunk at once, and an SSH handshake costs a round trip and
// a key exchange, so one pooled session carries the lot. See internal/sftp.
type sftpProvider struct {
	base
	pool *sandsftp.Pool
	root string

	mu     sync.Mutex
	notify func(map[string]string)
}

func newSFTPProvider(cfg Config) (Provider, error) {
	port := 0
	if raw := strings.TrimSpace(cfg.Option("port")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > 65535 {
			return nil, fmt.Errorf("%q is not a port number", raw)
		}
		port = parsed
	}

	// A fingerprint typed in by hand is checked now rather than at connect
	// time, so a typo is a message under the field instead of a host key
	// mismatch on the first transfer — which reads like an attack.
	hostKey, err := sandsftp.NormalizeHostKey(cfg.Option("host_key"))
	if err != nil {
		return nil, err
	}

	root := sandsftp.CleanPath(cfg.Option("path"))
	if root == "" {
		return nil, errors.New("no folder given: name one, e.g. sand")
	}

	p := &sftpProvider{
		base: base{cfg: cfg},
		root: root,
		pool: sandsftp.NewPool(sandsftp.Config{
			Host:       strings.TrimSpace(cfg.Option("host")),
			Port:       port,
			User:       strings.TrimSpace(cfg.Option("username")),
			PrivateKey: cfg.Option("private_key"),
			Passphrase: cfg.Option("passphrase"),
			Password:   cfg.Option("password"),
			HostKey:    hostKey,
		}),
	}

	// Trust on first use is only trust if what was learned is written down.
	// A fingerprint learned and then forgotten pins nothing: the next
	// connection learns again, and would learn an impostor's key just as
	// happily. The vault wires this sink up through CredentialRotator, which
	// exists for exactly this — an option that changes as the backend is used.
	p.pool.OnHostKeyLearned(func(fingerprint string) {
		p.mu.Lock()
		notify := p.notify
		p.mu.Unlock()
		if notify != nil {
			notify(map[string]string{"host_key": fingerprint})
		}
	})

	return p, nil
}

// OnCredentialChange registers the sink that persists a learned host key.
func (p *sftpProvider) OnCredentialChange(fn func(map[string]string)) {
	p.mu.Lock()
	p.notify = fn
	p.mu.Unlock()
}

// Close drops the pooled connection. The vault calls this when an account is
// disconnected or the vault is locked; without it the session would stay open
// on the far end until sshd's own timeout noticed.
func (p *sftpProvider) Close() error { return p.pool.Close() }

// resolve maps an object key onto a path inside the configured folder,
// refusing any key that would leave it.
func (p *sftpProvider) resolve(key string) (string, error) {
	return sandsftp.Under(p.root, key)
}

func (p *sftpProvider) client(ctx context.Context) (*sandsftp.Client, error) {
	client, err := p.pool.Get(ctx)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func (p *sftpProvider) Put(ctx context.Context, key string, data []byte) error {
	full, err := p.resolve(key)
	if err != nil {
		return err
	}
	client, err := p.client(ctx)
	if err != nil {
		return err
	}
	fs := client.SFTP()

	dir := path.Dir(full)
	if err := fs.MkdirAll(dir); err != nil {
		return fmt.Errorf("sftp: creating %s: %w", dir, err)
	}

	// Written under a temporary name and renamed into place, so an interrupted
	// write can never leave a half-written shard under the name a reader will
	// come looking for. It matters more here than on a local disk: the thing
	// that interrupts a network write is a dropped connection, which is
	// common, rather than a crash, which is not.
	tmp := path.Join(dir, ".sand-tmp-"+randomSuffix())
	f, err := fs.Create(tmp)
	if err != nil {
		return fmt.Errorf("sftp: creating %s: %w", tmp, err)
	}
	// Before the bytes rather than after, so the file is never briefly
	// readable by everyone on a server with a permissive umask.
	if err := fs.Chmod(tmp, 0600); err != nil {
		f.Close()
		fs.Remove(tmp)
		return fmt.Errorf("sftp: setting permissions on %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		fs.Remove(tmp)
		return fmt.Errorf("sftp: writing %s: %w", key, err)
	}
	if err := f.Close(); err != nil {
		fs.Remove(tmp)
		return fmt.Errorf("sftp: closing %s: %w", key, err)
	}

	if err := renameOver(client, tmp, full); err != nil {
		fs.Remove(tmp)
		return fmt.Errorf("sftp: finalizing %s: %w", key, err)
	}
	return nil
}

// renameOver puts a temporary file in its final place, overwriting whatever is
// there.
//
// Two ways round, because the protocol's own rename cannot do it. SFTP v3
// leaves the behaviour of a rename onto an existing name to the server, and
// OpenSSH's answer is to refuse — so OpenSSH also ships posix-rename@openssh.com,
// which is atomic and overwrites, and is what nearly every server SAND will
// meet supports. The fallback for the ones that do not is remove-then-rename,
// which has a window where the object does not exist. That window is survivable
// where an atomic overwrite would be nicer: a shard is one of several, a reader
// that misses it reads the others, and Put is only overwriting at all when
// something is being rewritten in place.
func renameOver(client *sandsftp.Client, from, to string) error {
	fs := client.SFTP()
	if err := fs.PosixRename(from, to); err == nil {
		return nil
	}
	if err := fs.Remove(to); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fs.Rename(from, to)
}

// randomSuffix names a temporary file. Random rather than counted because two
// SAND instances may be writing into the same folder — a vault on a laptop and
// one on a Pi, pointed at the same box — and a collision would have one of
// them renaming the other's half-written file into place.
func randomSuffix() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail in practice, and a temp name is not a
		// secret: any name nobody else is using will do.
		return "fallback"
	}
	return hex.EncodeToString(buf[:])
}

func (p *sftpProvider) Get(ctx context.Context, key string) ([]byte, error) {
	full, err := p.resolve(key)
	if err != nil {
		return nil, err
	}
	client, err := p.client(ctx)
	if err != nil {
		return nil, err
	}

	f, err := client.SFTP().Open(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("sftp: opening %s: %w", key, err)
	}
	defer f.Close()

	// The file's own WriteTo rather than io.ReadAll, because on an *sftp.File
	// they differ by an order of magnitude on a distant server: WriteTo keeps
	// many requests in flight at once, where ReadAll's Read loop starts as one
	// packet per round trip and only overlaps requests once its buffer has
	// grown. A shard is a few megabytes, so the difference is the fetch taking
	// a moment rather than half a minute. Grown to the shard's size up front so
	// the buffer is allocated once instead of doubled into place.
	var buf bytes.Buffer
	if info, err := f.Stat(); err == nil && info.Size() > 0 {
		buf.Grow(int(info.Size()))
	}
	if _, err := f.WriteTo(&buf); err != nil {
		return nil, fmt.Errorf("sftp: reading %s: %w", key, err)
	}
	return buf.Bytes(), nil
}

func (p *sftpProvider) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	full, err := p.resolve(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	client, err := p.client(ctx)
	if err != nil {
		return ObjectInfo{}, err
	}

	info, err := client.SFTP().Stat(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, fmt.Errorf("sftp: stat %s: %w", key, err)
	}
	return ObjectInfo{Key: key, Size: info.Size()}, nil
}

func (p *sftpProvider) Delete(ctx context.Context, key string) error {
	full, err := p.resolve(key)
	if err != nil {
		return err
	}
	client, err := p.client(ctx)
	if err != nil {
		return err
	}
	fs := client.SFTP()

	if err := fs.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sftp: removing %s: %w", key, err)
	}
	// Prune the parent if this emptied it, ignoring the failure that means it
	// did not — the same shrug the local backend gives.
	if dir := path.Dir(full); dir != p.root {
		fs.RemoveDirectory(dir)
	}
	return nil
}

func (p *sftpProvider) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	client, err := p.client(ctx)
	if err != nil {
		return nil, err
	}

	var out []ObjectInfo
	walker := client.SFTP().Walk(p.root)
	for walker.Step() {
		if err := walker.Err(); err != nil {
			// A folder that is not there yet holds no objects, which is an
			// answer rather than a failure: it is what an account reports
			// before anything has been written to it.
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("sftp: listing %s: %w", p.root, err)
		}
		info := walker.Stat()
		if info == nil || info.IsDir() {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(walker.Path(), p.root), "/")
		if rel == "" || !strings.HasPrefix(rel, prefix) {
			continue
		}
		// A half-written shard from an interrupted Put is not an object, and
		// listing it would have the recovery scan try to read it.
		if strings.HasPrefix(path.Base(rel), ".sand-tmp-") {
			continue
		}
		out = append(out, ObjectInfo{Key: rel, Size: info.Size()})
	}
	return out, nil
}

func (p *sftpProvider) Ping(ctx context.Context) error {
	client, err := p.client(ctx)
	if err != nil {
		return err
	}
	fs := client.SFTP()

	if err := fs.MkdirAll(p.root); err != nil {
		return fmt.Errorf("cannot use %s on the server: %w", p.root, err)
	}

	// Reachable and writable are different questions, and an account that
	// answers the first and not the second fails on the first upload instead
	// of here. A read-only home directory and a quota already spent both look
	// like a working connection until something is written.
	probe := path.Join(p.root, ".sand-write-probe-"+randomSuffix())
	f, err := fs.Create(probe)
	if err != nil {
		return fmt.Errorf("%s is not writable by %s: %w", p.root, p.cfg.Option("username"), err)
	}
	_, writeErr := f.Write([]byte("ok"))
	closeErr := f.Close()
	fs.Remove(probe)
	if writeErr != nil {
		return fmt.Errorf("%s is not writable by %s: %w", p.root, p.cfg.Option("username"), writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("%s is not writable by %s: %w", p.root, p.cfg.Option("username"), closeErr)
	}
	return nil
}

// Usage reports the filesystem the folder sits on.
//
// SFTP is the rare backend that can answer this honestly and cheaply: OpenSSH
// ships statvfs@openssh.com, which is one round trip and the server's own
// bookkeeping rather than a listing. So this is a UsageReporter — taken on
// every ping — where a bucket has to be a UsageMeasurer and counted.
//
// Like the local backend, what is reported is the drive rather than the
// folder: what SAND put here is already known from the index, and the figure
// the bar needs is how much room is left after everything else on the disk.
// Free comes from the non-root count, because a filesystem reserve SAND cannot
// spend is not free space to SAND.
func (p *sftpProvider) Usage(ctx context.Context) (Usage, error) {
	client, err := p.client(ctx)
	if err != nil {
		return Usage{}, err
	}

	stat, err := client.SFTP().StatVFS(p.root)
	if err != nil {
		// Not every server has the extension — a locked-down sftp-only build,
		// or a non-OpenSSH server. An account whose usage cannot be read is
		// one whose card draws no bar, which is the same as a backend that
		// reports no quota at all, so this is never surfaced as a failure.
		return Usage{}, fmt.Errorf("sftp: this server does not report disk usage: %w", err)
	}

	total := stat.Frsize * stat.Blocks
	free := stat.Frsize * stat.Bavail
	used := stat.Frsize * (stat.Blocks - stat.Bfree)
	return Usage{
		Used:  int64(used),
		Total: int64(total),
		Free:  int64(free),
	}, nil
}
