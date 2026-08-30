package tableimage

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// fontDirs is where an operating system keeps typefaces.
//
// Directories rather than files, which is the difference between this and the
// list it replaced. A path to one file is a guess about a distribution's
// packaging, an OS release and what somebody has installed; a directory is
// where those things all end up.
//
// The user's own comes first everywhere. Somebody who installed a typeface
// because the system's would not do meant it.
func fontDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}

	var dirs []string
	add := func(paths ...string) {
		for _, path := range paths {
			if path != "" {
				dirs = append(dirs, path)
			}
		}
	}

	switch runtime.GOOS {
	case "darwin":
		if home != "" {
			add(filepath.Join(home, "Library", "Fonts"))
		}
		add("/Library/Fonts", "/System/Library/Fonts",
			"/System/Library/Fonts/Supplemental")
	case "windows":
		add(filepath.Join(os.Getenv("LOCALAPPDATA"), "Microsoft", "Windows", "Fonts"))
		add(filepath.Join(os.Getenv("windir"), "Fonts"))
	default:
		if home != "" {
			add(filepath.Join(home, ".fonts"),
				filepath.Join(home, ".local", "share", "fonts"))
		}
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			add(filepath.Join(xdg, "fonts"))
		}
		add("/usr/local/share/fonts", "/usr/share/fonts")
	}
	return dirs
}

// installed is every typeface file this machine has, nearest first.
//
// Sorted within each directory so that two runs of the same daemon on the
// same machine pick the same typeface. Walking order is not promised to be
// stable, and a table that changed appearance between restarts would be a
// thing somebody spent an afternoon on.
func installed() []string {
	var found []string

	for _, dir := range fontDirs() {
		var here []string
		// Errors are skipped rather than reported: a directory that is not
		// there is the ordinary case on every platform, since these lists
		// describe several and this machine is one.
		_ = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return nil //nolint:nilerr // an unreadable subtree is not a failure
			}
			switch strings.ToLower(filepath.Ext(path)) {
			case ".ttf", ".otf", ".ttc", ".otc":
				here = append(here, path)
			}
			return nil
		})
		sort.Strings(here)
		found = append(found, here...)
	}
	return found
}

// preferred are tried before the machine is searched.
//
// Not because they are the only ones that work — the search below finds
// whatever is installed — but because parsing a twenty megabyte collection is
// not free, and a hit here means a handful of files are read instead of
// several hundred. Every one of them is a typeface these systems ship, so on
// an ordinary machine the search never runs.
//
// Two kinds: something with Chinese, and something with the marks a status
// column uses. They are rarely the same file. PingFang has 漢 and no ✓ at
// all; the system interface typeface has ✓ ✗ ⚠ and no Chinese.
var preferred = map[string][]string{
	"darwin": {
		"/System/Library/Fonts/PingFang.ttc",
		"/System/Library/Fonts/Hiragino Sans GB.ttc",
		"/System/Library/Fonts/SFNS.ttf",
		"/System/Library/Fonts/Menlo.ttc",
		"/System/Library/Fonts/Apple Symbols.ttf",
	},
	"windows": {
		"C:\\Windows\\Fonts\\msjh.ttc",
		"C:\\Windows\\Fonts\\msyh.ttc",
		"C:\\Windows\\Fonts\\seguisym.ttf",
		"C:\\Windows\\Fonts\\segoeui.ttf",
	},
	"linux": {
		"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
		"/usr/share/fonts/truetype/noto/NotoSansCJK-Regular.ttc",
		"/usr/share/fonts/noto-cjk/NotoSansCJK-Regular.ttc",
		"/usr/share/fonts/truetype/wqy/wqy-zenhei.ttc",
		"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
		"/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf",
	},
}

// searchOrder is the fast path and then everything else.
func searchOrder() []string {
	known := preferred[runtime.GOOS]
	if known == nil {
		known = preferred["linux"]
	}

	order := make([]string, 0, len(known))
	seen := make(map[string]bool, len(known))
	for _, path := range known {
		if _, err := os.Stat(path); err == nil {
			order = append(order, path)
			seen[path] = true
		}
	}
	for _, path := range installed() {
		if !seen[path] {
			order = append(order, path)
		}
	}
	return order
}
