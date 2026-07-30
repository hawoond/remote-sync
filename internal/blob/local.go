package blob

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"syscall"

	"github.com/google/uuid"
	"github.com/hawoond/remote-sync/internal/domain"
)

const MaxChunkSize = 4 << 20

var (
	ErrInvalidSessionID = errors.New("invalid upload session ID")
	ErrOffsetMismatch   = errors.New("upload offset mismatch")
	ErrSizeMismatch     = errors.New("object size mismatch")
	ErrHashMismatch     = errors.New("object hash mismatch")
	ErrObjectNotFound   = errors.New("object not found")
)

type Object struct {
	Hash domain.Hash
	Size int64
}

type Local struct {
	root *os.Root
}

func NewLocal(rootPath string) (*Local, error) {
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		return nil, fmt.Errorf("create blob root: %w", err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open blob root: %w", err)
	}
	store := &Local{root: root}
	for _, directory := range []string{"uploads", "objects/sha256"} {
		if err := root.MkdirAll(directory, 0o700); err != nil {
			root.Close()
			return nil, fmt.Errorf("create %s: %w", directory, err)
		}
	}
	return store, nil
}

func (s *Local) Close() error {
	return s.root.Close()
}

func (s *Local) Resume(_ context.Context, sessionID string) (int64, error) {
	name, err := uploadName(sessionID)
	if err != nil {
		return 0, err
	}
	info, err := s.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("stat upload: %w", err)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("stat upload: %w", ErrInvalidSessionID)
	}
	return info.Size(), nil
}

func (s *Local) Append(ctx context.Context, sessionID string, offset int64, data []byte) (int64, error) {
	if offset < 0 {
		return 0, fmt.Errorf("%w: negative offset", ErrOffsetMismatch)
	}
	if len(data) > MaxChunkSize {
		return 0, fmt.Errorf("chunk size %d outside allowed range", len(data))
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	name, err := uploadName(sessionID)
	if err != nil {
		return 0, err
	}
	file, err := s.root.OpenFile(name, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, fmt.Errorf("open upload: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat upload: %w", err)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("stat upload: %w", ErrInvalidSessionID)
	}
	if info.Size() != offset {
		return info.Size(), fmt.Errorf("%w: expected %d, got %d", ErrOffsetMismatch, info.Size(), offset)
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return offset, fmt.Errorf("seek upload: %w", err)
	}
	if err := writeFull(ctx, file, data); err != nil {
		return offset, fmt.Errorf("write upload: %w", err)
	}
	if err := file.Sync(); err != nil {
		return offset, fmt.Errorf("sync upload: %w", err)
	}
	return offset + int64(len(data)), nil
}

func (s *Local) Finalize(ctx context.Context, sessionID string, expected domain.Hash, expectedSize int64) (Object, error) {
	if expected.IsZero() {
		return Object{}, domain.ErrInvalidHash
	}
	if expectedSize < 0 {
		return Object{}, ErrSizeMismatch
	}
	upload, err := uploadName(sessionID)
	if err != nil {
		return Object{}, err
	}
	file, err := s.root.OpenFile(upload, os.O_RDWR, 0)
	if err != nil {
		return Object{}, fmt.Errorf("open upload for finalize: %w", err)
	}

	info, statErr := file.Stat()
	if statErr != nil {
		file.Close()
		return Object{}, fmt.Errorf("stat upload for finalize: %w", statErr)
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return Object{}, ErrInvalidSessionID
	}
	if info.Size() != expectedSize {
		file.Close()
		return Object{}, fmt.Errorf("%w: expected %d, got %d", ErrSizeMismatch, expectedSize, info.Size())
	}

	actual, hashErr := hashFile(ctx, file)
	if hashErr != nil {
		file.Close()
		return Object{}, hashErr
	}
	if actual != expected {
		file.Close()
		_ = s.root.Remove(upload)
		return Object{}, fmt.Errorf("%w: expected %s, got %s", ErrHashMismatch, expected, actual)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return Object{}, fmt.Errorf("sync upload before promotion: %w", err)
	}
	if err := file.Close(); err != nil {
		return Object{}, fmt.Errorf("close upload before promotion: %w", err)
	}

	target := objectName(expected)
	if object, exists, err := s.existingObject(target, expected, expectedSize); err != nil {
		return Object{}, err
	} else if exists {
		if err := s.root.Remove(upload); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Object{}, fmt.Errorf("remove duplicate upload: %w", err)
		}
		return object, nil
	}

	if err := s.root.MkdirAll(path.Dir(target), 0o700); err != nil {
		return Object{}, fmt.Errorf("create object directory: %w", err)
	}
	if err := s.root.Rename(upload, target); err != nil {
		if object, exists, existingErr := s.existingObject(target, expected, expectedSize); existingErr == nil && exists {
			_ = s.root.Remove(upload)
			return object, nil
		}
		return Object{}, fmt.Errorf("promote object: %w", err)
	}
	if err := s.syncDirectory(path.Dir(target)); err != nil {
		return Object{}, err
	}
	return Object{Hash: expected, Size: expectedSize}, nil
}

func (s *Local) Exists(_ context.Context, hash domain.Hash, size int64) (bool, error) {
	_, exists, err := s.existingObject(objectName(hash), hash, size)
	return exists, err
}

func (s *Local) Open(_ context.Context, hash domain.Hash) (*os.File, error) {
	file, err := s.root.Open(objectName(hash))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrObjectNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open object: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("stat object: %w", err)
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, ErrObjectNotFound
	}
	return file, nil
}

func (s *Local) Abort(_ context.Context, sessionID string) error {
	name, err := uploadName(sessionID)
	if err != nil {
		return err
	}
	if err := s.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("abort upload: %w", err)
	}
	return nil
}

func (s *Local) Delete(_ context.Context, hash domain.Hash) error {
	name := objectName(hash)
	if err := s.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete object: %w", err)
	}
	firstDirectory := path.Dir(name)
	for firstDirectory != "objects/sha256" {
		if err := s.root.Remove(firstDirectory); err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTEMPTY) {
				break
			}
			return fmt.Errorf("remove empty object directory: %w", err)
		}
		firstDirectory = path.Dir(firstDirectory)
	}
	return s.syncDirectory("objects")
}

func (s *Local) existingObject(name string, expected domain.Hash, size int64) (Object, bool, error) {
	info, err := s.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return Object{}, false, nil
	}
	if err != nil {
		return Object{}, false, fmt.Errorf("stat object: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Object{}, false, fmt.Errorf("object path is not a regular file: %w", ErrHashMismatch)
	}
	if info.Size() != size {
		return Object{}, false, fmt.Errorf("%w: existing object size %d, expected %d", ErrHashMismatch, info.Size(), size)
	}
	return Object{Hash: expected, Size: size}, true, nil
}

func (s *Local) syncDirectory(name string) error {
	directory, err := s.root.Open(name)
	if err != nil {
		return fmt.Errorf("open object directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync object directory: %w", err)
	}
	return nil
}

func uploadName(sessionID string) (string, error) {
	parsed, err := uuid.Parse(sessionID)
	if err != nil || parsed.String() != sessionID {
		return "", ErrInvalidSessionID
	}
	return path.Join("uploads", sessionID+".part"), nil
}

func objectName(hash domain.Hash) string {
	value := hash.String()
	return path.Join("objects", "sha256", value[:2], value[2:4], value)
}

func StorageKey(hash domain.Hash) string {
	return objectName(hash)
}

func hashFile(ctx context.Context, file *os.File) (domain.Hash, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return domain.Hash{}, fmt.Errorf("seek upload for hashing: %w", err)
	}
	hasher := sha256.New()
	buffer := make([]byte, 256*1024)
	for {
		if err := ctx.Err(); err != nil {
			return domain.Hash{}, err
		}
		n, err := file.Read(buffer)
		if n > 0 {
			if _, writeErr := hasher.Write(buffer[:n]); writeErr != nil {
				return domain.Hash{}, writeErr
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return domain.Hash{}, fmt.Errorf("read upload for hashing: %w", err)
		}
	}
	return domain.HashFromBytes(hasher.Sum(nil))
}

func writeFull(ctx context.Context, writer io.Writer, data []byte) error {
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
