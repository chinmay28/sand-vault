//go:build unix

package provider

import "golang.org/x/sys/unix"

// diskUsage asks the filesystem holding path how big it is and how much of it
// is spoken for.
//
// Used counts everything on the volume, not only what SAND put there: a drive
// is shared with whatever else lives on it, and an account card that drew the
// vault's parts as the whole of a 2 TB disk would be describing a machine
// nobody has. Free is Bavail rather than Bfree — the blocks an ordinary user
// may actually spend, which is what the service writing parts is.
func diskUsage(path string) (Usage, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return Usage{}, err
	}
	block := int64(fs.Bsize)
	if block <= 0 {
		return Usage{}, errNoDiskUsage
	}
	total := int64(fs.Blocks) * block
	free := int64(fs.Bavail) * block
	used := total - int64(fs.Bfree)*block
	if used < 0 {
		used = 0
	}
	return Usage{Used: used, Total: total, Free: free}, nil
}
