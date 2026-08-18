//go:build windows

package provider

import "golang.org/x/sys/windows"

// diskUsage asks the volume holding path how big it is and how much of it is
// spoken for. See the unix version for what the three figures mean; Windows
// hands back the same three, with the caller's quota already applied to the
// free one.
func diskUsage(path string) (Usage, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return Usage{}, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return Usage{}, err
	}
	used := int64(total) - int64(totalFree)
	if used < 0 {
		used = 0
	}
	return Usage{Used: used, Total: int64(total), Free: int64(free)}, nil
}
