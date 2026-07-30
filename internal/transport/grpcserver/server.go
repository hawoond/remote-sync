package grpcserver

import (
	"context"
	"errors"
	"io"

	syncv1 "github.com/hawoond/remote-sync/api/sync/v1"
	"github.com/hawoond/remote-sync/internal/auth"
	"github.com/hawoond/remote-sync/internal/blob"
	"github.com/hawoond/remote-sync/internal/domain"
	"github.com/hawoond/remote-sync/internal/metadata"
	"github.com/hawoond/remote-sync/internal/pathpolicy"
	"github.com/hawoond/remote-sync/internal/syncengine"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const downloadChunkSize = 1 << 20

type Server struct {
	syncv1.UnimplementedSyncServiceServer
	engine   *syncengine.Engine
	resolver auth.DeviceResolver
}

func New(engine *syncengine.Engine, resolver auth.DeviceResolver) *Server {
	return &Server{engine: engine, resolver: resolver}
}

func (s *Server) BeginUpload(
	ctx context.Context,
	request *syncv1.BeginUploadRequest,
) (*syncv1.BeginUploadResponse, error) {
	deviceID, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	hash, err := domain.HashFromBytes(request.GetSha256())
	if err != nil {
		return nil, toStatus(err)
	}
	reservation, err := s.engine.BeginUpload(ctx, syncengine.BeginUploadRequest{
		DeviceID:      deviceID,
		OperationID:   request.GetOperationId(),
		FolderID:      request.GetFolderId(),
		RelativePath:  request.GetRelativePath(),
		BaseVersionID: request.GetBaseVersionId(),
		Kind:          fromProtoKind(request.GetKind()),
		Hash:          hash,
		Size:          request.GetSize(),
		MTimeUnixNano: request.GetMtimeUnixNano(),
		PortableMode:  request.GetPortableMode(),
	})
	if err != nil {
		return nil, toStatus(err)
	}
	response := &syncv1.BeginUploadResponse{
		Disposition:          toProtoUploadDisposition(reservation.Disposition),
		NextOffset:           reservation.NextOffset,
		MaxChunkSize:         reservation.MaxChunkSize,
		CanonicalDisplayPath: reservation.DisplayPath,
	}
	if reservation.SessionID != "" {
		response.UploadSessionId = &reservation.SessionID
	}
	if !reservation.ExpiresAt.IsZero() {
		response.ExpiresAt = timestamppb.New(reservation.ExpiresAt)
	}
	return response, nil
}

func (s *Server) Upload(stream grpc.ClientStreamingServer[syncv1.UploadChunk, syncv1.UploadResult]) error {
	deviceID, err := s.resolver.Resolve(stream.Context())
	if err != nil {
		return toStatus(err)
	}
	var sessionID string
	for {
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return toStatus(recvErr)
		}
		if sessionID == "" {
			sessionID = chunk.GetUploadSessionId()
		}
		if sessionID == "" || chunk.GetUploadSessionId() != sessionID {
			return toStatus(syncengine.ErrInvalidChange)
		}
		if _, err := s.engine.AppendUpload(
			stream.Context(),
			deviceID,
			sessionID,
			chunk.GetOffset(),
			chunk.GetData(),
		); err != nil {
			return toStatus(err)
		}
	}
	if sessionID == "" {
		return toStatus(syncengine.ErrInvalidChange)
	}
	object, err := s.engine.FinalizeUpload(stream.Context(), deviceID, sessionID)
	if err != nil {
		return toStatus(err)
	}
	return stream.SendAndClose(&syncv1.UploadResult{
		Sha256: object.Hash.Bytes(),
		Size:   object.Size,
	})
}

func (s *Server) CommitChange(
	ctx context.Context,
	request *syncv1.CommitChangeRequest,
) (*syncv1.CommitChangeResponse, error) {
	deviceID, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	var hash domain.Hash
	if len(request.GetObjectSha256()) > 0 {
		hash, err = domain.HashFromBytes(request.GetObjectSha256())
		if err != nil {
			return nil, toStatus(err)
		}
	}
	result, err := s.engine.Commit(ctx, syncengine.CommitRequest{
		DeviceID:      deviceID,
		OperationID:   request.GetOperationId(),
		FolderID:      request.GetFolderId(),
		RelativePath:  request.GetRelativePath(),
		BaseVersionID: request.GetBaseVersionId(),
		Kind:          fromProtoKind(request.GetKind()),
		UploadSession: request.GetUploadSessionId(),
		ObjectHash:    hash,
		Size:          request.GetSize(),
		MTimeUnixNano: request.GetMtimeUnixNano(),
		PortableMode:  request.GetPortableMode(),
	})
	if err != nil {
		return nil, toStatus(err)
	}
	response := &syncv1.CommitChangeResponse{
		Disposition:          toProtoCommitDisposition(result.Disposition),
		VersionId:            result.VersionID,
		FolderSequence:       result.Sequence,
		CanonicalDisplayPath: result.DisplayPath,
	}
	if result.QuarantineID != "" {
		response.QuarantineId = &result.QuarantineID
	}
	return response, nil
}

func (s *Server) Download(
	request *syncv1.DownloadRequest,
	stream grpc.ServerStreamingServer[syncv1.DownloadChunk],
) error {
	deviceID, err := s.resolver.Resolve(stream.Context())
	if err != nil {
		return toStatus(err)
	}
	if request.GetOffset() < 0 {
		return toStatus(syncengine.ErrInvalidChange)
	}
	hash, err := domain.HashFromBytes(request.GetSha256())
	if err != nil {
		return toStatus(err)
	}
	file, err := s.engine.OpenObjectForDevice(
		stream.Context(),
		deviceID,
		request.GetFolderId(),
		hash,
	)
	if err != nil {
		return toStatus(err)
	}
	defer file.Close()
	if request.GetOffset() > 0 {
		if _, err := io.CopyN(io.Discard, file, request.GetOffset()); err != nil {
			return toStatus(syncengine.ErrInvalidChange)
		}
	}
	offset := request.GetOffset()
	buffer := make([]byte, downloadChunkSize)
	for {
		n, readErr := file.Read(buffer)
		if n > 0 {
			if err := stream.Send(&syncv1.DownloadChunk{
				Offset: offset,
				Data:   append([]byte(nil), buffer[:n]...),
			}); err != nil {
				return toStatus(err)
			}
			offset += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return toStatus(readErr)
		}
	}
}

func (s *Server) ListChanges(
	ctx context.Context,
	request *syncv1.ListChangesRequest,
) (*syncv1.ListChangesResponse, error) {
	deviceID, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	changes, latest, err := s.engine.ListChanges(
		ctx,
		deviceID,
		request.GetFolderId(),
		request.GetAfterSequence(),
		int(request.GetLimit()),
	)
	if err != nil {
		return nil, toStatus(err)
	}
	response := &syncv1.ListChangesResponse{LatestSequence: latest}
	for _, change := range changes {
		item := &syncv1.Change{
			Sequence:      change.Sequence,
			EntryId:       change.EntryID,
			VersionId:     change.VersionID,
			RelativePath:  change.DisplayPath,
			Kind:          toProtoKind(change.Kind),
			Size:          change.Size,
			MtimeUnixNano: change.MTimeUnixNano,
			PortableMode:  change.PortableMode,
		}
		if !change.ObjectHash.IsZero() {
			value := change.ObjectHash.Bytes()
			item.ObjectSha256 = value
		}
		response.Changes = append(response.Changes, item)
	}
	return response, nil
}

func (s *Server) AckChanges(
	ctx context.Context,
	request *syncv1.AckChangesRequest,
) (*emptypb.Empty, error) {
	deviceID, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	if err := s.engine.AckChanges(ctx, deviceID, request.GetFolderId(), request.GetSequence()); err != nil {
		return nil, toStatus(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) OpenSync(stream grpc.BidiStreamingServer[syncv1.ClientFrame, syncv1.ServerFrame]) error {
	deviceID, err := s.resolver.Resolve(stream.Context())
	if err != nil {
		return toStatus(err)
	}
	for {
		frame, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			return nil
		}
		if recvErr != nil {
			return toStatus(recvErr)
		}
		_, latest, err := s.engine.ListChanges(
			stream.Context(),
			deviceID,
			frame.GetFolderId(),
			frame.GetAcknowledgedSequence(),
			1,
		)
		if err != nil {
			return toStatus(err)
		}
		if err := stream.Send(&syncv1.ServerFrame{
			FolderId:       frame.GetFolderId(),
			LatestSequence: latest,
		}); err != nil {
			return toStatus(err)
		}
	}
}

func toStatus(err error) error {
	if err == nil {
		return nil
	}
	code := codes.Internal
	reason := "INTERNAL"
	message := "internal error"

	var violation *pathpolicy.Violation
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "request deadline exceeded")
	case errors.Is(err, auth.ErrUnauthenticated):
		code, reason, message = codes.Unauthenticated, "DEVICE_AUTHENTICATION_FAILED", "authentication failed"
	case errors.As(err, &violation):
		code, reason, message = codes.InvalidArgument, string(violation.Code), "path policy violation"
	case errors.Is(err, syncengine.ErrInvalidIdentifier):
		code, reason, message = codes.InvalidArgument, "INVALID_IDENTIFIER", "invalid identifier"
	case errors.Is(err, syncengine.ErrInvalidChange), errors.Is(err, domain.ErrInvalidHash):
		code, reason, message = codes.InvalidArgument, "INVALID_CHANGE", "invalid change"
	case errors.Is(err, metadata.ErrPermissionDenied):
		code, reason, message = codes.PermissionDenied, "FOLDER_ACCESS_DENIED", "permission denied"
	case errors.Is(err, metadata.ErrOperationIDReused):
		code, reason, message = codes.AlreadyExists, "OPERATION_ID_REUSED", "operation ID already used"
	case errors.Is(err, metadata.ErrBaseVersionConflict):
		code, reason, message = codes.FailedPrecondition, "BASE_VERSION_CONFLICT", "base version conflict"
	case errors.Is(err, metadata.ErrFolderUnavailable):
		code, reason, message = codes.FailedPrecondition, "FOLDER_UNAVAILABLE", "folder unavailable"
	case errors.Is(err, metadata.ErrUploadNotVerified), errors.Is(err, metadata.ErrInvalidState):
		code, reason, message = codes.FailedPrecondition, "UPLOAD_NOT_VERIFIED", "upload not verified"
	case errors.Is(err, metadata.ErrSessionExpired):
		code, reason, message = codes.FailedPrecondition, "UPLOAD_SESSION_EXPIRED", "upload session expired"
	case errors.Is(err, metadata.ErrEnrollmentExpired):
		code, reason, message = codes.FailedPrecondition, "ENROLLMENT_UNAVAILABLE", "enrollment token expired or unavailable"
	case errors.Is(err, metadata.ErrRestoreIncomplete):
		code, reason, message = codes.FailedPrecondition, "RESTORE_INCOMPLETE", "restore has incomplete items"
	case errors.Is(err, metadata.ErrNotFound), errors.Is(err, blob.ErrObjectNotFound):
		code, reason, message = codes.NotFound, "NOT_FOUND", "resource not found"
	case errors.Is(err, metadata.ErrQuotaExceeded), errors.Is(err, syncengine.ErrFileTooLarge):
		code, reason, message = codes.ResourceExhausted, "QUOTA_EXCEEDED", "resource limit exceeded"
	case errors.Is(err, blob.ErrHashMismatch), errors.Is(err, blob.ErrSizeMismatch):
		code, reason, message = codes.DataLoss, "OBJECT_VERIFICATION_FAILED", "object verification failed"
	}

	st := status.New(code, message)
	withDetails, detailsErr := st.WithDetails(&errdetails.ErrorInfo{
		Reason: reason,
		Domain: "remote-sync",
	})
	if detailsErr != nil {
		return st.Err()
	}
	return withDetails.Err()
}

func fromProtoKind(kind syncv1.ChangeKind) domain.ChangeKind {
	switch kind {
	case syncv1.ChangeKind_CHANGE_KIND_CREATE:
		return domain.ChangeKindCreate
	case syncv1.ChangeKind_CHANGE_KIND_MODIFY:
		return domain.ChangeKindModify
	case syncv1.ChangeKind_CHANGE_KIND_DELETE:
		return domain.ChangeKindDelete
	case syncv1.ChangeKind_CHANGE_KIND_RESTORE:
		return domain.ChangeKindRestore
	default:
		return domain.ChangeKindUnspecified
	}
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

func toProtoUploadDisposition(value domain.UploadDisposition) syncv1.UploadDisposition {
	switch value {
	case domain.UploadDispositionObjectPresent:
		return syncv1.UploadDisposition_UPLOAD_DISPOSITION_OBJECT_PRESENT
	case domain.UploadDispositionRequired:
		return syncv1.UploadDisposition_UPLOAD_DISPOSITION_UPLOAD_REQUIRED
	default:
		return syncv1.UploadDisposition_UPLOAD_DISPOSITION_UNSPECIFIED
	}
}

func toProtoCommitDisposition(value domain.CommitDisposition) syncv1.CommitDisposition {
	switch value {
	case domain.CommitDispositionCommitted:
		return syncv1.CommitDisposition_COMMIT_DISPOSITION_COMMITTED
	case domain.CommitDispositionIdempotentReplay:
		return syncv1.CommitDisposition_COMMIT_DISPOSITION_IDEMPOTENT_REPLAY
	case domain.CommitDispositionQuarantined:
		return syncv1.CommitDisposition_COMMIT_DISPOSITION_QUARANTINED
	case domain.CommitDispositionConflictCopy:
		return syncv1.CommitDisposition_COMMIT_DISPOSITION_CONFLICT_COPY
	default:
		return syncv1.CommitDisposition_COMMIT_DISPOSITION_UNSPECIFIED
	}
}
