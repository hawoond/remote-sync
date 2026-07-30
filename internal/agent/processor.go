package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	rand "math/rand/v2"
	"os"
	"path/filepath"
	"time"

	"github.com/hawoond/remote-sync/internal/domain"
	"github.com/hawoond/remote-sync/internal/hashing"
	"github.com/hawoond/remote-sync/internal/localdb"
	"github.com/hawoond/remote-sync/internal/metadata"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Remote interface {
	BeginUpload(context.Context, localdb.Operation) (domain.UploadReservation, error)
	Upload(context.Context, string, int64, io.Reader, int, func(int64) error) (domain.Hash, int64, error)
	Commit(context.Context, localdb.Operation) (domain.CommitResult, error)
}

type Processor struct {
	root         *os.Root
	store        *localdb.Store
	remote       Remote
	logger       *slog.Logger
	pollInterval time.Duration
	maxBackoff   time.Duration
}

func NewProcessor(
	root *os.Root,
	store *localdb.Store,
	remote Remote,
	logger *slog.Logger,
) *Processor {
	return &Processor{
		root:         root,
		store:        store,
		remote:       remote,
		logger:       logger,
		pollInterval: time.Second,
		maxBackoff:   5 * time.Minute,
	}
}

func (p *Processor) Run(ctx context.Context) error {
	recovered, err := p.store.RecoverInFlight(ctx)
	if err != nil {
		return err
	}
	if recovered > 0 {
		p.logger.Info("recovered interrupted operations", "count", recovered)
	}

	ticker := time.NewTicker(p.pollInterval)
	defer ticker.Stop()
	for {
		for {
			err := p.ProcessNext(ctx)
			if errors.Is(err, localdb.ErrNotFound) {
				break
			}
			if err != nil {
				p.logger.Error("process operation failed", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (p *Processor) ProcessNext(ctx context.Context) error {
	operation, err := p.store.ClaimNext(ctx)
	if err != nil {
		return err
	}
	state := "RESERVING"

	if operation.Kind == domain.ChangeKindCreate || operation.Kind == domain.ChangeKindModify {
		snapshot, err := hashing.Capture(ctx, p.root, filepath.FromSlash(operation.DisplayPath))
		if err != nil {
			return p.deferOperation(ctx, operation, state, err)
		}
		if snapshot.Hash != operation.Hash ||
			snapshot.Size != operation.Size ||
			snapshot.MTimeUnixNano != operation.MTimeUnixNano {
			return p.deferOperation(ctx, operation, state, hashing.ErrChanged)
		}

		reservation, err := p.remote.BeginUpload(ctx, operation)
		if err != nil {
			return p.deferOperation(ctx, operation, state, err)
		}
		switch reservation.Disposition {
		case domain.UploadDispositionRequired:
			if err := p.store.Transition(ctx, operation.OperationID, state, "UPLOADING", localdb.Transition{
				UploadSession: reservation.SessionID,
				NextOffset:    reservation.NextOffset,
			}); err != nil {
				return err
			}
			state = "UPLOADING"
			operation.UploadSession = reservation.SessionID
			operation.NextOffset = reservation.NextOffset

			file, err := p.root.Open(filepath.FromSlash(operation.DisplayPath))
			if err != nil {
				return p.deferOperation(ctx, operation, state, err)
			}
			if _, err := file.Seek(operation.NextOffset, io.SeekStart); err != nil {
				file.Close()
				return p.deferOperation(ctx, operation, state, err)
			}
			uploadedHash, uploadedSize, uploadErr := p.remote.Upload(
				ctx,
				operation.UploadSession,
				operation.NextOffset,
				file,
				int(reservation.MaxChunkSize),
				func(next int64) error {
					operation.NextOffset = next
					return p.store.UpdateProgress(
						ctx,
						operation.OperationID,
						operation.UploadSession,
						next,
					)
				},
			)
			closeErr := file.Close()
			if uploadErr != nil {
				return p.deferOperation(ctx, operation, state, uploadErr)
			}
			if closeErr != nil {
				return p.deferOperation(ctx, operation, state, closeErr)
			}
			if uploadedHash != operation.Hash || uploadedSize != operation.Size {
				return p.deferOperation(ctx, operation, state, metadata.ErrUploadNotVerified)
			}
			if err := p.store.Transition(
				ctx,
				operation.OperationID,
				state,
				"COMMITTING",
				localdb.Transition{NextOffset: -1},
			); err != nil {
				return err
			}
			state = "COMMITTING"
		case domain.UploadDispositionObjectPresent:
			if err := p.store.Transition(
				ctx,
				operation.OperationID,
				state,
				"COMMITTING",
				localdb.Transition{NextOffset: -1},
			); err != nil {
				return err
			}
			state = "COMMITTING"
		default:
			return p.deferOperation(ctx, operation, state, metadata.ErrInvalidState)
		}
	} else if operation.Kind == domain.ChangeKindDelete {
		if err := p.store.Transition(
			ctx,
			operation.OperationID,
			state,
			"COMMITTING",
			localdb.Transition{NextOffset: -1},
		); err != nil {
			return err
		}
		state = "COMMITTING"
	} else {
		return p.blockOperation(ctx, operation, state, syncengineError("unsupported change kind"))
	}

	result, err := p.remote.Commit(ctx, operation)
	if err != nil {
		return p.deferOperation(ctx, operation, state, err)
	}
	if result.Disposition == domain.CommitDispositionQuarantined {
		return p.store.Transition(ctx, operation.OperationID, state, "QUARANTINED", localdb.Transition{
			NextOffset:   -1,
			ErrorCode:    "QUARANTINED",
			ErrorMessage: result.QuarantineID,
		})
	}
	if result.Disposition != domain.CommitDispositionCommitted &&
		result.Disposition != domain.CommitDispositionIdempotentReplay &&
		result.Disposition != domain.CommitDispositionConflictCopy {
		return p.blockOperation(ctx, operation, state, metadata.ErrInvalidState)
	}
	if err := p.store.SetServerVersion(ctx, operation.FolderID, operation.PathKey, result.VersionID); err != nil {
		return p.deferOperation(ctx, operation, state, err)
	}
	return p.store.Transition(ctx, operation.OperationID, state, "COMPLETED", localdb.Transition{
		NextOffset: -1,
	})
}

func (p *Processor) deferOperation(
	ctx context.Context,
	operation localdb.Operation,
	state string,
	err error,
) error {
	if !retryable(err) {
		return p.blockOperation(ctx, operation, state, err)
	}
	backoff := p.backoff(operation.Attempt)
	transitionErr := p.store.Transition(ctx, operation.OperationID, state, "RETRY_WAIT", localdb.Transition{
		NextOffset:    -1,
		NextAttemptAt: time.Now().Add(backoff),
		ErrorCode:     errorCode(err),
		ErrorMessage:  err.Error(),
	})
	if transitionErr != nil {
		return errors.Join(err, transitionErr)
	}
	return err
}

func (p *Processor) blockOperation(
	ctx context.Context,
	operation localdb.Operation,
	state string,
	err error,
) error {
	transitionErr := p.store.Transition(ctx, operation.OperationID, state, "BLOCKED", localdb.Transition{
		NextOffset:   -1,
		ErrorCode:    errorCode(err),
		ErrorMessage: err.Error(),
	})
	if transitionErr != nil {
		return errors.Join(err, transitionErr)
	}
	return err
}

func (p *Processor) backoff(attempt int) time.Duration {
	exponent := math.Min(float64(max(attempt-1, 0)), 18)
	base := time.Second * time.Duration(1<<int(exponent))
	if base > p.maxBackoff {
		base = p.maxBackoff
	}
	jitter := 0.8 + rand.Float64()*0.4
	return time.Duration(float64(base) * jitter)
}

func retryable(err error) bool {
	if errors.Is(err, hashing.ErrChanged) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Aborted:
		return true
	default:
		return false
	}
}

func errorCode(err error) string {
	if errors.Is(err, hashing.ErrChanged) {
		return "LOCAL_FILE_CHANGED"
	}
	if code := status.Code(err); code != codes.Unknown {
		return code.String()
	}
	return "LOCAL_ERROR"
}

type syncengineError string

func (e syncengineError) Error() string {
	return string(e)
}
