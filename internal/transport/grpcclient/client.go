package grpcclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"

	syncv1 "github.com/hawoond/remote-sync/api/sync/v1"
	"github.com/hawoond/remote-sync/internal/auth"
	"github.com/hawoond/remote-sync/internal/domain"
	"github.com/hawoond/remote-sync/internal/localdb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

type Client struct {
	connection *grpc.ClientConn
	client     syncv1.SyncServiceClient
	token      string
}

func New(address, token string, tlsConfig *tls.Config, allowInsecure bool) (*Client, error) {
	var transport credentials.TransportCredentials
	if allowInsecure {
		transport = insecure.NewCredentials()
	} else {
		if tlsConfig == nil {
			tlsConfig = &tls.Config{MinVersion: tls.VersionTLS13}
		}
		transport = credentials.NewTLS(tlsConfig)
	}
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(transport))
	if err != nil {
		return nil, fmt.Errorf("create gRPC client: %w", err)
	}
	return &Client{
		connection: connection,
		client:     syncv1.NewSyncServiceClient(connection),
		token:      token,
	}, nil
}

func (c *Client) Close() error {
	return c.connection.Close()
}

func (c *Client) BeginUpload(
	ctx context.Context,
	operation localdb.Operation,
) (domain.UploadReservation, error) {
	ctx = auth.OutgoingContext(ctx, c.token)
	response, err := c.client.BeginUpload(ctx, &syncv1.BeginUploadRequest{
		OperationId:   operation.OperationID,
		FolderId:      operation.FolderID,
		RelativePath:  operation.DisplayPath,
		BaseVersionId: optionalString(operation.BaseVersionID),
		Size:          operation.Size,
		Sha256:        operation.Hash.Bytes(),
		MtimeUnixNano: operation.MTimeUnixNano,
		PortableMode:  operation.PortableMode,
		Kind:          toProtoKind(operation.Kind),
	})
	if err != nil {
		return domain.UploadReservation{}, err
	}
	result := domain.UploadReservation{
		Disposition:  fromProtoUploadDisposition(response.GetDisposition()),
		SessionID:    response.GetUploadSessionId(),
		NextOffset:   response.GetNextOffset(),
		MaxChunkSize: response.GetMaxChunkSize(),
		DisplayPath:  response.GetCanonicalDisplayPath(),
	}
	if response.GetExpiresAt() != nil {
		result.ExpiresAt = response.GetExpiresAt().AsTime()
	}
	return result, nil
}

func (c *Client) Upload(
	ctx context.Context,
	sessionID string,
	offset int64,
	reader io.Reader,
	chunkSize int,
	progress func(int64) error,
) (domain.Hash, int64, error) {
	if chunkSize <= 0 {
		return domain.Hash{}, 0, fmt.Errorf("invalid upload chunk size")
	}
	ctx = auth.OutgoingContext(ctx, c.token)
	stream, err := c.client.Upload(ctx)
	if err != nil {
		return domain.Hash{}, 0, err
	}
	buffer := make([]byte, chunkSize)
	next := offset
	sent := false
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			sent = true
			if err := stream.Send(&syncv1.UploadChunk{
				UploadSessionId: sessionID,
				Offset:          next,
				Data:            buffer[:n],
			}); err != nil {
				return domain.Hash{}, next, err
			}
			next += int64(n)
			if progress != nil {
				if err := progress(next); err != nil {
					return domain.Hash{}, next, err
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return domain.Hash{}, next, readErr
		}
	}
	if !sent {
		if err := stream.Send(&syncv1.UploadChunk{
			UploadSessionId: sessionID,
			Offset:          next,
		}); err != nil {
			return domain.Hash{}, next, err
		}
	}
	result, err := stream.CloseAndRecv()
	if err != nil {
		return domain.Hash{}, next, err
	}
	hash, err := domain.HashFromBytes(result.GetSha256())
	if err != nil {
		return domain.Hash{}, next, err
	}
	return hash, result.GetSize(), nil
}

func (c *Client) Commit(
	ctx context.Context,
	operation localdb.Operation,
) (domain.CommitResult, error) {
	ctx = auth.OutgoingContext(ctx, c.token)
	request := &syncv1.CommitChangeRequest{
		OperationId:   operation.OperationID,
		FolderId:      operation.FolderID,
		RelativePath:  operation.DisplayPath,
		BaseVersionId: optionalString(operation.BaseVersionID),
		Kind:          toProtoKind(operation.Kind),
		MtimeUnixNano: operation.MTimeUnixNano,
		PortableMode:  operation.PortableMode,
		Size:          operation.Size,
	}
	if operation.UploadSession != "" {
		request.UploadSessionId = optionalString(operation.UploadSession)
	}
	if !operation.Hash.IsZero() {
		value := operation.Hash.Bytes()
		request.ObjectSha256 = value
	}
	response, err := c.client.CommitChange(ctx, request)
	if err != nil {
		return domain.CommitResult{}, err
	}
	return domain.CommitResult{
		Disposition:  fromProtoCommitDisposition(response.GetDisposition()),
		VersionID:    response.GetVersionId(),
		Sequence:     response.GetFolderSequence(),
		DisplayPath:  response.GetCanonicalDisplayPath(),
		QuarantineID: response.GetQuarantineId(),
	}, nil
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func toProtoKind(kind domain.ChangeKind) syncv1.ChangeKind {
	switch kind {
	case domain.ChangeKindCreate:
		return syncv1.ChangeKind_CHANGE_KIND_CREATE
	case domain.ChangeKindModify:
		return syncv1.ChangeKind_CHANGE_KIND_MODIFY
	case domain.ChangeKindDelete:
		return syncv1.ChangeKind_CHANGE_KIND_DELETE
	case domain.ChangeKindRestore:
		return syncv1.ChangeKind_CHANGE_KIND_RESTORE
	default:
		return syncv1.ChangeKind_CHANGE_KIND_UNSPECIFIED
	}
}

func fromProtoUploadDisposition(value syncv1.UploadDisposition) domain.UploadDisposition {
	switch value {
	case syncv1.UploadDisposition_UPLOAD_DISPOSITION_OBJECT_PRESENT:
		return domain.UploadDispositionObjectPresent
	case syncv1.UploadDisposition_UPLOAD_DISPOSITION_UPLOAD_REQUIRED:
		return domain.UploadDispositionRequired
	default:
		return domain.UploadDispositionUnspecified
	}
}

func fromProtoCommitDisposition(value syncv1.CommitDisposition) domain.CommitDisposition {
	switch value {
	case syncv1.CommitDisposition_COMMIT_DISPOSITION_COMMITTED:
		return domain.CommitDispositionCommitted
	case syncv1.CommitDisposition_COMMIT_DISPOSITION_IDEMPOTENT_REPLAY:
		return domain.CommitDispositionIdempotentReplay
	case syncv1.CommitDisposition_COMMIT_DISPOSITION_QUARANTINED:
		return domain.CommitDispositionQuarantined
	case syncv1.CommitDisposition_COMMIT_DISPOSITION_CONFLICT_COPY:
		return domain.CommitDispositionConflictCopy
	default:
		return domain.CommitDispositionUnspecified
	}
}
