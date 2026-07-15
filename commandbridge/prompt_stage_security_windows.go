//go:build windows

package commandbridge

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// securePromptResourceStage replaces inherited permissions with a protected
// DACL before prompt bytes are written. Its inheritable entries limit both the
// stage and subsequently created children to the process user and SYSTEM.
func securePromptResourceStage(path string) error {
	allowed, err := promptStageAllowedSIDs()
	if err != nil {
		return fmt.Errorf("resolve prompt stage principals: %w", err)
	}
	entries := make([]windows.EXPLICIT_ACCESS, 0, len(allowed))
	for _, sid := range allowed {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.SET_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("build prompt stage ACL: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	); err != nil {
		return fmt.Errorf("apply prompt stage ACL: %w", err)
	}
	if err := verifyPromptResourceStageACL(path, allowed); err != nil {
		return fmt.Errorf("verify prompt stage ACL: %w", err)
	}
	return nil
}

func promptStageAllowedSIDs() ([]*windows.SID, error) {
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	userSID, err := tokenUser.User.Sid.Copy()
	if err != nil {
		return nil, err
	}
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, err
	}
	if userSID.Equals(systemSID) {
		return []*windows.SID{userSID}, nil
	}
	return []*windows.SID{userSID, systemSID}, nil
}

func verifyPromptResourceStageACL(path string, allowed []*windows.SID) error {
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return verifyPromptResourceACL(
		descriptor,
		allowed,
		true,
		uint8(windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE),
		uint8(windows.INHERITED_ACE),
	)
}

func verifyPrivatePromptResourceFile(file *os.File) error {
	if file == nil {
		return errors.New("staged resource file is nil")
	}
	allowed, err := promptStageAllowedSIDs()
	if err != nil {
		return err
	}
	descriptor, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return verifyPromptResourceACL(descriptor, allowed, false, uint8(windows.INHERITED_ACE), 0)
}

func verifyPromptResourceACL(descriptor *windows.SECURITY_DESCRIPTOR, allowed []*windows.SID, requireProtected bool, requiredFlags, forbiddenFlags uint8) error {
	if descriptor == nil {
		return errors.New("security descriptor is nil")
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return err
	}
	if requireProtected && control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("DACL is not protected")
	}
	dacl, defaulted, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if dacl == nil || defaulted {
		return errors.New("DACL is missing or defaulted")
	}
	if int(dacl.AceCount) != len(allowed) {
		return fmt.Errorf("DACL has %d entries, want %d", dacl.AceCount, len(allowed))
	}
	seen := make([]bool, len(allowed))
	for index := uint16(0); index < dacl.AceCount; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(index), &ace); err != nil {
			return err
		}
		if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New("DACL contains a non-allow entry")
		}
		if ace.Header.AceFlags&requiredFlags != requiredFlags || ace.Header.AceFlags&forbiddenFlags != 0 {
			return errors.New("DACL entry flags do not match the private-stage contract")
		}
		if !isFullPromptResourceAccess(ace.Mask) {
			return errors.New("DACL entry does not grant full control")
		}
		entrySID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		matched := -1
		for allowedIndex, sid := range allowed {
			if sid.Equals(entrySID) {
				matched = allowedIndex
				break
			}
		}
		if matched < 0 || seen[matched] {
			return errors.New("DACL contains an unexpected or duplicate principal")
		}
		seen[matched] = true
	}
	for _, found := range seen {
		if !found {
			return errors.New("DACL is missing an allowed principal")
		}
	}
	return nil
}

func isFullPromptResourceAccess(mask windows.ACCESS_MASK) bool {
	const fileAllAccess windows.ACCESS_MASK = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff
	return mask == windows.GENERIC_ALL || mask == fileAllAccess
}
