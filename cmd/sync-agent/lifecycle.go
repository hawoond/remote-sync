package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hawoond/remote-sync/internal/domain"
	"github.com/hawoond/remote-sync/internal/hashing"
	"github.com/hawoond/remote-sync/internal/localdb"
	"github.com/hawoond/remote-sync/internal/transport/grpcclient"
)

type connectionConfig struct {
	serverAddress string
	deviceToken   string
	folderID      string
	tlsServerName string
	tlsCAFile     string
	allowInsecure bool
}

const restoreErrorMessageLimit = 2048

func runEnrollmentCreateCommand(
	ctx context.Context,
	arguments []string,
	output, errorOutput io.Writer,
) error {
	flags := flag.NewFlagSet("enrollment create", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	roleName := flags.String("role", "writer", "reader, writer, or restore-admin")
	expires := flags.Duration("expires", 15*time.Minute, "one-time token lifetime")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("enrollment create accepts flags only")
	}
	role, err := parseRole(*roleName)
	if err != nil {
		return err
	}
	cfg, err := loadConnectionConfig(true, true)
	if err != nil {
		return err
	}
	client, err := openCommandClient(cfg)
	if err != nil {
		return err
	}
	defer client.Close()
	enrollment, err := client.CreateEnrollment(ctx, cfg.folderID, role, *expires)
	if err != nil {
		return err
	}
	return writeJSON(output, map[string]any{
		"enrollment_id":    enrollment.ID,
		"enrollment_token": enrollment.Token,
		"folder_id":        enrollment.FolderID,
		"role":             enrollment.Role.String(),
		"expires_at":       enrollment.ExpiresAt.Format(time.RFC3339),
	})
}

func runEnrollCommand(
	ctx context.Context,
	arguments []string,
	output, errorOutput io.Writer,
) error {
	hostname, _ := os.Hostname()
	flags := flag.NewFlagSet("enroll", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	deviceName := flags.String("name", hostname, "device name")
	platform := flags.String("platform", runtime.GOOS+"/"+runtime.GOARCH, "device platform")
	token := flags.String("token", os.Getenv("SYNC_ENROLLMENT_TOKEN"), "one-time enrollment token")
	capabilities := flags.String("capabilities", "{}", "JSON capability object")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("enroll accepts flags only")
	}
	if *token == "" {
		return errors.New("SYNC_ENROLLMENT_TOKEN or --token is required")
	}
	cfg, err := loadConnectionConfig(false, false)
	if err != nil {
		return err
	}
	client, err := openCommandClient(cfg)
	if err != nil {
		return err
	}
	defer client.Close()
	credentials, err := client.EnrollDevice(
		ctx,
		*token,
		*deviceName,
		*platform,
		*capabilities,
	)
	if err != nil {
		return err
	}
	return writeJSON(output, map[string]any{
		"device_id":    credentials.DeviceID,
		"device_token": credentials.DeviceToken,
		"folder_id":    credentials.FolderID,
		"role":         credentials.Role.String(),
	})
}

func runPolicyCommand(
	ctx context.Context,
	arguments []string,
	output, errorOutput io.Writer,
) error {
	if len(arguments) == 0 {
		return errors.New("policy requires get or set")
	}
	cfg, err := loadConnectionConfig(true, true)
	if err != nil {
		return err
	}
	client, err := openCommandClient(cfg)
	if err != nil {
		return err
	}
	defer client.Close()

	var policy domain.FolderPolicy
	switch arguments[0] {
	case "get":
		if len(arguments) != 1 {
			return errors.New("policy get accepts no arguments")
		}
		policy, err = client.GetFolderPolicy(ctx, cfg.folderID)
	case "set":
		flags := flag.NewFlagSet("policy set", flag.ContinueOnError)
		flags.SetOutput(errorOutput)
		safetyValue := flags.String("safety-window", "", "retention window, for example 720h")
		graceValue := flags.String("gc-grace-period", "", "two-phase deletion grace period")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || (*safetyValue == "" && *graceValue == "") {
			return errors.New("policy set requires --safety-window and/or --gc-grace-period")
		}
		current, err := client.GetFolderPolicy(ctx, cfg.folderID)
		if err != nil {
			return err
		}
		safetyWindow := current.SafetyWindow
		gracePeriod := current.GCGracePeriod
		if *safetyValue != "" {
			safetyWindow, err = time.ParseDuration(*safetyValue)
			if err != nil {
				return fmt.Errorf("parse safety window: %w", err)
			}
		}
		if *graceValue != "" {
			gracePeriod, err = time.ParseDuration(*graceValue)
			if err != nil {
				return fmt.Errorf("parse GC grace period: %w", err)
			}
		}
		policy, err = client.UpdateFolderPolicy(
			ctx,
			cfg.folderID,
			safetyWindow,
			gracePeriod,
		)
	default:
		return fmt.Errorf("unknown policy command %q", arguments[0])
	}
	if err != nil {
		return err
	}
	return writeJSON(output, map[string]any{
		"folder_id":       policy.FolderID,
		"safety_window":   policy.SafetyWindow.String(),
		"gc_grace_period": policy.GCGracePeriod.String(),
		"revision":        policy.Revision,
		"updated_at":      policy.UpdatedAt.Format(time.RFC3339),
	})
}

func runRestoreCommand(
	ctx context.Context,
	arguments []string,
	output, errorOutput io.Writer,
) error {
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	target := flags.String("target", "", "destination directory")
	sequence := flags.Int64("sequence", 0, "folder sequence; zero uses the latest")
	overwrite := flags.Bool("overwrite", false, "replace existing regular files")
	resumeID := flags.String("resume", "", "existing restore job ID")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *target == "" {
		return errors.New("restore requires --target")
	}
	if *sequence < 0 {
		return errors.New("restore sequence must not be negative")
	}
	if *resumeID != "" {
		if _, err := uuid.Parse(*resumeID); err != nil {
			return errors.New("restore resume ID must be a UUID")
		}
		if *sequence != 0 || *overwrite {
			return errors.New("--resume cannot be combined with --sequence or --overwrite")
		}
	}
	cfg, err := loadConnectionConfig(true, true)
	if err != nil {
		return err
	}
	targetPath, err := filepath.Abs(*target)
	if err != nil {
		return fmt.Errorf("resolve restore target: %w", err)
	}
	if err := os.MkdirAll(targetPath, 0o700); err != nil {
		return fmt.Errorf("create restore target: %w", err)
	}
	root, err := os.OpenRoot(targetPath)
	if err != nil {
		return fmt.Errorf("open restore target: %w", err)
	}
	defer root.Close()
	statePath, err := restoreStatePath(cfg.folderID, targetPath)
	if err != nil {
		return err
	}
	store, err := localdb.Open(statePath)
	if err != nil {
		return err
	}
	defer store.Close()
	client, err := openCommandClient(cfg)
	if err != nil {
		return err
	}
	defer client.Close()

	var job domain.RestoreJob
	if *resumeID == "" {
		job, err = client.StartRestore(ctx, cfg.folderID, *sequence, *overwrite)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(
			output,
			"restore %s created at sequence %d with %d items\n",
			job.ID,
			job.SnapshotSequence,
			job.TotalItems,
		)
	} else {
		job.ID = *resumeID
	}

	var firstFailure error
	var after int64
	for {
		current, items, err := client.ListRestoreItems(ctx, job.ID, after, 250)
		if err != nil {
			return err
		}
		job = current
		if len(items) == 0 {
			break
		}
		for _, item := range items {
			after = item.Ordinal
			if item.State != domain.RestoreItemStatePending {
				continue
			}
			state, applyErr := applyRestoreItem(ctx, root, store, client, job, item)
			message := ""
			if applyErr != nil {
				message = truncateMessage(applyErr.Error())
				if firstFailure == nil {
					firstFailure = applyErr
				}
				state = domain.RestoreItemStateFailed
			}
			if _, err := client.ReportRestoreItem(
				ctx,
				job.ID,
				item.Ordinal,
				state,
				message,
			); err != nil {
				return err
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if firstFailure != nil {
		_, finishErr := client.FinishRestore(
			ctx,
			job.ID,
			false,
			truncateMessage(firstFailure.Error()),
		)
		if finishErr != nil {
			return errors.Join(firstFailure, finishErr)
		}
		return firstFailure
	}
	job, err = client.FinishRestore(ctx, job.ID, true, "")
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(
		output,
		"restore %s completed: applied=%d skipped=%d failed=%d state=%s\n",
		job.ID,
		job.AppliedItems,
		job.SkippedItems,
		job.FailedItems,
		stateName(job.State),
	)
	return nil
}

func applyRestoreItem(
	ctx context.Context,
	root *os.Root,
	store *localdb.Store,
	client *grpcclient.Client,
	job domain.RestoreJob,
	item domain.RestoreItem,
) (domain.RestoreItemState, error) {
	name := filepath.FromSlash(item.DisplayPath)
	info, err := root.Lstat(name)
	switch {
	case err == nil:
		if info.Mode().IsRegular() {
			snapshot, captureErr := hashing.Capture(ctx, root, name)
			if captureErr == nil &&
				snapshot.Hash == item.ObjectHash &&
				snapshot.Size == item.Size {
				if err := seedRestoredEntry(ctx, store, job.FolderID, item); err != nil {
					return domain.RestoreItemStateFailed, err
				}
				return domain.RestoreItemStateApplied, nil
			}
		}
		if !job.Overwrite {
			return domain.RestoreItemStateSkipped, nil
		}
		if !info.Mode().IsRegular() {
			return domain.RestoreItemStateFailed, fmt.Errorf(
				"refusing to replace non-regular path %s",
				item.DisplayPath,
			)
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return domain.RestoreItemStateFailed, fmt.Errorf(
			"inspect restore path %s: %w",
			item.DisplayPath,
			err,
		)
	}

	parent := filepath.Dir(name)
	if parent != "." {
		if err := root.MkdirAll(parent, 0o700); err != nil {
			return domain.RestoreItemStateFailed, fmt.Errorf("create restore directory: %w", err)
		}
	}
	tempName := filepath.Join(
		parent,
		fmt.Sprintf(".remote-sync-%s-%d.part", job.ID, item.Ordinal),
	)
	_ = root.Remove(tempName)
	file, err := root.OpenFile(tempName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return domain.RestoreItemStateFailed, fmt.Errorf("create restore file: %w", err)
	}
	actualHash, actualSize, downloadErr := client.Download(
		ctx,
		job.FolderID,
		item.ObjectHash,
		file,
	)
	if downloadErr == nil {
		downloadErr = file.Sync()
	}
	closeErr := file.Close()
	if downloadErr == nil {
		downloadErr = closeErr
	}
	if downloadErr != nil {
		_ = root.Remove(tempName)
		return domain.RestoreItemStateFailed, downloadErr
	}
	if actualHash != item.ObjectHash || actualSize != item.Size {
		_ = root.Remove(tempName)
		return domain.RestoreItemStateFailed, errors.New("restored object verification failed")
	}
	if err := publishRestoredFile(root, tempName, name, info != nil); err != nil {
		_ = root.Remove(tempName)
		return domain.RestoreItemStateFailed, err
	}
	mode := os.FileMode(item.PortableMode & 0o777)
	if err := root.Chmod(name, mode); err != nil {
		return domain.RestoreItemStateFailed, fmt.Errorf("restore file mode: %w", err)
	}
	mtime := time.Unix(0, item.MTimeUnixNano)
	if err := root.Chtimes(name, mtime, mtime); err != nil {
		return domain.RestoreItemStateFailed, fmt.Errorf("restore file time: %w", err)
	}
	if err := seedRestoredEntry(ctx, store, job.FolderID, item); err != nil {
		return domain.RestoreItemStateFailed, err
	}
	return domain.RestoreItemStateApplied, nil
}

func publishRestoredFile(root *os.Root, tempName, name string, replace bool) error {
	if !replace {
		if err := root.Link(tempName, name); err != nil {
			return fmt.Errorf("publish restored file without replacement: %w", err)
		}
		if err := root.Remove(tempName); err != nil {
			return fmt.Errorf("remove published temporary link: %w", err)
		}
		return nil
	}

	if err := root.Rename(tempName, name); err == nil {
		return nil
	}

	info, err := root.Lstat(name)
	if err != nil {
		return fmt.Errorf("inspect file after atomic replacement failed: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to replace non-regular path %s", name)
	}
	backupName := filepath.Join(
		filepath.Dir(name),
		".remote-sync-backup-"+uuid.NewString(),
	)
	if err := root.Rename(name, backupName); err != nil {
		return fmt.Errorf("preserve replaced file: %w", err)
	}
	if err := root.Rename(tempName, name); err != nil {
		if rollbackErr := root.Rename(backupName, name); rollbackErr != nil {
			return errors.Join(
				fmt.Errorf("publish restored file: %w", err),
				fmt.Errorf("restore replaced file after publish failure: %w", rollbackErr),
			)
		}
		return fmt.Errorf("publish restored file: %w", err)
	}
	if err := root.Remove(backupName); err != nil {
		return fmt.Errorf("remove restore backup: %w", err)
	}
	return nil
}

func seedRestoredEntry(
	ctx context.Context,
	store *localdb.Store,
	folderID string,
	item domain.RestoreItem,
) error {
	return store.UpsertEntry(ctx, localdb.Entry{
		FolderID:      folderID,
		PathKey:       item.PathKey,
		DisplayPath:   item.DisplayPath,
		Size:          item.Size,
		MTimeUnixNano: item.MTimeUnixNano,
		PortableMode:  item.PortableMode,
		Hash:          item.ObjectHash,
		ServerVersion: item.VersionID,
		Present:       true,
	})
}

func loadConnectionConfig(requireToken, requireFolder bool) (connectionConfig, error) {
	allowInsecure, err := boolEnvironment("ALLOW_INSECURE", false)
	if err != nil {
		return connectionConfig{}, err
	}
	cfg := connectionConfig{
		serverAddress: environmentOrDefault("SYNC_SERVER_ADDRESS", defaultServerAddress),
		deviceToken:   os.Getenv("SYNC_DEVICE_TOKEN"),
		folderID:      os.Getenv("SYNC_FOLDER_ID"),
		tlsServerName: os.Getenv("TLS_SERVER_NAME"),
		tlsCAFile:     os.Getenv("TLS_CA_FILE"),
		allowInsecure: allowInsecure,
	}
	if requireToken && len(cfg.deviceToken) < 32 {
		return connectionConfig{}, errors.New("SYNC_DEVICE_TOKEN must contain at least 32 characters")
	}
	if requireFolder {
		if _, err := uuid.Parse(cfg.folderID); err != nil {
			return connectionConfig{}, errors.New("SYNC_FOLDER_ID must be a UUID")
		}
	}
	return cfg, nil
}

func openCommandClient(cfg connectionConfig) (*grpcclient.Client, error) {
	tlsConfig, err := clientTLSConfig(config{
		tlsServerName: cfg.tlsServerName,
		tlsCAFile:     cfg.tlsCAFile,
		allowInsecure: cfg.allowInsecure,
	})
	if err != nil {
		return nil, err
	}
	return grpcclient.New(
		cfg.serverAddress,
		cfg.deviceToken,
		tlsConfig,
		cfg.allowInsecure,
	)
}

func restoreStatePath(folderID, targetPath string) (string, error) {
	statePath := os.Getenv("SYNC_STATE_PATH")
	if statePath == "" {
		configDirectory, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("resolve user configuration directory: %w", err)
		}
		statePath = filepath.Join(
			configDirectory,
			"remote-sync",
			defaultStateFilename(folderID, targetPath),
		)
	}
	absolute, err := filepath.Abs(statePath)
	if err != nil {
		return "", fmt.Errorf("resolve SYNC_STATE_PATH: %w", err)
	}
	return absolute, nil
}

func parseRole(value string) (domain.FolderRole, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "reader":
		return domain.FolderRoleReader, nil
	case "writer":
		return domain.FolderRoleWriter, nil
	case "restore-admin", "restore_admin":
		return domain.FolderRoleRestoreAdmin, nil
	default:
		return domain.FolderRoleUnspecified, fmt.Errorf("unsupported folder role %q", value)
	}
}

func stateName(value domain.RestoreState) string {
	switch value {
	case domain.RestoreStateReady:
		return "READY"
	case domain.RestoreStateRunning:
		return "RUNNING"
	case domain.RestoreStateCompleted:
		return "COMPLETED"
	case domain.RestoreStateFailed:
		return "FAILED"
	default:
		return "UNSPECIFIED"
	}
}

func truncateMessage(value string) string {
	if len(value) <= restoreErrorMessageLimit {
		return value
	}
	return value[:restoreErrorMessageLimit]
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
