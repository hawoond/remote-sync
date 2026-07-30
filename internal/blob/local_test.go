package blob

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"testing"

	"github.com/google/uuid"
	"github.com/hawoond/remote-sync/internal/domain"
)

func TestLocalUploadResumeFinalizeAndOpen(t *testing.T) {
	t.Parallel()

	store, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	sessionID := uuid.NewString()
	first := []byte("hello ")
	second := []byte("world")
	content := append(append([]byte(nil), first...), second...)
	expected := domain.Hash(sha256.Sum256(content))

	next, err := store.Append(ctx, sessionID, 0, first)
	if err != nil {
		t.Fatalf("Append(first) error = %v", err)
	}
	if next != int64(len(first)) {
		t.Fatalf("next offset = %d, want %d", next, len(first))
	}
	resumed, err := store.Resume(ctx, sessionID)
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if resumed != next {
		t.Fatalf("resumed offset = %d, want %d", resumed, next)
	}
	if _, err := store.Append(ctx, sessionID, resumed, second); err != nil {
		t.Fatalf("Append(second) error = %v", err)
	}

	object, err := store.Finalize(ctx, sessionID, expected, int64(len(content)))
	if err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}
	if object.Hash != expected || object.Size != int64(len(content)) {
		t.Fatalf("object = %+v", object)
	}

	exists, err := store.Exists(ctx, expected, int64(len(content)))
	if err != nil || !exists {
		t.Fatalf("Exists() = %v, %v", exists, err)
	}
	reader, err := store.Open(ctx, expected)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("content = %q, want %q", got, content)
	}
}

func TestLocalRejectsOffsetMismatch(t *testing.T) {
	t.Parallel()

	store, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	sessionID := uuid.NewString()
	if _, err := store.Append(context.Background(), sessionID, 0, []byte("first")); err != nil {
		t.Fatal(err)
	}
	next, err := store.Append(context.Background(), sessionID, 0, []byte("duplicate"))
	if !errors.Is(err, ErrOffsetMismatch) {
		t.Fatalf("expected ErrOffsetMismatch, got %v", err)
	}
	if next != int64(len("first")) {
		t.Fatalf("next offset = %d, want %d", next, len("first"))
	}
}

func TestLocalRemovesHashMismatchUpload(t *testing.T) {
	t.Parallel()

	store, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	sessionID := uuid.NewString()
	content := []byte("content")
	if _, err := store.Append(ctx, sessionID, 0, content); err != nil {
		t.Fatal(err)
	}
	wrong := domain.Hash(sha256.Sum256([]byte("different")))
	if _, err := store.Finalize(ctx, sessionID, wrong, int64(len(content))); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("expected ErrHashMismatch, got %v", err)
	}
	offset, err := store.Resume(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if offset != 0 {
		t.Fatalf("offset after mismatch = %d, want 0", offset)
	}
}

func TestLocalDeduplicatesVerifiedObject(t *testing.T) {
	t.Parallel()

	store, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	content := []byte("same object")
	hash := domain.Hash(sha256.Sum256(content))

	for range 2 {
		sessionID := uuid.NewString()
		if _, err := store.Append(ctx, sessionID, 0, content); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Finalize(ctx, sessionID, hash, int64(len(content))); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLocalRejectsUnsafeSessionID(t *testing.T) {
	t.Parallel()

	store, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.Append(context.Background(), "../escape", 0, []byte("x")); !errors.Is(err, ErrInvalidSessionID) {
		t.Fatalf("expected ErrInvalidSessionID, got %v", err)
	}
}

func TestLocalDeleteIsIdempotent(t *testing.T) {
	t.Parallel()

	store, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	content := []byte("collect after grace")
	hash := domain.Hash(sha256.Sum256(content))
	sessionID := uuid.NewString()
	if _, err := store.Append(ctx, sessionID, 0, content); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finalize(ctx, sessionID, hash, int64(len(content))); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(ctx, hash); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("Open() error = %v, want ErrObjectNotFound", err)
	}
	if err := store.Delete(ctx, hash); err != nil {
		t.Fatalf("second Delete() error = %v", err)
	}
}
