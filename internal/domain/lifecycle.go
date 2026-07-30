package domain

import "time"

type FolderRole uint8

const (
	FolderRoleUnspecified FolderRole = iota
	FolderRoleReader
	FolderRoleWriter
	FolderRoleRestoreAdmin
)

func (r FolderRole) Valid() bool {
	switch r {
	case FolderRoleReader, FolderRoleWriter, FolderRoleRestoreAdmin:
		return true
	default:
		return false
	}
}

func (r FolderRole) String() string {
	switch r {
	case FolderRoleReader:
		return "READER"
	case FolderRoleWriter:
		return "WRITER"
	case FolderRoleRestoreAdmin:
		return "RESTORE_ADMIN"
	default:
		return ""
	}
}

type Enrollment struct {
	ID        string
	FolderID  string
	Role      FolderRole
	Token     string
	ExpiresAt time.Time
}

type DeviceCredentials struct {
	DeviceID    string
	DeviceToken string
	FolderID    string
	Role        FolderRole
}

type FolderRegistration struct {
	FolderID    string
	ClientKey   string
	DisplayName string
	Role        FolderRole
	Created     bool
}

type FolderPolicy struct {
	FolderID      string
	SafetyWindow  time.Duration
	GCGracePeriod time.Duration
	Revision      int64
	UpdatedAt     time.Time
}

type RestoreState uint8

const (
	RestoreStateUnspecified RestoreState = iota
	RestoreStateReady
	RestoreStateRunning
	RestoreStateCompleted
	RestoreStateFailed
)

type RestoreItemState uint8

const (
	RestoreItemStateUnspecified RestoreItemState = iota
	RestoreItemStatePending
	RestoreItemStateApplied
	RestoreItemStateSkipped
	RestoreItemStateFailed
)

type RestoreJob struct {
	ID               string
	FolderID         string
	TargetDeviceID   string
	SnapshotSequence int64
	State            RestoreState
	Overwrite        bool
	TotalItems       int64
	AppliedItems     int64
	SkippedItems     int64
	FailedItems      int64
	ErrorMessage     string
	CreatedAt        time.Time
	CompletedAt      time.Time
}

type RestoreItem struct {
	Ordinal       int64
	EntryID       string
	VersionID     string
	PathKey       string
	DisplayPath   string
	ObjectHash    Hash
	Size          int64
	MTimeUnixNano int64
	PortableMode  uint32
	State         RestoreItemState
	ErrorMessage  string
}

type GarbageObject struct {
	Hash       Hash
	StorageKey string
}

type GarbageCollectionReport struct {
	ExpiredUploads int
	PrunedVersions int
	MarkedObjects  int
	DeletedObjects int
}
