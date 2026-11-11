package snapshot

import (
	"context"
	"fmt"
	"io"
)

// Store is somewhere part files are kept: a folder on a cloud account, a
// directory on a machine. It is the whole of what this package asks of the
// outside world.
//
// Everything a Store is handed is already sealed. It never sees a file's
// name or a byte of its contents, only part files and the names this package
// gives them.
type Store interface {
	// Create begins a part. Nothing written to it is visible under any name
	// until it is committed.
	//
	// The name is given at the end rather than the start because it is not
	// known at the start: a part is named differently when it turns out to
	// be the last, and whether it is the last is known only once the stream
	// has ended or the next part has begun.
	Create(ctx context.Context) (Part, error)

	// Open reads one part back from its first byte.
	Open(ctx context.Context, name string) (io.ReadCloser, error)

	// List answers with everything in the store. What is not a snapshot's
	// part is passed over by the caller, so a store need not filter.
	List(ctx context.Context) ([]Object, error)

	// Remove erases one part. Removing a part that is not there is not an
	// error.
	Remove(ctx context.Context, name string) error
}

// Part is one part file being written.
type Part interface {
	io.Writer

	// Commit finishes the part and makes it visible under name. It is the
	// whole part or nothing: a commit that fails leaves no file under that
	// name.
	Commit(name string) error

	// Abort discards the part. Safe after a failed write or a failed commit.
	Abort() error
}

// Object is one file in a store.
type Object struct {
	Name string
	Size int64
}

// List answers with the snapshots a store holds, newest first.
func List(ctx context.Context, store Store) ([]Set, error) {
	objects, err := store.List(ctx)
	if err != nil {
		return nil, err
	}
	return group(objects), nil
}

// Find is one snapshot of a store, by name.
func Find(ctx context.Context, store Store, name string) (Set, error) {
	sets, err := List(ctx, store)
	if err != nil {
		return Set{}, err
	}
	for _, set := range sets {
		if set.Name == name {
			return set, nil
		}
	}
	return Set{}, fmt.Errorf("no snapshot called %s is there", name)
}

// Remove erases every part of one snapshot, and reports the first part that
// would not go — having tried them all, so one stubborn part does not leave
// the rest behind.
func Remove(ctx context.Context, store Store, set Set) error {
	var first error
	for _, name := range set.Parts {
		if err := store.Remove(ctx, name); err != nil && first == nil {
			first = fmt.Errorf("removing %s: %w", name, err)
		}
	}
	return first
}
