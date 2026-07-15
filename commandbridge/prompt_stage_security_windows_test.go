//go:build windows

package commandbridge

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestSecurePromptResourceStageUsesProtectedInheritablePrivateDACL(t *testing.T) {
	dir := t.TempDir()
	if err := securePromptResourceStage(dir); err != nil {
		t.Fatalf("securePromptResourceStage: %v", err)
	}
	allowed, err := promptStageAllowedSIDs()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("stage DACL is not protected")
	}
	assertPromptStageACLEntries(t, descriptor, allowed, true)

	child := filepath.Join(dir, "input.bin")
	childFile, err := os.OpenFile(child, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyPrivatePromptResourceFile(childFile); err != nil {
		_ = childFile.Close()
		t.Fatalf("verify inherited child DACL before write: %v", err)
	}
	if _, err := childFile.Write([]byte("private")); err != nil {
		_ = childFile.Close()
		t.Fatal(err)
	}
	if err := childFile.Close(); err != nil {
		t.Fatal(err)
	}
	childDescriptor, err := windows.GetNamedSecurityInfo(child, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	assertPromptStageACLEntries(t, childDescriptor, allowed, false)
}

func assertPromptStageACLEntries(t *testing.T, descriptor *windows.SECURITY_DESCRIPTOR, allowed []*windows.SID, requireInheritance bool) {
	t.Helper()
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if dacl == nil || int(dacl.AceCount) != len(allowed) {
		t.Fatalf("DACL entry count = %v, want %d", dacl, len(allowed))
	}
	seen := make([]bool, len(allowed))
	for index := uint16(0); index < dacl.AceCount; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(index), &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !isFullPromptResourceAccess(ace.Mask) {
			t.Fatalf("ACE %d type/mask = %d/%#x, want allow/full control", index, ace.Header.AceType, ace.Mask)
		}
		if requireInheritance {
			want := uint8(windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE)
			if ace.Header.AceFlags&want != want {
				t.Fatalf("ACE %d flags = %#x, want object+container inheritance", index, ace.Header.AceFlags)
			}
		}
		entrySID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		for allowedIndex, sid := range allowed {
			if sid.Equals(entrySID) {
				seen[allowedIndex] = true
			}
		}
	}
	for index, found := range seen {
		if !found {
			t.Fatalf("DACL missing principal %s", allowed[index])
		}
	}
}
