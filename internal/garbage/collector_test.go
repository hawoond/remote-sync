package garbage

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/hawoond/remote-sync/internal/domain"
	"github.com/hawoond/remote-sync/internal/metadata"
)

type fakeStore struct {
	sessionIDs []string
	cleaned    []string
	pruned     metadata.PruneResult
	objects    []domain.GarbageObject
	deleted    []domain.Hash
}

func (s *fakeStore) ExpireUploadSessions(
	context.Context,
	time.Time,
	int,
) ([]string, error) {
	return append([]string(nil), s.sessionIDs...), nil
}

func (s *fakeStore) CompleteUploadCleanup(
	_ context.Context,
	sessionID string,
) error {
	s.cleaned = append(s.cleaned, sessionID)
	return nil
}

func (s *fakeStore) PruneVersions(
	context.Context,
	time.Time,
	int,
) (metadata.PruneResult, error) {
	return s.pruned, nil
}

func (s *fakeStore) PendingGarbage(
	context.Context,
	time.Time,
	int,
) ([]domain.GarbageObject, error) {
	return append([]domain.GarbageObject(nil), s.objects...), nil
}

func (s *fakeStore) DeleteGarbageRecord(
	_ context.Context,
	hash domain.Hash,
) (bool, error) {
	s.deleted = append(s.deleted, hash)
	return true, nil
}

type fakeBlobs struct {
	aborted []string
	deleted []domain.Hash
}

func (b *fakeBlobs) Abort(_ context.Context, sessionID string) error {
	b.aborted = append(b.aborted, sessionID)
	return nil
}

func (b *fakeBlobs) Delete(_ context.Context, hash domain.Hash) error {
	b.deleted = append(b.deleted, hash)
	return nil
}

func TestCollectorRunsExpiryPruneAndSweep(t *testing.T) {
	t.Parallel()

	hash := domain.Hash(sha256.Sum256([]byte("garbage")))
	store := &fakeStore{
		sessionIDs: []string{"4be970f1-d273-4727-860b-8b785111304c"},
		pruned:     metadata.PruneResult{Versions: 3, Objects: 1},
		objects:    []domain.GarbageObject{{Hash: hash, StorageKey: "object-key"}},
	}
	blobs := &fakeBlobs{}
	collector, err := New(store, blobs, 100)
	if err != nil {
		t.Fatal(err)
	}
	report, err := collector.RunOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report != (domain.GarbageCollectionReport{
		ExpiredUploads: 1,
		PrunedVersions: 3,
		MarkedObjects:  1,
		DeletedObjects: 1,
	}) {
		t.Fatalf("report = %+v", report)
	}
	if len(blobs.aborted) != 1 || blobs.aborted[0] != store.sessionIDs[0] {
		t.Fatalf("aborted uploads = %v", blobs.aborted)
	}
	if len(store.cleaned) != 1 || store.cleaned[0] != store.sessionIDs[0] {
		t.Fatalf("cleaned uploads = %v", store.cleaned)
	}
	if len(blobs.deleted) != 1 || blobs.deleted[0] != hash {
		t.Fatalf("deleted blobs = %v", blobs.deleted)
	}
	if len(store.deleted) != 1 || store.deleted[0] != hash {
		t.Fatalf("deleted metadata = %v", store.deleted)
	}
}
