// Package fsperm keeps a credential file readable only by its owner, on every
// platform the daemon runs on.
//
// A credential file that other local accounts can read is a credential to
// treat as compromised. On Unix that guarantee is a file mode; there is no
// mode on Windows, so the same guarantee is a discretionary access control
// list. Callers deal in the guarantee, not the mechanism:
//
//   - Restrict tightens a file or directory down to its owner, the way the
//     surrounding code already writes 0600 and 0700 on Unix.
//   - EnsureOwnerOnly reports whether a file already meets that bar, and is
//     what replaces a "mode & 0o077 != 0" check on the load path.
//
// The two are deliberately not symmetric: Restrict narrows to the owner alone,
// while EnsureOwnerOnly also accepts the machine's own accounts on Windows
// (LocalSystem and Administrators), because a file created by an installer or
// left at the directory's inherited permissions is not an exposure the way a
// world-readable one is, and refusing it would break files this package never
// wrote.
package fsperm
