package vault

// What this machine can read on its own, answered from the index alone.
//
// A listing is drawn for a machine, and a file whose parts sit on a disk
// plugged into a different machine is not a file this machine can open —
// however whole it is. The browser greys such a row out and says where the
// file can be read instead. That answer must cost nothing: it is asked for
// every row of every folder, and a round trip per row to find out which clouds
// are answering would make the browser unusable. So it is asked of the index
// and the register of machines, both already in memory (instance.go), and
// never of a cloud. Whether the clouds this machine *can* reach are answering
// right now is the health check's question, and a different one.

// Reach says why a file in a listing is out of this machine's reach, and
// where it is not.
type Reach struct {
	// Here counts the distinct parts on accounts this machine reaches, and
	// Needed is how many rebuild the file. Here is less than Needed, or the
	// file would not be in the map.
	Here   int `json:"here"`
	Needed int `json:"needed"`

	// Elsewhere names the other machines that reach enough parts to read the
	// file, most recently seen first. Empty when no machine the index knows of
	// can — the parts are on accounts nobody connected has claimed.
	Elsewhere []string `json:"elsewhere,omitempty"`
}

// reachOf works out a file's reach from one machine: the parts it can rebuild
// from, counted against the file's own code, with the replica counted the
// same way, since a read falls back to it. ok is false when the machine can
// read the file, and the Reach is then meaningless.
//
// here is the set of accounts the machine reaches. Pure, so the same rule
// answers for this machine and, with a peer's account list, for every other.
func reachOf(e *Entry, here map[string]bool) (reach Reach, ok bool) {
	reach.Needed = e.Scheme().Data
	reach.Here = partsWithin(e.Shards, here)
	if reach.Here >= reach.Needed {
		return reach, false
	}
	if r := e.Replica; r != nil && r.Scheme().Data > 0 && partsWithin(r.Shards, here) >= r.Scheme().Data {
		return reach, false
	}
	return reach, true
}

// partsWithin counts the distinct parts of a placement that sit on accounts
// in the set.
func partsWithin(shards []Shard, accounts map[string]bool) int {
	seen := map[int]bool{}
	for _, s := range shards {
		if accounts[s.ProviderID] {
			seen[s.Part] = true
		}
	}
	return len(seen)
}

// outOfReachLocked is the listing's map of the files this machine cannot read
// on its own, by file ID, each saying where it can be. Only for the main vault:
// a sub vault's index does not travel between machines, so every file in one
// is this machine's to read or not as the health check finds. nil when every
// file is within reach, which is every listing on a vault that runs on one
// machine. The caller holds at least the read lock.
func (v *Vault) outOfReachLocked(scope Scope, files []*Entry) map[string]Reach {
	if scope != MainScope || len(files) == 0 {
		return nil
	}
	here := v.reachableIDsLocked()
	peers := v.peersLocked()

	var out map[string]Reach
	for _, e := range files {
		reach, away := reachOf(e, here)
		if !away {
			continue
		}
		for _, peer := range peers {
			theirs := make(map[string]bool, len(peer.Accounts))
			for _, id := range peer.Accounts {
				theirs[id] = true
			}
			if _, awayThere := reachOf(e, theirs); !awayThere {
				reach.Elsewhere = append(reach.Elsewhere, peer.Name)
			}
		}
		if out == nil {
			out = map[string]Reach{}
		}
		out[e.ID] = reach
	}
	return out
}
