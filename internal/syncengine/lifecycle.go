package syncengine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hawoond/remote-sync/internal/domain"
	"github.com/hawoond/remote-sync/internal/metadata"
)

const (
	minEnrollmentTTL = time.Minute
	maxEnrollmentTTL = 24 * time.Hour
	minSafetyWindow  = time.Minute
	maxSafetyWindow  = 365 * 24 * time.Hour
	minGCGracePeriod = time.Minute
	maxGCGracePeriod = 30 * 24 * time.Hour
	maxErrorMessage  = 2048
)

func (e *Engine) CreateEnrollment(
	ctx context.Context,
	deviceID, folderID string,
	role domain.FolderRole,
	ttl time.Duration,
) (domain.Enrollment, error) {
	if err := validateRequiredIDs(deviceID, folderID); err != nil {
		return domain.Enrollment{}, err
	}
	if !role.Valid() || ttl < minEnrollmentTTL || ttl > maxEnrollmentTTL {
		return domain.Enrollment{}, ErrInvalidChange
	}
	token, digest, err := generateSecret("rse_")
	if err != nil {
		return domain.Enrollment{}, err
	}
	enrollment := domain.Enrollment{
		ID:        uuid.NewString(),
		FolderID:  folderID,
		Role:      role,
		Token:     token,
		ExpiresAt: e.now().UTC().Add(ttl),
	}
	if err := e.metadata.CreateEnrollment(ctx, metadata.CreateEnrollmentParams{
		ID:              enrollment.ID,
		CreatorDeviceID: deviceID,
		FolderID:        folderID,
		SecretDigest:    digest,
		Role:            role,
		ExpiresAt:       enrollment.ExpiresAt,
	}); err != nil {
		return domain.Enrollment{}, err
	}
	return enrollment, nil
}

func (e *Engine) EnrollDevice(
	ctx context.Context,
	enrollmentToken, deviceName, platform, capabilitiesJSON string,
) (domain.DeviceCredentials, error) {
	deviceName = strings.TrimSpace(deviceName)
	platform = strings.TrimSpace(platform)
	if len(enrollmentToken) < 32 ||
		len(deviceName) == 0 || len(deviceName) > 128 ||
		len(platform) == 0 || len(platform) > 64 ||
		len(capabilitiesJSON) > 16*1024 {
		return domain.DeviceCredentials{}, ErrInvalidChange
	}
	if capabilitiesJSON == "" {
		capabilitiesJSON = "{}"
	}
	var capabilities map[string]any
	if err := json.Unmarshal([]byte(capabilitiesJSON), &capabilities); err != nil ||
		capabilities == nil {
		return domain.DeviceCredentials{}, ErrInvalidChange
	}
	deviceToken, credentialDigest, err := generateSecret("rsd_")
	if err != nil {
		return domain.DeviceCredentials{}, err
	}
	enrollmentDigest := domain.Hash(sha256.Sum256([]byte(enrollmentToken)))
	result, err := e.metadata.ConsumeEnrollment(ctx, metadata.ConsumeEnrollmentParams{
		SecretDigest:     enrollmentDigest,
		DeviceID:         uuid.NewString(),
		CredentialDigest: credentialDigest,
		DeviceName:       deviceName,
		Platform:         platform,
		CapabilitiesJSON: capabilitiesJSON,
	})
	if err != nil {
		return domain.DeviceCredentials{}, err
	}
	result.DeviceToken = deviceToken
	return result, nil
}

func (e *Engine) GetFolderPolicy(
	ctx context.Context,
	deviceID, folderID string,
) (domain.FolderPolicy, error) {
	if err := validateRequiredIDs(deviceID, folderID); err != nil {
		return domain.FolderPolicy{}, err
	}
	return e.metadata.GetFolderPolicy(ctx, deviceID, folderID)
}

func (e *Engine) UpdateFolderPolicy(
	ctx context.Context,
	deviceID, folderID string,
	safetyWindow, gcGracePeriod time.Duration,
) (domain.FolderPolicy, error) {
	if err := validateRequiredIDs(deviceID, folderID); err != nil {
		return domain.FolderPolicy{}, err
	}
	if safetyWindow < minSafetyWindow || safetyWindow > maxSafetyWindow ||
		gcGracePeriod < minGCGracePeriod || gcGracePeriod > maxGCGracePeriod {
		return domain.FolderPolicy{}, ErrInvalidChange
	}
	return e.metadata.UpdateFolderPolicy(ctx, deviceID, domain.FolderPolicy{
		FolderID:      folderID,
		SafetyWindow:  safetyWindow,
		GCGracePeriod: gcGracePeriod,
	})
}

func (e *Engine) StartRestore(
	ctx context.Context,
	deviceID, folderID string,
	snapshotSequence int64,
	overwrite bool,
) (domain.RestoreJob, error) {
	if err := validateRequiredIDs(deviceID, folderID); err != nil {
		return domain.RestoreJob{}, err
	}
	if snapshotSequence < 0 {
		return domain.RestoreJob{}, ErrInvalidChange
	}
	return e.metadata.StartRestore(ctx, metadata.StartRestoreParams{
		ID:               uuid.NewString(),
		DeviceID:         deviceID,
		FolderID:         folderID,
		SnapshotSequence: snapshotSequence,
		Overwrite:        overwrite,
	})
}

func (e *Engine) ListRestoreItems(
	ctx context.Context,
	deviceID, restoreID string,
	after int64,
	limit int,
) (domain.RestoreJob, []domain.RestoreItem, error) {
	if err := validateRequiredIDs(deviceID, restoreID); err != nil {
		return domain.RestoreJob{}, nil, err
	}
	if after < 0 {
		return domain.RestoreJob{}, nil, ErrInvalidChange
	}
	return e.metadata.ListRestoreItems(ctx, deviceID, restoreID, after, limit)
}

func (e *Engine) ReportRestoreItem(
	ctx context.Context,
	deviceID, restoreID string,
	ordinal int64,
	state domain.RestoreItemState,
	errorMessage string,
) (domain.RestoreJob, error) {
	if err := validateRequiredIDs(deviceID, restoreID); err != nil {
		return domain.RestoreJob{}, err
	}
	if ordinal <= 0 ||
		(state != domain.RestoreItemStateApplied &&
			state != domain.RestoreItemStateSkipped &&
			state != domain.RestoreItemStateFailed) ||
		len(errorMessage) > maxErrorMessage ||
		(state == domain.RestoreItemStateFailed && strings.TrimSpace(errorMessage) == "") {
		return domain.RestoreJob{}, ErrInvalidChange
	}
	if state != domain.RestoreItemStateFailed {
		errorMessage = ""
	}
	return e.metadata.ReportRestoreItem(
		ctx,
		deviceID,
		restoreID,
		ordinal,
		state,
		errorMessage,
	)
}

func (e *Engine) FinishRestore(
	ctx context.Context,
	deviceID, restoreID string,
	success bool,
	errorMessage string,
) (domain.RestoreJob, error) {
	if err := validateRequiredIDs(deviceID, restoreID); err != nil {
		return domain.RestoreJob{}, err
	}
	if len(errorMessage) > maxErrorMessage ||
		(!success && strings.TrimSpace(errorMessage) == "") {
		return domain.RestoreJob{}, ErrInvalidChange
	}
	if success {
		errorMessage = ""
	}
	return e.metadata.FinishRestore(ctx, deviceID, restoreID, success, errorMessage)
}

func (e *Engine) OpenObjectForDevice(
	ctx context.Context,
	deviceID, folderID string,
	hash domain.Hash,
) (io.ReadCloser, error) {
	if err := validateRequiredIDs(deviceID, folderID); err != nil {
		return nil, err
	}
	if hash.IsZero() {
		return nil, domain.ErrInvalidHash
	}
	if err := e.metadata.AuthorizeObjectRead(ctx, deviceID, folderID, hash); err != nil {
		return nil, err
	}
	return e.blobs.Open(ctx, hash)
}

func generateSecret(prefix string) (string, domain.Hash, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", domain.Hash{}, fmt.Errorf("generate credential: %w", err)
	}
	token := prefix + base64.RawURLEncoding.EncodeToString(value)
	digest := domain.Hash(sha256.Sum256([]byte(token)))
	return token, digest, nil
}
