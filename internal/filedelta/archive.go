package filedelta

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type treeEntry struct {
	Name       string
	FullPath   string
	Type       string
	Info       os.FileInfo
	LinkTarget string
	Size       int64
	Hash       string
}

func createArchive(destination string, entries []treeEntry) error {
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".filedelta-archive-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary archive: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		if err := writeEntry(tw, entry); err != nil {
			tw.Close()
			gz.Close()
			tmp.Close()
			return err
		}
	}
	if err := tw.Close(); err != nil {
		gz.Close()
		tmp.Close()
		return fmt.Errorf("finish tar archive: %w", err)
	}
	if err := gz.Close(); err != nil {
		tmp.Close()
		return fmt.Errorf("finish gzip archive: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync archive: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close archive: %w", err)
	}
	if err := replaceFile(tmpName, destination); err != nil {
		return fmt.Errorf("publish archive: %w", err)
	}
	return nil
}

func writeEntry(tw *tar.Writer, entry treeEntry) error {
	info, err := os.Lstat(entry.FullPath)
	if err != nil {
		return fmt.Errorf("inspect %s before archiving: %w", entry.Name, err)
	}
	header, err := tar.FileInfoHeader(info, entry.LinkTarget)
	if err != nil {
		return fmt.Errorf("create archive header for %s: %w", entry.Name, err)
	}
	header.Name = entry.Name
	header.Format = tar.FormatPAX
	header.ModTime = info.ModTime()
	header.AccessTime = time.Time{}
	header.ChangeTime = time.Time{}

	switch entry.Type {
	case "file":
		if !info.Mode().IsRegular() || info.Size() != entry.Size ||
			info.Mode().Perm() != entry.Info.Mode().Perm() ||
			info.ModTime().UnixNano() != entry.Info.ModTime().UnixNano() {
			return fmt.Errorf("file type changed while creating archive: %s", entry.Name)
		}
		header.Size = entry.Size
	case "dir":
		if !info.IsDir() {
			return fmt.Errorf("directory type changed while creating archive: %s", entry.Name)
		}
	case "symlink":
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("symlink type changed while creating archive: %s", entry.Name)
		}
		target, err := os.Readlink(entry.FullPath)
		if err != nil {
			return fmt.Errorf("read symlink %s: %w", entry.Name, err)
		}
		if target != entry.LinkTarget {
			return fmt.Errorf("symlink changed while creating archive: %s", entry.Name)
		}
		header.Linkname = target
	default:
		return fmt.Errorf("unsupported archive entry type %q", entry.Type)
	}
	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("write header for %s: %w", entry.Name, err)
	}
	if entry.Type != "file" {
		if info.Mode().Perm() != entry.Info.Mode().Perm() || info.ModTime().UnixNano() != entry.Info.ModTime().UnixNano() {
			return fmt.Errorf("entry changed while creating archive: %s", entry.Name)
		}
		return nil
	}

	file, err := os.Open(entry.FullPath)
	if err != nil {
		return fmt.Errorf("open %s for archiving: %w", entry.Name, err)
	}
	openedInfo, err := file.Stat()
	if err != nil {
		file.Close()
		return fmt.Errorf("inspect opened file %s: %w", entry.Name, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(entry.Info, openedInfo) {
		file.Close()
		return fmt.Errorf("file changed while opening archive entry: %s", entry.Name)
	}
	hash := sha256.New()
	written, copyErr := io.Copy(tw, io.TeeReader(file, hash))
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("archive %s: %w", entry.Name, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s: %w", entry.Name, closeErr)
	}
	if written != entry.Size || hex.EncodeToString(hash.Sum(nil)) != entry.Hash {
		return fmt.Errorf("file changed while creating archive: %s", entry.Name)
	}
	return nil
}

func createLatest(backupDir string) (string, error) {
	release, err := acquireLock(backupDir)
	if err != nil {
		return "", err
	}
	defer release()

	state, err := loadState(backupDir)
	if err != nil {
		return "", err
	}
	work, err := os.MkdirTemp("", "filedelta-restore-*")
	if err != nil {
		return "", fmt.Errorf("create reconstruction directory: %w", err)
	}
	defer os.RemoveAll(work)

	directories := make(map[string]directoryMetadata)
	for _, record := range state.Archives {
		if err := applyArchive(filepath.Join(backupDir, record.File), work, directories); err != nil {
			return "", err
		}
		for _, deleted := range record.Deleted {
			if err := removeTreePath(work, deleted); err != nil {
				return "", fmt.Errorf("apply deletion %s: %w", deleted, err)
			}
			delete(directories, deleted)
		}
	}
	if err := restoreDirectoryMetadata(work, directories); err != nil {
		return "", err
	}

	manifest, entries, err := scanTree(work)
	if err != nil {
		return "", fmt.Errorf("verify reconstructed files: %w", err)
	}
	if !manifestsEqual(manifest, state.Manifest) {
		return "", errors.New("reconstructed files do not match the latest backup state")
	}

	name := "latest.tar.gz"
	if err := createArchive(filepath.Join(backupDir, name), entries); err != nil {
		return "", err
	}
	return name, nil
}

type directoryMetadata struct {
	Mode    os.FileMode
	ModTime time.Time
}

func applyArchive(archivePath, root string, directories map[string]directoryMetadata) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open archive %s: %w", filepath.Base(archivePath), err)
	}
	gz, err := gzip.NewReader(file)
	if err != nil {
		file.Close()
		return fmt.Errorf("open gzip archive %s: %w", filepath.Base(archivePath), err)
	}
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			gz.Close()
			file.Close()
			return fmt.Errorf("read archive %s: %w", filepath.Base(archivePath), err)
		}
		rel, err := safeArchivePath(header.Name)
		if err != nil {
			gz.Close()
			file.Close()
			return fmt.Errorf("unsafe path in %s: %w", filepath.Base(archivePath), err)
		}
		if header.Size < 0 {
			gz.Close()
			file.Close()
			return fmt.Errorf("negative file size in %s", filepath.Base(archivePath))
		}
		target, err := ensureSafeParents(root, rel)
		if err != nil {
			gz.Close()
			file.Close()
			return fmt.Errorf("prepare %s from %s: %w", header.Name, filepath.Base(archivePath), err)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := makeDirectory(target); err != nil {
				gz.Close()
				file.Close()
				return fmt.Errorf("create directory %s: %w", header.Name, err)
			}
			directories[rel] = directoryMetadata{Mode: os.FileMode(header.Mode), ModTime: header.ModTime}
		case tar.TypeReg, tar.TypeRegA:
			clearDirectoryMetadata(directories, rel)
			if err := removeExisting(target); err != nil {
				gz.Close()
				file.Close()
				return fmt.Errorf("replace %s: %w", header.Name, err)
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				gz.Close()
				file.Close()
				return fmt.Errorf("create file %s: %w", header.Name, err)
			}
			n, copyErr := io.CopyN(out, tr, header.Size)
			if copyErr == nil && n != header.Size {
				copyErr = io.ErrUnexpectedEOF
			}
			if copyErr == nil {
				copyErr = out.Chmod(os.FileMode(header.Mode))
			}
			if copyErr == nil {
				copyErr = os.Chtimes(target, header.ModTime, header.ModTime)
			}
			closeErr := out.Close()
			if copyErr != nil || closeErr != nil {
				os.Remove(target)
				gz.Close()
				file.Close()
				if copyErr != nil {
					return fmt.Errorf("extract file %s: %w", header.Name, copyErr)
				}
				return fmt.Errorf("close extracted file %s: %w", header.Name, closeErr)
			}
		case tar.TypeSymlink:
			clearDirectoryMetadata(directories, rel)
			if err := removeExisting(target); err != nil {
				gz.Close()
				file.Close()
				return fmt.Errorf("replace symlink %s: %w", header.Name, err)
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				gz.Close()
				file.Close()
				return fmt.Errorf("create symlink %s: %w", header.Name, err)
			}
		default:
			gz.Close()
			file.Close()
			return fmt.Errorf("unsupported tar entry type %d in %s", header.Typeflag, filepath.Base(archivePath))
		}
	}
	gzErr := gz.Close()
	fileErr := file.Close()
	if gzErr != nil {
		return fmt.Errorf("finish reading archive %s: %w", filepath.Base(archivePath), gzErr)
	}
	if fileErr != nil {
		return fmt.Errorf("close archive %s: %w", filepath.Base(archivePath), fileErr)
	}
	return nil
}

func safeArchivePath(name string) (string, error) {
	if name == "" || strings.Contains(name, "\\") || path.IsAbs(name) || looksLikeDrivePath(name) {
		return "", fmt.Errorf("invalid archive path %q", name)
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path escapes archive root: %q", name)
	}
	return filepath.FromSlash(clean), nil
}

func looksLikeDrivePath(name string) bool {
	return len(name) >= 2 &&
		((name[0] >= 'a' && name[0] <= 'z') || (name[0] >= 'A' && name[0] <= 'Z')) &&
		name[1] == ':'
}

func ensureSafeParents(root, rel string) (string, error) {
	parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
	current := root
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if err := os.Mkdir(current, 0700); err != nil {
				return "", err
			}
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("parent is not a real directory: %s", current)
		}
	}
	return filepath.Join(root, rel), nil
}

func makeDirectory(name string) error {
	info, err := os.Lstat(name)
	if os.IsNotExist(err) {
		return os.Mkdir(name, 0700)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		if err := removeExisting(name); err != nil {
			return err
		}
		return os.Mkdir(name, 0700)
	}
	return os.Chmod(name, 0700)
}

func removeExisting(name string) error {
	info, err := os.Lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return os.RemoveAll(name)
	}
	return os.Remove(name)
}

func removeTreePath(root, rel string) error {
	clean, err := safeArchivePath(filepath.ToSlash(rel))
	if err != nil {
		return err
	}
	parts := strings.Split(filepath.Clean(clean), string(filepath.Separator))
	current := root
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
	}
	target := filepath.Join(root, clean)
	return removeExisting(target)
}

func clearDirectoryMetadata(directories map[string]directoryMetadata, name string) {
	prefix := name + string(filepath.Separator)
	for directory := range directories {
		if directory == name || strings.HasPrefix(directory, prefix) {
			delete(directories, directory)
		}
	}
}

func restoreDirectoryMetadata(root string, directories map[string]directoryMetadata) error {
	names := make([]string, 0, len(directories))
	for name := range directories {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		return strings.Count(names[i], string(filepath.Separator)) > strings.Count(names[j], string(filepath.Separator))
	})
	for _, name := range names {
		target := filepath.Join(root, name)
		meta := directories[name]
		info, err := os.Lstat(target)
		if err != nil {
			return fmt.Errorf("inspect reconstructed directory %s: %w", name, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("reconstructed path is not a directory: %s", name)
		}
		if err := os.Chmod(target, meta.Mode); err != nil {
			return fmt.Errorf("restore directory permissions for %s: %w", name, err)
		}
		if err := os.Chtimes(target, meta.ModTime, meta.ModTime); err != nil {
			return fmt.Errorf("restore directory timestamps for %s: %w", name, err)
		}
	}
	return nil
}

func manifestsEqual(a, b map[string]manifestEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for name, entry := range a {
		if other, ok := b[name]; !ok || other != entry {
			return false
		}
	}
	return true
}
