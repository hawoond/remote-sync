package domain

import "time"

type ChangeKind uint8

const (
	ChangeKindUnspecified ChangeKind = iota
	ChangeKindCreate
	ChangeKindModify
	ChangeKindDelete
	ChangeKindRestore
)

func (k ChangeKind) Valid() bool {
	switch k {
	case ChangeKindCreate, ChangeKindModify, ChangeKindDelete, ChangeKindRestore:
		return true
	default:
		return false
	}
}

type UploadDisposition uint8

const (
	UploadDispositionUnspecified UploadDisposition = iota
	UploadDispositionObjectPresent
	UploadDispositionRequired
)

type CommitDisposition uint8

const (
	CommitDispositionUnspecified CommitDisposition = iota
	CommitDispositionCommitted
	CommitDispositionIdempotentReplay
	CommitDispositionQuarantined
	CommitDispositionConflictCopy
)

type BeginUpload struct {
	DeviceID      string
	OperationID   string
	FolderID      string
	DisplayPath   string
	PathKey       string
	BaseVersionID string
	Hash          Hash
	Size          int64
	MTimeUnixNano int64
	PortableMode  uint32
	RequestDigest Hash
	RequestedAt   time.Time
}

type UploadReservation struct {
	Disposition  UploadDisposition
	SessionID    string
	NextOffset   int64
	MaxChunkSize int32
	ExpiresAt    time.Time
	DisplayPath  string
}

type CommitChange struct {
	DeviceID      string
	OperationID   string
	FolderID      string
	DisplayPath   string
	PathKey       string
	BaseVersionID string
	Kind          ChangeKind
	UploadSession string
	ObjectHash    Hash
	Size          int64
	MTimeUnixNano int64
	PortableMode  uint32
	RequestDigest Hash
	RequestedAt   time.Time
}

type CommitResult struct {
	Disposition  CommitDisposition
	VersionID    string
	Sequence     int64
	DisplayPath  string
	QuarantineID string
}

type Change struct {
	Sequence      int64
	EntryID       string
	VersionID     string
	DisplayPath   string
	Kind          ChangeKind
	ObjectHash    Hash
	Size          int64
	MTimeUnixNano int64
	PortableMode  uint32
}
