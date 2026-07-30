package grpcserver_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	syncv1 "github.com/hawoond/remote-sync/api/sync/v1"
	"github.com/hawoond/remote-sync/internal/auth"
	"github.com/hawoond/remote-sync/internal/blob"
	"github.com/hawoond/remote-sync/internal/domain"
	"github.com/hawoond/remote-sync/internal/localdb"
	"github.com/hawoond/remote-sync/internal/metadata"
	"github.com/hawoond/remote-sync/internal/migrate"
	"github.com/hawoond/remote-sync/internal/syncengine"
	"github.com/hawoond/remote-sync/internal/transport/grpcclient"
	"github.com/hawoond/remote-sync/internal/transport/grpcserver"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestUploadCommitOverGRPC(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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
	projectRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..", ".."))
	if err := migrate.Up(ctx, pool, filepath.Join(projectRoot, "migrations")); err != nil {
		t.Fatal(err)
	}

	limits := metadata.Limits{
		MaxFileSize:                 64 << 20,
		MaxFolderLiveSize:           1 << 30,
		MaxUserLiveSize:             1 << 30,
		MaxPendingUploadSizePerUser: 128 << 20,
		MaxChunkSize:                4,
		UploadSessionTTL:            time.Hour,
	}
	userID := uuid.NewString()
	deviceID := uuid.NewString()
	folderID := uuid.NewString()
	token := "test-device-token-" + strings.Repeat("x", 32)

	store := metadata.NewPostgres(pool, limits)
	credentialDigest := domain.Hash(sha256.Sum256([]byte(token)))
	if err := store.BootstrapDevelopment(
		ctx,
		userID,
		deviceID,
		folderID,
		credentialDigest,
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})

	blobStore, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := auth.NewDatabaseResolver(store)
	if err != nil {
		t.Fatal(err)
	}
	engine := syncengine.New(store, blobStore, limits)
	rpcServer := grpc.NewServer()
	syncv1.RegisterSyncServiceServer(rpcServer, grpcserver.New(engine, resolver))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- rpcServer.Serve(listener)
	}()
	t.Cleanup(func() {
		rpcServer.Stop()
		_ = listener.Close()
		select {
		case err := <-serveErrors:
			if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
				t.Errorf("serve gRPC: %v", err)
			}
		default:
		}
	})

	client, err := grpcclient.New(listener.Addr().String(), token, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	primaryFolder, err := client.EnsureFolder(
		ctx,
		folderID,
		"root:v1:"+strings.Repeat("a", 64),
		"primary worktree",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if primaryFolder.FolderID != folderID || primaryFolder.Created {
		t.Fatalf("primary folder registration = %+v", primaryFolder)
	}
	secondaryFolder, err := client.EnsureFolder(
		ctx,
		folderID,
		"root:v1:"+strings.Repeat("b", 64),
		"secondary worktree",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if secondaryFolder.FolderID == folderID || !secondaryFolder.Created {
		t.Fatalf("secondary folder registration = %+v", secondaryFolder)
	}
	replayedFolder, err := client.EnsureFolder(
		ctx,
		folderID,
		secondaryFolder.ClientKey,
		"renamed locally",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayedFolder.FolderID != secondaryFolder.FolderID || replayedFolder.Created {
		t.Fatalf("replayed folder registration = %+v", replayedFolder)
	}

	legacyFolderID := uuid.NewString()
	if err := store.BootstrapDevelopment(
		ctx,
		userID,
		deviceID,
		legacyFolderID,
		credentialDigest,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE folders SET current_sequence = 1 WHERE id = $1
	`, legacyFolderID); err != nil {
		t.Fatal(err)
	}
	unclaimedLegacy, err := client.EnsureFolder(
		ctx,
		legacyFolderID,
		"root:v1:"+strings.Repeat("d", 64),
		"unknown legacy root",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if unclaimedLegacy.FolderID == legacyFolderID || !unclaimedLegacy.Created {
		t.Fatalf("unclaimed legacy registration = %+v", unclaimedLegacy)
	}
	claimedLegacy, err := client.EnsureFolder(
		ctx,
		legacyFolderID,
		"root:v1:"+strings.Repeat("e", 64),
		"known legacy root",
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if claimedLegacy.FolderID != legacyFolderID || claimedLegacy.Created {
		t.Fatalf("claimed legacy registration = %+v", claimedLegacy)
	}

	content := []byte("grpc integration " + userID)
	sum := sha256.Sum256(content)
	operation := localdb.Operation{
		OperationID:   uuid.NewString(),
		FolderID:      folderID,
		PathKey:       "documents/notes.txt",
		DisplayPath:   "documents/notes.txt",
		Kind:          domain.ChangeKindCreate,
		Hash:          domain.Hash(sum),
		Size:          int64(len(content)),
		MTimeUnixNano: time.Now().UnixNano(),
		PortableMode:  0o640,
	}
	reservation, err := client.BeginUpload(ctx, operation)
	if err != nil {
		t.Fatal(err)
	}
	if reservation.Disposition != domain.UploadDispositionRequired {
		t.Fatalf("upload disposition = %v, want required", reservation.Disposition)
	}

	var progress int64
	uploadedHash, uploadedSize, err := client.Upload(
		ctx,
		reservation.SessionID,
		reservation.NextOffset,
		bytes.NewReader(content),
		int(reservation.MaxChunkSize),
		func(next int64) error {
			progress = next
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if uploadedHash != operation.Hash || uploadedSize != operation.Size {
		t.Fatalf("uploaded object = %s/%d, want %s/%d", uploadedHash, uploadedSize, operation.Hash, operation.Size)
	}
	if progress != operation.Size {
		t.Fatalf("upload progress = %d, want %d", progress, operation.Size)
	}

	operation.UploadSession = reservation.SessionID
	result, err := client.Commit(ctx, operation)
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != domain.CommitDispositionCommitted {
		t.Fatalf("commit disposition = %v, want committed", result.Disposition)
	}
	if result.VersionID == "" || result.Sequence != 1 {
		t.Fatalf("commit result = version %q sequence %d", result.VersionID, result.Sequence)
	}

	replay, err := client.Commit(ctx, operation)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Disposition != domain.CommitDispositionIdempotentReplay {
		t.Fatalf("replay disposition = %v, want idempotent replay", replay.Disposition)
	}
	if replay.VersionID != result.VersionID || replay.Sequence != result.Sequence {
		t.Fatal("idempotent replay returned a different commit")
	}

	object, err := blobStore.Open(ctx, operation.Hash)
	if err != nil {
		t.Fatal(err)
	}
	defer object.Close()
	stored, err := io.ReadAll(object)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, content) {
		t.Fatal("stored object differs from uploaded content")
	}

	policy, err := client.UpdateFolderPolicy(
		ctx,
		folderID,
		72*time.Hour,
		2*time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if policy.SafetyWindow != 72*time.Hour || policy.GCGracePeriod != 2*time.Hour {
		t.Fatalf("updated policy = %+v", policy)
	}

	enrollment, err := client.CreateEnrollment(
		ctx,
		folderID,
		domain.FolderRoleRestoreAdmin,
		10*time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	enrollmentClient, err := grpcclient.New(listener.Addr().String(), "", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	defer enrollmentClient.Close()
	credentials, err := enrollmentClient.EnrollDevice(
		ctx,
		enrollment.Token,
		"restore-device",
		"test",
		`{"restore":true}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.FolderID != folderID ||
		credentials.Role != domain.FolderRoleRestoreAdmin ||
		credentials.DeviceID == "" ||
		credentials.DeviceToken == "" {
		t.Fatalf("enrolled credentials = %+v", credentials)
	}
	if _, err := enrollmentClient.EnrollDevice(
		ctx,
		enrollment.Token,
		"replay-device",
		"test",
		`{}`,
	); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("enrollment replay status = %v, want FailedPrecondition", status.Code(err))
	}

	readerEnrollment, err := client.CreateEnrollment(
		ctx,
		folderID,
		domain.FolderRoleReader,
		10*time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	readerCredentials, err := enrollmentClient.EnrollDevice(
		ctx,
		readerEnrollment.Token,
		"reader-device",
		"test",
		`{}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	readerClient, err := grpcclient.New(
		listener.Addr().String(),
		readerCredentials.DeviceToken,
		nil,
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer readerClient.Close()
	if _, err := readerClient.EnsureFolder(
		ctx,
		folderID,
		"root:v1:"+strings.Repeat("c", 64),
		"reader folder",
		false,
	); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("reader folder registration status = %v, want PermissionDenied", status.Code(err))
	}

	restoreClient, err := grpcclient.New(
		listener.Addr().String(),
		credentials.DeviceToken,
		nil,
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer restoreClient.Close()
	job, err := restoreClient.StartRestore(ctx, folderID, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if job.TotalItems != 1 || job.SnapshotSequence != 1 {
		t.Fatalf("restore job = %+v", job)
	}
	job, items, err := restoreClient.ListRestoreItems(ctx, job.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != domain.RestoreStateRunning || len(items) != 1 {
		t.Fatalf("restore manifest = job %+v items %+v", job, items)
	}
	var restored bytes.Buffer
	downloadedHash, downloadedSize, err := restoreClient.Download(
		ctx,
		folderID,
		items[0].ObjectHash,
		&restored,
	)
	if err != nil {
		t.Fatal(err)
	}
	if downloadedHash != operation.Hash ||
		downloadedSize != operation.Size ||
		!bytes.Equal(restored.Bytes(), content) {
		t.Fatal("restored object differs from committed content")
	}
	if _, err := restoreClient.ReportRestoreItem(
		ctx,
		job.ID,
		items[0].Ordinal,
		domain.RestoreItemStateApplied,
		"",
	); err != nil {
		t.Fatal(err)
	}
	job, err = restoreClient.FinishRestore(ctx, job.ID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if job.State != domain.RestoreStateCompleted || job.AppliedItems != 1 {
		t.Fatalf("completed restore job = %+v", job)
	}

	unauthorized, err := grpcclient.New(
		listener.Addr().String(),
		"wrong-device-token-"+strings.Repeat("y", 32),
		nil,
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer unauthorized.Close()
	operation.OperationID = uuid.NewString()
	if _, err := unauthorized.BeginUpload(ctx, operation); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unauthorized status = %v, want Unauthenticated", status.Code(err))
	}
}
