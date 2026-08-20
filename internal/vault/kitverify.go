package vault

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// The fire drill.
//
// An untested backup is a rumour. VerifyKit opens a kit and answers the one
// question the export dialog cannot — *if I needed this today, what would I get
// back?* — against the live accounts, changing nothing anywhere.
//
// It asks for the code rather than opening the kit from the running vault's own
// key material, and that is half of why it exists. The failure nothing else in
// this design can catch is the slip of paper that went missing in a house move
// two years ago, and the only way to catch it is to make somebody find the
// paper. A drill that passes proves the credentials still work *and* that the
// secret is still in the world — and the day it fails on the second, the vault
// is still alive and a fresh kit is ten seconds away.

// KitCredentialResult is one carried credential, tried against the real thing.
type KitCredentialResult struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// KitVerifyReport is what a drill found.
type KitVerifyReport struct {
	KitID      string    `json:"kit_id"`
	KitCreated time.Time `json:"kit_created_at"`
	Secret     string    `json:"secret"`
	AgeDays    int       `json:"age_days"`

	// Accounts is every credential in the kit, pinged.
	Accounts  []KitCredentialResult `json:"accounts"`
	Working   int                   `json:"working"`
	Unusable  int                   `json:"unusable"`
	Listed    int                   `json:"listed"`
	NotListed int                   `json:"not_listed"`

	// KitFiles is what the kit's own index describes, and Recoverable how many
	// of those the accounts could actually rebuild today. VaultFiles is what
	// the running vault holds now, so the drift is a subtraction the reader
	// does not have to do.
	KitFiles         int   `json:"kit_files"`
	KitBytes         int64 `json:"kit_bytes"`
	Recoverable      int   `json:"recoverable"`
	RecoverableBytes int64 `json:"recoverable_bytes"`
	VaultFiles       int   `json:"vault_files"`
	AddedSince       int   `json:"added_since"`

	// AccountsAdded names the accounts connected after this kit was made. They
	// are the honest ceiling of an old kit: it cannot restore a credential it
	// never had, and this is the list of the ones somebody would have to
	// connect by hand.
	AccountsAdded []KitCredentialResult `json:"accounts_added,omitempty"`

	// StaleIndex says the kit's index is older than what this vault holds.
	// Not a failure — in a real recovery the newer copy on the clouds closes
	// the gap — which is exactly why the numbers above are the floor and not
	// the forecast.
	StaleIndex bool `json:"stale_index"`

	Warnings []string `json:"warnings,omitempty"`
}

// VerifyKit runs the drill: every credential pinged, every part the kit's index
// names looked for, and the drift against the vault as it stands now.
//
// It writes nothing, anywhere. The vault it runs against is untouched and so
// are the accounts.
func (v *Vault) VerifyKit(ctx context.Context, kit *Kit) (*KitVerifyReport, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	live := append([]provider.Config(nil), v.providers...)
	vaultFiles := len(v.manifest.Entries)
	vaultUpdated := v.manifest.UpdatedAt
	v.mu.RUnlock()

	secret := kit.SecretKind
	if secret == "" {
		secret = KitSecretCode
	}
	report := &KitVerifyReport{
		KitID:      kit.KitID,
		KitCreated: kit.CreatedAt,
		Secret:     secret,
		AgeDays:    int(time.Since(kit.CreatedAt).Hours() / 24),
		KitFiles:   len(kit.Snapshot.Manifest.Entries),
		VaultFiles: vaultFiles,
		StaleIndex: vaultUpdated.After(kit.Snapshot.CreatedAt),
	}
	if added := vaultFiles - len(kit.Snapshot.Manifest.Entries); added > 0 {
		report.AddedSince = added
	}
	for _, e := range kit.Snapshot.Manifest.Entries {
		report.KitBytes += e.Size
	}

	// Every credential in the kit, built from the kit's own config rather than
	// from what the vault is running on — the point is to prove the *carried*
	// credential works, not the one already in use.
	holders := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	report.Accounts = make([]KitCredentialResult, len(kit.Accounts))

	for i, cfg := range kit.Accounts {
		report.Accounts[i] = KitCredentialResult{ID: cfg.ID, Name: cfg.Name, Kind: string(cfg.Kind)}
		wg.Add(1)
		go func(i int, cfg provider.Config) {
			defer wg.Done()

			status, detail, _ := v.probeRestored(ctx, cfg)
			mu.Lock()
			report.Accounts[i].Status = status
			report.Accounts[i].Detail = detail
			mu.Unlock()
			if status != KitAccountConnected {
				return
			}

			p, err := v.buildProvider(cfg)
			if err != nil {
				return
			}
			listCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			objects, err := p.List(listCtx, "")
			if err != nil {
				mu.Lock()
				report.Warnings = append(report.Warnings,
					fmt.Sprintf("%s answered but could not be listed, so its parts were not checked: %v",
						cfg.Name, err))
				mu.Unlock()
				return
			}

			mu.Lock()
			report.Listed++
			for _, obj := range objects {
				if obj.Key != BackupKey {
					holders[obj.Key] = true
				}
			}
			mu.Unlock()
		}(i, cfg)
	}
	wg.Wait()

	for _, a := range report.Accounts {
		if a.Status == KitAccountConnected {
			report.Working++
			continue
		}
		report.Unusable++
	}
	report.NotListed = report.Working - report.Listed

	// What the kit alone would bring back if every cloud were also out of
	// reach — the floor, and the number worth putting in front of somebody.
	for _, entry := range kit.Snapshot.Manifest.Entries {
		found := 0
		for _, shard := range entry.Shards {
			if holders[shard.Key] {
				found++
			}
		}
		if found >= entry.Scheme().Data {
			report.Recoverable++
			report.RecoverableBytes += entry.Size
		}
	}

	// An account connected after the kit was made is one the kit cannot
	// restore, because it never held a credential for it.
	known := map[string]bool{}
	for _, cfg := range kit.Accounts {
		known[cfg.ID] = true
	}
	for _, cfg := range live {
		if known[cfg.ID] {
			continue
		}
		report.AccountsAdded = append(report.AccountsAdded, KitCredentialResult{
			ID: cfg.ID, Name: cfg.Name, Kind: string(cfg.Kind),
			Status: "not_in_kit",
			Detail: "connected after this kit was made, so it holds no credentials for it",
		})
	}

	return report, nil
}
