package filedelta

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const stateVersion = 1
const stateDirName = ".filedelta"
const stateFileName = "state.json"

type manifestEntry struct {
	Type    string `json:"type"`
	Mode    uint32 `json:"mode"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
	Hash    string `json:"sha256,omitempty"`
}

type archiveRecord struct {
	File    string   `json:"file"`
	Deleted []string `json:"deleted,omitempty"`
}

type backupState struct {
	Version  int                      `json:"version"`
	Source   string                   `json:"source"`
	Manifest map[string]manifestEntry `json:"manifest"`
	Archives []archiveRecord          `json:"archives"`
}

func prepareSource(source, backupDir string) (string, error) {
	sourcePath, err := filepath.Abs(source)
	if err != nil {
		return "", fmt.Errorf("resolve source folder: %w", err)
	}
	sourcePath, err = filepath.EvalSymlinks(sourcePath)
	if err != nil {
		return "", fmt.Errorf("resolve source folder: %w", err)
	}
	info, err := os.Stat(sourcePath)
	if err != nil {
		return "", fmt.Errorf("open source folder: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("source is not a directory: %s", sourcePath)
	}
	resolvedBackup, err := resolveFuturePath(backupDir)
	if err != nil {
		return "", fmt.Errorf("resolve backup directory: %w", err)
	}
	rel, err := filepath.Rel(sourcePath, resolvedBackup)
	if err != nil {
		return "", fmt.Errorf("compare source and backup paths: %w", err)
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return "", errors.New("backup directory must not be inside the source folder")
	}
	return sourcePath, nil
}

func resolveFuturePath(name string) (string, error) {
	absolute, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(absolute)
	if parent == absolute {
		return "", err
	}
	resolvedParent, err := resolveFuturePath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(absolute)), nil
}

func metadataDir(backupDir string) string {
	return filepath.Join(backupDir, stateDirName)
}

func statePath(backupDir string) string {
	return filepath.Join(metadataDir(backupDir), stateFileName)
}

func createFull(source, backupDir string) (string, error) {
	release, err := acquireLock(backupDir)
	if err != nil {
		return "", err
	}
	defer release()

	manifest, entries, err := scanTree(source)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return "", fmt.Errorf("create backup directory: %w", err)
	}
	name, err := uniqueArchiveName(backupDir, "full")
	if err != nil {
		return "", err
	}
	if err := createArchive(filepath.Join(backupDir, name), entries); err != nil {
		return "", err
	}
	state := backupState{
		Version:  stateVersion,
		Source:   source,
		Manifest: manifest,
		Archives: []archiveRecord{{File: name}},
	}
	if err := saveState(backupDir, state); err != nil {
		return "", err
	}
	return name, nil
}

func createDiff(source, backupDir string) (string, bool, error) {
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return "", false, fmt.Errorf("create backup directory: %w", err)
	}
	release, err := acquireLock(backupDir)
	if err != nil {
		return "", false, err
	}
	defer release()

	state, err := loadState(backupDir)
	if err != nil {
		return "", false, err
	}
	if filepath.Clean(state.Source) != filepath.Clean(source) {
		return "", false, fmt.Errorf("source folder does not match the full backup (%s)", state.Source)
	}

	current, entries, err := scanTree(source)
	if err != nil {
		return "", false, err
	}
	changed := make([]string, 0)
	deleted := make([]string, 0)
	for name, entry := range current {
		if previous, ok := state.Manifest[name]; !ok || previous != entry {
			changed = append(changed, name)
		}
	}
	for name := range state.Manifest {
		if _, ok := current[name]; !ok {
			deleted = append(deleted, name)
		}
	}
	if len(changed) == 0 && len(deleted) == 0 {
		return "", false, nil
	}
	sort.Strings(changed)
	sort.Strings(deleted)
	selected := make([]string, 0, len(changed))
	changedSet := make(map[string]struct{}, len(changed))
	for _, name := range changed {
		changedSet[name] = struct{}{}
		selected = append(selected, name)
	}
	selectedEntries := make([]treeEntry, 0, len(selected))
	for _, entry := range entries {
		if _, ok := changedSet[entry.Name]; ok {
			selectedEntries = append(selectedEntries, entry)
		}
	}

	name, err := uniqueArchiveName(backupDir, "diff")
	if err != nil {
		return "", false, err
	}
	if err := createArchive(filepath.Join(backupDir, name), selectedEntries); err != nil {
		return "", false, err
	}
	state.Manifest = current
	state.Archives = append(state.Archives, archiveRecord{File: name, Deleted: deleted})
	if err := saveState(backupDir, state); err != nil {
		return "", false, err
	}
	return name, true, nil
}

func scanTree(root string) (map[string]manifestEntry, []treeEntry, error) {
	manifest := make(map[string]manifestEntry)
	entries := make([]treeEntry, 0)
	err := filepath.WalkDir(root, func(fullPath string, dirEntry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("scan %s: %w", fullPath, walkErr)
		}
		if fullPath == root {
			return nil
		}
		rel, err := filepath.Rel(root, fullPath)
		if err != nil {
			return fmt.Errorf("get relative path for %s: %w", fullPath, err)
		}
		name := filepath.ToSlash(rel)
		if strings.Contains(name, "\\") || strings.HasPrefix(name, "/") || name == ".." ||
			strings.HasPrefix(name, "../") || looksLikeDrivePath(name) {
			return fmt.Errorf("path is not portable in an archive: %q", name)
		}
		info, err := os.Lstat(fullPath)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", fullPath, err)
		}
		entry := treeEntry{Name: name, FullPath: fullPath, Info: info}
		switch {
		case info.IsDir():
			entry.Type = "dir"
		case info.Mode()&os.ModeSymlink != 0:
			entry.Type = "symlink"
			target, err := os.Readlink(fullPath)
			if err != nil {
				return fmt.Errorf("read symlink %s: %w", fullPath, err)
			}
			entry.LinkTarget = target
			entry.Hash = hashBytes([]byte(target))
			entry.Size = int64(len(target))
		case info.Mode().IsRegular():
			entry.Type = "file"
			entry.Size = info.Size()
			hash, err := hashFile(fullPath, info)
			if err != nil {
				return fmt.Errorf("hash %s: %w", fullPath, err)
			}
			entry.Hash = hash
		default:
			return fmt.Errorf("unsupported file type at %s: %s", fullPath, info.Mode().Type())
		}
		mode := uint32(info.Mode().Perm() | info.Mode().Type())
		manifest[name] = manifestEntry{
			Type:    entry.Type,
			Mode:    mode,
			Size:    entry.Size,
			ModTime: info.ModTime().UnixNano(),
			Hash:    entry.Hash,
		}
		entries = append(entries, entry)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return manifest, entries, nil
}

func hashFile(name string, expected os.FileInfo) (string, error) {
	file, err := os.Open(name)
	if err != nil {
		return "", err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return "", err
	}
	if !info.Mode().IsRegular() || !os.SameFile(expected, info) {
		file.Close()
		return "", errors.New("file changed while hashing")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func uniqueArchiveName(backupDir, prefix string) (string, error) {
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	name := prefix + "-" + stamp + ".tar.gz"
	if _, err := os.Stat(filepath.Join(backupDir, name)); err == nil {
		return "", fmt.Errorf("archive already exists: %s", name)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("check archive path: %w", err)
	}
	return name, nil
}

func loadState(backupDir string) (backupState, error) {
	data, err := os.ReadFile(statePath(backupDir))
	if err != nil {
		if os.IsNotExist(err) {
			return backupState{}, errors.New("no backup state found; create a full backup first")
		}
		return backupState{}, fmt.Errorf("read backup state: %w", err)
	}
	var state backupState
	if err := json.Unmarshal(data, &state); err != nil {
		return backupState{}, fmt.Errorf("parse backup state: %w", err)
	}
	if state.Version != stateVersion {
		return backupState{}, fmt.Errorf("unsupported backup state version %d", state.Version)
	}
	if state.Source == "" || state.Manifest == nil {
		return backupState{}, errors.New("backup state is missing its source or manifest")
	}
	if len(state.Archives) == 0 || !strings.HasPrefix(state.Archives[0].File, "full-") {
		return backupState{}, errors.New("backup state has no valid full archive")
	}
	for name, entry := range state.Manifest {
		if clean, err := safeArchivePath(name); err != nil || filepath.ToSlash(clean) != name {
			return backupState{}, fmt.Errorf("invalid manifest path: %q", name)
		}
		switch entry.Type {
		case "file", "dir", "symlink":
		default:
			return backupState{}, fmt.Errorf("invalid manifest entry type %q for %s", entry.Type, name)
		}
	}
	for i, record := range state.Archives {
		prefix := "diff-"
		if i == 0 {
			prefix = "full-"
		}
		if filepath.Base(record.File) != record.File || record.File == "." || record.File == ".." ||
			!strings.HasPrefix(record.File, prefix) || !strings.HasSuffix(record.File, ".tar.gz") {
			return backupState{}, fmt.Errorf("invalid archive path in state: %q", record.File)
		}
		info, err := os.Lstat(filepath.Join(backupDir, record.File))
		if err != nil {
			return backupState{}, fmt.Errorf("missing backup archive %s: %w", record.File, err)
		}
		if !info.Mode().IsRegular() {
			return backupState{}, fmt.Errorf("backup archive is not a regular file: %s", record.File)
		}
		for _, deleted := range record.Deleted {
			if clean, err := safeArchivePath(deleted); err != nil || filepath.ToSlash(clean) != deleted {
				return backupState{}, fmt.Errorf("invalid deleted path in state: %q", deleted)
			}
		}
	}
	return state, nil
}

func saveState(backupDir string, state backupState) error {
	if err := os.MkdirAll(metadataDir(backupDir), 0755); err != nil {
		return fmt.Errorf("create metadata directory: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode backup state: %w", err)
	}
	tmp, err := os.CreateTemp(metadataDir(backupDir), "state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary state file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write backup state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync backup state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close backup state: %w", err)
	}
	if err := replaceFile(tmpName, statePath(backupDir)); err != nil {
		return fmt.Errorf("publish backup state: %w", err)
	}
	return nil
}

func acquireLock(backupDir string) (func(), error) {
	if err := os.MkdirAll(metadataDir(backupDir), 0755); err != nil {
		return nil, fmt.Errorf("create metadata directory: %w", err)
	}
	name := filepath.Join(metadataDir(backupDir), "lock")
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("backup operation is locked (%s); if no FileDelta process is running, remove the stale lock file", name)
		}
		return nil, fmt.Errorf("create backup lock: %w", err)
	}
	if _, err := fmt.Fprintf(file, "pid=%d\n", os.Getpid()); err != nil {
		file.Close()
		os.Remove(name)
		return nil, fmt.Errorf("write backup lock: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(name)
		return nil, fmt.Errorf("close backup lock: %w", err)
	}
	return func() {
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "filedelta: remove backup lock: %v\n", err)
		}
	}, nil
}

func replaceFile(source, target string) error {
	return os.Rename(source, target)
}
