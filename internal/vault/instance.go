package vault

import (
	"context"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// One vault, opened on more than one machine.
//
// A vault file is small and a cloud account answers from anywhere, so the
// same vault can run on a desk and on a Raspberry Pi at once — the same
// password, the same clouds, the same tree. Two things stop that from being
// simply true, and this file is about the first of them.
//
// A local folder is not a cloud. The disk under the desk is a part-holder like
// any other to the machine it is plugged into, and a path that does not exist
// to every other machine. A vault copied to the Pi carries that account's
// config, and the Pi would otherwise try the path, fail, draw the account red
// and count every file with a part on it as damaged — when the only thing
// wrong is that the Pi is not the desk. So every machine opening a vault has
// a name (InstanceName), every local account remembers the name of the
// machine it lives on (provider.Config.Host), and the index carries a register
// of the machines that have written it and which accounts each can reach
// (Manifest.Instances). From those three facts a listing can say, without a
// network call, which files this machine can rebuild on its own and which it
// would have to ask another machine for — see reach.go.
//
// The second thing is two machines writing the index at once, which is
// merge.go and sync.go.

// InstanceName is the name this machine opens vaults under: the SAND_INSTANCE
// environment variable when set, and the hostname otherwise.
//
// The hostname, because it is the name the person already knows the machine
// by — "lives on raspberrypi" is what the panel should say, and a random id
// would need a table to say it. The variable is for the two cases a hostname
// does not cover: two machines that share one, and a test that wants to be
// several machines in one process.
func InstanceName() string {
	if name := strings.TrimSpace(os.Getenv("SAND_INSTANCE")); name != "" {
		return name
	}
	if host, err := os.Hostname(); err == nil && strings.TrimSpace(host) != "" {
		return strings.TrimSpace(host)
	}
	return "this machine"
}

// Instance is one machine that has written this index, as the register in
// the index remembers it.
type Instance struct {
	Name string `json:"name"`

	// LastSeen is when this machine last wrote the index. A machine that has
	// not written for a month is still listed — it may simply have had
	// nothing to say — and the panel shows the date rather than guessing.
	LastSeen time.Time `json:"last_seen"`

	// Accounts are the connected accounts that machine can reach: every cloud
	// it holds credentials for, and the local folders that live on it. This
	// is what lets another machine say "readable on study-pc" about a file
	// whose parts it cannot reach itself, without a round trip to find out.
	Accounts []string `json:"accounts"`
}

// Instance is the name this vault runs under on this machine.
func (v *Vault) Instance() string { return v.instance }

// SetInstance renames this machine for the vault's purposes. Tests use it to
// be two machines in one process; nothing else should need to.
func (v *Vault) SetInstance(name string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.instance = name
}

// reachableHere reports whether this machine can count on reaching an
// account: every cloud, and a local account that lives here. A local account
// whose Host is empty has not been claimed by any machine yet — its folder was
// not seen at the last unlock — and is not counted as here either, though it
// is still tried (see livesElsewhere): a ping that answers is what claims it.
func (v *Vault) reachableHere(cfg provider.Config) bool {
	if !provider.IsLocal(cfg.Kind) {
		return true
	}
	return cfg.Host != "" && cfg.Host == v.instance
}

// livesElsewhere reports whether an account is known to live on another
// machine: a local account another machine has claimed. Such an account is
// never pinged from here — the path would be tried on the wrong machine and
// the failure would say nothing true about the account.
func (v *Vault) livesElsewhere(cfg provider.Config) bool {
	return provider.IsLocal(cfg.Kind) && cfg.Host != "" && cfg.Host != v.instance
}

// claimAnswered stamps this machine's name on every unclaimed local account
// among those that have just answered a ping, and writes the vault file if
// any was. A folder that answers from here is here: this is how a drive
// plugged in after the unlock is claimed the moment the sidebar sees it,
// rather than at the next sign-in.
func (v *Vault) claimAnswered(found []CloudHealth) {
	answered := map[string]bool{}
	for _, one := range found {
		if one.Healthy {
			answered[one.ID] = true
		}
	}
	if len(answered) == 0 {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return
	}
	changed := false
	for _, i := range v.unclaimedLocalLocked() {
		if answered[v.providers[i].ID] {
			v.providers[i].Host = v.instance
			changed = true
		}
	}
	if changed {
		_ = v.persistLocked()
	}
}

// reachableIDsLocked is the set of connected accounts this machine can reach.
// The caller holds at least the read lock.
func (v *Vault) reachableIDsLocked() map[string]bool {
	out := make(map[string]bool, len(v.providers))
	for _, cfg := range v.providers {
		if v.reachableHere(cfg) {
			out[cfg.ID] = true
		}
	}
	return out
}

// homeOfLocked is the machine an account lives on, as far as this one knows:
// the Host a local account carries, else the most recently seen machine whose
// register entry lists the account, else "". For an account this machine can
// reach it answers this machine's own name. The caller holds the read lock.
func (v *Vault) homeOfLocked(cfg provider.Config) string {
	if v.reachableHere(cfg) {
		return v.instance
	}
	if cfg.Host != "" {
		return cfg.Host
	}
	for _, peer := range v.peersLocked() {
		for _, id := range peer.Accounts {
			if id == cfg.ID {
				return peer.Name
			}
		}
	}
	return ""
}

// awayLocked is the machine an account known to live elsewhere lives on. The
// caller holds the read lock.
func (v *Vault) awayLocked(cfg provider.Config) string {
	if home := v.homeOfLocked(cfg); home != "" && home != v.instance {
		return home
	}
	return cfg.Host
}

// peersLocked is every other machine in the register, most recently seen
// first. The caller holds at least the read lock.
func (v *Vault) peersLocked() []*Instance {
	if v.manifest == nil {
		return nil
	}
	peers := make([]*Instance, 0, len(v.manifest.Instances))
	for name, inst := range v.manifest.Instances {
		if name == v.instance || inst == nil {
			continue
		}
		peers = append(peers, inst)
	}
	sort.Slice(peers, func(i, j int) bool {
		if !peers[i].LastSeen.Equal(peers[j].LastSeen) {
			return peers[i].LastSeen.After(peers[j].LastSeen)
		}
		return peers[i].Name < peers[j].Name
	})
	return peers
}

// hasPeersLocked says whether any other machine has written this index. The
// caller holds at least the read lock.
func (v *Vault) hasPeersLocked() bool { return len(v.peersLocked()) > 0 }

// registerSelfLocked writes this machine into the index's register: now, and
// the accounts it can reach. Called on every index write, under the write
// lock, so the register is as current as the index it travels in.
func (v *Vault) registerSelfLocked(now time.Time) {
	if v.manifest == nil || v.instance == "" {
		return
	}
	ids := make([]string, 0, len(v.providers))
	for _, cfg := range v.providers {
		if v.reachableHere(cfg) {
			ids = append(ids, cfg.ID)
		}
	}
	sort.Strings(ids)
	if v.manifest.Instances == nil {
		v.manifest.Instances = map[string]*Instance{}
	}
	v.manifest.Instances[v.instance] = &Instance{Name: v.instance, LastSeen: now, Accounts: ids}
}

// InstanceInfo is one machine as the instances panel lists it.
type InstanceInfo struct {
	Instance
	// Here marks the machine answering the request.
	Here bool `json:"here"`
}

// Instances lists every machine that has written this index, this one first.
func (v *Vault) Instances() ([]InstanceInfo, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}

	self := Instance{Name: v.instance}
	if own := v.manifest.Instances[v.instance]; own != nil {
		self = *own
	}
	ids := make([]string, 0, len(v.providers))
	for id := range v.reachableIDsLocked() {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	self.Accounts = ids

	out := []InstanceInfo{{Instance: self, Here: true}}
	for _, peer := range v.peersLocked() {
		out = append(out, InstanceInfo{Instance: *peer})
	}
	return out, nil
}

// localRootOf is the folder a local account is reached through on the machine
// it lives on: the directory field for the folder-backed kinds, and the state
// directory for Proton's client, whose sign-in lives there.
func localRootOf(cfg provider.Config) string {
	if cfg.Kind == provider.KindProtonCLI {
		return provider.ExpandHome(cfg.Option("state_dir"))
	}
	return provider.ExpandHome(cfg.Option("path"))
}

// claimWithin is how long a folder gets to answer a stat before it is taken to
// be absent. A mount that has gone away can hang a stat for minutes, and an
// unlock must not wait on it.
var claimWithin = 2 * time.Second

// dirExistsWithin reports whether path is a directory, giving the filesystem
// at most `within` to say so.
func dirExistsWithin(path string, within time.Duration) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	answer := make(chan bool, 1)
	go func() {
		info, err := os.Stat(path)
		answer <- err == nil && info.IsDir()
	}()
	select {
	case ok := <-answer:
		return ok
	case <-time.After(within):
		return false
	}
}

// claimLocalAccounts stamps this machine's name on every local account that no
// machine has claimed yet and whose folder is here, and writes the vault file
// if any was. Run at unlock, and again by the scheduled health check, so a
// drive plugged in after the unlock is claimed within the hour rather than at
// the next sign-in.
//
// A vault written before machines had names carries local accounts with no
// Host at all. The machine that can see the folder is the machine the account
// lives on, which is why the folder is looked for rather than the name simply
// written: the same vault file copied to a second machine must not have that
// machine claim a disk it has never seen.
func (v *Vault) claimLocalAccounts(ctx context.Context) {
	v.mu.RLock()
	unlocked := v.dataKey != nil
	unclaimed := v.unclaimedLocalLocked()
	v.mu.RUnlock()
	if !unlocked || len(unclaimed) == 0 || ctx.Err() != nil {
		return
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	v.claimLocalAccountsLocked()
}

// claimLocalAccountsLocked is the claim itself, under the write lock: the
// folders are looked for, each within claimWithin, and the vault file is
// written if any was found. A failure to write leaves the stamps in memory;
// the next write carries them, and the claim is cheap to make again.
func (v *Vault) claimLocalAccountsLocked() {
	if v.dataKey == nil {
		return
	}
	changed := false
	for _, i := range v.unclaimedLocalLocked() {
		cfg := &v.providers[i]
		if dirExistsWithin(localRootOf(*cfg), claimWithin) {
			cfg.Host = v.instance
			changed = true
		}
	}
	if changed {
		_ = v.persistLocked()
	}
}

// unclaimedLocalLocked is the index of every local account no machine has
// claimed. The caller holds at least the read lock.
func (v *Vault) unclaimedLocalLocked() []int {
	var out []int
	for i, cfg := range v.providers {
		if provider.IsLocal(cfg.Kind) && cfg.Host == "" {
			out = append(out, i)
		}
	}
	return out
}
