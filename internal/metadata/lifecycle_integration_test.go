package metadata_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hawoond/remote-sync/internal/blob"
	"github.com/hawoond/remote-sync/internal/domain"
	"github.com/hawoond/remote-sync/internal/metadata"
	"github.com/hawoond/remote-sync/internal/migrate"
	"github.com/hawoond/remote-sync/internal/syncengine"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSafetyWindowProtectsActiveRestoreBeforeGarbageCollection(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test file")
	}
	projectRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	if err := migrate.Up(ctx, pool, filepath.Join(projectRoot, "migrations")); err != nil {
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
	credential := domain.Hash(sha256.Sum256(
		[]byte("lifecycle-test-device-token-" + strings.Repeat("x", 32)),
	))
	if err := store.BootstrapDevelopment(
		ctx,
		userID,
		deviceID,
		folderID,
		credential,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.UpdateFolderPolicy(
		ctx,
		deviceID,
		folderID,
		time.Minute,
		time.Minute,
	); err != nil {
		t.Fatal(err)
	}

	firstContent := []byte("first version")
	first := commitIntegrationVersion(
		t,
		ctx,
		engine,
		deviceID,
		folderID,
		"",
		domain.ChangeKindCreate,
		firstContent,
	)
	if _, err := pool.Exec(ctx, `
		UPDATE file_versions
		SET created_at = now() - interval '2 minutes'
		WHERE id = $1
	`, first.VersionID); err != nil {
		t.Fatal(err)
	}

	secondContent := []byte("second version")
	second := commitIntegrationVersion(
		t,
		ctx,
		engine,
		deviceID,
		folderID,
		first.VersionID,
		domain.ChangeKindModify,
		secondContent,
	)
	if err := engine.AckChanges(ctx, deviceID, folderID, second.Sequence); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	protectedBySafetyWindow, err := store.PruneVersions(ctx, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if protectedBySafetyWindow.Versions != 0 || protectedBySafetyWindow.Objects != 0 {
		t.Fatalf("newly superseded content was pruned: %+v", protectedBySafetyWindow)
	}

	job, err := engine.StartRestore(ctx, deviceID, folderID, first.Sequence, false)
	if err != nil {
		t.Fatal(err)
	}
	job, items, err := engine.ListRestoreItems(ctx, deviceID, job.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != domain.RestoreStateRunning || len(items) != 1 {
		t.Fatalf("active restore = %+v, items = %+v", job, items)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE file_versions
		SET superseded_at = now() - interval '2 minutes'
		WHERE id = $1
	`, first.VersionID); err != nil {
		t.Fatal(err)
	}

	protected, err := store.PruneVersions(ctx, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if protected.Versions != 0 || protected.Objects != 0 {
		t.Fatalf("active restore pruned content: %+v", protected)
	}
	if _, err := engine.ReportRestoreItem(
		ctx,
		deviceID,
		job.ID,
		items[0].Ordinal,
		domain.RestoreItemStateApplied,
		"",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.FinishRestore(ctx, deviceID, job.ID, true, ""); err != nil {
		t.Fatal(err)
	}

	pruned, err := store.PruneVersions(ctx, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if pruned.Versions != 1 || pruned.Objects != 1 {
		t.Fatalf("prune result = %+v, want one version and object", pruned)
	}
	if _, err := engine.StartRestore(
		ctx,
		deviceID,
		folderID,
		first.Sequence,
		false,
	); !errors.Is(err, metadata.ErrInvalidState) {
		t.Fatalf("historical restore after prune error = %v, want ErrInvalidState", err)
	}
	if garbage, err := store.PendingGarbage(ctx, now, 100); err != nil {
		t.Fatal(err)
	} else if len(garbage) != 0 {
		t.Fatalf("garbage available before grace period: %+v", garbage)
	}
	garbage, err := store.PendingGarbage(ctx, now.Add(2*time.Minute), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(garbage) != 1 || garbage[0].Hash != domain.Hash(sha256.Sum256(firstContent)) {
		t.Fatalf("pending garbage = %+v", garbage)
	}
	if err := blobStore.Delete(ctx, garbage[0].Hash); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.DeleteGarbageRecord(ctx, garbage[0].Hash)
	if err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("garbage metadata was not deleted")
	}
	secondHash := domain.Hash(sha256.Sum256(secondContent))
	if exists, err := blobStore.Exists(ctx, secondHash, int64(len(secondContent))); err != nil {
		t.Fatal(err)
	} else if !exists {
		t.Fatal("current object was collected")
	}

	abandonedContent := []byte("abandoned-upload-" + uuid.NewString())
	abandonedHash := domain.Hash(sha256.Sum256(abandonedContent))
	abandonedOperationID := uuid.NewString()
	abandoned, err := engine.BeginUpload(ctx, syncengine.BeginUploadRequest{
		DeviceID:      deviceID,
		OperationID:   abandonedOperationID,
		FolderID:      folderID,
		RelativePath:  "history/abandoned.txt",
		Kind:          domain.ChangeKindCreate,
		Hash:          abandonedHash,
		Size:          int64(len(abandonedContent)),
		MTimeUnixNano: time.Now().UnixNano(),
		PortableMode:  0o600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if abandoned.Disposition != domain.UploadDispositionRequired {
		t.Fatalf("abandoned upload disposition = %v", abandoned.Disposition)
	}
	if _, err := engine.AppendUpload(
		ctx,
		deviceID,
		abandoned.SessionID,
		0,
		abandonedContent[:4],
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE upload_sessions
		SET expires_at = now() - interval '1 minute'
		WHERE id = $1
	`, abandoned.SessionID); err != nil {
		t.Fatal(err)
	}
	expired, err := store.ExpireUploadSessions(ctx, time.Now().UTC(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0] != abandoned.SessionID {
		t.Fatalf("first expired upload pass = %v", expired)
	}
	retry, err := store.ExpireUploadSessions(ctx, time.Now().UTC(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(retry) != 1 || retry[0] != abandoned.SessionID {
		t.Fatalf("retryable expired upload pass = %v", retry)
	}
	if err := blobStore.Abort(ctx, abandoned.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteUploadCleanup(ctx, abandoned.SessionID); err != nil {
		t.Fatal(err)
	}
	cleaned, err := store.ExpireUploadSessions(ctx, time.Now().UTC(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(cleaned) != 0 {
		t.Fatalf("cleaned upload was selected again: %v", cleaned)
	}
}

func commitIntegrationVersion(
	t *testing.T,
	ctx context.Context,
	engine *syncengine.Engine,
	deviceID, folderID, baseVersionID string,
	kind domain.ChangeKind,
	content []byte,
) domain.CommitResult {
	t.Helper()
	hash := domain.Hash(sha256.Sum256(content))
	operationID := uuid.NewString()
	mtime := time.Now().UnixNano()
	begin, err := engine.BeginUpload(ctx, syncengine.BeginUploadRequest{
		DeviceID:      deviceID,
		OperationID:   operationID,
		FolderID:      folderID,
		RelativePath:  "history/file.txt",
		BaseVersionID: baseVersionID,
		Kind:          kind,
		Hash:          hash,
		Size:          int64(len(content)),
		MTimeUnixNano: mtime,
		PortableMode:  0o600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if begin.Disposition == domain.UploadDispositionRequired {
		if _, err := engine.AppendUpload(
			ctx,
			deviceID,
			begin.SessionID,
			begin.NextOffset,
			content,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.FinalizeUpload(ctx, deviceID, begin.SessionID); err != nil {
			t.Fatal(err)
		}
	}
	result, err := engine.Commit(ctx, syncengine.CommitRequest{
		DeviceID:      deviceID,
		OperationID:   operationID,
		FolderID:      folderID,
		RelativePath:  "history/file.txt",
		BaseVersionID: baseVersionID,
		Kind:          kind,
		UploadSession: begin.SessionID,
		ObjectHash:    hash,
		Size:          int64(len(content)),
		MTimeUnixNano: mtime,
		PortableMode:  0o600,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
