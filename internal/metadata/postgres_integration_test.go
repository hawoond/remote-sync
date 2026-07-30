package metadata_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/hawoond/remote-sync/internal/blob"
	"github.com/hawoond/remote-sync/internal/domain"
	"github.com/hawoond/remote-sync/internal/metadata"
	"github.com/hawoond/remote-sync/internal/migrate"
	"github.com/hawoond/remote-sync/internal/syncengine"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresUploadCommitReplayDeleteAndDeduplicate(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	if err := migrate.Up(ctx, pool, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE users, objects CASCADE`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `TRUNCATE users, objects CASCADE`)
	})

	limits := metadata.DefaultLimits()
	store := metadata.NewPostgres(pool, limits)
	blobStore, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer blobStore.Close()
	engine := syncengine.New(store, blobStore, limits)

	userID := uuid.NewString()
	deviceID := uuid.NewString()
	folderID := uuid.NewString()
	credentialDigest := domain.Hash(sha256.Sum256([]byte("integration-device-token")))
	if err := store.BootstrapDevelopment(
		ctx,
		userID,
		deviceID,
		folderID,
		credentialDigest,
	); err != nil {
		t.Fatal(err)
	}

	content := []byte("immutable content")
	hash := domain.Hash(sha256.Sum256(content))
	operationID := uuid.NewString()
	begin := syncengine.BeginUploadRequest{
		DeviceID:      deviceID,
		OperationID:   operationID,
		FolderID:      folderID,
		RelativePath:  "Documents/report.txt",
		Kind:          domain.ChangeKindCreate,
		Hash:          hash,
		Size:          int64(len(content)),
		MTimeUnixNano: 123,
		PortableMode:  0o640,
	}
	reservation, err := engine.BeginUpload(ctx, begin)
	if err != nil {
		t.Fatalf("BeginUpload() error = %v", err)
	}
	if reservation.Disposition != domain.UploadDispositionRequired {
		t.Fatalf("disposition = %v", reservation.Disposition)
	}

	split := 7
	next, err := engine.AppendUpload(ctx, deviceID, reservation.SessionID, 0, content[:split])
	if err != nil {
		t.Fatalf("AppendUpload(first) error = %v", err)
	}
	replayed, err := engine.BeginUpload(ctx, begin)
	if err != nil {
		t.Fatalf("BeginUpload(replay) error = %v", err)
	}
	if replayed.SessionID != reservation.SessionID || replayed.NextOffset != next {
		t.Fatalf("replayed reservation = %+v, want session %s offset %d", replayed, reservation.SessionID, next)
	}
	if _, err := engine.AppendUpload(ctx, deviceID, reservation.SessionID, next, content[split:]); err != nil {
		t.Fatalf("AppendUpload(second) error = %v", err)
	}
	if _, err := engine.FinalizeUpload(ctx, deviceID, reservation.SessionID); err != nil {
		t.Fatalf("FinalizeUpload() error = %v", err)
	}

	commit := syncengine.CommitRequest{
		DeviceID:      deviceID,
		OperationID:   operationID,
		FolderID:      folderID,
		RelativePath:  begin.RelativePath,
		Kind:          begin.Kind,
		UploadSession: reservation.SessionID,
		ObjectHash:    hash,
		Size:          int64(len(content)),
		MTimeUnixNano: begin.MTimeUnixNano,
		PortableMode:  begin.PortableMode,
	}
	committed, err := engine.Commit(ctx, commit)
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if committed.Disposition != domain.CommitDispositionCommitted || committed.Sequence != 1 {
		t.Fatalf("commit result = %+v", committed)
	}
	replayedCommit, err := engine.Commit(ctx, commit)
	if err != nil {
		t.Fatalf("Commit(replay) error = %v", err)
	}
	if replayedCommit.Disposition != domain.CommitDispositionIdempotentReplay ||
		replayedCommit.VersionID != committed.VersionID ||
		replayedCommit.Sequence != committed.Sequence {
		t.Fatalf("replayed commit = %+v, committed = %+v", replayedCommit, committed)
	}

	changes, latest, err := engine.ListChanges(ctx, deviceID, folderID, 0, 100)
	if err != nil {
		t.Fatalf("ListChanges() error = %v", err)
	}
	if len(changes) != 1 || latest != 1 || changes[0].ObjectHash != hash {
		t.Fatalf("changes = %+v, latest = %d", changes, latest)
	}
	if err := engine.AckChanges(ctx, deviceID, folderID, latest); err != nil {
		t.Fatalf("AckChanges() error = %v", err)
	}

	secondOperation := uuid.NewString()
	secondBegin := begin
	secondBegin.OperationID = secondOperation
	secondBegin.RelativePath = "Documents/copy.txt"
	secondReservation, err := engine.BeginUpload(ctx, secondBegin)
	if err != nil {
		t.Fatalf("BeginUpload(deduplicated) error = %v", err)
	}
	if secondReservation.Disposition != domain.UploadDispositionObjectPresent {
		t.Fatalf("deduplicated disposition = %v", secondReservation.Disposition)
	}
	if _, err := engine.Commit(ctx, syncengine.CommitRequest{
		DeviceID:      deviceID,
		OperationID:   secondOperation,
		FolderID:      folderID,
		RelativePath:  secondBegin.RelativePath,
		Kind:          secondBegin.Kind,
		ObjectHash:    hash,
		Size:          int64(len(content)),
		MTimeUnixNano: secondBegin.MTimeUnixNano,
		PortableMode:  secondBegin.PortableMode,
	}); err != nil {
		t.Fatalf("Commit(deduplicated) error = %v", err)
	}

	deleteResult, err := engine.Commit(ctx, syncengine.CommitRequest{
		DeviceID:      deviceID,
		OperationID:   uuid.NewString(),
		FolderID:      folderID,
		RelativePath:  begin.RelativePath,
		BaseVersionID: committed.VersionID,
		Kind:          domain.ChangeKindDelete,
	})
	if err != nil {
		t.Fatalf("Commit(delete) error = %v", err)
	}
	if deleteResult.Sequence != 3 {
		t.Fatalf("delete sequence = %d, want 3", deleteResult.Sequence)
	}

	reused := begin
	reused.RelativePath = "different.txt"
	if _, err := engine.BeginUpload(ctx, reused); !errors.Is(err, metadata.ErrOperationIDReused) {
		t.Fatalf("expected ErrOperationIDReused, got %v", err)
	}
}
