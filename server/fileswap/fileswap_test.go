package fileswap

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func busyRename(string, string) error {
	return &os.LinkError{Op: "rename", Err: syscall.EBUSY}
}

func TestReplaceRenamesNormally(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cred.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Replace(path, tmp, []byte("new"), 0o600); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Fatalf("content = %q, want new", got)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("tmp should be consumed by rename, stat err = %v", err)
	}
}

func TestReplaceWritesInPlaceOnMountPoint(t *testing.T) {
	rename = busyRename
	t.Cleanup(func() { rename = os.Rename })

	dir := t.TempDir()
	path := filepath.Join(dir, "cred.json")
	if err := os.WriteFile(path, []byte("old-and-longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Replace(path, tmp, []byte("new"), 0o600); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) {
		t.Fatal("in-place fallback must keep the inode a bind mount points at")
	}
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Fatalf("content = %q, want new (old bytes must be truncated)", got)
	}
	if after.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", after.Mode().Perm())
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("tmp should be removed, stat err = %v", err)
	}
}

func TestReplacePassesOtherErrorsThrough(t *testing.T) {
	rename = func(string, string) error { return &os.LinkError{Op: "rename", Err: syscall.EACCES} }
	t.Cleanup(func() { rename = os.Rename })
	path := filepath.Join(t.TempDir(), "cred.json")
	if err := Replace(path, path+".tmp", []byte("x"), 0o600); err == nil {
		t.Fatal("non-EBUSY rename error must be returned")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("no in-place write on a non-EBUSY failure")
	}
}

func TestRemoveTruncatesMountPoint(t *testing.T) {
	remove = func(string) error { return &os.PathError{Op: "remove", Err: syscall.EBUSY} }
	t.Cleanup(func() { remove = os.Remove })
	path := filepath.Join(t.TempDir(), "cred.json")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Size() != 0 {
		t.Fatalf("mount point should be truncated to empty, stat = %v, %v", fi, err)
	}
}

func TestRemoveMissingIsNil(t *testing.T) {
	if err := Remove(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("Remove(absent) = %v", err)
	}
}
