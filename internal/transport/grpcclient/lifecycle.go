package grpcclient

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"time"

	syncv1 "github.com/hawoond/remote-sync/api/sync/v1"
	"github.com/hawoond/remote-sync/internal/auth"
	"github.com/hawoond/remote-sync/internal/domain"
)

func (c *Client) EnsureFolder(
	ctx context.Context,
	sourceFolderID, clientKey, displayName string,
	allowSourceBinding bool,
) (domain.FolderRegistration, error) {
	ctx = auth.OutgoingContext(ctx, c.token)
	response, err := c.client.EnsureFolder(ctx, &syncv1.EnsureFolderRequest{
		SourceFolderId:     sourceFolderID,
		ClientKey:          clientKey,
		DisplayName:        displayName,
		AllowSourceBinding: allowSourceBinding,
	})
	if err != nil {
		return domain.FolderRegistration{}, err
	}
	return domain.FolderRegistration{
		FolderID:    response.GetFolderId(),
		ClientKey:   response.GetClientKey(),
		DisplayName: response.GetDisplayName(),
		Role:        fromProtoFolderRole(response.GetRole()),
		Created:     response.GetCreated(),
	}, nil
}

func (c *Client) CreateEnrollment(
	ctx context.Context,
	folderID string,
	role domain.FolderRole,
	ttl time.Duration,
) (domain.Enrollment, error) {
	ctx = auth.OutgoingContext(ctx, c.token)
	response, err := c.client.CreateEnrollment(ctx, &syncv1.CreateEnrollmentRequest{
		FolderId:         folderID,
		Role:             toProtoFolderRole(role),
		ExpiresInSeconds: int64(ttl / time.Second),
	})
	if err != nil {
		return domain.Enrollment{}, err
	}
	result := domain.Enrollment{
		ID:       response.GetEnrollmentId(),
		FolderID: response.GetFolderId(),
		Role:     fromProtoFolderRole(response.GetRole()),
		Token:    response.GetEnrollmentToken(),
	}
	if response.GetExpiresAt() != nil {
		result.ExpiresAt = response.GetExpiresAt().AsTime()
	}
	return result, nil
}

func (c *Client) EnrollDevice(
	ctx context.Context,
	enrollmentToken, deviceName, platform, capabilitiesJSON string,
) (domain.DeviceCredentials, error) {
	response, err := c.client.EnrollDevice(ctx, &syncv1.EnrollDeviceRequest{
		EnrollmentToken:  enrollmentToken,
		DeviceName:       deviceName,
		Platform:         platform,
		CapabilitiesJson: capabilitiesJSON,
	})
	if err != nil {
		return domain.DeviceCredentials{}, err
	}
	return domain.DeviceCredentials{
		DeviceID:    response.GetDeviceId(),
		DeviceToken: response.GetDeviceToken(),
		FolderID:    response.GetFolderId(),
		Role:        fromProtoFolderRole(response.GetRole()),
	}, nil
}

func (c *Client) GetFolderPolicy(
	ctx context.Context,
	folderID string,
) (domain.FolderPolicy, error) {
	ctx = auth.OutgoingContext(ctx, c.token)
	response, err := c.client.GetFolderPolicy(ctx, &syncv1.GetFolderPolicyRequest{
		FolderId: folderID,
	})
	if err != nil {
		return domain.FolderPolicy{}, err
	}
	return fromProtoFolderPolicy(response), nil
}

func (c *Client) UpdateFolderPolicy(
	ctx context.Context,
	folderID string,
	safetyWindow, gcGracePeriod time.Duration,
) (domain.FolderPolicy, error) {
	ctx = auth.OutgoingContext(ctx, c.token)
	response, err := c.client.UpdateFolderPolicy(ctx, &syncv1.UpdateFolderPolicyRequest{
		FolderId:             folderID,
		SafetyWindowSeconds:  int64(safetyWindow / time.Second),
		GcGracePeriodSeconds: int64(gcGracePeriod / time.Second),
	})
	if err != nil {
		return domain.FolderPolicy{}, err
	}
	return fromProtoFolderPolicy(response), nil
}

func (c *Client) StartRestore(
	ctx context.Context,
	folderID string,
	snapshotSequence int64,
	overwrite bool,
) (domain.RestoreJob, error) {
	ctx = auth.OutgoingContext(ctx, c.token)
	response, err := c.client.StartRestore(ctx, &syncv1.StartRestoreRequest{
		FolderId:          folderID,
		SnapshotSequence:  snapshotSequence,
		OverwriteExisting: overwrite,
	})
	if err != nil {
		return domain.RestoreJob{}, err
	}
	return fromProtoRestoreJob(response), nil
}

func (c *Client) ListRestoreItems(
	ctx context.Context,
	restoreID string,
	after int64,
	limit int,
) (domain.RestoreJob, []domain.RestoreItem, error) {
	ctx = auth.OutgoingContext(ctx, c.token)
	response, err := c.client.ListRestoreItems(ctx, &syncv1.ListRestoreItemsRequest{
		RestoreId:    restoreID,
		AfterOrdinal: after,
		Limit:        int32(limit),
	})
	if err != nil {
		return domain.RestoreJob{}, nil, err
	}
	items := make([]domain.RestoreItem, 0, len(response.GetItems()))
	for _, value := range response.GetItems() {
		hash, err := domain.HashFromBytes(value.GetObjectSha256())
		if err != nil {
			return domain.RestoreJob{}, nil, err
		}
		items = append(items, domain.RestoreItem{
			Ordinal:       value.GetOrdinal(),
			EntryID:       value.GetEntryId(),
			VersionID:     value.GetVersionId(),
			PathKey:       value.GetPathKey(),
			DisplayPath:   value.GetRelativePath(),
			ObjectHash:    hash,
			Size:          value.GetSize(),
			MTimeUnixNano: value.GetMtimeUnixNano(),
			PortableMode:  value.GetPortableMode(),
			State:         fromProtoRestoreItemState(value.GetState()),
			ErrorMessage:  value.GetErrorMessage(),
		})
	}
	return fromProtoRestoreJob(response.GetJob()), items, nil
}

func (c *Client) ReportRestoreItem(
	ctx context.Context,
	restoreID string,
	ordinal int64,
	state domain.RestoreItemState,
	errorMessage string,
) (domain.RestoreJob, error) {
	ctx = auth.OutgoingContext(ctx, c.token)
	response, err := c.client.ReportRestoreItem(ctx, &syncv1.ReportRestoreItemRequest{
		RestoreId:    restoreID,
		Ordinal:      ordinal,
		State:        toProtoRestoreItemState(state),
		ErrorMessage: errorMessage,
	})
	if err != nil {
		return domain.RestoreJob{}, err
	}
	return fromProtoRestoreJob(response), nil
}

func (c *Client) FinishRestore(
	ctx context.Context,
	restoreID string,
	success bool,
	errorMessage string,
) (domain.RestoreJob, error) {
	ctx = auth.OutgoingContext(ctx, c.token)
	response, err := c.client.FinishRestore(ctx, &syncv1.FinishRestoreRequest{
		RestoreId:    restoreID,
		Success:      success,
		ErrorMessage: errorMessage,
	})
	if err != nil {
		return domain.RestoreJob{}, err
	}
	return fromProtoRestoreJob(response), nil
}

func (c *Client) Download(
	ctx context.Context,
	folderID string,
	hash domain.Hash,
	writer io.Writer,
) (domain.Hash, int64, error) {
	ctx = auth.OutgoingContext(ctx, c.token)
	stream, err := c.client.Download(ctx, &syncv1.DownloadRequest{
		Sha256:   hash.Bytes(),
		FolderId: folderID,
	})
	if err != nil {
		return domain.Hash{}, 0, err
	}
	hasher := sha256.New()
	var offset int64
	for {
		chunk, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			return domain.Hash{}, offset, recvErr
		}
		if chunk.GetOffset() != offset {
			return domain.Hash{}, offset, fmt.Errorf("download offset mismatch")
		}
		written, err := writer.Write(chunk.GetData())
		if err != nil {
			return domain.Hash{}, offset, err
		}
		if written != len(chunk.GetData()) {
			return domain.Hash{}, offset, io.ErrShortWrite
		}
		if _, err := hasher.Write(chunk.GetData()); err != nil {
			return domain.Hash{}, offset, err
		}
		offset += int64(len(chunk.GetData()))
	}
	actual, err := domain.HashFromBytes(hasher.Sum(nil))
	if err != nil {
		return domain.Hash{}, offset, err
	}
	return actual, offset, nil
}

func fromProtoFolderPolicy(value *syncv1.FolderPolicy) domain.FolderPolicy {
	if value == nil {
		return domain.FolderPolicy{}
	}
	result := domain.FolderPolicy{
		FolderID:      value.GetFolderId(),
		SafetyWindow:  time.Duration(value.GetSafetyWindowSeconds()) * time.Second,
		GCGracePeriod: time.Duration(value.GetGcGracePeriodSeconds()) * time.Second,
		Revision:      value.GetRevision(),
	}
	if value.GetUpdatedAt() != nil {
		result.UpdatedAt = value.GetUpdatedAt().AsTime()
	}
	return result
}

func fromProtoRestoreJob(value *syncv1.RestoreJob) domain.RestoreJob {
	if value == nil {
		return domain.RestoreJob{}
	}
	result := domain.RestoreJob{
		ID:               value.GetRestoreId(),
		FolderID:         value.GetFolderId(),
		TargetDeviceID:   value.GetTargetDeviceId(),
		SnapshotSequence: value.GetSnapshotSequence(),
		State:            fromProtoRestoreState(value.GetState()),
		Overwrite:        value.GetOverwriteExisting(),
		TotalItems:       value.GetTotalItems(),
		AppliedItems:     value.GetAppliedItems(),
		SkippedItems:     value.GetSkippedItems(),
		FailedItems:      value.GetFailedItems(),
		ErrorMessage:     value.GetErrorMessage(),
	}
	if value.GetCreatedAt() != nil {
		result.CreatedAt = value.GetCreatedAt().AsTime()
	}
	if value.GetCompletedAt() != nil {
		result.CompletedAt = value.GetCompletedAt().AsTime()
	}
	return result
}

func toProtoFolderRole(value domain.FolderRole) syncv1.FolderRole {
	switch value {
	case domain.FolderRoleReader:
		return syncv1.FolderRole_FOLDER_ROLE_READER
	case domain.FolderRoleWriter:
		return syncv1.FolderRole_FOLDER_ROLE_WRITER
	case domain.FolderRoleRestoreAdmin:
		return syncv1.FolderRole_FOLDER_ROLE_RESTORE_ADMIN
	default:
		return syncv1.FolderRole_FOLDER_ROLE_UNSPECIFIED
	}
}

func fromProtoFolderRole(value syncv1.FolderRole) domain.FolderRole {
	switch value {
	case syncv1.FolderRole_FOLDER_ROLE_READER:
		return domain.FolderRoleReader
	case syncv1.FolderRole_FOLDER_ROLE_WRITER:
		return domain.FolderRoleWriter
	case syncv1.FolderRole_FOLDER_ROLE_RESTORE_ADMIN:
		return domain.FolderRoleRestoreAdmin
	default:
		return domain.FolderRoleUnspecified
	}
}

func fromProtoRestoreState(value syncv1.RestoreState) domain.RestoreState {
	switch value {
	case syncv1.RestoreState_RESTORE_STATE_READY:
		return domain.RestoreStateReady
	case syncv1.RestoreState_RESTORE_STATE_RUNNING:
		return domain.RestoreStateRunning
	case syncv1.RestoreState_RESTORE_STATE_COMPLETED:
		return domain.RestoreStateCompleted
	case syncv1.RestoreState_RESTORE_STATE_FAILED:
		return domain.RestoreStateFailed
	default:
		return domain.RestoreStateUnspecified
	}
}

func toProtoRestoreItemState(value domain.RestoreItemState) syncv1.RestoreItemState {
	switch value {
	case domain.RestoreItemStateApplied:
		return syncv1.RestoreItemState_RESTORE_ITEM_STATE_APPLIED
	case domain.RestoreItemStateSkipped:
		return syncv1.RestoreItemState_RESTORE_ITEM_STATE_SKIPPED
	case domain.RestoreItemStateFailed:
		return syncv1.RestoreItemState_RESTORE_ITEM_STATE_FAILED
	default:
		return syncv1.RestoreItemState_RESTORE_ITEM_STATE_UNSPECIFIED
	}
}

func fromProtoRestoreItemState(value syncv1.RestoreItemState) domain.RestoreItemState {
	switch value {
	case syncv1.RestoreItemState_RESTORE_ITEM_STATE_PENDING:
		return domain.RestoreItemStatePending
	case syncv1.RestoreItemState_RESTORE_ITEM_STATE_APPLIED:
		return domain.RestoreItemStateApplied
	case syncv1.RestoreItemState_RESTORE_ITEM_STATE_SKIPPED:
		return domain.RestoreItemStateSkipped
	case syncv1.RestoreItemState_RESTORE_ITEM_STATE_FAILED:
		return domain.RestoreItemStateFailed
	default:
		return domain.RestoreItemStateUnspecified
	}
}
