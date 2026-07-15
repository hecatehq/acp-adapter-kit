//go:build darwin

package commandbridge

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDarwinPromptResourcesRejectUnsafeAncestorAndStripInheritedACLs(t *testing.T) {
	parent := t.TempDir()
	cmd := exec.Command("/bin/chmod", "+a", "everyone allow read,list,search,file_inherit,directory_inherit", parent)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install inheritable ACL: %v (%s)", err, output)
	}
	if _, stage, err := preparePromptResources(context.Background(), oneImagePrompt(), PromptResourceLimits{}, parent, nil); stage != nil || err == nil {
		t.Fatalf("prepare result stage=%#v err=%v, want ACL-bearing ancestor rejection", stage, err)
	}
	stagePath := parent + "/inherited-stage"
	if err := os.Mkdir(stagePath, 0o700); err != nil {
		t.Fatal(err)
	}
	stageFile, err := os.Open(stagePath)
	if err != nil {
		t.Fatal(err)
	}
	defer stageFile.Close()
	if err := verifyDarwinPromptResourceACLAbsent(stageFile); err == nil {
		t.Fatal("inherited stage unexpectedly has no ACL before hardening")
	}
	filePath := stagePath + "/input.bin"
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := securePromptResourceDirectoryFile(stageFile, 0o700); err != nil {
		t.Fatalf("strip inherited stage ACL: %v", err)
	}
	if err := securePrivatePromptResourceFile(file, 0o600); err != nil {
		t.Fatalf("strip inherited file ACL before bytes: %v", err)
	}
}

func TestStripDarwinPromptResourceACLRemovesACLFromRetainedFile(t *testing.T) {
	path := t.TempDir() + "/resource.bin"
	if err := os.WriteFile(path, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/chmod", "+a", "everyone allow read", path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install file ACL: %v (%s)", err, output)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := verifyDarwinPromptResourceACLAbsent(file); err == nil {
		t.Fatal("test resource unexpectedly has no ACL before stripping")
	}
	if err := stripDarwinPromptResourceACL(file); err != nil {
		t.Fatalf("strip retained file ACL: %v", err)
	}
	if err := verifyDarwinPromptResourceACLAbsent(file); err != nil {
		t.Fatalf("ACL remains after stripping: %v", err)
	}
}

func TestDarwinPromptResourceFilesystemRequiresLocalMount(t *testing.T) {
	file, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := verifyDarwinPromptResourceLocalFilesystem(file); err != nil {
		t.Fatalf("verify native temporary filesystem: %v", err)
	}
	if darwinPromptResourceFilesystemIsLocal(0) {
		t.Fatal("zero mount flags unexpectedly satisfy local-filesystem policy")
	}
	if !darwinPromptResourceFilesystemIsLocal(unix.MNT_LOCAL) {
		t.Fatal("MNT_LOCAL mount flags were rejected")
	}
}
