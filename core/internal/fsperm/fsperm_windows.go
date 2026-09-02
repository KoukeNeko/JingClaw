//go:build windows

package fsperm

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// accessAllowedACEType is windows.ACCESS_ALLOWED_ACE_TYPE. Only allow entries
// widen access; deny and audit entries are not an exposure, so the walk in
// EnsureOwnerOnly skips anything that is not this.
const accessAllowedACEType = 0

// Restrict replaces path's DACL with a single entry granting its owner full
// control, and marks the list protected so that no entry inherited from a
// parent directory can grant anyone else access. It is the Windows equivalent
// of chmod 0600 (or 0700 for a directory — a DACL draws no distinction).
func Restrict(path string) error {
	owner, err := currentUserSID()
	if err != nil {
		return err
	}

	// The SID is referenced by the ACL the kernel builds; keep it put for the
	// duration of the two calls that read it.
	var pinner runtime.Pinner
	defer pinner.Unpin()
	pinner.Pin(owner)

	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(owner),
		},
	}}, nil)
	if err != nil {
		return fmt.Errorf("fsperm: build ACL for %s: %w", path, err)
	}

	err = windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil,
	)
	if err != nil {
		return fmt.Errorf("fsperm: restrict %s: %w", path, err)
	}
	return nil
}

// EnsureOwnerOnly reports whether path grants access to nobody but its owner
// and the machine's own accounts — LocalSystem and the Administrators group,
// which can take ownership regardless of what the list says. detail names the
// principal that fails the test when the answer is false; it never contains
// the file's contents.
func EnsureOwnerOnly(path string) (ownerOnly bool, detail string, err error) {
	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return false, "", fmt.Errorf("fsperm: read security of %s: %w", path, err)
	}

	owner, _, err := sd.Owner()
	if err != nil {
		return false, "", fmt.Errorf("fsperm: read owner of %s: %w", path, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return false, "", fmt.Errorf("fsperm: read ACL of %s: %w", path, err)
	}
	// A nil DACL is not an empty one: it grants everyone full access.
	if dacl == nil {
		return false, "has no access control list, which grants everyone access", nil
	}

	trusted, err := trustedSIDs(owner)
	if err != nil {
		return false, "", err
	}

	for index := uint16(0); index < dacl.AceCount; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(index), &ace); err != nil {
			return false, "", fmt.Errorf("fsperm: read ACL entry %d of %s: %w", index, path, err)
		}
		if ace.Header.AceType != accessAllowedACEType || ace.Mask == 0 {
			continue
		}

		granted := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if isTrusted(granted, trusted) {
			continue
		}
		return false, fmt.Sprintf("grants access to %s", granted), nil
	}

	return true, "", nil
}

// currentUserSID is the SID of the account this process runs as, copied out of
// the process token so it outlives the token's buffer.
func currentUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("fsperm: current user: %w", err)
	}
	owner, err := user.User.Sid.Copy()
	if err != nil {
		return nil, fmt.Errorf("fsperm: copy current user SID: %w", err)
	}
	return owner, nil
}

// trustedSIDs are the principals whose access does not count as an exposure:
// the file's own owner, LocalSystem, and the Administrators group.
func trustedSIDs(owner *windows.SID) ([]*windows.SID, error) {
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, fmt.Errorf("fsperm: resolve LocalSystem SID: %w", err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, fmt.Errorf("fsperm: resolve Administrators SID: %w", err)
	}
	return []*windows.SID{owner, system, admins}, nil
}

func isTrusted(sid *windows.SID, trusted []*windows.SID) bool {
	for _, candidate := range trusted {
		if sid.Equals(candidate) {
			return true
		}
	}
	return false
}
