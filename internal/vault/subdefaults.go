package vault

import (
	"fmt"

	"github.com/chinmay28/sand-vault/internal/archive"
)

// A sub vault's default clouds.
//
// Every vault has a standing answer to "where does an upload go when it does
// not say": the default accounts and scheme set with SetDefaults. A sub vault
// follows that answer unless it has one of its own, in which case its own wins
// for every upload made into it. Clearing a sub vault's default puts it back to
// following the vault's — there is no third state, because "pick three at
// random" is what the vault does with no default and a sub vault that wants it
// can name its own clouds instead.
//
// The choice lives in the sub vault's own index, sealed under its password,
// and so the rules are shaped by what can be done while it is shut:
//
//   - It is set only while the sub vault is open, like everything else in it.
//   - It is read only while open, which is the only time anything is uploaded
//     into it anyway.
//   - Disconnecting an account cannot rewrite a shut sub vault's index, so the
//     default is pruned each time it is read, by the same rule a disconnect
//     applies to the vault's (pruneDefaults). One pruned down to nothing falls
//     back to the vault's default rather than to a random pick: the sub vault
//     asked for particular clouds, and the vault's are the nearest thing to
//     that it has.

// Defaults is where uploads into one vault go unless they choose otherwise.
type Defaults struct {
	// Accounts is the selection, by ID. Empty means each upload picks its own
	// at random.
	Accounts []string `json:"default_accounts"`

	// Scheme is the code those uploads are cut with, written "k-of-n". Empty
	// means how many accounts a file lands on settles it.
	Scheme string `json:"default_scheme,omitempty"`

	// Inherited says the answer is the vault's rather than the sub vault's
	// own. Always false for the main vault, which has nothing to inherit from.
	Inherited bool `json:"inherited"`
}

// DefaultsIn returns where uploads into a scope go unless they choose
// otherwise. A sub vault has to be open to be asked.
func (v *Vault) DefaultsIn(scope Scope) (Defaults, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return Defaults{}, ErrLocked
	}
	if scope.Main() {
		return Defaults{
			Accounts: append([]string{}, v.store.DefaultAccounts...),
			Scheme:   schemeString(v.defaultSchemeLocked()),
		}, nil
	}
	sub, err := v.openSubVaultLocked(scope)
	if err != nil {
		return Defaults{}, err
	}
	return v.scopeDefaultsLocked(sub), nil
}

// SetDefaultsIn records a scope's own default clouds. For the main vault it is
// SetDefaults. For a sub vault the selection is judged exactly as the vault's
// is, and passing no accounts and no scheme clears it, putting the sub vault
// back to following the vault's default.
func (v *Vault) SetDefaultsIn(scope Scope, ids []string, scheme archive.Scheme) error {
	if scope.Main() {
		return v.SetDefaults(ids, scheme)
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return ErrLocked
	}
	sub, err := v.openSubVaultLocked(scope)
	if err != nil {
		return err
	}
	chosen, stored, err := v.checkDefaultsLocked(ids, scheme)
	if err != nil {
		return err
	}

	m := sub.manifest
	previousAccounts, previousScheme := m.DefaultAccounts, m.DefaultScheme
	m.DefaultAccounts, m.DefaultScheme = chosen, stored
	if err := v.persistLocked(); err != nil {
		m.DefaultAccounts, m.DefaultScheme = previousAccounts, previousScheme
		return err
	}
	return nil
}

// openSubVaultLocked finds an open sub vault, or says why it cannot.
func (v *Vault) openSubVaultLocked(scope Scope) (*subVault, error) {
	if sub, ok := v.subs[string(scope)]; ok {
		return sub, nil
	}
	if v.manifest.SubVaultByID(string(scope)) != nil {
		return nil, fmt.Errorf("%w: %s", ErrSubVaultLocked, v.subVaultLabelLocked(string(scope)))
	}
	return nil, fmt.Errorf("%w: %s", ErrNoSubVault, scope)
}

// scopeDefaultsLocked is the answer for one open sub vault: its own default,
// pruned to what is still connected, or the vault's where that leaves nothing.
func (v *Vault) scopeDefaultsLocked(sub *subVault) Defaults {
	accounts, scheme := pruneDefaults(sub.manifest.DefaultAccounts, sub.manifest.DefaultScheme,
		func(id string) bool { _, ok := v.configForLocked(id); return ok })
	if len(accounts) == 0 {
		return Defaults{
			Accounts:  append([]string{}, v.store.DefaultAccounts...),
			Scheme:    schemeString(v.defaultSchemeLocked()),
			Inherited: true,
		}
	}
	return Defaults{Accounts: accounts, Scheme: scheme}
}

// defaultsForLocked is what an upload into a scope starts from when it names no
// accounts of its own: the selection, and the scheme that goes with it. A scope
// that is not open answers with the vault's — the caller will have refused it
// already for want of a data key.
func (v *Vault) defaultsForLocked(scope Scope) ([]string, archive.Scheme) {
	d := Defaults{
		Accounts: append([]string(nil), v.store.DefaultAccounts...),
		Scheme:   v.store.DefaultScheme,
	}
	if !scope.Main() {
		if sub, ok := v.subs[string(scope)]; ok {
			d = v.scopeDefaultsLocked(sub)
		}
	}
	scheme, err := archive.ParseScheme(d.Scheme)
	if d.Scheme == "" || err != nil {
		scheme = archive.Scheme{}
	}
	return d.Accounts, scheme
}

// schemeString writes a scheme the way Defaults carries one, with the zero
// value as the empty string rather than as "0-of-0".
func schemeString(scheme archive.Scheme) string {
	if scheme == (archive.Scheme{}) {
		return ""
	}
	return scheme.String()
}
