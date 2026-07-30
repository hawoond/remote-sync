package syncengine

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/hawoond/remote-sync/internal/blob"
	"github.com/hawoond/remote-sync/internal/domain"
	"github.com/hawoond/remote-sync/internal/metadata"
	"github.com/hawoond/remote-sync/internal/pathpolicy"
)

var (
	ErrInvalidIdentifier = errors.New("invalid identifier")
	ErrInvalidChange     = errors.New("invalid change")
	ErrFileTooLarge      = errors.New("file exceeds maximum size")
)

type BlobStore interface {
	Resume(context.Context, string) (int64, error)
	Append(context.Context, string, int64, []byte) (int64, error)
	Finalize(context.Context, string, domain.Hash, int64) (blob.Object, error)
	Exists(context.Context, domain.Hash, int64) (bool, error)
	Open(context.Context, domain.Hash) (*os.File, error)
	Abort(context.Context, string) error
}

type Engine struct {
	metadata metadata.Store
	blobs    BlobStore
	limits   metadata.Limits
	now      func() time.Time
}

func New(store metadata.Store, blobs BlobStore, limits metadata.Limits) *Engine {
	return &Engine{
		metadata: store,
		blobs:    blobs,
		limits:   limits,
		now:      time.Now,
	}
}

type BeginUploadRequest struct {
	DeviceID      string
	OperationID   string
	FolderID      string
	RelativePath  string
	BaseVersionID string
	Kind          domain.ChangeKind
	Hash          domain.Hash
	Size          int64
	MTimeUnixNano int64
	PortableMode  uint32
}

func (e *Engine) BeginUpload(ctx context.Context, request BeginUploadRequest) (domain.UploadReservation, error) {
	if err := validateRequiredIDs(request.DeviceID, request.OperationID, request.FolderID); err != nil {
		return domain.UploadReservation{}, err
	}
	if err := validateOptionalID(request.BaseVersionID); err != nil {
		return domain.UploadReservation{}, err
	}
	if request.Kind != domain.ChangeKindCreate && request.Kind != domain.ChangeKindModify {
		return domain.UploadReservation{}, ErrInvalidChange
	}
	if request.Hash.IsZero() {
		return domain.UploadReservation{}, domain.ErrInvalidHash
	}
	if request.Size < 0 {
		return domain.UploadReservation{}, ErrInvalidChange
	}
	if request.Size > e.limits.MaxFileSize {
		return domain.UploadReservation{}, ErrFileTooLarge
	}
	canonical, err := pathpolicy.Canonicalize(request.RelativePath)
	if err != nil {
		return domain.UploadReservation{}, err
	}
	digest := intentDigest(
		request.Kind,
		request.FolderID,
		canonical.Key,
		request.BaseVersionID,
		request.Hash,
		request.Size,
		request.MTimeUnixNano,
		request.PortableMode,
	)
	present, err := e.blobs.Exists(ctx, request.Hash, request.Size)
	if err != nil {
		return domain.UploadReservation{}, err
	}
	if present {
		if err := e.metadata.EnsureObject(
			ctx,
			request.Hash,
			request.Size,
			blob.StorageKey(request.Hash),
		); err != nil {
			return domain.UploadReservation{}, err
		}
	}

	reservation, err := e.metadata.BeginUpload(ctx, metadata.BeginUploadParams{
		Upload: domain.BeginUpload{
			DeviceID:      request.DeviceID,
			OperationID:   request.OperationID,
			FolderID:      request.FolderID,
			DisplayPath:   canonical.Display,
			PathKey:       canonical.Key,
			BaseVersionID: request.BaseVersionID,
			Hash:          request.Hash,
			Size:          request.Size,
			MTimeUnixNano: request.MTimeUnixNano,
			PortableMode:  request.PortableMode,
			RequestDigest: digest,
			RequestedAt:   e.now().UTC(),
		},
		ObjectPresent: present,
		StorageKey:    blob.StorageKey(request.Hash),
	})
	if err != nil {
		return domain.UploadReservation{}, err
	}
	if reservation.Disposition != domain.UploadDispositionRequired {
		return reservation, nil
	}

	offset, err := e.blobs.Resume(ctx, reservation.SessionID)
	if err != nil {
		return domain.UploadReservation{}, err
	}
	if offset > request.Size {
		_ = e.blobs.Abort(ctx, reservation.SessionID)
		return domain.UploadReservation{}, metadata.ErrInvalidState
	}
	if err := e.metadata.UpdateUploadProgress(ctx, reservation.SessionID, offset); err != nil {
		return domain.UploadReservation{}, err
	}
	reservation.NextOffset = offset
	return reservation, nil
}

func (e *Engine) AppendUpload(
	ctx context.Context,
	deviceID, sessionID string,
	offset int64,
	data []byte,
) (int64, error) {
	if _, err := uuid.Parse(deviceID); err != nil {
		return 0, ErrInvalidIdentifier
	}
	session, err := e.metadata.UploadSession(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	if session.DeviceID != deviceID || session.State != "ACTIVE" {
		return 0, metadata.ErrPermissionDenied
	}
	if len(data) > int(e.limits.MaxChunkSize) || offset > session.DeclaredSize-int64(len(data)) {
		return 0, ErrInvalidChange
	}
	next, err := e.blobs.Append(ctx, sessionID, offset, data)
	if err != nil {
		return next, err
	}
	if err := e.metadata.UpdateUploadProgress(ctx, sessionID, next); err != nil {
		return next, err
	}
	return next, nil
}

func (e *Engine) FinalizeUpload(ctx context.Context, deviceID, sessionID string) (blob.Object, error) {
	session, err := e.metadata.UploadSession(ctx, sessionID)
	if err != nil {
		return blob.Object{}, err
	}
	if session.DeviceID != deviceID || session.State != "ACTIVE" {
		return blob.Object{}, metadata.ErrPermissionDenied
	}
	if session.ReceivedSize != session.DeclaredSize {
		return blob.Object{}, metadata.ErrUploadNotVerified
	}
	object, err := e.blobs.Finalize(ctx, sessionID, session.Hash, session.DeclaredSize)
	if err != nil {
		return blob.Object{}, err
	}
	if err := e.metadata.MarkUploadVerified(ctx, sessionID, blob.StorageKey(object.Hash)); err != nil {
		return blob.Object{}, err
	}
	return object, nil
}

type CommitRequest struct {
	DeviceID      string
	OperationID   string
	FolderID      string
	RelativePath  string
	BaseVersionID string
	Kind          domain.ChangeKind
	UploadSession string
	ObjectHash    domain.Hash
	Size          int64
	MTimeUnixNano int64
	PortableMode  uint32
}

func (e *Engine) Commit(ctx context.Context, request CommitRequest) (domain.CommitResult, error) {
	if err := validateRequiredIDs(request.DeviceID, request.OperationID, request.FolderID); err != nil {
		return domain.CommitResult{}, err
	}
	if err := validateOptionalID(request.BaseVersionID); err != nil {
		return domain.CommitResult{}, err
	}
	if !request.Kind.Valid() {
		return domain.CommitResult{}, ErrInvalidChange
	}
	canonical, err := pathpolicy.Canonicalize(request.RelativePath)
	if err != nil {
		return domain.CommitResult{}, err
	}
	if request.Kind == domain.ChangeKindDelete {
		if request.Size != 0 || !request.ObjectHash.IsZero() || request.UploadSession != "" {
			return domain.CommitResult{}, ErrInvalidChange
		}
	} else {
		if request.Size < 0 || request.Size > e.limits.MaxFileSize || request.ObjectHash.IsZero() {
			return domain.CommitResult{}, ErrInvalidChange
		}
		present, err := e.blobs.Exists(ctx, request.ObjectHash, request.Size)
		if err != nil {
			return domain.CommitResult{}, err
		}
		if !present {
			return domain.CommitResult{}, metadata.ErrUploadNotVerified
		}
		if err := e.metadata.EnsureObject(
			ctx,
			request.ObjectHash,
			request.Size,
			blob.StorageKey(request.ObjectHash),
		); err != nil {
			return domain.CommitResult{}, err
		}
		if request.UploadSession != "" {
			session, err := e.metadata.UploadSession(ctx, request.UploadSession)
			if err != nil {
				return domain.CommitResult{}, err
			}
			if session.DeviceID != request.DeviceID ||
				session.OperationID != request.OperationID ||
				session.FolderID != request.FolderID ||
				session.Hash != request.ObjectHash ||
				session.DeclaredSize != request.Size ||
				(session.State != "VERIFIED" && session.State != "COMMITTED") {
				return domain.CommitResult{}, metadata.ErrUploadNotVerified
			}
		}
	}

	digest := intentDigest(
		request.Kind,
		request.FolderID,
		canonical.Key,
		request.BaseVersionID,
		request.ObjectHash,
		request.Size,
		request.MTimeUnixNano,
		request.PortableMode,
	)
	return e.metadata.Commit(ctx, domain.CommitChange{
		DeviceID:      request.DeviceID,
		OperationID:   request.OperationID,
		FolderID:      request.FolderID,
		DisplayPath:   canonical.Display,
		PathKey:       canonical.Key,
		BaseVersionID: request.BaseVersionID,
		Kind:          request.Kind,
		UploadSession: request.UploadSession,
		ObjectHash:    request.ObjectHash,
		Size:          request.Size,
		MTimeUnixNano: request.MTimeUnixNano,
		PortableMode:  request.PortableMode,
		RequestDigest: digest,
		RequestedAt:   e.now().UTC(),
	})
}

func (e *Engine) ListChanges(
	ctx context.Context,
	deviceID, folderID string,
	after int64,
	limit int,
) ([]domain.Change, int64, error) {
	if err := validateRequiredIDs(deviceID, folderID); err != nil {
		return nil, 0, err
	}
	if after < 0 {
		return nil, 0, ErrInvalidChange
	}
	return e.metadata.ListChanges(ctx, deviceID, folderID, after, limit)
}

func (e *Engine) AckChanges(ctx context.Context, deviceID, folderID string, sequence int64) error {
	if err := validateRequiredIDs(deviceID, folderID); err != nil {
		return err
	}
	if sequence < 0 {
		return ErrInvalidChange
	}
	return e.metadata.AckChanges(ctx, deviceID, folderID, sequence)
}

func (e *Engine) OpenObject(ctx context.Context, hash domain.Hash) (io.ReadCloser, error) {
	if hash.IsZero() {
		return nil, domain.ErrInvalidHash
	}
	return e.blobs.Open(ctx, hash)
}

func validateRequiredIDs(values ...string) error {
	for _, value := range values {
		if value == "" {
			return ErrInvalidIdentifier
		}
		if _, err := uuid.Parse(value); err != nil {
			return ErrInvalidIdentifier
		}
	}
	return nil
}

func validateOptionalID(value string) error {
	if value == "" {
		return nil
	}
	return validateRequiredIDs(value)
}

func intentDigest(
	kind domain.ChangeKind,
	folderID, pathKey, baseVersionID string,
	hash domain.Hash,
	size, mtime int64,
	mode uint32,
) domain.Hash {
	hasher := sha256.New()
	writeByte(hasher, 1)
	writeByte(hasher, byte(kind))
	writeString(hasher, folderID)
	writeString(hasher, pathKey)
	writeString(hasher, baseVersionID)
	_, _ = hasher.Write(hash[:])
	writeInt64(hasher, size)
	writeInt64(hasher, mtime)
	var encodedMode [4]byte
	binary.BigEndian.PutUint32(encodedMode[:], mode)
	_, _ = hasher.Write(encodedMode[:])
	result, _ := domain.HashFromBytes(hasher.Sum(nil))
	return result
}

func writeString(writer io.Writer, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = writer.Write(length[:])
	_, _ = io.WriteString(writer, value)
}

func writeInt64(writer io.Writer, value int64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	_, _ = writer.Write(encoded[:])
}

func writeByte(writer io.Writer, value byte) {
	_, _ = writer.Write([]byte{value})
}

func ValidateConfiguration(limits metadata.Limits) error {
	if limits.MaxFileSize <= 0 ||
		limits.MaxFolderLiveSize <= 0 ||
		limits.MaxUserLiveSize <= 0 ||
		limits.MaxPendingUploadSizePerUser <= 0 ||
		limits.MaxChunkSize <= 0 ||
		limits.MaxChunkSize > blob.MaxChunkSize ||
		limits.UploadSessionTTL <= 0 {
		return fmt.Errorf("invalid limits configuration")
	}
	return nil
}
