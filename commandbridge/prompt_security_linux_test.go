//go:build linux

package commandbridge

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLinuxPromptResourceFilesystemAllowlist(t *testing.T) {
	for _, magic := range []uint32{
		unix.EXT4_SUPER_MAGIC,
		unix.XFS_SUPER_MAGIC,
		unix.BTRFS_SUPER_MAGIC,
		unix.TMPFS_MAGIC,
		unix.OVERLAYFS_SUPER_MAGIC,
		unix.RAMFS_MAGIC,
		unix.F2FS_SUPER_MAGIC,
	} {
		if !linuxPromptResourceFilesystemAllowed(magic) {
			t.Errorf("filesystem magic %#x was not allowed", magic)
		}
	}
	for _, magic := range []uint32{
		unix.NFS_SUPER_MAGIC,
		unix.CIFS_SUPER_MAGIC,
		unix.SMB2_SUPER_MAGIC,
		unix.FUSE_SUPER_MAGIC,
		unix.V9FS_MAGIC,
		0x7fffffff,
	} {
		if linuxPromptResourceFilesystemAllowed(magic) {
			t.Errorf("remote or unknown filesystem magic %#x was allowed", magic)
		}
	}
}

func TestLinuxPromptResourceFilesystemAcceptsNativeTempDirectory(t *testing.T) {
	file, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := verifyLinuxPromptResourceLocalFilesystem(file); err != nil {
		t.Fatalf("verify native temporary filesystem: %v", err)
	}
}
