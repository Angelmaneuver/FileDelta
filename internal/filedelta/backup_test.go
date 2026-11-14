package filedelta

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFullDiffAndLatest(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	backupDir := filepath.Join(root, "backups")
	if err := os.MkdirAll(filepath.Join(source, "empty"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(source, "shape"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "shape", "child.txt"), []byte("child"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "changed.txt"), []byte("before"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "deleted.txt"), []byte("remove me"), 0644); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	if err := run([]string{"full", "-source", source, "-backup-dir", backupDir}, &stdout, io.Discard); err != nil {
		t.Fatalf("run full backup: %v", err)
	}

	if err := os.WriteFile(filepath.Join(source, "changed.txt"), []byte("after"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "added.txt"), []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(source, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(source, "shape")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "shape"), []byte("now a file"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := run([]string{"diff", "-source", source, "-backup-dir", backupDir}, &stdout, io.Discard); err != nil {
		t.Fatalf("run differential backup: %v", err)
	}
	state, err := loadState(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Archives) != 2 {
		t.Fatalf("unexpected archive chain: %+v", state.Archives)
	}
	if len(state.Archives[1].Deleted) != 2 || state.Archives[1].Deleted[0] != "deleted.txt" ||
		state.Archives[1].Deleted[1] != "shape/child.txt" {
		t.Fatalf("deletion was not recorded: %+v", state.Archives[1].Deleted)
	}

	stdout.Reset()
	if err := run([]string{"diff", "-source", source, "-backup-dir", backupDir}, &stdout, io.Discard); err != nil {
		t.Fatalf("run unchanged differential backup: %v", err)
	}
	if !strings.Contains(stdout.String(), "No changes detected.") {
		t.Fatalf("expected no-change message, got %q", stdout.String())
	}
	if err := run([]string{"latest", "-backup-dir", backupDir}, &stdout, io.Discard); err != nil {
		t.Fatalf("run latest archive: %v", err)
	}
	if err := run([]string{"latest", "-backup-dir", backupDir}, &stdout, io.Discard); err != nil {
		t.Fatalf("replace latest archive: %v", err)
	}

	contents := readTarFiles(t, filepath.Join(backupDir, "latest.tar.gz"))
	if string(contents["changed.txt"]) != "after" || string(contents["added.txt"]) != "new" {
		t.Fatalf("latest archive has unexpected file contents: %v", contents)
	}
	if _, ok := contents["deleted.txt"]; ok {
		t.Fatal("deleted file remains in latest archive")
	}
	if _, ok := contents["empty"]; !ok {
		t.Fatal("empty directory was not preserved in latest archive")
	}
	if string(contents["shape"]) != "now a file" {
		t.Fatalf("directory-to-file replacement was not applied: %q", contents["shape"])
	}
}

func TestSafeArchivePath(t *testing.T) {
	for _, name := range []string{"../outside", "/absolute", `folder\file`, "C:/outside"} {
		if _, err := safeArchivePath(name); err == nil {
			t.Errorf("safeArchivePath(%q) unexpectedly succeeded", name)
		}
	}
	if got, err := safeArchivePath("folder/file.txt"); err != nil || filepath.ToSlash(got) != "folder/file.txt" {
		t.Fatalf("safe relative path rejected: got %q, err %v", got, err)
	}
}

func TestPrepareSourceRejectsBackupInsideSource(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(source, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareSource(source, filepath.Join(source, "backups")); err == nil {
		t.Fatal("expected backup inside source to be rejected")
	}
}

func readTarFiles(t *testing.T, name string) map[string][]byte {
	t.Helper()
	file, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := make(map[string][]byte)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(header.Name, "/") {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = data
	}
	return files
}
