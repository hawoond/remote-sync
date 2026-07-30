package metadata

import (
	"context"
	"errors"
	"time"

	"github.com/hawoond/remote-sync/internal/domain"
)

var (
	ErrNotFound            = errors.New("metadata not found")
	ErrPermissionDenied    = errors.New("permission denied")
	ErrOperationIDReused   = errors.New("operation ID reused with different intent")
	ErrUploadNotVerified   = errors.New("upload is not verified")
	ErrBaseVersionConflict = errors.New("base version conflict")
	ErrFolderUnavailable   = errors.New("folder is unavailable")
	ErrQuotaExceeded       = errors.New("quota exceeded")
	ErrSessionExpired      = errors.New("upload session expired")
	ErrInvalidState        = errors.New("invalid state transition")
)

type Limits struct {
	MaxFileSize                 int64
	MaxFolderLiveSize           int64
	MaxUserLiveSize             int64
	MaxPendingUploadSizePerUser int64
	MaxChunkSize                int32
	UploadSessionTTL            time.Duration
}

func DefaultLimits() Limits {
	return Limits{
		MaxFileSize:                 10 << 30,
		MaxFolderLiveSize:           1 << 40,
		MaxUserLiveSize:             2 << 40,
		MaxPendingUploadSizePerUser: 50 << 30,
		MaxChunkSize:                4 << 20,
		UploadSessionTTL:            24 * time.Hour,
	}
}

type BeginUploadParams struct {
	Upload        domain.BeginUpload
	ObjectPresent bool
	StorageKey    string
}

type UploadSession struct {
	ID           string
	DeviceID     string
	OperationID  string
	FolderID     string
	Hash         domain.Hash
	DeclaredSize int64
	ReceivedSize int64
	State        string
	ExpiresAt    time.Time
}

type Store interface {
	BeginUpload(context.Context, BeginUploadParams) (domain.UploadReservation, error)
	UploadSession(context.Context, string) (UploadSession, error)
	UpdateUploadProgress(context.Context, string, int64) error
	MarkUploadVerified(context.Context, string, string) error
	EnsureObject(context.Context, domain.Hash, int64, string) error
	Commit(context.Context, domain.CommitChange) (domain.CommitResult, error)
	ListChanges(context.Context, string, string, int64, int) ([]domain.Change, int64, error)
	AckChanges(context.Context, string, string, int64) error
}

type DevelopmentBootstrapper interface {
	BootstrapDevelopment(context.Context, string, string, string) error
}
