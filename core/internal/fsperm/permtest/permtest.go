// Package permtest exposes a file to accounts other than its owner, so a test
// can check that the credential loader refuses one.
//
// It exists because "readable by others" is not the same act on every
// platform. On Unix it is chmod 0644; on Windows a mode has no such meaning
// and os.Chmod there only toggles the read-only bit, so the exposure a test
// needs has to be a real access control entry for a well-known group. A test
// that wrote 0644 directly would exercise nothing on Windows.
package permtest
