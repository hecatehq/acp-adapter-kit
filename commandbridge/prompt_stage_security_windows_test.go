//go:build windows

package commandbridge

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"github.com/hecatehq/acp-adapter-kit/runtimeacp"
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

func TestWindowsPromptResourceDirectoriesArePrivateAtCreation(t *testing.T) {
	parent, err := preparePromptResourceParent(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, _, rawGuard, err := createPromptResourceStage(parent)
	if err != nil {
		t.Fatal(err)
	}
	guard := rawGuard.(*windowsPromptResourceStageGuard)
	defer func() { _ = guard.Cleanup(nil) }()
	allowed, err := promptStageAllowedSIDs()
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyPromptResourceStageACLHandle(guard.anchor.handle, allowed); err != nil {
		t.Fatalf("anchor ACL immediately after creation: %v", err)
	}
	if err := verifyPromptResourceStageACLHandle(guard.stage.handle, allowed); err != nil {
		t.Fatalf("stage ACL immediately after creation: %v", err)
	}
}

func TestWindowsPromptResourceGuardBlocksReplacementAndRetainsCleanupRetry(t *testing.T) {
	params := runtimeacp.PromptParams{Prompt: []runtimeacp.ContentBlock{{
		Type:     "image",
		MimeType: "image/png",
		Data:     base64.StdEncoding.EncodeToString([]byte("image")),
	}}}
	root := t.TempDir()
	parent := filepath.Join(root, "trusted-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	_, stage, err := preparePromptResources(context.Background(), params, PromptResourceLimits{}, parent, nil)
	if err != nil || stage == nil {
		t.Fatalf("prepare result stage=%#v err=%v", stage, err)
	}
	if err := os.Rename(stage.dir, stage.dir+"-replacement"); err == nil {
		t.Fatal("stage rename succeeded while delete-sharing protection was retained")
	}
	if err := os.Rename(stage.anchor, stage.anchor+"-replacement"); err == nil {
		t.Fatal("anchor rename succeeded while delete-sharing protection was retained")
	}
	if err := os.Rename(parent, parent+"-replacement"); err == nil {
		if restoreErr := os.Rename(parent+"-replacement", parent); restoreErr != nil {
			t.Fatalf("trusted ancestor rename unexpectedly succeeded and could not be restored: %v", restoreErr)
		}
		t.Fatal("trusted ancestor rename succeeded while no-delete-sharing protection was retained")
	}
	attempts := 0
	stage.cleanupHook = func(path string) error {
		attempts++
		if attempts == 1 {
			return errors.New("transient failure")
		}
		return nil
	}
	if err := stage.cleanup(); err == nil {
		t.Fatal("first cleanup succeeded, want transient failure")
	}
	if stage.guard == nil {
		t.Fatal("failed cleanup discarded Windows identity protection")
	}
	if err := stage.cleanup(); err != nil {
		t.Fatalf("retry cleanup: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("cleanup attempts = %d, want 2", attempts)
	}
}

func TestWindowsPromptResourceGuardRetainsFilesWithoutRequiringDeleteSharing(t *testing.T) {
	params := runtimeacp.PromptParams{Prompt: []runtimeacp.ContentBlock{{
		Type:     "image",
		MimeType: "image/png",
		Data:     base64.StdEncoding.EncodeToString([]byte("image")),
	}}}
	prepared, stage, err := preparePromptResources(context.Background(), params, PromptResourceLimits{}, t.TempDir(), nil)
	if err != nil || stage == nil {
		t.Fatalf("prepare result stage=%#v err=%v", stage, err)
	}
	defer func() { _ = stage.cleanup() }()
	directoryReader, err := os.Open(stage.dir)
	if err != nil {
		t.Fatalf("open staged directory without delete sharing: %v", err)
	}
	if _, err := directoryReader.Readdirnames(-1); err != nil {
		_ = directoryReader.Close()
		t.Fatalf("enumerate staged directory without delete sharing: %v", err)
	}
	if err := directoryReader.Close(); err != nil {
		t.Fatal(err)
	}
	path := prepared.Prompt[0].PreparedFile.Path
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := windows.CreateFile(
		path16,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatalf("open staged input without delete sharing: %v", err)
	}
	if err := windows.CloseHandle(reader); err != nil {
		t.Fatal(err)
	}
	if err := stage.verify(); err != nil {
		t.Fatalf("verify after agent-style read: %v", err)
	}
}

func TestWindowsPromptResourceCleanupRestoresGuardAfterReaderBlocksDelete(t *testing.T) {
	params := runtimeacp.PromptParams{Prompt: []runtimeacp.ContentBlock{{
		Type:     "image",
		MimeType: "image/png",
		Data:     base64.StdEncoding.EncodeToString([]byte("image")),
	}}}
	_, stage, err := preparePromptResources(context.Background(), params, PromptResourceLimits{}, t.TempDir(), nil)
	if err != nil || stage == nil {
		t.Fatalf("prepare result stage=%#v err=%v", stage, err)
	}
	reader, err := os.Open(stage.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := stage.cleanup(); err == nil {
		_ = reader.Close()
		t.Fatal("cleanup succeeded while a reader denied delete sharing")
	}
	if stage.guard == nil {
		_ = reader.Close()
		t.Fatal("blocked cleanup discarded restored stage identity protection")
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stage.cleanup(); err != nil {
		t.Fatalf("cleanup after reader closed: %v", err)
	}
}

func TestWindowsPromptResourceCleanupRestoresFileGuardAfterReaderBlocksDelete(t *testing.T) {
	params := runtimeacp.PromptParams{Prompt: []runtimeacp.ContentBlock{{
		Type:     "image",
		MimeType: "image/png",
		Data:     base64.StdEncoding.EncodeToString([]byte("image")),
	}}}
	prepared, stage, err := preparePromptResources(context.Background(), params, PromptResourceLimits{}, t.TempDir(), nil)
	if err != nil || stage == nil {
		t.Fatalf("prepare result stage=%#v err=%v", stage, err)
	}
	reader, err := os.Open(prepared.Prompt[0].PreparedFile.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := stage.cleanup(); err == nil {
		_ = reader.Close()
		t.Fatal("cleanup succeeded while a file reader denied delete sharing")
	}
	if stage.guard == nil {
		_ = reader.Close()
		t.Fatal("blocked cleanup discarded restored file identity protection")
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stage.cleanup(); err != nil {
		t.Fatalf("cleanup after file reader closed: %v", err)
	}
}

func TestWindowsPromptResourceCleanupRefusesUnexpectedStageEntry(t *testing.T) {
	params := runtimeacp.PromptParams{Prompt: []runtimeacp.ContentBlock{{
		Type:     "image",
		MimeType: "image/png",
		Data:     base64.StdEncoding.EncodeToString([]byte("image")),
	}}}
	_, stage, err := preparePromptResources(context.Background(), params, PromptResourceLimits{}, t.TempDir(), nil)
	if err != nil || stage == nil {
		t.Fatalf("prepare result stage=%#v err=%v", stage, err)
	}
	extra := filepath.Join(stage.dir, "unexpected.bin")
	if err := os.WriteFile(extra, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stage.cleanup(); err == nil {
		t.Fatal("cleanup removed a stage containing an unretained entry")
	}
	if stage.guard == nil {
		t.Fatal("failed cleanup discarded exact stage guards")
	}
	if err := os.Remove(extra); err != nil {
		t.Fatal(err)
	}
	if err := stage.cleanup(); err != nil {
		t.Fatalf("cleanup after removing unexpected entry: %v", err)
	}
}

func TestWindowsPromptResourceAncestorRejectsUntrustedDirectDeleteGrant(t *testing.T) {
	dir := t.TempDir()
	handle, err := openWindowsPromptResourceDirectory(dir, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.WRITE_DAC)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle.handle)
	defer func() {
		if err := securePromptResourceDirectoryHandle(handle.handle); err != nil {
			t.Errorf("restore private temporary directory ACL: %v", err)
		}
	}()
	allowed, err := promptStageAllowedSIDs()
	if err != nil {
		t.Fatal(err)
	}
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	entries := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.SET_ACCESS,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(allowed[0]),
			},
		},
		{
			AccessPermissions: windows.DELETE,
			AccessMode:        windows.SET_ACCESS,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(everyone),
			},
		},
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetSecurityInfo(handle.handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if err := verifyWindowsPromptResourceAncestorSecurity(handle.handle); err == nil || !strings.Contains(err.Error(), "untrusted principal") {
		t.Fatalf("ancestor verification error = %v, want untrusted direct-delete grant", err)
	}
}

func TestWindowsPromptResourceParentRejectsRemoteUNCPath(t *testing.T) {
	err := verifyWindowsPromptResourceLocalPath(`\\server\share\prompt-inputs`)
	if err == nil || !strings.Contains(err.Error(), "local drive") {
		t.Fatalf("remote parent error = %v, want local-drive rejection", err)
	}
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
