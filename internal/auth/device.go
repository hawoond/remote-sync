package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/hawoond/remote-sync/internal/domain"
	"google.golang.org/grpc/metadata"
)

var ErrUnauthenticated = errors.New("device authentication failed")

type DeviceResolver interface {
	Resolve(context.Context) (string, error)
}

type CredentialLookup interface {
	DeviceForCredential(context.Context, domain.Hash) (string, error)
}

type DatabaseResolver struct {
	lookup CredentialLookup
}

func NewDatabaseResolver(lookup CredentialLookup) (*DatabaseResolver, error) {
	if lookup == nil {
		return nil, ErrUnauthenticated
	}
	return &DatabaseResolver{lookup: lookup}, nil
}

func (r *DatabaseResolver) Resolve(ctx context.Context) (string, error) {
	token, err := bearerToken(ctx)
	if err != nil {
		return "", err
	}
	digest := domain.Hash(sha256.Sum256([]byte(token)))
	deviceID, err := r.lookup.DeviceForCredential(ctx, digest)
	if err != nil {
		return "", err
	}
	if deviceID == "" {
		return "", ErrUnauthenticated
	}
	return deviceID, nil
}

type TokenResolver struct {
	deviceID  string
	tokenHash [sha256.Size]byte
}

func NewTokenResolver(deviceID, token string) (*TokenResolver, error) {
	if _, err := uuid.Parse(deviceID); err != nil {
		return nil, ErrUnauthenticated
	}
	if len(token) < 32 {
		return nil, ErrUnauthenticated
	}
	return &TokenResolver{
		deviceID:  deviceID,
		tokenHash: sha256.Sum256([]byte(token)),
	}, nil
}

func (r *TokenResolver) Resolve(ctx context.Context) (string, error) {
	token, err := bearerToken(ctx)
	if err != nil {
		return "", err
	}
	supplied := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(supplied[:], r.tokenHash[:]) != 1 {
		return "", ErrUnauthenticated
	}
	return r.deviceID, nil
}

func bearerToken(ctx context.Context) (string, error) {
	incoming, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", ErrUnauthenticated
	}
	values := incoming.Get("authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return "", ErrUnauthenticated
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	if len(token) < 32 {
		return "", ErrUnauthenticated
	}
	return token, nil
}

func OutgoingContext(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}
