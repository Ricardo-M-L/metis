//go:build windows

package mcpoauth

import "golang.org/x/sys/windows"

func secureTokenStoreDirectory(path string) error {
	return setCurrentUserOnlyACL(path)
}

func secureTokenStoreFile(path string) error {
	return setCurrentUserOnlyACL(path)
}

// setCurrentUserOnlyACL replaces inherited permissions with a protected DACL
// containing exactly the current process user's SID. Windows chmod bits do
// not provide a confidentiality boundary, so failure is terminal rather than
// silently persisting bearer/refresh credentials under an unknown ACL.
func setCurrentUserOnlyACL(path string) error {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	access := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}
	acl, err := windows.ACLFromEntries(access, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	)
}
