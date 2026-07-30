package grpcserver

import (
	"context"
	"time"

	syncv1 "github.com/hawoond/remote-sync/api/sync/v1"
	"github.com/hawoond/remote-sync/internal/domain"
	"github.com/hawoond/remote-sync/internal/syncengine"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *Server) CreateEnrollment(
	ctx context.Context,
	request *syncv1.CreateEnrollmentRequest,
) (*syncv1.CreateEnrollmentResponse, error) {
	deviceID, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	seconds := request.GetExpiresInSeconds()
	if seconds < 60 || seconds > int64((24*time.Hour)/time.Second) {
		return nil, toStatus(syncengine.ErrInvalidChange)
	}
	enrollment, err := s.engine.CreateEnrollment(
		ctx,
		deviceID,
		request.GetFolderId(),
		fromProtoFolderRole(request.GetRole()),
		time.Duration(seconds)*time.Second,
	)
	if err != nil {
		return nil, toStatus(err)
	}
	return &syncv1.CreateEnrollmentResponse{
		EnrollmentId:    enrollment.ID,
		EnrollmentToken: enrollment.Token,
		FolderId:        enrollment.FolderID,
		Role:            toProtoFolderRole(enrollment.Role),
		ExpiresAt:       timestamp(enrollment.ExpiresAt),
	}, nil
}

func (s *Server) EnrollDevice(
	ctx context.Context,
	request *syncv1.EnrollDeviceRequest,
) (*syncv1.EnrollDeviceResponse, error) {
	credentials, err := s.engine.EnrollDevice(
		ctx,
		request.GetEnrollmentToken(),
		request.GetDeviceName(),
		request.GetPlatform(),
		request.GetCapabilitiesJson(),
	)
	if err != nil {
		return nil, toStatus(err)
	}
	return &syncv1.EnrollDeviceResponse{
		DeviceId:    credentials.DeviceID,
		DeviceToken: credentials.DeviceToken,
		FolderId:    credentials.FolderID,
		Role:        toProtoFolderRole(credentials.Role),
	}, nil
}

func (s *Server) GetFolderPolicy(
	ctx context.Context,
	request *syncv1.GetFolderPolicyRequest,
) (*syncv1.FolderPolicy, error) {
	deviceID, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	policy, err := s.engine.GetFolderPolicy(ctx, deviceID, request.GetFolderId())
	if err != nil {
		return nil, toStatus(err)
	}
	return toProtoFolderPolicy(policy), nil
}

func (s *Server) UpdateFolderPolicy(
	ctx context.Context,
	request *syncv1.UpdateFolderPolicyRequest,
) (*syncv1.FolderPolicy, error) {
	deviceID, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	safetySeconds := request.GetSafetyWindowSeconds()
	graceSeconds := request.GetGcGracePeriodSeconds()
	if safetySeconds < 60 || safetySeconds > int64((365*24*time.Hour)/time.Second) ||
		graceSeconds < 60 || graceSeconds > int64((30*24*time.Hour)/time.Second) {
		return nil, toStatus(syncengine.ErrInvalidChange)
	}
	policy, err := s.engine.UpdateFolderPolicy(
		ctx,
		deviceID,
		request.GetFolderId(),
		time.Duration(safetySeconds)*time.Second,
		time.Duration(graceSeconds)*time.Second,
	)
	if err != nil {
		return nil, toStatus(err)
	}
	return toProtoFolderPolicy(policy), nil
}

func (s *Server) StartRestore(
	ctx context.Context,
	request *syncv1.StartRestoreRequest,
) (*syncv1.RestoreJob, error) {
	deviceID, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	job, err := s.engine.StartRestore(
		ctx,
		deviceID,
		request.GetFolderId(),
		request.GetSnapshotSequence(),
		request.GetOverwriteExisting(),
	)
	if err != nil {
		return nil, toStatus(err)
	}
	return toProtoRestoreJob(job), nil
}

func (s *Server) ListRestoreItems(
	ctx context.Context,
	request *syncv1.ListRestoreItemsRequest,
) (*syncv1.ListRestoreItemsResponse, error) {
	deviceID, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	job, items, err := s.engine.ListRestoreItems(
		ctx,
		deviceID,
		request.GetRestoreId(),
		request.GetAfterOrdinal(),
		int(request.GetLimit()),
	)
	if err != nil {
		return nil, toStatus(err)
	}
	response := &syncv1.ListRestoreItemsResponse{Job: toProtoRestoreJob(job)}
	for _, item := range items {
		response.Items = append(response.Items, &syncv1.RestoreItem{
			Ordinal:       item.Ordinal,
			EntryId:       item.EntryID,
			VersionId:     item.VersionID,
			PathKey:       item.PathKey,
			RelativePath:  item.DisplayPath,
			ObjectSha256:  item.ObjectHash.Bytes(),
			Size:          item.Size,
			MtimeUnixNano: item.MTimeUnixNano,
			PortableMode:  item.PortableMode,
			State:         toProtoRestoreItemState(item.State),
			ErrorMessage:  item.ErrorMessage,
		})
	}
	return response, nil
}

func (s *Server) ReportRestoreItem(
	ctx context.Context,
	request *syncv1.ReportRestoreItemRequest,
) (*syncv1.RestoreJob, error) {
	deviceID, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	job, err := s.engine.ReportRestoreItem(
		ctx,
		deviceID,
		request.GetRestoreId(),
		request.GetOrdinal(),
		fromProtoRestoreItemState(request.GetState()),
		request.GetErrorMessage(),
	)
	if err != nil {
		return nil, toStatus(err)
	}
	return toProtoRestoreJob(job), nil
}

func (s *Server) FinishRestore(
	ctx context.Context,
	request *syncv1.FinishRestoreRequest,
) (*syncv1.RestoreJob, error) {
	deviceID, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	job, err := s.engine.FinishRestore(
		ctx,
		deviceID,
		request.GetRestoreId(),
		request.GetSuccess(),
		request.GetErrorMessage(),
	)
	if err != nil {
		return nil, toStatus(err)
	}
	return toProtoRestoreJob(job), nil
}

func toProtoFolderPolicy(policy domain.FolderPolicy) *syncv1.FolderPolicy {
	return &syncv1.FolderPolicy{
		FolderId:             policy.FolderID,
		SafetyWindowSeconds:  int64(policy.SafetyWindow / time.Second),
		GcGracePeriodSeconds: int64(policy.GCGracePeriod / time.Second),
		Revision:             policy.Revision,
		UpdatedAt:            timestamp(policy.UpdatedAt),
	}
}

func toProtoRestoreJob(job domain.RestoreJob) *syncv1.RestoreJob {
	result := &syncv1.RestoreJob{
		RestoreId:         job.ID,
		FolderId:          job.FolderID,
		TargetDeviceId:    job.TargetDeviceID,
		SnapshotSequence:  job.SnapshotSequence,
		State:             toProtoRestoreState(job.State),
		OverwriteExisting: job.Overwrite,
		TotalItems:        job.TotalItems,
		AppliedItems:      job.AppliedItems,
		SkippedItems:      job.SkippedItems,
		FailedItems:       job.FailedItems,
		ErrorMessage:      job.ErrorMessage,
		CreatedAt:         timestamp(job.CreatedAt),
	}
	if !job.CompletedAt.IsZero() {
		result.CompletedAt = timestamp(job.CompletedAt)
	}
	return result
}

func timestamp(value time.Time) *timestamppb.Timestamp {
	return timestamppb.New(value)
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

func toProtoRestoreState(value domain.RestoreState) syncv1.RestoreState {
	switch value {
	case domain.RestoreStateReady:
		return syncv1.RestoreState_RESTORE_STATE_READY
	case domain.RestoreStateRunning:
		return syncv1.RestoreState_RESTORE_STATE_RUNNING
	case domain.RestoreStateCompleted:
		return syncv1.RestoreState_RESTORE_STATE_COMPLETED
	case domain.RestoreStateFailed:
		return syncv1.RestoreState_RESTORE_STATE_FAILED
	default:
		return syncv1.RestoreState_RESTORE_STATE_UNSPECIFIED
	}
}

func fromProtoRestoreItemState(value syncv1.RestoreItemState) domain.RestoreItemState {
	switch value {
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

func toProtoRestoreItemState(value domain.RestoreItemState) syncv1.RestoreItemState {
	switch value {
	case domain.RestoreItemStatePending:
		return syncv1.RestoreItemState_RESTORE_ITEM_STATE_PENDING
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
