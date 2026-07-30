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
	ErrEnrollmentExpired   = errors.New("enrollment token expired or unavailable")
	ErrRestoreIncomplete   = errors.New("restore has incomplete items")
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

type CreateEnrollmentParams struct {
	ID              string
	CreatorDeviceID string
	FolderID        string
	SecretDigest    domain.Hash
	Role            domain.FolderRole
	ExpiresAt       time.Time
}

type ConsumeEnrollmentParams struct {
	SecretDigest     domain.Hash
	DeviceID         string
	CredentialDigest domain.Hash
	DeviceName       string
	Platform         string
	CapabilitiesJSON string
}

type StartRestoreParams struct {
	ID               string
	DeviceID         string
	FolderID         string
	SnapshotSequence int64
	Overwrite        bool
}

type PruneResult struct {
	Versions int
	Objects  int
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
	DeviceForCredential(context.Context, domain.Hash) (string, error)
	CreateEnrollment(context.Context, CreateEnrollmentParams) error
	ConsumeEnrollment(context.Context, ConsumeEnrollmentParams) (domain.DeviceCredentials, error)
	GetFolderPolicy(context.Context, string, string) (domain.FolderPolicy, error)
	UpdateFolderPolicy(context.Context, string, domain.FolderPolicy) (domain.FolderPolicy, error)
	StartRestore(context.Context, StartRestoreParams) (domain.RestoreJob, error)
	ListRestoreItems(context.Context, string, string, int64, int) (domain.RestoreJob, []domain.RestoreItem, error)
	ReportRestoreItem(context.Context, string, string, int64, domain.RestoreItemState, string) (domain.RestoreJob, error)
	FinishRestore(context.Context, string, string, bool, string) (domain.RestoreJob, error)
	AuthorizeObjectRead(context.Context, string, string, domain.Hash) error
	ExpireUploadSessions(context.Context, time.Time, int) ([]string, error)
	PruneVersions(context.Context, time.Time, int) (PruneResult, error)
	PendingGarbage(context.Context, time.Time, int) ([]domain.GarbageObject, error)
	DeleteGarbageRecord(context.Context, domain.Hash) (bool, error)
}

type DevelopmentBootstrapper interface {
	BootstrapDevelopment(context.Context, string, string, string, domain.Hash) error
}
