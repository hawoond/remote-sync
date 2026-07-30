package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/grpc/metadata"
)

var ErrUnauthenticated = errors.New("device authentication failed")

type DeviceResolver interface {
	Resolve(context.Context) (string, error)
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
	incoming, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", ErrUnauthenticated
	}
	values := incoming.Get("authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return "", ErrUnauthenticated
	}
	supplied := sha256.Sum256([]byte(strings.TrimPrefix(values[0], "Bearer ")))
	if subtle.ConstantTimeCompare(supplied[:], r.tokenHash[:]) != 1 {
		return "", ErrUnauthenticated
	}
	return r.deviceID, nil
}

func OutgoingContext(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}
