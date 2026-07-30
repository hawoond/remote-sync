package hashing

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/hawoond/remote-sync/internal/domain"
)

var (
	ErrChanged         = errors.New("file changed while hashing")
	ErrUnsupportedType = errors.New("unsupported file type")
)

type Snapshot struct {
	Hash          domain.Hash
	Size          int64
	MTimeUnixNano int64
	Mode          os.FileMode
}

func Capture(ctx context.Context, root *os.Root, relativePath string) (Snapshot, error) {
	before, err := root.Lstat(relativePath)
	if err != nil {
		return Snapshot{}, fmt.Errorf("lstat before hashing: %w", err)
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return Snapshot{}, ErrUnsupportedType
	}

	file, err := root.Open(relativePath)
	if err != nil {
		return Snapshot{}, fmt.Errorf("open for hashing: %w", err)
	}
	defer file.Close()

	opened, err := file.Stat()
	if err != nil {
		return Snapshot{}, fmt.Errorf("stat opened file: %w", err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return Snapshot{}, ErrChanged
	}

	hasher := sha256.New()
	size, err := copyWithContext(ctx, hasher, file)
	if err != nil {
		return Snapshot{}, fmt.Errorf("hash file: %w", err)
	}

	afterOpen, err := file.Stat()
	if err != nil {
		return Snapshot{}, fmt.Errorf("stat after hashing: %w", err)
	}
	afterPath, err := root.Lstat(relativePath)
	if err != nil {
		return Snapshot{}, fmt.Errorf("lstat after hashing: %w", err)
	}

	if !stable(before, afterOpen) || !stable(before, afterPath) ||
		afterPath.Mode()&os.ModeSymlink != 0 || size != afterPath.Size() {
		return Snapshot{}, ErrChanged
	}

	hash, err := domain.HashFromBytes(hasher.Sum(nil))
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		Hash:          hash,
		Size:          size,
		MTimeUnixNano: afterPath.ModTime().UnixNano(),
		Mode:          afterPath.Mode().Perm(),
	}, nil
}

func stable(before, after os.FileInfo) bool {
	return os.SameFile(before, after) &&
		before.Size() == after.Size() &&
		before.ModTime().Equal(after.ModTime()) &&
		before.Mode() == after.Mode()
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buffer := make([]byte, 256*1024)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, readErr := src.Read(buffer)
		if n > 0 {
			wrote, writeErr := dst.Write(buffer[:n])
			written += int64(wrote)
			if writeErr != nil {
				return written, writeErr
			}
			if wrote != n {
				return written, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}
