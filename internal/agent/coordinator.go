package agent

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

type Coordinator struct {
	scanner      *Scanner
	watcher      *Watcher
	scanInterval time.Duration
	debounce     time.Duration
	logger       *slog.Logger
}

func NewCoordinator(
	scanner *Scanner,
	watcher *Watcher,
	scanInterval, debounce time.Duration,
	logger *slog.Logger,
) *Coordinator {
	return &Coordinator{
		scanner:      scanner,
		watcher:      watcher,
		scanInterval: scanInterval,
		debounce:     debounce,
		logger:       logger,
	}
}

func (c *Coordinator) Run(ctx context.Context) error {
	if _, err := c.runScan(ctx, "startup"); err != nil {
		return err
	}

	triggers := make(chan struct{}, 1)
	watcherErrors := make(chan error, 1)
	go func() {
		watcherErrors <- c.watcher.Run(ctx, triggers)
	}()

	ticker := time.NewTicker(c.scanInterval)
	defer ticker.Stop()

	var debounceTimer *time.Timer
	var debounceChannel <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			return ctx.Err()
		case err := <-watcherErrors:
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				return ctx.Err()
			}
			_, _ = c.runScan(ctx, "watcher_error")
			return err
		case <-triggers:
			if debounceTimer == nil {
				debounceTimer = time.NewTimer(c.debounce)
				debounceChannel = debounceTimer.C
			} else {
				if !debounceTimer.Stop() {
					select {
					case <-debounceTimer.C:
					default:
					}
				}
				debounceTimer.Reset(c.debounce)
			}
		case <-debounceChannel:
			debounceTimer = nil
			debounceChannel = nil
			_, _ = c.runScan(ctx, "filesystem_event")
		case <-ticker.C:
			_, _ = c.runScan(ctx, "periodic")
		}
	}
}

func (c *Coordinator) runScan(ctx context.Context, reason string) (ScanReport, error) {
	report, err := c.scanner.Scan(ctx)
	if err != nil {
		c.logger.Error("folder scan failed", "reason", reason, "error", err)
		return report, err
	}
	c.logger.Info(
		"folder scan completed",
		"reason", reason,
		"generation", report.Generation,
		"files_seen", report.FilesSeen,
		"planned", report.Planned,
		"deleted", report.Deleted,
		"issues", len(report.Issues),
	)
	for _, issue := range report.Issues {
		c.logger.Warn("folder scan issue", "path", issue.Path, "error", issue.Err)
	}
	return report, nil
}
