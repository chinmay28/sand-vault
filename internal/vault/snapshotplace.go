package vault

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/chinmay28/SandHarbor/internal/provider"
	sandsftp "github.com/chinmay28/SandHarbor/internal/sftp"
	"github.com/chinmay28/SandHarbor/internal/snapshot"
)

// The two places a snapshot is kept, each as the snapshot.Store that package
// writes to. Both are handed bytes that are already sealed and names that say
// nothing; neither does anything but keep them.

// snapshotPlace is a target opened: the store, what to call it, and whether
// it is a cloud — which is what decides how big a part may be.
type snapshotPlace struct {
	store snapshot.Store
	label string
	cloud bool
	close func()
}

// openSnapshotPlace turns a target into somewhere parts can be read and
// written.
func (v *Vault) openSnapshotPlace(ctx context.Context, target SnapshotTarget) (*snapshotPlace, error) {
	if err := target.validate(); err != nil {
		return nil, err
	}
	if target.Account != "" {
		return v.openSnapshotAccount(target.Account)
	}
	return v.openSnapshotMachine(ctx, target)
}

func (v *Vault) openSnapshotAccount(id string) (*snapshotPlace, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	cfg, ok := v.configForLocked(id)
	elsewhere := ok && v.livesElsewhere(cfg)
	v.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("no connected account with the ID %s", id)
	}
	if elsewhere {
		return nil, fmt.Errorf("%s is a folder on %s, which is not this machine", cfg.Name, cfg.Host)
	}
	p, err := v.buildProvider(cfg)
	if err != nil {
		return nil, err
	}
	return &snapshotPlace{
		store: accountSnapshots{p: p},
		label: cfg.Name,
		cloud: true,
		close: func() {},
	}, nil
}

func (v *Vault) openSnapshotMachine(ctx context.Context, target SnapshotTarget) (*snapshotPlace, error) {
	source, err := v.source(target.Machine)
	if err != nil {
		return nil, err
	}
	// Checked against the root before anything is dialled, as an export's
	// destination is: the answer to a folder outside it is no, and it does
	// not need a connection to say so.
	dir := sandsftp.CleanPath(strings.TrimPrefix(strings.TrimSpace(target.Dir), "/"))
	if _, err := sandsftp.Under(source.Root, dir); err != nil {
		return nil, err
	}

	client, err := sandsftp.Dial(ctx, source.dialConfig())
	if err != nil {
		return nil, err
	}
	label := source.Name
	if dir != "" {
		label += " · " + dir
	}
	return &snapshotPlace{
		store: machineSnapshots{client: client, root: source.Root, dir: dir},
		label: label,
		close: func() { client.Close() },
	}, nil
}

// snapshotPrefix starts the name of every part file. It is what a listing of
// an account is narrowed by; internal/snapshot is what decides whether a name
// that starts this way is really a part.
const snapshotPrefix = "snapshot-"

// accountSnapshots keeps parts on a connected account, in the folder its
// shards are in.
//
// They sit beside the vault's own objects and are told from them by name:
// nothing the vault writes ends in .sandsnap, and nothing that sweeps an
// account for parts the index has lost track of takes a name it does not
// recognise for one of its own (see partKeyPattern in orphans.go).
type accountSnapshots struct {
	p provider.Provider
}

func (a accountSnapshots) Create(ctx context.Context) (snapshot.Part, error) {
	return &accountPart{ctx: ctx, p: a.p}, nil
}

func (a accountSnapshots) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	data, err := a.p.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (a accountSnapshots) List(ctx context.Context) ([]snapshot.Object, error) {
	objects, err := a.p.List(ctx, snapshotPrefix)
	if err != nil {
		return nil, err
	}
	out := make([]snapshot.Object, 0, len(objects))
	for _, o := range objects {
		out = append(out, snapshot.Object{Name: o.Key, Size: o.Size})
	}
	return out, nil
}

func (a accountSnapshots) Remove(ctx context.Context, name string) error {
	// Only ever a part: this is a delete on the account the vault's own
	// objects are in, and it is not to be reachable with any other name.
	if !snapshot.IsPartName(name) {
		return fmt.Errorf("%q is not a snapshot's part", name)
	}
	return a.p.Delete(ctx, name)
}

// accountPart is a part bound for a cloud: held whole until it is committed,
// because that is the only way a backend takes an object.
type accountPart struct {
	ctx context.Context
	p   provider.Provider
	buf bytes.Buffer
}

func (p *accountPart) Write(b []byte) (int, error) { return p.buf.Write(b) }

func (p *accountPart) Commit(name string) error {
	if !snapshot.IsPartName(name) {
		return fmt.Errorf("%q is not a snapshot's part", name)
	}
	err := p.p.Put(p.ctx, name, p.buf.Bytes())
	p.buf = bytes.Buffer{}
	return err
}

func (p *accountPart) Abort() error {
	p.buf = bytes.Buffer{}
	return nil
}

// machineSnapshots keeps parts in a folder on a machine, written as they
// arrive: a part there is never held whole, so it can be any size.
type machineSnapshots struct {
	client *sandsftp.Client
	root   string
	dir    string
}

func (m machineSnapshots) Create(ctx context.Context) (snapshot.Part, error) {
	pending, err := m.client.CreatePending(m.root, m.dir)
	if err != nil {
		return nil, err
	}
	// Buffered so that what reaches the SFTP file is a megabyte at a time:
	// that is big enough for the client to keep many packets in flight, where
	// the small writes an archive makes would each be a round trip.
	return &machinePart{pending: pending, buf: bufio.NewWriterSize(pending, 1<<20)}, nil
}

func (m machineSnapshots) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	f, _, err := m.client.OpenUnder(m.root, path.Join(m.dir, name))
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (m machineSnapshots) List(ctx context.Context) ([]snapshot.Object, error) {
	listing, err := m.client.ReadDirAll(m.root, m.dir)
	if err != nil {
		// A folder that has never had a snapshot put in it holds none.
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []snapshot.Object
	for _, e := range listing.Entries {
		if e.Dir || e.Unreachable {
			continue
		}
		out = append(out, snapshot.Object{Name: e.Name, Size: e.Size})
	}
	return out, nil
}

func (m machineSnapshots) Remove(ctx context.Context, name string) error {
	// The same fence the account's store has, for the stronger reason: this
	// folder is the user's own, and holds whatever else they keep there.
	if !snapshot.IsPartName(name) {
		return fmt.Errorf("%q is not a snapshot's part", name)
	}
	return m.client.RemoveUnder(m.root, path.Join(m.dir, name))
}

type machinePart struct {
	pending *sandsftp.Pending
	buf     *bufio.Writer
}

func (p *machinePart) Write(b []byte) (int, error) { return p.buf.Write(b) }

func (p *machinePart) Commit(name string) error {
	if err := p.buf.Flush(); err != nil {
		p.pending.Abort()
		return err
	}
	return p.pending.Commit(name)
}

func (p *machinePart) Abort() error { return p.pending.Abort() }
