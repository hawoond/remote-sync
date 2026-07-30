package garbage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hawoond/remote-sync/internal/domain"
	"github.com/hawoond/remote-sync/internal/metadata"
)

type Store interface {
	ExpireUploadSessions(context.Context, time.Time, int) ([]string, error)
	CompleteUploadCleanup(context.Context, string) error
	PruneVersions(context.Context, time.Time, int) (metadata.PruneResult, error)
	PendingGarbage(context.Context, time.Time, int) ([]domain.GarbageObject, error)
	DeleteGarbageRecord(context.Context, domain.Hash) (bool, error)
}

type BlobStore interface {
	Abort(context.Context, string) error
	Delete(context.Context, domain.Hash) error
}

type Collector struct {
	store Store
	blobs BlobStore
	limit int
	now   func() time.Time
}

func New(store Store, blobs BlobStore, limit int) (*Collector, error) {
	if store == nil || blobs == nil || limit <= 0 || limit > 1000 {
		return nil, errors.New("invalid garbage collector configuration")
	}
	return &Collector{
		store: store,
		blobs: blobs,
		limit: limit,
		now:   time.Now,
	}, nil
}

func (c *Collector) RunOnce(ctx context.Context) (domain.GarbageCollectionReport, error) {
	now := c.now().UTC()
	report := domain.GarbageCollectionReport{}

	expired, err := c.store.ExpireUploadSessions(ctx, now, c.limit)
	if err != nil {
		return report, fmt.Errorf("expire uploads: %w", err)
	}
	for _, sessionID := range expired {
		if err := c.blobs.Abort(ctx, sessionID); err != nil {
			return report, fmt.Errorf("remove expired upload %s: %w", sessionID, err)
		}
		if err := c.store.CompleteUploadCleanup(ctx, sessionID); err != nil {
			return report, fmt.Errorf("complete expired upload %s: %w", sessionID, err)
		}
		report.ExpiredUploads++
	}

	pruned, err := c.store.PruneVersions(ctx, now, c.limit)
	if err != nil {
		return report, fmt.Errorf("prune versions: %w", err)
	}
	report.PrunedVersions = pruned.Versions
	report.MarkedObjects = pruned.Objects

	objects, err := c.store.PendingGarbage(ctx, now, c.limit)
	if err != nil {
		return report, fmt.Errorf("list pending objects: %w", err)
	}
	for _, object := range objects {
		if err := c.blobs.Delete(ctx, object.Hash); err != nil {
			return report, fmt.Errorf("delete object %s: %w", object.Hash.String(), err)
		}
		deleted, err := c.store.DeleteGarbageRecord(ctx, object.Hash)
		if err != nil {
			return report, err
		}
		if !deleted {
			return report, fmt.Errorf("object %s became referenced during deletion", object.Hash.String())
		}
		report.DeletedObjects++
	}
	return report, nil
}

func (c *Collector) Run(
	ctx context.Context,
	interval time.Duration,
	report func(domain.GarbageCollectionReport, error),
) error {
	if interval <= 0 {
		return errors.New("garbage collection interval must be positive")
	}
	run := func() {
		result, err := c.RunOnce(ctx)
		if report != nil {
			report(result, err)
		}
	}
	run()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			run()
		}
	}
}
