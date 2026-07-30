package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/hawoond/remote-sync/internal/domain"
	"github.com/hawoond/remote-sync/internal/localdb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeRemote struct {
	beginError error
	beginCalls int
	uploadData []byte
	committed  []localdb.Operation
}

func (f *fakeRemote) BeginUpload(
	_ context.Context,
	_ localdb.Operation,
) (domain.UploadReservation, error) {
	f.beginCalls++
	if f.beginError != nil {
		return domain.UploadReservation{}, f.beginError
	}
	return domain.UploadReservation{
		Disposition:  domain.UploadDispositionRequired,
		SessionID:    uuid.NewString(),
		MaxChunkSize: 4,
	}, nil
}

func (f *fakeRemote) Upload(
	_ context.Context,
	_ string,
	offset int64,
	reader io.Reader,
	_ int,
	progress func(int64) error,
) (domain.Hash, int64, error) {
	content, err := io.ReadAll(reader)
	if err != nil {
		return domain.Hash{}, 0, err
	}
	f.uploadData = append(f.uploadData, content...)
	if err := progress(offset + int64(len(content))); err != nil {
		return domain.Hash{}, 0, err
	}
	sum := sha256.Sum256(f.uploadData)
	return domain.Hash(sum), int64(len(f.uploadData)), nil
}

func (f *fakeRemote) Commit(
	_ context.Context,
	operation localdb.Operation,
) (domain.CommitResult, error) {
	f.committed = append(f.committed, operation)
	return domain.CommitResult{
		Disposition: domain.CommitDispositionCommitted,
		VersionID:   uuid.NewString(),
		Sequence:    int64(len(f.committed)),
		DisplayPath: operation.DisplayPath,
	}, nil
}

func TestProcessorUploadsAndCommitsFile(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rootPath := t.TempDir()
	content := []byte("durable upload")
	if err := os.WriteFile(filepath.Join(rootPath, "notes.txt"), content, 0o640); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(rootPath, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}

	store, err := localdb.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	folderID := uuid.NewString()
	sum := sha256.Sum256(content)
	hash := domain.Hash(sum)
	entry := localdb.Entry{
		FolderID:      folderID,
		PathKey:       "notes.txt",
		DisplayPath:   "notes.txt",
		Size:          int64(len(content)),
		MTimeUnixNano: info.ModTime().UnixNano(),
		PortableMode:  uint32(info.Mode().Perm()),
		Hash:          hash,
		Present:       true,
	}
	if err := store.UpsertEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}
	operationID := uuid.NewString()
	if err := store.Enqueue(ctx, localdb.Operation{
		OperationID:   operationID,
		FolderID:      folderID,
		PathKey:       entry.PathKey,
		DisplayPath:   entry.DisplayPath,
		Kind:          domain.ChangeKindCreate,
		Hash:          hash,
		Size:          entry.Size,
		MTimeUnixNano: entry.MTimeUnixNano,
		PortableMode:  entry.PortableMode,
	}); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	remote := &fakeRemote{}
	processor := NewProcessor(root, store, remote, testLogger())

	if err := processor.ProcessNext(ctx); err != nil {
		t.Fatalf("process operation: %v", err)
	}
	if string(remote.uploadData) != string(content) {
		t.Fatalf("uploaded %q, want %q", remote.uploadData, content)
	}
	if len(remote.committed) != 1 {
		t.Fatalf("commit calls = %d, want 1", len(remote.committed))
	}
	if remote.committed[0].UploadSession == "" {
		t.Fatal("commit did not retain upload session")
	}
	updated, err := store.Entry(ctx, folderID, entry.PathKey)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ServerVersion == "" {
		t.Fatal("server version was not stored")
	}
	if _, err := store.ClaimNext(ctx); !errors.Is(err, localdb.ErrNotFound) {
		t.Fatalf("claim after completion = %v, want ErrNotFound", err)
	}
}

func TestProcessorRetriesUnavailableReservation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rootPath := t.TempDir()
	content := []byte("retry me")
	filePath := filepath.Join(rootPath, "retry.txt")
	if err := os.WriteFile(filePath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatal(err)
	}

	store, err := localdb.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	folderID := uuid.NewString()
	sum := sha256.Sum256(content)
	hash := domain.Hash(sum)
	entry := localdb.Entry{
		FolderID:      folderID,
		PathKey:       "retry.txt",
		DisplayPath:   "retry.txt",
		Size:          int64(len(content)),
		MTimeUnixNano: info.ModTime().UnixNano(),
		PortableMode:  uint32(info.Mode().Perm()),
		Hash:          hash,
		Present:       true,
	}
	if err := store.UpsertEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}
	if err := store.Enqueue(ctx, localdb.Operation{
		OperationID:   uuid.NewString(),
		FolderID:      folderID,
		PathKey:       entry.PathKey,
		DisplayPath:   entry.DisplayPath,
		Kind:          domain.ChangeKindCreate,
		Hash:          entry.Hash,
		Size:          entry.Size,
		MTimeUnixNano: entry.MTimeUnixNano,
		PortableMode:  entry.PortableMode,
	}); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	remote := &fakeRemote{beginError: status.Error(codes.Unavailable, "offline")}
	processor := NewProcessor(root, store, remote, testLogger())
	processor.maxBackoff = 0

	err = processor.ProcessNext(ctx)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("process error = %v, want Unavailable", err)
	}
	retry, err := store.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("claim retry: %v", err)
	}
	if retry.Attempt != 2 {
		t.Fatalf("attempt = %d, want 2", retry.Attempt)
	}
	if retry.LastErrorCode != codes.Unavailable.String() {
		t.Fatalf("last error code = %q, want %q", retry.LastErrorCode, codes.Unavailable.String())
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
