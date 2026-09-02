//go:build windows

package permtest

import (
	"fmt"
	"runtime"

	"golang.org/x/sys/windows"
)

// Expose adds an access control entry letting the Everyone group read path, so
// that fsperm.EnsureOwnerOnly sees a principal other than the owner and refuses
// it. The list is left unprotected so the entry is genuinely additive to
// whatever the file already carried.
func Expose(path string) error {
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		return fmt.Errorf("permtest: resolve Everyone SID: %w", err)
	}

	var pinner runtime.Pinner
	defer pinner.Unpin()
	pinner.Pin(everyone)

	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_READ,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(everyone),
		},
	}}, nil)
	if err != nil {
		return fmt.Errorf("permtest: build ACL for %s: %w", path, err)
	}

	err = windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil,
	)
	if err != nil {
		return fmt.Errorf("permtest: expose %s: %w", path, err)
	}
	return nil
}
