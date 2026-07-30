package hashing

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCaptureStableFile(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	const content = "stable content"
	if err := os.WriteFile(filepath.Join(rootPath, "file.txt"), []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	snapshot, err := Capture(context.Background(), root, "file.txt")
	if err != nil {
		t.Fatalf("Capture() error = %v", err)
	}
	expected := sha256.Sum256([]byte(content))
	if snapshot.Hash != expected {
		t.Errorf("hash = %x, want %x", snapshot.Hash, expected)
	}
	if snapshot.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", snapshot.Size, len(content))
	}
	if snapshot.Mode.Perm() != 0o640 {
		t.Errorf("mode = %o, want 640", snapshot.Mode.Perm())
	}
}

func TestCaptureRejectsSymlink(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "target.txt"), []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(rootPath, "link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	_, err = Capture(context.Background(), root, "link.txt")
	if !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("expected ErrUnsupportedType, got %v", err)
	}
}

func TestCaptureHonorsCancelledContext(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "file.txt"), make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Capture(ctx, root, "file.txt")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
