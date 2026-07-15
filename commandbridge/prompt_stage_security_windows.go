//go:build windows

package commandbridge

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsPromptResourcePathHandle struct {
	path     string
	handle   windows.Handle
	info     windows.ByHandleFileInformation
	security string
	access   uint32
}

type windowsPromptResourceStageGuard struct {
	ancestors     []windowsPromptResourcePathHandle
	anchor        windowsPromptResourcePathHandle
	stage         windowsPromptResourcePathHandle
	files         map[string]*windowsPromptResourceFileHandle
	expectedFiles int
	stageRemoved  bool
}

type windowsPromptResourceFileHandle struct {
	name   string
	handle windows.Handle
	info   windows.ByHandleFileInformation
}

const (
	windowsPromptResourceShareNoDelete   = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
	windowsPromptResourceShareAll        = windowsPromptResourceShareNoDelete | windows.FILE_SHARE_DELETE
	windowsPromptResourceDeleteChild     = 0x0040
	windowsPromptResourceFullAccess      = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff
	windowsPromptResourceDirectoryAccess = windows.FILE_READ_ATTRIBUTES |
		windows.READ_CONTROL | windows.WRITE_DAC | windows.WRITE_OWNER
	windowsPromptResourceStageAccess = windowsPromptResourceDirectoryAccess |
		windows.FILE_LIST_DIRECTORY | windows.FILE_TRAVERSE
)

type windowsFileFullDirectoryInfo struct {
	NextEntryOffset uint32
	FileIndex       uint32
	CreationTime    int64
	LastAccessTime  int64
	LastWriteTime   int64
	ChangeTime      int64
	EndOfFile       int64
	AllocationSize  int64
	FileAttributes  uint32
	FileNameLength  uint32
	EaSize          uint32
	FileName        [1]uint16
}

func preparePromptResourceParent(raw string) (string, error) {
	if raw == "" {
		raw = os.TempDir()
	}
	if !filepath.IsAbs(raw) {
		return "", errors.New("prompt resource temporary parent must be absolute")
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", err
	}
	if err := verifyWindowsPromptResourceLocalPath(abs); err != nil {
		return "", err
	}
	if err := rejectWindowsPromptResourceReparseAncestors(abs); err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	if err := verifyWindowsPromptResourceLocalPath(canonical); err != nil {
		return "", err
	}
	handles, err := openWindowsPromptResourceAncestors(canonical)
	if err != nil {
		return "", err
	}
	closeWindowsPromptResourceHandles(handles)
	return canonical, nil
}

func verifyWindowsPromptResourceLocalPath(path string) error {
	volume := filepath.VolumeName(path)
	if volume == "" || strings.HasPrefix(volume, `\\`) {
		return errors.New("prompt resource temporary parent must be on a local drive")
	}
	root := volume + `\`
	root16, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return err
	}
	switch windows.GetDriveType(root16) {
	case windows.DRIVE_FIXED, windows.DRIVE_REMOVABLE, windows.DRIVE_RAMDISK:
		return nil
	default:
		return errors.New("prompt resource temporary parent must be on a local drive")
	}
}

func windowsPromptResourcePathPrefixes(path string) ([]string, error) {
	path = filepath.Clean(path)
	volume := filepath.VolumeName(path)
	if volume == "" {
		return nil, errors.New("Windows prompt resource path has no volume")
	}
	root := volume + `\`
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, `..\`) {
		return nil, errors.New("Windows prompt resource path escapes its volume")
	}
	prefixes := []string{root}
	current := root
	if relative != "." {
		for _, component := range strings.Split(relative, `\`) {
			current = filepath.Join(current, component)
			prefixes = append(prefixes, current)
		}
	}
	return prefixes, nil
}

func rejectWindowsPromptResourceReparseAncestors(path string) error {
	prefixes, err := windowsPromptResourcePathPrefixes(path)
	if err != nil {
		return err
	}
	for _, prefix := range prefixes {
		prefix16, convertErr := windows.UTF16PtrFromString(prefix)
		if convertErr != nil {
			return convertErr
		}
		attributes, attributeErr := windows.GetFileAttributes(prefix16)
		if attributeErr != nil {
			return attributeErr
		}
		if attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return errors.New("prompt resource temporary parent ancestors must be non-reparse directories")
		}
	}
	return nil
}

func openWindowsPromptResourceAncestors(path string) ([]windowsPromptResourcePathHandle, error) {
	prefixes, err := windowsPromptResourcePathPrefixes(path)
	if err != nil {
		return nil, err
	}
	handles := make([]windowsPromptResourcePathHandle, 0, len(prefixes))
	for _, prefix := range prefixes {
		handle, openErr := openWindowsPromptResourceDirectory(prefix, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL)
		if openErr != nil {
			closeWindowsPromptResourceHandles(handles)
			return nil, openErr
		}
		if openErr := verifyWindowsPromptResourceAncestorSecurity(handle.handle); openErr != nil {
			_ = windows.CloseHandle(handle.handle)
			closeWindowsPromptResourceHandles(handles)
			return nil, openErr
		}
		handles = append(handles, handle)
	}
	return handles, nil
}

func createPromptResourceStage(parent string) (string, string, promptResourceStageGuard, error) {
	ancestors, err := openWindowsPromptResourceAncestors(parent)
	if err != nil {
		return "", "", nil, err
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			closeWindowsPromptResourceHandles(ancestors)
		}
	}()

	descriptor, err := privateWindowsPromptResourceSecurityDescriptor()
	if err != nil {
		return "", "", nil, err
	}
	anchor, err := createPrivateWindowsPromptResourceDirectory(parent, "acp-commandbridge-private-", descriptor)
	if err != nil {
		return "", "", nil, err
	}
	anchorHandle, err := openWindowsPromptResourceDirectory(anchor, windowsPromptResourceDirectoryAccess)
	if err != nil {
		return "", "", nil, errors.New("open protected prompt resource anchor; a protected remnant may require manual removal")
	}
	allowed, err := promptStageAllowedSIDs()
	if err == nil {
		err = verifyPromptResourceStageACLHandle(anchorHandle.handle, allowed)
	}
	if err != nil {
		_, cleanupErr := deleteExactWindowsPromptResourceDirectory(ancestors[len(ancestors)-1].handle, &anchorHandle)
		closeWindowsPromptResourcePathHandle(&anchorHandle)
		return "", "", nil, errors.Join(fmt.Errorf("verify protected prompt resource anchor at creation: %w", err), cleanupErr)
	}
	stage, err := createPrivateWindowsPromptResourceDirectory(anchor, "inputs-", descriptor)
	if err != nil {
		_, cleanupErr := deleteExactWindowsPromptResourceDirectory(ancestors[len(ancestors)-1].handle, &anchorHandle)
		closeWindowsPromptResourcePathHandle(&anchorHandle)
		return "", "", nil, errors.Join(err, cleanupErr)
	}
	stageHandle, err := openWindowsPromptResourceDirectory(stage, windowsPromptResourceStageAccess)
	if err != nil {
		_ = windows.CloseHandle(anchorHandle.handle)
		return "", "", nil, errors.New("open protected prompt resource stage; a protected remnant may require manual removal")
	}
	if err := verifyPromptResourceStageACLHandle(stageHandle.handle, allowed); err != nil {
		_, stageCleanupErr := deleteExactWindowsPromptResourceDirectory(anchorHandle.handle, &stageHandle)
		_, anchorCleanupErr := deleteExactWindowsPromptResourceDirectory(ancestors[len(ancestors)-1].handle, &anchorHandle)
		closeWindowsPromptResourcePathHandle(&stageHandle)
		closeWindowsPromptResourcePathHandle(&anchorHandle)
		return "", "", nil, errors.Join(errors.New("verify protected prompt resource stage at creation"), err, stageCleanupErr, anchorCleanupErr)
	}
	guard := &windowsPromptResourceStageGuard{ancestors: ancestors, anchor: anchorHandle, stage: stageHandle, files: map[string]*windowsPromptResourceFileHandle{}, expectedFiles: DefaultMaxPromptResourceFiles}
	closeOnError = false
	return anchor, stage, guard, nil
}

func privateWindowsPromptResourceSecurityDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	allowed, err := promptStageAllowedSIDs()
	if err != nil {
		return nil, err
	}
	return privateWindowsPromptResourceSecurityDescriptorFor(allowed)
}

func privateWindowsPromptResourceSecurityDescriptorFor(allowed []*windows.SID) (*windows.SECURITY_DESCRIPTOR, error) {
	return privateWindowsPromptResourceSecurityDescriptorWithFlags(allowed, "OICI")
}

func privateWindowsPromptResourceFileSecurityDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	allowed, err := promptStageAllowedSIDs()
	if err != nil {
		return nil, err
	}
	return privateWindowsPromptResourceSecurityDescriptorWithFlags(allowed, "")
}

func privateWindowsPromptResourceSecurityDescriptorWithFlags(allowed []*windows.SID, inheritance string) (*windows.SECURITY_DESCRIPTOR, error) {
	if len(allowed) == 0 || allowed[0] == nil {
		return nil, errors.New("prompt resource owner is unavailable")
	}
	if inheritance != "" && inheritance != "OICI" {
		return nil, errors.New("unsupported prompt resource ACL inheritance")
	}
	for _, sid := range allowed {
		if sid == nil {
			return nil, errors.New("prompt resource principal is unavailable")
		}
	}
	var sddl strings.Builder
	sddl.WriteString("O:")
	sddl.WriteString(allowed[0].String())
	sddl.WriteString("D:P")
	for _, sid := range allowed {
		sddl.WriteString("(A;")
		sddl.WriteString(inheritance)
		sddl.WriteString(";0x")
		sddl.WriteString(strconv.FormatUint(uint64(uint32(windowsPromptResourceFullAccess)), 16))
		sddl.WriteString(";;;")
		sddl.WriteString(sid.String())
		sddl.WriteByte(')')
	}
	return windows.SecurityDescriptorFromString(sddl.String())
}

func createPrivatePromptResourceFile(path string) (*os.File, error) {
	descriptor, err := privateWindowsPromptResourceFileSecurityDescriptor()
	if err != nil {
		return nil, err
	}
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	attributes := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	handle, err := windows.CreateFile(
		path16,
		windows.GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL,
		windowsPromptResourceShareNoDelete,
		&attributes,
		windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	runtime.KeepAlive(descriptor)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("staged resource must be a non-reparse file")
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("wrap staged resource handle")
	}
	return file, nil
}

func createPrivateWindowsPromptResourceDirectory(parent, prefix string, descriptor *windows.SECURITY_DESCRIPTOR) (string, error) {
	if descriptor == nil {
		return "", errors.New("private prompt resource security descriptor is unavailable")
	}
	attributes := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	for attempt := 0; attempt < 128; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", err
		}
		path := filepath.Join(parent, prefix+hex.EncodeToString(random[:]))
		path16, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return "", err
		}
		if err := windows.CreateDirectory(path16, &attributes); err != nil {
			if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
				continue
			}
			return "", err
		}
		return path, nil
	}
	return "", errors.New("could not allocate a unique private prompt resource directory")
}

func newPromptResourceStageGuard(anchor, stage string) (promptResourceStageGuard, error) {
	ancestors, err := openWindowsPromptResourceAncestors(filepath.Dir(anchor))
	if err != nil {
		return nil, err
	}
	anchorHandle, err := openWindowsPromptResourceDirectory(anchor, windowsPromptResourceDirectoryAccess)
	if err != nil {
		closeWindowsPromptResourceHandles(ancestors)
		return nil, err
	}
	stageHandle, err := openWindowsPromptResourceDirectory(stage, windowsPromptResourceStageAccess)
	if err != nil {
		_ = windows.CloseHandle(anchorHandle.handle)
		closeWindowsPromptResourceHandles(ancestors)
		return nil, err
	}
	return &windowsPromptResourceStageGuard{ancestors: ancestors, anchor: anchorHandle, stage: stageHandle, files: map[string]*windowsPromptResourceFileHandle{}, expectedFiles: DefaultMaxPromptResourceFiles}, nil
}

func (g *windowsPromptResourceStageGuard) SetExpectedFiles(count int) error {
	if g == nil || count < 0 {
		return errors.New("invalid expected prompt resource file count")
	}
	if len(g.files) != 0 {
		return errors.New("expected prompt resource file count cannot change after staging begins")
	}
	g.expectedFiles = count
	return nil
}

func openWindowsPromptResourceDirectory(path string, access uint32) (windowsPromptResourcePathHandle, error) {
	return openWindowsPromptResourceDirectoryShared(path, access, windowsPromptResourceShareNoDelete)
}

func openWindowsPromptResourceDirectoryShared(path string, access, share uint32) (windowsPromptResourcePathHandle, error) {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windowsPromptResourcePathHandle{}, err
	}
	handle, err := windows.CreateFile(
		path16,
		access,
		share,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return windowsPromptResourcePathHandle{}, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return windowsPromptResourcePathHandle{}, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return windowsPromptResourcePathHandle{}, errors.New("prompt resource path must be a non-reparse directory")
	}
	security, err := windowsPromptResourceSecuritySnapshot(handle)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return windowsPromptResourcePathHandle{}, err
	}
	return windowsPromptResourcePathHandle{path: path, handle: handle, info: info, security: security, access: access}, nil
}

func openWindowsPromptResourceDirectoryRelative(parent windows.Handle, path string, access, share uint32) (windowsPromptResourcePathHandle, error) {
	name := filepath.Base(path)
	if parent == windows.InvalidHandle || name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return windowsPromptResourcePathHandle{}, errors.New("invalid prompt resource directory identity")
	}
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windowsPromptResourcePathHandle{}, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	var status windows.IO_STATUS_BLOCK
	var handle windows.Handle
	if err := windows.NtCreateFile(
		&handle,
		access,
		attributes,
		&status,
		nil,
		0,
		share,
		windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT,
		0,
		0,
	); err != nil {
		return windowsPromptResourcePathHandle{}, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return windowsPromptResourcePathHandle{}, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return windowsPromptResourcePathHandle{}, errors.New("prompt resource path must be a non-reparse directory")
	}
	security, err := windowsPromptResourceSecuritySnapshot(handle)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return windowsPromptResourcePathHandle{}, err
	}
	return windowsPromptResourcePathHandle{path: path, handle: handle, info: info, security: security, access: access}, nil
}

func openWindowsPromptResourceFileRelative(stage windows.Handle, name string, access, share uint32) (*windowsPromptResourceFileHandle, error) {
	if stage == windows.InvalidHandle || name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return nil, errors.New("invalid staged resource identity")
	}
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: stage,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	var status windows.IO_STATUS_BLOCK
	var handle windows.Handle
	if err := windows.NtCreateFile(
		&handle,
		access,
		attributes,
		&status,
		nil,
		0,
		share,
		windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT,
		0,
		0,
	); err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("staged resource must be a non-reparse file")
	}
	return &windowsPromptResourceFileHandle{name: name, handle: handle, info: info}, nil
}

func windowsPromptResourceNameKey(name string) string {
	return strings.ToLower(name)
}

func listWindowsPromptResourceStageEntries(stage windows.Handle, expectedFiles int) ([]string, error) {
	if stage == windows.InvalidHandle {
		return nil, errors.New("prompt resource stage handle is unavailable")
	}
	if expectedFiles < 0 {
		return nil, errors.New("invalid expected prompt resource file count")
	}
	buffer := make([]byte, 64*1024)
	class := uint32(windows.FileFullDirectoryRestartInfo)
	var names []string
	for {
		err := windows.GetFileInformationByHandleEx(stage, class, &buffer[0], uint32(len(buffer)))
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return names, nil
		}
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) && class == windows.FileFullDirectoryRestartInfo && len(names) == 0 {
			// Some filesystem drivers report an empty directory this way for the
			// restart information class. The retained handle was already validated
			// above, so this cannot be confused with a missing stage path.
			return names, nil
		}
		if err != nil {
			return nil, err
		}
		class = windows.FileFullDirectoryInfo
		batch, err := decodeWindowsPromptResourceDirectoryEntries(buffer, expectedFiles-len(names))
		if err != nil {
			return nil, err
		}
		names = append(names, batch...)
		if len(names) > expectedFiles {
			return nil, errors.New("prompt resource stage contains unexpected entries")
		}
	}
}

func decodeWindowsPromptResourceDirectoryEntries(buffer []byte, remainingExpected int) ([]string, error) {
	if remainingExpected < 0 {
		return nil, errors.New("prompt resource stage contains unexpected entries")
	}
	entryHeaderBytes := int(unsafe.Offsetof(windowsFileFullDirectoryInfo{}.FileName))
	var names []string
	for offset := 0; ; {
		if offset < 0 || offset > len(buffer) || entryHeaderBytes > len(buffer)-offset {
			return nil, errors.New("malformed Windows prompt resource directory listing")
		}
		entry := (*windowsFileFullDirectoryInfo)(unsafe.Pointer(&buffer[offset]))
		nameBytes64 := uint64(entry.FileNameLength)
		availableNameBytes := len(buffer) - offset - entryHeaderBytes
		if nameBytes64 == 0 || nameBytes64%2 != 0 || nameBytes64 > uint64(availableNameBytes) {
			return nil, errors.New("malformed Windows prompt resource entry name")
		}
		nameBytes := int(nameBytes64)
		nameUnits := unsafe.Slice((*uint16)(unsafe.Pointer(&buffer[offset+entryHeaderBytes])), nameBytes/2)
		name := string(utf16.Decode(nameUnits))
		if name != "." && name != ".." {
			if name == "" || strings.ContainsAny(name, `/\`) {
				return nil, errors.New("prompt resource stage contains an invalid entry name")
			}
			if len(names) >= remainingExpected {
				return nil, errors.New("prompt resource stage contains unexpected entries")
			}
			names = append(names, name)
		}
		if entry.NextEntryOffset == 0 {
			return names, nil
		}
		delta := uint64(entry.NextEntryOffset)
		minimumDelta := uint64(entryHeaderBytes) + nameBytes64
		if delta < minimumDelta || delta%8 != 0 || delta > uint64(len(buffer)-offset) {
			return nil, errors.New("malformed Windows prompt resource directory offset")
		}
		offset += int(delta)
	}
}

func (g *windowsPromptResourceStageGuard) Secure() error {
	if g == nil {
		return errors.New("prompt resource stage protection is unavailable")
	}
	if err := securePromptResourceDirectoryHandle(g.anchor.handle); err != nil {
		return fmt.Errorf("secure private anchor: %w", err)
	}
	if err := securePromptResourceDirectoryHandle(g.stage.handle); err != nil {
		return fmt.Errorf("secure private stage: %w", err)
	}
	if err := refreshWindowsPromptResourceSecurity(&g.anchor); err != nil {
		return err
	}
	if err := refreshWindowsPromptResourceSecurity(&g.stage); err != nil {
		return err
	}
	return g.Verify()
}

func (g *windowsPromptResourceStageGuard) ProtectFile(path string) error {
	if g == nil || g.stage.handle == windows.InvalidHandle || !strings.EqualFold(filepath.Clean(filepath.Dir(path)), filepath.Clean(g.stage.path)) {
		return errors.New("staged resource is outside the protected stage")
	}
	name := filepath.Base(path)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return errors.New("staged resource has an invalid name")
	}
	file, err := openWindowsPromptResourceFileRelative(g.stage.handle, name, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, windowsPromptResourceShareNoDelete)
	if err != nil {
		return err
	}
	if err := verifyPrivatePromptResourceFileHandle(file.handle); err != nil {
		_ = windows.CloseHandle(file.handle)
		return err
	}
	key := windowsPromptResourceNameKey(name)
	if g.files == nil {
		g.files = map[string]*windowsPromptResourceFileHandle{}
	}
	if _, exists := g.files[key]; exists {
		_ = windows.CloseHandle(file.handle)
		return errors.New("staged resource identity is already protected")
	}
	g.files[key] = file
	return nil
}

func (g *windowsPromptResourceStageGuard) Seal() error { return g.Verify() }

func securePromptResourceDirectoryHandle(handle windows.Handle) error {
	allowed, err := promptStageAllowedSIDs()
	if err != nil {
		return fmt.Errorf("resolve prompt stage principals: %w", err)
	}
	descriptor, err := privateWindowsPromptResourceSecurityDescriptorFor(allowed)
	if err != nil {
		return fmt.Errorf("build prompt stage security descriptor: %w", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return fmt.Errorf("read prompt stage owner: %w", err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return fmt.Errorf("read prompt stage DACL: %w", err)
	}
	if owner == nil || dacl == nil {
		return errors.New("prompt stage security descriptor is incomplete")
	}
	setErr := windows.SetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner,
		nil,
		dacl,
		nil,
	)
	// owner and dacl point into descriptor-owned memory.
	runtime.KeepAlive(descriptor)
	if setErr != nil {
		return fmt.Errorf("apply prompt stage owner and ACL: %w", setErr)
	}
	return verifyPromptResourceStageACLHandle(handle, allowed)
}

func (g *windowsPromptResourceStageGuard) Verify() error {
	if err := g.verifyIdentity(); err != nil {
		return err
	}
	allowed, err := promptStageAllowedSIDs()
	if err != nil {
		return err
	}
	if err := verifyPromptResourceStageACLHandle(g.anchor.handle, allowed); err != nil {
		return err
	}
	if !g.stageRemoved {
		if err := verifyPromptResourceStageACLHandle(g.stage.handle, allowed); err != nil {
			return err
		}
		return g.verifyFiles()
	}
	return nil
}

func (g *windowsPromptResourceStageGuard) verifyFiles() error {
	if g == nil || g.stage.handle == windows.InvalidHandle {
		return errors.New("prompt resource stage handle is unavailable")
	}
	entries, err := listWindowsPromptResourceStageEntries(g.stage.handle, g.expectedFiles)
	if err != nil {
		return err
	}
	if len(entries) != len(g.files) {
		return errors.New("prompt resource stage contents changed")
	}
	seen := make(map[string]struct{}, len(entries))
	for _, name := range entries {
		key := windowsPromptResourceNameKey(name)
		target := g.files[key]
		if target == nil {
			return errors.New("prompt resource stage contains an unexpected entry")
		}
		if _, duplicate := seen[key]; duplicate {
			return errors.New("prompt resource stage contains duplicate case-insensitive names")
		}
		seen[key] = struct{}{}
		if err := verifyWindowsPromptResourceFileHandle(g.stage.handle, target); err != nil {
			return err
		}
	}
	return nil
}

func verifyWindowsPromptResourceFileHandle(stage windows.Handle, expected *windowsPromptResourceFileHandle) error {
	if expected == nil || expected.handle == windows.InvalidHandle {
		return errors.New("staged resource handle is unavailable")
	}
	var current windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(expected.handle, &current); err != nil {
		return err
	}
	if !samePromptResourceFileID(expected.info, current) || current.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 || current.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("staged resource changed identity")
	}
	relative, err := openWindowsPromptResourceFileRelative(stage, expected.name, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, windowsPromptResourceShareNoDelete)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(relative.handle)
	if !samePromptResourceFileID(expected.info, relative.info) {
		return errors.New("staged resource path was replaced")
	}
	return verifyPrivatePromptResourceFileHandle(expected.handle)
}

func (g *windowsPromptResourceStageGuard) verifyIdentity() error {
	if g == nil || g.anchor.handle == windows.InvalidHandle {
		return errors.New("prompt resource stage protection is unavailable")
	}
	for _, handle := range g.ancestors {
		if err := verifyWindowsPromptResourcePathHandle(handle); err != nil {
			return err
		}
		if err := verifyWindowsPromptResourceAncestorSecurity(handle.handle); err != nil {
			return err
		}
	}
	if err := verifyWindowsPromptResourcePathHandle(g.anchor); err != nil {
		return err
	}
	if !g.stageRemoved {
		if err := verifyWindowsPromptResourcePathHandle(g.stage); err != nil {
			return err
		}
	}
	return nil
}

func verifyWindowsPromptResourcePathHandle(expected windowsPromptResourcePathHandle) error {
	if expected.handle == windows.InvalidHandle {
		return errors.New("prompt resource path handle is unavailable")
	}
	var current windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(expected.handle, &current); err != nil {
		return err
	}
	if !samePromptResourceFileID(expected.info, current) || current.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || current.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("protected prompt resource path changed identity")
	}
	pathHandle, err := openWindowsPromptResourceDirectoryShared(expected.path, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, windowsPromptResourceShareAll)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(pathHandle.handle)
	if !samePromptResourceFileID(expected.info, pathHandle.info) {
		return errors.New("prompt resource path was replaced")
	}
	security, err := windowsPromptResourceSecuritySnapshot(expected.handle)
	if err != nil {
		return err
	}
	if expected.security == "" || security != expected.security {
		return errors.New("prompt resource path owner or DACL changed")
	}
	return nil
}

func refreshWindowsPromptResourceSecurity(target *windowsPromptResourcePathHandle) error {
	if target == nil || target.handle == windows.InvalidHandle {
		return errors.New("prompt resource path handle is unavailable")
	}
	security, err := windowsPromptResourceSecuritySnapshot(target.handle)
	if err != nil {
		return err
	}
	target.security = security
	return nil
}

func windowsPromptResourceSecuritySnapshot(handle windows.Handle) (string, error) {
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return "", err
	}
	return descriptor.String(), nil
}

func verifyWindowsPromptResourceAncestorSecurity(handle windows.Handle) error {
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	if owner == nil || !trustedWindowsPromptResourcePrincipal(owner, user.User.Sid) {
		return errors.New("prompt resource ancestor has an untrusted owner")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		return errors.New("prompt resource ancestor has an unprotected null DACL")
	}
	const dangerous = windows.DELETE | windowsPromptResourceDeleteChild | windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_ALL
	for index := uint16(0); index < dacl.AceCount; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(index), &ace); err != nil {
			return err
		}
		if ace == nil || ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || ace.Mask&dangerous == 0 || ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New("prompt resource ancestor has an unsupported dangerous allow ACE")
		}
		principal := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !trustedWindowsPromptResourcePrincipal(principal, user.User.Sid) {
			return errors.New("prompt resource ancestor grants dangerous access to an untrusted principal")
		}
	}
	return nil
}

func trustedWindowsPromptResourcePrincipal(principal, user *windows.SID) bool {
	if principal == nil {
		return false
	}
	if user != nil && principal.Equals(user) {
		return true
	}
	if principal.IsWellKnown(windows.WinLocalSystemSid) || principal.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
		return true
	}
	// TrustedInstaller owns protected Windows system ancestors on standard
	// installations and is part of the trusted computing base.
	return principal.String() == "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
}

func samePromptResourceFileID(left, right windows.ByHandleFileInformation) bool {
	return left.VolumeSerialNumber == right.VolumeSerialNumber && left.FileIndexHigh == right.FileIndexHigh && left.FileIndexLow == right.FileIndexLow
}

func (g *windowsPromptResourceStageGuard) Cleanup(beforeRemove func(string) error) error {
	if err := g.verifyIdentity(); err != nil {
		return fmt.Errorf("verify stage identity before cleanup: %w", err)
	}
	if beforeRemove != nil {
		if err := beforeRemove(g.anchor.path); err != nil {
			return err
		}
		if err := g.verifyIdentity(); err != nil {
			return fmt.Errorf("verify stage identity after cleanup hook: %w", err)
		}
	}
	if !g.stageRemoved {
		if err := securePromptResourceDirectoryHandle(g.anchor.handle); err != nil {
			return err
		}
		if err := securePromptResourceDirectoryHandle(g.stage.handle); err != nil {
			return err
		}
		if err := refreshWindowsPromptResourceSecurity(&g.anchor); err != nil {
			return err
		}
		if err := refreshWindowsPromptResourceSecurity(&g.stage); err != nil {
			return err
		}
		if err := g.removeFiles(); err != nil {
			return fmt.Errorf("remove private prompt resource contents: %w", err)
		}
		removed, err := deleteExactWindowsPromptResourceDirectory(g.anchor.handle, &g.stage)
		if removed {
			g.stageRemoved = true
		}
		if err != nil {
			return fmt.Errorf("remove exact private prompt resource stage: %w", err)
		}
	}
	if len(g.ancestors) == 0 {
		return errors.New("prompt resource anchor parent handle is unavailable")
	}
	removed, err := deleteExactWindowsPromptResourceDirectory(g.ancestors[len(g.ancestors)-1].handle, &g.anchor)
	if err != nil {
		return fmt.Errorf("remove private prompt resource anchor: %w", err)
	}
	if !removed {
		return errors.New("private prompt resource anchor was not removed")
	}
	closeWindowsPromptResourceHandles(g.ancestors)
	g.ancestors = nil
	return nil
}

func (g *windowsPromptResourceStageGuard) Abandon() error {
	if g == nil {
		return nil
	}
	var result error
	for key, target := range g.files {
		if target != nil && target.handle != windows.InvalidHandle {
			result = errors.Join(result, windows.CloseHandle(target.handle))
			target.handle = windows.InvalidHandle
		}
		delete(g.files, key)
	}
	for _, target := range []*windowsPromptResourcePathHandle{&g.stage, &g.anchor} {
		if target.handle != windows.InvalidHandle {
			result = errors.Join(result, windows.CloseHandle(target.handle))
			target.handle = windows.InvalidHandle
		}
	}
	for index := len(g.ancestors) - 1; index >= 0; index-- {
		if g.ancestors[index].handle != windows.InvalidHandle {
			result = errors.Join(result, windows.CloseHandle(g.ancestors[index].handle))
			g.ancestors[index].handle = windows.InvalidHandle
		}
	}
	g.ancestors = nil
	return result
}

func (g *windowsPromptResourceStageGuard) removeFiles() error {
	if err := g.verifyFiles(); err != nil {
		return err
	}
	entries, err := listWindowsPromptResourceStageEntries(g.stage.handle, g.expectedFiles)
	if err != nil {
		return err
	}
	for _, name := range entries {
		key := windowsPromptResourceNameKey(name)
		target := g.files[key]
		if target == nil {
			return errors.New("prompt resource stage contains an unexpected entry")
		}
		removed, err := deleteExactWindowsPromptResourceFile(g.stage.handle, target)
		if removed {
			delete(g.files, key)
		}
		if err != nil {
			return err
		}
	}
	remaining, err := listWindowsPromptResourceStageEntries(g.stage.handle, g.expectedFiles)
	if err != nil {
		return err
	}
	if len(remaining) != 0 || len(g.files) != 0 {
		return errors.New("prompt resource stage is not empty after exact cleanup")
	}
	return nil
}

func deleteExactWindowsPromptResourceFile(stage windows.Handle, target *windowsPromptResourceFileHandle) (bool, error) {
	return deleteExactWindowsPromptResourceFileWithTransition(stage, target, nil)
}

func deleteExactWindowsPromptResourceFileWithTransition(stage windows.Handle, target *windowsPromptResourceFileHandle, afterHandoff func() error) (bool, error) {
	if err := verifyWindowsPromptResourceFileHandle(stage, target); err != nil {
		return false, err
	}
	handoff, err := openWindowsPromptResourceFileRelative(stage, target.name, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, windowsPromptResourceShareAll)
	if err != nil {
		return false, fmt.Errorf("open staged resource identity handoff: %w", err)
	}
	if !samePromptResourceFileID(target.info, handoff.info) {
		_ = windows.CloseHandle(handoff.handle)
		return false, errors.New("staged resource path changed before cleanup handoff")
	}
	if err := verifyPrivatePromptResourceFileHandle(handoff.handle); err != nil {
		_ = windows.CloseHandle(handoff.handle)
		return false, err
	}
	if err := windows.CloseHandle(target.handle); err != nil {
		_ = windows.CloseHandle(handoff.handle)
		return false, fmt.Errorf("close retained staged resource handle: %w", err)
	}
	target.handle = handoff.handle
	if afterHandoff != nil {
		if err := afterHandoff(); err != nil {
			restoreErr := restoreWindowsPromptResourceFileNoDeleteGuard(stage, target)
			return false, errors.Join(err, restoreErr)
		}
	}
	deleteHandle, err := openWindowsPromptResourceFileRelative(stage, target.name, windows.DELETE|windows.FILE_READ_ATTRIBUTES, windowsPromptResourceShareNoDelete)
	if err != nil {
		restoreErr := restoreWindowsPromptResourceFileNoDeleteGuard(stage, target)
		return false, errors.Join(err, restoreErr)
	}
	if !samePromptResourceFileID(target.info, deleteHandle.info) {
		closeErr := windows.CloseHandle(deleteHandle.handle)
		return false, errors.Join(errors.New("staged resource path was replaced before exact cleanup"), closeErr)
	}
	if err := markWindowsPromptResourceHandleForDeletion(deleteHandle.handle); err != nil {
		closeErr := windows.CloseHandle(deleteHandle.handle)
		restoreErr := restoreWindowsPromptResourceFileNoDeleteGuard(stage, target)
		return false, errors.Join(err, closeErr, restoreErr)
	}
	if err := windows.CloseHandle(deleteHandle.handle); err != nil {
		return false, fmt.Errorf("close exact staged resource deletion handle: %w", err)
	}
	if err := windows.CloseHandle(target.handle); err != nil {
		return false, fmt.Errorf("close staged resource identity handoff: %w", err)
	}
	target.handle = windows.InvalidHandle
	return true, nil
}

func restoreWindowsPromptResourceFileNoDeleteGuard(stage windows.Handle, target *windowsPromptResourceFileHandle) error {
	if target == nil || target.handle == windows.InvalidHandle {
		return errors.New("staged resource identity handoff is unavailable")
	}
	restored, err := openWindowsPromptResourceFileRelative(stage, target.name, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, windowsPromptResourceShareNoDelete)
	if err != nil {
		return fmt.Errorf("restore retained staged resource identity: %w", err)
	}
	if !samePromptResourceFileID(target.info, restored.info) {
		_ = windows.CloseHandle(restored.handle)
		return errors.New("staged resource was replaced while restoring cleanup protection")
	}
	if err := verifyPrivatePromptResourceFileHandle(restored.handle); err != nil {
		_ = windows.CloseHandle(restored.handle)
		return err
	}
	if err := windows.CloseHandle(target.handle); err != nil {
		_ = windows.CloseHandle(restored.handle)
		return fmt.Errorf("close staged resource identity handoff: %w", err)
	}
	target.handle = restored.handle
	return nil
}

func deleteExactWindowsPromptResourceDirectory(parent windows.Handle, target *windowsPromptResourcePathHandle) (bool, error) {
	return deleteExactWindowsPromptResourceDirectoryWithTransition(parent, target, nil)
}

func deleteExactWindowsPromptResourceDirectoryWithTransition(parent windows.Handle, target *windowsPromptResourcePathHandle, afterHandoff func() error) (bool, error) {
	if parent == windows.InvalidHandle || target == nil || target.handle == windows.InvalidHandle {
		return false, errors.New("prompt resource directory handle is unavailable")
	}
	original := *target
	if err := verifyWindowsPromptResourcePathHandle(original); err != nil {
		return false, err
	}
	handoff, err := openWindowsPromptResourceDirectoryRelative(parent, original.path, original.access, windowsPromptResourceShareAll)
	if err != nil {
		return false, fmt.Errorf("open prompt resource directory identity handoff: %w", err)
	}
	if !samePromptResourceFileID(original.info, handoff.info) || original.security == "" || handoff.security != original.security {
		_ = windows.CloseHandle(handoff.handle)
		return false, errors.New("prompt resource directory changed before cleanup handoff")
	}
	if err := windows.CloseHandle(original.handle); err != nil {
		_ = windows.CloseHandle(handoff.handle)
		return false, fmt.Errorf("close retained prompt resource directory handle: %w", err)
	}
	*target = handoff
	if afterHandoff != nil {
		if err := afterHandoff(); err != nil {
			restoreErr := restoreWindowsPromptResourceDirectoryNoDeleteGuard(parent, target)
			return false, errors.Join(err, restoreErr)
		}
	}
	deleteHandle, err := openWindowsPromptResourceDirectoryRelative(parent, original.path, original.access|windows.DELETE, windowsPromptResourceShareNoDelete)
	if err != nil {
		restoreErr := restoreWindowsPromptResourceDirectoryNoDeleteGuard(parent, target)
		return false, errors.Join(err, restoreErr)
	}
	if !samePromptResourceFileID(original.info, deleteHandle.info) || original.security == "" || deleteHandle.security != original.security {
		closeErr := windows.CloseHandle(deleteHandle.handle)
		return false, errors.Join(errors.New("a replacement appeared before deleting the exact prompt resource directory"), closeErr)
	}
	if err := markWindowsPromptResourceHandleForDeletion(deleteHandle.handle); err != nil {
		closeErr := windows.CloseHandle(deleteHandle.handle)
		restoreErr := restoreWindowsPromptResourceDirectoryNoDeleteGuard(parent, target)
		return false, errors.Join(err, closeErr, restoreErr)
	}
	if err := windows.CloseHandle(deleteHandle.handle); err != nil {
		return false, fmt.Errorf("close exact prompt resource directory deletion handle: %w", err)
	}
	if err := windows.CloseHandle(target.handle); err != nil {
		return false, fmt.Errorf("close prompt resource directory identity handoff: %w", err)
	}
	target.handle = windows.InvalidHandle
	return true, nil
}

func restoreWindowsPromptResourceDirectoryNoDeleteGuard(parent windows.Handle, target *windowsPromptResourcePathHandle) error {
	if target == nil || target.handle == windows.InvalidHandle {
		return errors.New("prompt resource directory identity handoff is unavailable")
	}
	restored, err := openWindowsPromptResourceDirectoryRelative(parent, target.path, target.access, windowsPromptResourceShareNoDelete)
	if err != nil {
		return fmt.Errorf("restore retained prompt resource directory identity: %w", err)
	}
	if !samePromptResourceFileID(target.info, restored.info) || target.security == "" || restored.security != target.security {
		_ = windows.CloseHandle(restored.handle)
		return errors.New("prompt resource directory was replaced while restoring cleanup protection")
	}
	if err := windows.CloseHandle(target.handle); err != nil {
		_ = windows.CloseHandle(restored.handle)
		return fmt.Errorf("close prompt resource directory identity handoff: %w", err)
	}
	*target = restored
	return nil
}

func markWindowsPromptResourceHandleForDeletion(handle windows.Handle) error {
	flags := uint32(windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS | windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE)
	err := windows.SetFileInformationByHandle(handle, windows.FileDispositionInfoEx, (*byte)(unsafe.Pointer(&flags)), uint32(unsafe.Sizeof(flags)))
	if err != nil {
		deleteFile := byte(1)
		if fallbackErr := windows.SetFileInformationByHandle(handle, windows.FileDispositionInfo, &deleteFile, 1); fallbackErr != nil {
			return errors.Join(err, fallbackErr)
		}
	}
	return nil
}

func closeWindowsPromptResourceHandles(handles []windowsPromptResourcePathHandle) {
	for index := len(handles) - 1; index >= 0; index-- {
		if handles[index].handle != windows.InvalidHandle {
			_ = windows.CloseHandle(handles[index].handle)
		}
	}
}

func closeWindowsPromptResourcePathHandle(target *windowsPromptResourcePathHandle) {
	if target != nil && target.handle != windows.InvalidHandle {
		_ = windows.CloseHandle(target.handle)
		target.handle = windows.InvalidHandle
	}
}

func securePromptResourceStage(path string) error {
	handle, err := openWindowsPromptResourceDirectory(path, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle.handle)
	return securePromptResourceDirectoryHandle(handle.handle)
}

func verifyPromptResourceStageSecurity(path string) error {
	handle, err := openWindowsPromptResourceDirectoryShared(path, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, windowsPromptResourceShareAll)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle.handle)
	allowed, err := promptStageAllowedSIDs()
	if err != nil {
		return err
	}
	return verifyPromptResourceStageACLHandle(handle.handle, allowed)
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
	handle, err := openWindowsPromptResourceDirectoryShared(path, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, windowsPromptResourceShareAll)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle.handle)
	return verifyPromptResourceStageACLHandle(handle.handle, allowed)
}

func verifyPromptResourceStageACLHandle(handle windows.Handle, allowed []*windows.SID) error {
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	if len(allowed) == 0 {
		return errors.New("prompt resource owner is unavailable")
	}
	owner, defaulted, err := descriptor.Owner()
	if err != nil {
		return err
	}
	if owner == nil || defaulted || !owner.Equals(allowed[0]) {
		return errors.New("prompt resource owner is not the process user")
	}
	return verifyPromptResourceACL(descriptor, allowed, true, uint8(windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE))
}

func verifyPrivatePromptResourceFile(file *os.File) error {
	if file == nil {
		return errors.New("staged resource file is nil")
	}
	return verifyPrivatePromptResourceFileHandle(windows.Handle(file.Fd()))
}

func verifyPrivatePromptResourceFileHandle(handle windows.Handle) error {
	if handle == windows.InvalidHandle {
		return errors.New("staged resource file handle is unavailable")
	}
	allowed, err := promptStageAllowedSIDs()
	if err != nil {
		return err
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, defaulted, err := descriptor.Owner()
	if err != nil {
		return err
	}
	if owner == nil || defaulted || !owner.Equals(allowed[0]) {
		return errors.New("staged resource owner is not the process user")
	}
	return verifyPromptResourceACL(descriptor, allowed, true, 0)
}

func securePrivatePromptResourceFile(file *os.File, mode os.FileMode) error {
	if file == nil {
		return errors.New("staged resource file is nil")
	}
	if err := file.Chmod(mode); err != nil {
		return err
	}
	return verifyPrivatePromptResourceFile(file)
}

func verifyPromptResourceACL(descriptor *windows.SECURITY_DESCRIPTOR, allowed []*windows.SID, requireProtected bool, expectedFlags uint8) error {
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
		if ace.Header.AceFlags != expectedFlags {
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
