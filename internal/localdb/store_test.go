package localdb

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hawoond/remote-sync/internal/domain"
)

func TestOperationQueuePersistsAndSupersedes(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	hash := domain.Hash(sha256.Sum256([]byte("content")))
	first := Operation{
		OperationID:   uuid.NewString(),
		FolderID:      uuid.NewString(),
		PathKey:       "documents/report.txt",
		DisplayPath:   "Documents/report.txt",
		Kind:          domain.ChangeKindCreate,
		Hash:          hash,
		Size:          7,
		MTimeUnixNano: 1,
		PortableMode:  0o600,
	}
	if err := store.Enqueue(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.OperationID = uuid.NewString()
	second.Kind = domain.ChangeKindModify
	second.MTimeUnixNano = 2
	if err := store.Enqueue(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	claimed, err := store.ClaimNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.OperationID != second.OperationID || claimed.State != "RESERVING" || claimed.Attempt != 1 {
		t.Fatalf("claimed = %+v", claimed)
	}
	if err := store.Transition(ctx, second.OperationID, "RESERVING", "UPLOADING", Transition{
		UploadSession: uuid.NewString(),
		NextOffset:    4,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(ctx, second.OperationID, "UPLOADING", "COMMITTING", Transition{
		NextOffset: -1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(ctx, second.OperationID, "COMMITTING", "COMPLETED", Transition{
		NextOffset: -1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimNext(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected empty queue, got %v", err)
	}
}

func TestScanGenerationFindsMissingEntriesOnlyAfterSuccessfulScan(t *testing.T) {
	t.Parallel()

	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	folderID := uuid.NewString()
	generation, err := store.BeginScan(ctx, folderID)
	if err != nil {
		t.Fatal(err)
	}
	hash := domain.Hash(sha256.Sum256([]byte("one")))
	if err := store.UpsertEntry(ctx, Entry{
		FolderID:       folderID,
		PathKey:        "one.txt",
		DisplayPath:    "one.txt",
		Size:           3,
		Hash:           hash,
		Present:        true,
		ScanGeneration: generation,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteScan(ctx, folderID, generation); err != nil {
		t.Fatal(err)
	}

	nextGeneration, err := store.BeginScan(ctx, folderID)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := store.MissingEntries(ctx, folderID, nextGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0].PathKey != "one.txt" {
		t.Fatalf("missing = %+v", missing)
	}
}

func TestCursorIsMonotonic(t *testing.T) {
	t.Parallel()

	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	folderID := uuid.NewString()
	if err := store.SetCursor(ctx, folderID, 10, 8); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCursor(ctx, folderID, 9, 9); !errors.Is(err, ErrCursorRollback) {
		t.Fatalf("expected ErrCursorRollback, got %v", err)
	}
	if err := store.SetCursor(ctx, folderID, 11, 10); err != nil {
		t.Fatal(err)
	}
}

func TestRetryWaitBecomesClaimableWhenDue(t *testing.T) {
	t.Parallel()

	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Unix(1_800_000_000, 0).UTC()
	store.now = func() time.Time { return now }
	operation := Operation{
		OperationID: uuid.NewString(),
		FolderID:    uuid.NewString(),
		PathKey:     "file.txt",
		DisplayPath: "file.txt",
		Kind:        domain.ChangeKindCreate,
	}
	if err := store.Enqueue(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	retryAt := now.Add(time.Minute)
	if err := store.Transition(context.Background(), claimed.OperationID, "RESERVING", "RETRY_WAIT", Transition{
		NextOffset:    -1,
		NextAttemptAt: retryAt,
		ErrorCode:     "TRANSIENT",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimNext(context.Background()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected no due operation, got %v", err)
	}
	store.now = func() time.Time { return retryAt }
	if _, err := store.ClaimNext(context.Background()); err != nil {
		t.Fatalf("claim due retry: %v", err)
	}
}
