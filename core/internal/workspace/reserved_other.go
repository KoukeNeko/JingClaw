//go:build !windows

package workspace

// escapesByName is a Windows concern. On other systems a path component is
// just a name: no device names are reserved, and a colon is a legal character
// in a filename rather than a drive or stream separator.
func escapesByName(string) bool { return false }
