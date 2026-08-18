//go:build !unix && !windows

package provider

// diskUsage has no answer on a platform with neither statfs nor the Windows
// volume call. The account card then draws what SAND stored and says nothing
// about the drive, exactly as it does for a backend that reports no quota.
func diskUsage(string) (Usage, error) { return Usage{}, errNoDiskUsage }
