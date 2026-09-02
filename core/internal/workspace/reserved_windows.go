//go:build windows

package workspace

import (
	"path/filepath"
	"strings"
)

// reservedDeviceNames are opened as the device from any directory on Windows,
// so joining one onto the root does not place it inside: read_file("CON") and
// write_file("CON") reach the console device wherever the workspace is. The
// name Windows matches is the part before the first dot, case-insensitively,
// so "NUL.txt" is the NUL device too.
var reservedDeviceNames = map[string]struct{}{
	"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {},
	"COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {},
	"LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
	"CONIN$": {}, "CONOUT$": {},
}

// escapesByName reports whether any component of a cleaned, relative path names
// something Windows resolves specially rather than as a file under the root.
//
// A lexically contained path still reaches outside when a component is a
// reserved device name, or contains a colon — which is either a drive-relative
// reference ("C:file", read against the current directory of that drive) or an
// alternate data stream ("f.txt:hidden"). None of those is the plain file the
// join appeared to place inside the workspace.
func escapesByName(cleaned string) bool {
	for _, part := range strings.Split(cleaned, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		if strings.ContainsRune(part, ':') {
			return true
		}
		name := part
		if dot := strings.IndexByte(name, '.'); dot >= 0 {
			name = name[:dot]
		}
		if _, reserved := reservedDeviceNames[strings.ToUpper(name)]; reserved {
			return true
		}
	}
	return false
}
