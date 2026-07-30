package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/hawoond/remote-sync/internal/domain"
	"google.golang.org/grpc/metadata"
)

type credentialLookupFunc func(context.Context, domain.Hash) (string, error)

func (f credentialLookupFunc) DeviceForCredential(
	ctx context.Context,
	digest domain.Hash,
) (string, error) {
	return f(ctx, digest)
}

func TestDatabaseResolverHashesBearerCredential(t *testing.T) {
	t.Parallel()

	const token = "device-token-with-at-least-thirty-two-characters"
	expected := domain.Hash(sha256.Sum256([]byte(token)))
	resolver, err := NewDatabaseResolver(credentialLookupFunc(
		func(_ context.Context, digest domain.Hash) (string, error) {
			if digest != expected {
				t.Fatalf("credential digest = %s, want %s", digest, expected)
			}
			return "3efec0ac-f749-4899-ae25-c811116931bc", nil
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	ctx := metadata.NewIncomingContext(
		context.Background(),
		metadata.Pairs("authorization", "Bearer "+token),
	)
	deviceID, err := resolver.Resolve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if deviceID != "3efec0ac-f749-4899-ae25-c811116931bc" {
		t.Fatalf("device ID = %q", deviceID)
	}
}

func TestDatabaseResolverRejectsUnknownCredential(t *testing.T) {
	t.Parallel()

	resolver, err := NewDatabaseResolver(credentialLookupFunc(
		func(context.Context, domain.Hash) (string, error) {
			return "", nil
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	ctx := metadata.NewIncomingContext(
		context.Background(),
		metadata.Pairs(
			"authorization",
			"Bearer unknown-device-token-with-at-least-thirty-two-characters",
		),
	)
	if _, err := resolver.Resolve(ctx); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("Resolve() error = %v, want ErrUnauthenticated", err)
	}
}
