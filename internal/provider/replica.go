package provider

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Role says which of an account's two folders a provider is opened at.
//
// An account holds parts in one folder and, if it has been given one,
// replicas in another. They are the same credential and the same backend;
// the only difference is the root every object key is resolved against. A
// replica is told from a part by the folder it is in and by nothing else —
// not its name, which has the same shape, and not anything written on the
// object — so the two roots have to be kept apart by the opener, and the
// store never holds two accounts for one.
type Role int

const (
	// Parts is the account as it has always been opened: the folder its
	// config names, or the root when it names none.
	Parts Role = iota
	// Replicas is the same account at its ReplicaFolder.
	Replicas
)

func (r Role) String() string {
	if r == Replicas {
		return "replicas"
	}
	return "parts"
}

// Open builds a live provider from a stored config at one of its two roots.
// At the Parts role it is New. At the Replicas role it is New with the
// backend's folder setting swapped for ReplicaFolder, which the config has to
// carry: an account that was never given a replica folder cannot be opened
// for replicas, and says so rather than quietly opening at the parts folder.
func Open(cfg Config, role Role) (Provider, error) {
	if role != Replicas {
		return New(cfg)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.ReplicaFolder) == "" {
		return nil, fmt.Errorf("%s has no replica folder", cfg.Name)
	}
	key, ok := folderKey(cfg.Kind)
	if !ok {
		return nil, fmt.Errorf("%s: a %s account has no folder setting to put replicas in", cfg.Name, cfg.Kind)
	}

	at := cfg
	at.Options = make(map[string]string, len(cfg.Options)+1)
	for k, v := range cfg.Options {
		at.Options[k] = v
	}
	at.Options[key] = cfg.ReplicaFolder
	// Google Drive's pasted folder ID outranks its folder name, so a copy of
	// the options that still carried one would open at the parts folder
	// whatever the name said. Only Drive has the field; deleting a key no
	// backend declares is a no-op everywhere else.
	delete(at.Options, "folder_id")
	return New(at)
}

// folderKeys are the option keys, across every backend, that name where its
// objects live: a folder at the top of an account, a key prefix in a bucket,
// a directory on this machine. A backend has at most one of them, and the
// first one its spec declares is the one that is it.
var folderKeys = []string{"folder", "prefix", "path"}

// folderKey returns the option key that holds a backend's folder.
func folderKey(kind Kind) (string, bool) {
	spec, ok := SpecFor(kind)
	if !ok {
		return "", false
	}
	for _, f := range spec.Fields {
		for _, k := range folderKeys {
			if f.Key == k {
				return k, true
			}
		}
	}
	return "", false
}

// PartsFolder is where this account's parts live, as the backend would see
// it: the folder option with the backend's default filled in, so a Dropbox
// account that never named a folder reads as "sand" rather than as nothing.
// Empty means the root.
func (c Config) PartsFolder() string {
	key, ok := folderKey(c.Kind)
	if !ok {
		return ""
	}
	return WithDefaults(c).Option(key)
}

// Validate refuses a replica folder that could not keep replicas apart from
// parts. A config with no replica folder is always valid: it is every account
// there has ever been.
//
// The folder is the only thing that tells a replica from a part, so it has to
// be a real boundary. Four shapes are not one:
//
//   - the same folder as the parts, which is no boundary at all;
//   - a folder inside the parts folder, which every listing of the parts
//     folder would walk into;
//   - a folder containing the parts folder, the same the other way round;
//   - any folder beside parts kept at the root, since the root is everything
//     the account holds and a listing of it takes the replica folder in.
//
// Folders are compared as the backend would: a directory on this machine by
// its cleaned absolute path, anything else by the name with its slashes
// trimmed, so "sand/" and "sand" are the same folder and "sand-replica" is
// not inside "sand".
func (c Config) Validate() error {
	replica := strings.TrimSpace(c.ReplicaFolder)
	if replica == "" {
		return nil
	}
	key, ok := folderKey(c.Kind)
	if !ok {
		return fmt.Errorf("%s: a %s account has no folder setting to put replicas in", c.Name, c.Kind)
	}
	parts := c.PartsFolder()

	if key == "path" {
		return validateDiskFolders(c.Name, parts, replica)
	}

	parts = strings.Trim(parts, "/")
	replica = strings.Trim(replica, "/")
	switch {
	case replica == "":
		return fmt.Errorf("%s: the replica folder cannot be the root", c.Name)
	case parts == "":
		return fmt.Errorf("%s: parts are kept at the root, which takes in every folder — give the parts a folder of their own first", c.Name)
	case replica == parts:
		return fmt.Errorf("%s: the replica folder is the parts folder", c.Name)
	case strings.HasPrefix(replica, parts+"/"):
		return fmt.Errorf("%s: the replica folder %q is inside the parts folder %q", c.Name, replica, parts)
	case strings.HasPrefix(parts, replica+"/"):
		return fmt.Errorf("%s: the replica folder %q contains the parts folder %q", c.Name, replica, parts)
	}
	return nil
}

// validateDiskFolders is Validate for a backend whose folder is a directory
// on this machine, where "inside" is a question for the path cleaner.
func validateDiskFolders(name, parts, replica string) error {
	partsAbs, err := filepath.Abs(ExpandHome(parts))
	if err != nil {
		return fmt.Errorf("%s: resolving the parts directory %q: %w", name, parts, err)
	}
	replicaAbs, err := filepath.Abs(ExpandHome(replica))
	if err != nil {
		return fmt.Errorf("%s: resolving the replica directory %q: %w", name, replica, err)
	}
	if replicaAbs == partsAbs {
		return fmt.Errorf("%s: the replica directory is the parts directory", name)
	}
	if within(replicaAbs, partsAbs) {
		return fmt.Errorf("%s: the replica directory %q is inside the parts directory %q", name, replicaAbs, partsAbs)
	}
	if within(partsAbs, replicaAbs) {
		return fmt.Errorf("%s: the replica directory %q contains the parts directory %q", name, replicaAbs, partsAbs)
	}
	return nil
}

// within reports whether the cleaned absolute path child lies under parent.
func within(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
