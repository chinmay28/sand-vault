package provider

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func init() {
	Register(Spec{
		Kind:        KindLocal,
		Label:       "Local folder",
		Description: "A directory on this machine or a mounted network/removable drive. Useful as an offline third leg alongside two cloud accounts.",
		Fields: []FieldSpec{
			{
				Key:         "path",
				Label:       "Directory",
				Placeholder: "/mnt/backup/sand",
				Help:        "Created if it does not exist.",
				Required:    true,
			},
		},
	}, func(cfg Config) (Provider, error) {
		root := cfg.Option("path")
		abs, err := filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("resolving %q: %w", root, err)
		}
		return &localProvider{base: base{cfg: cfg}, root: abs}, nil
	})
}

// localProvider stores shards as files under a root directory.
type localProvider struct {
	base
	root string
}

// resolve maps an object key onto a path inside the root, refusing any key
// that would escape it.
func (p *localProvider) resolve(key string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash("/" + strings.TrimPrefix(key, "/")))
	full := filepath.Join(p.root, clean)
	if full != p.root && !strings.HasPrefix(full, p.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid key %q", key)
	}
	return full, nil
}

func (p *localProvider) Put(ctx context.Context, key string, data []byte) error {
	full, err := p.resolve(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}

	// Write to a temp file in the destination directory, then rename, so a
	// crash mid-write can never leave a half-written shard behind.
	tmp, err := os.CreateTemp(filepath.Dir(full), ".sand-tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("writing shard: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("closing shard: %w", err)
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("setting shard permissions: %w", err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("finalizing shard: %w", err)
	}
	return nil
}

func (p *localProvider) Get(ctx context.Context, key string) ([]byte, error) {
	full, err := p.resolve(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(full)
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (p *localProvider) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	full, err := p.resolve(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	info, err := os.Stat(full)
	if os.IsNotExist(err) {
		return ObjectInfo{}, ErrNotFound
	}
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Key: key, Size: info.Size()}, nil
}

func (p *localProvider) Delete(ctx context.Context, key string) error {
	full, err := p.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
		return err
	}
	// Prune the now-possibly-empty parent, ignoring failures.
	os.Remove(filepath.Dir(full))
	return nil
}

func (p *localProvider) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	err := filepath.WalkDir(p.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(p.root, path)
		if relErr != nil {
			return nil
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		out = append(out, ObjectInfo{Key: key, Size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (p *localProvider) Ping(ctx context.Context) error {
	if err := os.MkdirAll(p.root, 0700); err != nil {
		return fmt.Errorf("cannot use %s: %w", p.root, err)
	}
	probe := filepath.Join(p.root, ".sand-write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0600); err != nil {
		return fmt.Errorf("%s is not writable: %w", p.root, err)
	}
	return os.Remove(probe)
}
