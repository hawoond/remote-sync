package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

const SHA256Size = sha256.Size

var ErrInvalidHash = errors.New("invalid SHA-256 hash")

type Hash [SHA256Size]byte

func HashFromBytes(value []byte) (Hash, error) {
	var hash Hash
	if len(value) != len(hash) {
		return hash, fmt.Errorf("%w: got %d bytes", ErrInvalidHash, len(value))
	}
	copy(hash[:], value)
	return hash, nil
}

func ParseHash(value string) (Hash, error) {
	var hash Hash
	if len(value) != hex.EncodedLen(len(hash)) {
		return hash, fmt.Errorf("%w: got %d hex characters", ErrInvalidHash, len(value))
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return hash, fmt.Errorf("%w: %v", ErrInvalidHash, err)
	}
	copy(hash[:], decoded)
	return hash, nil
}

func (h Hash) Bytes() []byte {
	value := make([]byte, len(h))
	copy(value, h[:])
	return value
}

func (h Hash) String() string {
	return hex.EncodeToString(h[:])
}

func (h Hash) IsZero() bool {
	return h == Hash{}
}
