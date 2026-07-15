//go:build darwin || linux

package commandbridge

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

type unixPromptResourcePathHandle struct {
	path string
	file *os.File
	info os.FileInfo
}

type unixPromptResourceStageGuard struct {
	ancestors     []unixPromptResourcePathHandle
	anchor        unixPromptResourcePathHandle
	stage         unixPromptResourcePathHandle
	stageRemoved  bool
	anchorRemoved bool
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
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	handles, err := openTrustedPromptResourceAncestors(canonical)
	if err != nil {
		return "", err
	}
	closeUnixPromptResourceHandles(handles)
	return canonical, nil
}

func openTrustedPromptResourceAncestors(path string) ([]unixPromptResourcePathHandle, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return nil, errors.New("prompt resource ancestor path must be absolute")
	}
	relative, err := filepath.Rel(string(filepath.Separator), path)
	if err != nil || relative == ".." || len(relative) >= 3 && relative[:3] == ".."+string(filepath.Separator) {
		return nil, errors.New("prompt resource ancestor path escapes the filesystem root")
	}
	components := []string{string(filepath.Separator)}
	current := string(filepath.Separator)
	if relative != "." {
		for _, component := range splitCleanPath(relative) {
			current = filepath.Join(current, component)
			components = append(components, current)
		}
	}
	handles := make([]unixPromptResourcePathHandle, 0, len(components))
	for _, componentPath := range components {
		handle, openErr := openTrustedPromptResourceDirectory(componentPath, true)
		if openErr != nil {
			closeUnixPromptResourceHandles(handles)
			return nil, fmt.Errorf("unsafe prompt resource ancestor: %w", openErr)
		}
		handles = append(handles, handle)
	}
	return handles, nil
}

func splitCleanPath(relative string) []string {
	var components []string
	for relative != "." && relative != string(filepath.Separator) && relative != "" {
		dir, base := filepath.Split(relative)
		if base != "" {
			components = append([]string{base}, components...)
		}
		relative = filepath.Clean(dir)
	}
	return components
}

func openTrustedPromptResourceDirectory(path string, verifyACL bool) (unixPromptResourcePathHandle, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return unixPromptResourcePathHandle{}, err
	}
	if err := verifyTrustedPromptResourceDirectoryInfo(info); err != nil {
		return unixPromptResourcePathHandle{}, err
	}
	file, err := openUnixPromptResourceDirectoryNoFollow(path)
	if err != nil {
		return unixPromptResourcePathHandle{}, err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return unixPromptResourcePathHandle{}, err
	}
	if !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return unixPromptResourcePathHandle{}, errors.New("prompt resource ancestor changed while opening")
	}
	if verifyACL {
		if err := verifyPromptResourceAncestorFile(file); err != nil {
			_ = file.Close()
			return unixPromptResourcePathHandle{}, err
		}
	}
	return unixPromptResourcePathHandle{path: path, file: file, info: openedInfo}, nil
}

func openUnixPromptResourceDirectoryNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("wrap prompt resource directory handle")
	}
	return file, nil
}

func verifyTrustedPromptResourceDirectoryInfo(info os.FileInfo) error {
	if info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("prompt resource ancestor must be a non-symlink directory")
	}
	owner, err := fileOwnerUID(info)
	if err != nil {
		return err
	}
	if owner != uint32(os.Geteuid()) && owner != 0 {
		return errors.New("prompt resource ancestor has an untrusted owner")
	}
	if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
		return errors.New("prompt resource ancestor is writable by another principal without the sticky bit")
	}
	return nil
}

func fileOwnerUID(info os.FileInfo) (uint32, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return 0, errors.New("prompt resource filesystem does not expose file ownership")
	}
	return stat.Uid, nil
}

func createPromptResourceStage(parent string) (string, string, promptResourceStageGuard, error) {
	anchor, err := os.MkdirTemp(parent, "acp-commandbridge-private-")
	if err != nil {
		return "", "", nil, err
	}
	anchor, err = filepath.Abs(anchor)
	if err != nil {
		return "", "", nil, errors.New("resolve private prompt resource directory; an empty protected remnant may require manual removal")
	}
	stage, err := os.MkdirTemp(anchor, "inputs-")
	if err != nil {
		return "", "", nil, errors.New("create private prompt resource stage; an empty protected remnant may require manual removal")
	}
	guard, err := newPromptResourceStageGuard(anchor, stage)
	if err != nil {
		return "", "", nil, errors.New("retain private prompt resource identities; a protected remnant may require manual removal")
	}
	return anchor, stage, guard, nil
}

func newPromptResourceStageGuard(anchor, stage string) (promptResourceStageGuard, error) {
	ancestors, err := openTrustedPromptResourceAncestors(filepath.Dir(anchor))
	if err != nil {
		return nil, err
	}
	anchorHandle, err := openTrustedPromptResourceDirectory(anchor, false)
	if err != nil {
		closeUnixPromptResourceHandles(ancestors)
		return nil, err
	}
	stageHandle, err := openTrustedPromptResourceDirectory(stage, false)
	if err != nil {
		_ = anchorHandle.file.Close()
		closeUnixPromptResourceHandles(ancestors)
		return nil, err
	}
	guard := &unixPromptResourceStageGuard{ancestors: ancestors, anchor: anchorHandle, stage: stageHandle}
	return guard, nil
}

func (g *unixPromptResourceStageGuard) Secure() error {
	if g == nil {
		return errors.New("prompt resource stage protection is unavailable")
	}
	if err := securePromptResourceDirectoryFile(g.anchor.file, 0o700); err != nil {
		return fmt.Errorf("secure private anchor: %w", err)
	}
	if err := securePromptResourceDirectoryFile(g.stage.file, 0o700); err != nil {
		return fmt.Errorf("secure private stage: %w", err)
	}
	return g.Verify()
}

func (g *unixPromptResourceStageGuard) ProtectFile(path string) error {
	if g == nil || filepath.Clean(filepath.Dir(path)) != filepath.Clean(g.stage.path) {
		return errors.New("staged resource is outside the protected stage")
	}
	return g.Verify()
}

func (g *unixPromptResourceStageGuard) Seal() error {
	if g == nil || g.stage.file == nil {
		return errors.New("prompt resource stage protection is unavailable")
	}
	if err := securePromptResourceDirectoryFile(g.stage.file, 0o500); err != nil {
		return err
	}
	return g.Verify()
}

func (g *unixPromptResourceStageGuard) Verify() error {
	if err := g.verifyIdentity(); err != nil {
		return err
	}
	if err := verifyPromptResourceDirectoryFile(g.anchor.file); err != nil {
		return err
	}
	return verifyPromptResourceDirectoryFile(g.stage.file)
}

func (g *unixPromptResourceStageGuard) verifyIdentity() error {
	if g == nil || g.anchor.file == nil || g.stage.file == nil {
		return errors.New("prompt resource stage identity is unavailable")
	}
	for _, handle := range g.ancestors {
		if err := verifyUnixPromptResourcePathHandle(handle, true); err != nil {
			return err
		}
	}
	if g.anchorRemoved {
		if err := verifyUnixPromptResourceOpenHandle(g.anchor); err != nil {
			return err
		}
	} else {
		if err := verifyUnixPromptResourcePathHandle(g.anchor, false); err != nil {
			return err
		}
	}
	if g.stageRemoved {
		if err := verifyUnixPromptResourceOpenHandle(g.stage); err != nil {
			return err
		}
	} else {
		if err := verifyUnixPromptResourcePathHandle(g.stage, false); err != nil {
			return err
		}
	}
	return nil
}

func verifyUnixPromptResourcePathHandle(handle unixPromptResourcePathHandle, ancestor bool) error {
	if err := verifyUnixPromptResourceOpenHandle(handle); err != nil {
		return err
	}
	pathInfo, err := os.Lstat(handle.path)
	if err != nil {
		return err
	}
	if !os.SameFile(handle.info, pathInfo) || !pathInfo.IsDir() || pathInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("prompt resource path was replaced")
	}
	if ancestor {
		if err := verifyTrustedPromptResourceDirectoryInfo(pathInfo); err != nil {
			return err
		}
		return verifyPromptResourceAncestorFile(handle.file)
	}
	return nil
}

func verifyUnixPromptResourceOpenHandle(handle unixPromptResourcePathHandle) error {
	if handle.file == nil || handle.info == nil {
		return errors.New("prompt resource path handle is unavailable")
	}
	openedInfo, err := handle.file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(handle.info, openedInfo) || !openedInfo.IsDir() {
		return errors.New("protected prompt resource path changed identity")
	}
	return nil
}

func (g *unixPromptResourceStageGuard) Cleanup(beforeRemove func(string) error) error {
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
		if err := securePromptResourceDirectoryFile(g.stage.file, 0o700); err != nil {
			return fmt.Errorf("make private stage removable: %w", err)
		}
		if err := securePromptResourceDirectoryFile(g.anchor.file, 0o700); err != nil {
			return fmt.Errorf("make private anchor removable: %w", err)
		}
		if err := removeUnixPromptResourceStageContents(g.stage.file); err != nil {
			return fmt.Errorf("remove private prompt resource contents: %w", err)
		}
		if err := unix.Unlinkat(int(g.anchor.file.Fd()), filepath.Base(g.stage.path), unix.AT_REMOVEDIR); err != nil {
			return fmt.Errorf("remove exact private prompt resource stage: %w", err)
		}
		g.stageRemoved = true
	}
	if !g.anchorRemoved {
		if len(g.ancestors) == 0 {
			return errors.New("prompt resource anchor parent handle is unavailable")
		}
		parent := g.ancestors[len(g.ancestors)-1]
		if err := unix.Unlinkat(int(parent.file.Fd()), filepath.Base(g.anchor.path), unix.AT_REMOVEDIR); err != nil {
			return fmt.Errorf("remove exact private prompt resource anchor: %w", err)
		}
		g.anchorRemoved = true
	}
	return g.close()
}

func removeUnixPromptResourceStageContents(stage *os.File) error {
	if stage == nil {
		return errors.New("prompt resource stage handle is unavailable")
	}
	fd, err := unix.Openat(int(stage.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(fd), "prompt-resource-stage")
	if directory == nil {
		_ = unix.Close(fd)
		return errors.New("wrap prompt resource stage handle")
	}
	defer directory.Close()
	names, err := directory.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, name := range names {
		if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
			return errors.New("prompt resource stage contains an invalid entry name")
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(int(stage.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if stat.Mode&unix.S_IFMT == unix.S_IFDIR {
			return errors.New("prompt resource stage contains an unexpected directory")
		}
		if err := unix.Unlinkat(int(stage.Fd()), name, 0); err != nil {
			return err
		}
	}
	return nil
}

func (g *unixPromptResourceStageGuard) close() error {
	var result error
	if g.stage.file != nil {
		result = errors.Join(result, g.stage.file.Close())
		g.stage.file = nil
	}
	if g.anchor.file != nil {
		result = errors.Join(result, g.anchor.file.Close())
		g.anchor.file = nil
	}
	closeUnixPromptResourceHandles(g.ancestors)
	g.ancestors = nil
	return result
}

func closeUnixPromptResourceHandles(handles []unixPromptResourcePathHandle) {
	for index := len(handles) - 1; index >= 0; index-- {
		if handles[index].file != nil {
			_ = handles[index].file.Close()
		}
	}
}

func securePromptResourceStage(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("prompt resource stage must be a non-symlink directory")
	}
	file, err := openUnixPromptResourceDirectoryNoFollow(path)
	if err != nil {
		return err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, openedInfo) {
		return errors.New("prompt resource stage changed while opening")
	}
	return securePromptResourceDirectoryFile(file, 0o700)
}

func verifyPromptResourceStageSecurity(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("prompt resource stage must be a non-symlink directory")
	}
	file, err := openUnixPromptResourceDirectoryNoFollow(path)
	if err != nil {
		return err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, openedInfo) {
		return errors.New("prompt resource stage changed while opening")
	}
	return verifyPromptResourceDirectoryFile(file)
}
