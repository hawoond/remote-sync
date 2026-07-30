package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/hawoond/remote-sync/internal/agent"
	"github.com/hawoond/remote-sync/internal/buildinfo"
	"github.com/hawoond/remote-sync/internal/localdb"
	"github.com/hawoond/remote-sync/internal/transport/grpcclient"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const defaultServerAddress = "127.0.0.1:8443"

type sharedConfig struct {
	serverAddress     string
	deviceToken       string
	baseFolderID      string
	stateDirectory    string
	explicitStatePath string
	tlsServerName     string
	tlsCAFile         string
	allowInsecure     bool
	scanInterval      time.Duration
	debounce          time.Duration
}

type config struct {
	sharedConfig
	folderID  string
	rootPath  string
	statePath string
}

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func realMain(arguments []string, input *os.File, output, errorOutput io.Writer) int {
	if buildinfo.Requested(arguments) {
		_, _ = fmt.Fprintln(output, buildinfo.String("sync-agent"))
		return 0
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if len(arguments) > 0 {
		var commandErr error
		switch arguments[0] {
		case "discover":
			commandErr = runDiscoverCommand(ctx, arguments[1:], output, errorOutput)
		case "folders":
			commandErr = runFoldersCommand(arguments[1:], output, errorOutput)
		case "enrollment":
			if len(arguments) < 2 || arguments[1] != "create" {
				commandErr = errors.New("enrollment requires create")
			} else {
				commandErr = runEnrollmentCreateCommand(
					ctx,
					arguments[2:],
					output,
					errorOutput,
				)
			}
		case "enroll":
			commandErr = runEnrollCommand(ctx, arguments[1:], output, errorOutput)
		case "policy":
			commandErr = runPolicyCommand(ctx, arguments[1:], output, errorOutput)
		case "restore":
			commandErr = runRestoreCommand(ctx, arguments[1:], output, errorOutput)
		case "help", "-h", "--help":
			writeAgentUsage(output)
			return 0
		default:
			commandErr = fmt.Errorf("unknown command %q", arguments[0])
		}
		if errors.Is(commandErr, flag.ErrHelp) {
			return 0
		}
		if commandErr != nil {
			_, _ = fmt.Fprintln(errorOutput, "sync-agent:", commandErr)
			return 2
		}
		return 0
	}

	roots, err := resolveSyncRoots(ctx, input, errorOutput)
	if err != nil {
		_, _ = fmt.Fprintln(errorOutput, "sync-agent:", err)
		return 2
	}
	shared, err := loadSharedConfig()
	if err != nil {
		_, _ = fmt.Fprintln(errorOutput, "sync-agent: invalid configuration:", err)
		return 2
	}

	logger := slog.New(slog.NewJSONHandler(output, nil))
	slog.SetDefault(logger)
	configs, err := prepareConfigs(ctx, roots, shared, logger)
	if err != nil {
		logger.Error("configure sync folders", "error", err)
		return 2
	}
	if err := runAll(ctx, configs, logger); err != nil {
		logger.Error("agent stopped", "error", err)
		return 1
	}
	return 0
}

type workerResult struct {
	rootPath string
	err      error
}

func runAll(ctx context.Context, configs []config, logger *slog.Logger) error {
	if len(configs) == 0 {
		return errors.New("at least one sync folder is required")
	}
	if len(configs) == 1 {
		return run(ctx, configs[0], logger.With(
			"root", configs[0].rootPath,
			"folder_id", configs[0].folderID,
		))
	}

	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan workerResult, len(configs))
	for _, cfg := range configs {
		cfg := cfg
		go func() {
			results <- workerResult{
				rootPath: cfg.rootPath,
				err: run(runContext, cfg, logger.With(
					"root", cfg.rootPath,
					"folder_id", cfg.folderID,
				)),
			}
		}()
	}

	var firstError error
	for range configs {
		result := <-results
		if result.err == nil && ctx.Err() == nil && firstError == nil {
			firstError = fmt.Errorf("sync worker stopped unexpectedly: %s", result.rootPath)
			cancel()
			continue
		}
		if result.err != nil &&
			!errors.Is(result.err, context.Canceled) &&
			firstError == nil {
			firstError = fmt.Errorf("sync %s: %w", result.rootPath, result.err)
			cancel()
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return firstError
}

func run(ctx context.Context, cfg config, logger *slog.Logger) error {
	store, err := localdb.Open(cfg.statePath)
	if err != nil {
		return err
	}
	defer store.Close()

	recovered, err := store.RecoverInFlight(ctx)
	if err != nil {
		return fmt.Errorf("recover interrupted operations: %w", err)
	}
	if recovered > 0 {
		logger.Info("recovered interrupted operations", "count", recovered)
	}

	scanner, err := agent.NewScanner(cfg.rootPath, cfg.folderID, store)
	if err != nil {
		return fmt.Errorf("create scanner: %w", err)
	}
	defer scanner.Close()

	fileRoot, err := os.OpenRoot(cfg.rootPath)
	if err != nil {
		return fmt.Errorf("open sync root: %w", err)
	}
	defer fileRoot.Close()

	tlsConfig, err := clientTLSConfig(cfg.sharedConfig)
	if err != nil {
		return err
	}
	remote, err := grpcclient.New(
		cfg.serverAddress,
		cfg.deviceToken,
		tlsConfig,
		cfg.allowInsecure,
	)
	if err != nil {
		return err
	}
	defer remote.Close()

	coordinator := agent.NewCoordinator(
		scanner,
		agent.NewWatcher(cfg.rootPath),
		cfg.scanInterval,
		cfg.debounce,
		logger,
	)
	processor := agent.NewProcessor(fileRoot, store, remote, logger)

	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	errorsChannel := make(chan error, 2)
	go func() {
		errorsChannel <- coordinator.Run(runContext)
	}()
	go func() {
		errorsChannel <- processor.Run(runContext)
	}()

	first := <-errorsChannel
	cancel()
	second := <-errorsChannel
	if ctx.Err() != nil {
		return nil
	}
	if first != nil && !errors.Is(first, context.Canceled) {
		return first
	}
	if second != nil && !errors.Is(second, context.Canceled) {
		return second
	}
	return nil
}

func clientTLSConfig(cfg sharedConfig) (*tls.Config, error) {
	if cfg.allowInsecure {
		return nil, nil
	}
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: cfg.tlsServerName,
	}
	if cfg.tlsCAFile == "" {
		return tlsConfig, nil
	}
	pem, err := os.ReadFile(cfg.tlsCAFile)
	if err != nil {
		return nil, fmt.Errorf("read TLS CA file: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("TLS_CA_FILE contains no certificates")
	}
	tlsConfig.RootCAs = roots
	return tlsConfig, nil
}

func loadSharedConfig() (sharedConfig, error) {
	allowInsecure, err := boolEnvironment("ALLOW_INSECURE", false)
	if err != nil {
		return sharedConfig{}, err
	}
	scanInterval, err := durationEnvironment("SCAN_INTERVAL", 15*time.Minute)
	if err != nil {
		return sharedConfig{}, err
	}
	debounce, err := durationEnvironment("WATCH_DEBOUNCE", 500*time.Millisecond)
	if err != nil {
		return sharedConfig{}, err
	}

	folderID := os.Getenv("SYNC_FOLDER_ID")
	if _, err := uuid.Parse(folderID); err != nil {
		return sharedConfig{}, errors.New("SYNC_FOLDER_ID must be a UUID")
	}
	explicitStatePath := os.Getenv("SYNC_STATE_PATH")
	stateDirectory := os.Getenv("SYNC_STATE_DIR")
	if stateDirectory == "" {
		if explicitStatePath != "" {
			absoluteStatePath, err := filepath.Abs(explicitStatePath)
			if err != nil {
				return sharedConfig{}, fmt.Errorf("resolve SYNC_STATE_PATH: %w", err)
			}
			stateDirectory = filepath.Dir(absoluteStatePath)
		} else {
			configDirectory, err := os.UserConfigDir()
			if err != nil {
				return sharedConfig{}, fmt.Errorf("resolve user configuration directory: %w", err)
			}
			stateDirectory = filepath.Join(configDirectory, "remote-sync")
		}
	}
	stateDirectory, err = filepath.Abs(stateDirectory)
	if err != nil {
		return sharedConfig{}, fmt.Errorf("resolve SYNC_STATE_DIR: %w", err)
	}

	cfg := sharedConfig{
		serverAddress:     environmentOrDefault("SYNC_SERVER_ADDRESS", defaultServerAddress),
		deviceToken:       os.Getenv("SYNC_DEVICE_TOKEN"),
		baseFolderID:      folderID,
		stateDirectory:    stateDirectory,
		explicitStatePath: explicitStatePath,
		tlsServerName:     os.Getenv("TLS_SERVER_NAME"),
		tlsCAFile:         os.Getenv("TLS_CA_FILE"),
		allowInsecure:     allowInsecure,
		scanInterval:      scanInterval,
		debounce:          debounce,
	}
	if cfg.deviceToken == "" {
		return sharedConfig{}, errors.New("SYNC_DEVICE_TOKEN is required")
	}
	if len(cfg.deviceToken) < 32 {
		return sharedConfig{}, errors.New("SYNC_DEVICE_TOKEN must contain at least 32 characters")
	}
	return cfg, nil
}

func prepareConfigs(
	ctx context.Context,
	roots []syncRoot,
	shared sharedConfig,
	logger *slog.Logger,
) ([]config, error) {
	if len(roots) == 0 {
		return nil, errors.New("at least one sync root is required")
	}
	if len(roots) > 1 && shared.explicitStatePath != "" {
		return nil, errors.New(
			"SYNC_STATE_PATH supports one folder only; use SYNC_STATE_DIR for multiple folders",
		)
	}
	if err := validateStateConfiguration(roots, shared); err != nil {
		return nil, err
	}
	existingAnchorRoot, err := findExistingAnchorRoot(roots, shared)
	if err != nil {
		return nil, err
	}

	tlsConfig, err := clientTLSConfig(shared)
	if err != nil {
		return nil, err
	}
	client, err := grpcclient.New(
		shared.serverAddress,
		shared.deviceToken,
		tlsConfig,
		shared.allowInsecure,
	)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	configs := make([]config, 0, len(roots))
	folderIDs := make(map[string]string, len(roots))
	for _, root := range roots {
		clientKey := rootClientKey(root.Path)
		registration, err := client.EnsureFolder(
			ctx,
			shared.baseFolderID,
			clientKey,
			rootDisplayName(root),
			root.Path == existingAnchorRoot,
		)
		if status.Code(err) == codes.Unimplemented && len(roots) == 1 {
			registration.FolderID = shared.baseFolderID
			registration.ClientKey = clientKey
			logger.Info("server uses single-folder compatibility mode")
		} else if status.Code(err) == codes.Unimplemented {
			return nil, errors.New(
				"the server must be upgraded before multiple folders can be synchronized",
			)
		} else if err != nil {
			return nil, fmt.Errorf("register %s: %w", root.Path, err)
		}
		if _, err := uuid.Parse(registration.FolderID); err != nil {
			return nil, fmt.Errorf("server returned an invalid folder ID for %s", root.Path)
		}
		if existing, duplicate := folderIDs[registration.FolderID]; duplicate {
			return nil, fmt.Errorf(
				"server assigned the same folder to %s and %s",
				existing,
				root.Path,
			)
		}
		folderIDs[registration.FolderID] = root.Path

		cfg, err := configForRoot(shared, root.Path, registration.FolderID)
		if err != nil {
			return nil, err
		}
		configs = append(configs, cfg)
		logger.Info(
			"sync folder configured",
			"root", root.Path,
			"folder_id", registration.FolderID,
			"created", registration.Created,
		)
	}
	if err := validateStateIsolation(roots, configs, shared); err != nil {
		return nil, err
	}
	if err := writeFolderMap(shared, roots, configs); err != nil {
		return nil, err
	}
	return configs, nil
}

func validateStateConfiguration(roots []syncRoot, shared sharedConfig) error {
	mapPath, err := folderMapPath(shared)
	if err != nil {
		return err
	}
	stateLocation := shared.stateDirectory
	if shared.explicitStatePath != "" {
		stateLocation, err = filepath.Abs(shared.explicitStatePath)
		if err != nil {
			return fmt.Errorf("resolve SYNC_STATE_PATH: %w", err)
		}
	}
	for _, root := range roots {
		if pathWithinRoot(root.Path, mapPath) {
			return fmt.Errorf("SYNC_FOLDER_MAP_PATH must be outside sync root %s", root.Path)
		}
		if pathWithinRoot(root.Path, stateLocation) {
			return fmt.Errorf("local state must be outside sync root %s", root.Path)
		}
	}
	return nil
}

func validateStateIsolation(roots []syncRoot, configs []config, shared sharedConfig) error {
	mapPath, err := folderMapPath(shared)
	if err != nil {
		return err
	}
	for _, root := range roots {
		if pathWithinRoot(root.Path, mapPath) {
			return fmt.Errorf("SYNC_FOLDER_MAP_PATH must be outside sync root %s", root.Path)
		}
		for _, cfg := range configs {
			if pathWithinRoot(root.Path, cfg.statePath) {
				return fmt.Errorf("local state path must be outside sync root %s", root.Path)
			}
		}
	}
	return nil
}

func pathWithinRoot(rootPath, candidatePath string) bool {
	relative, err := filepath.Rel(
		filepath.Clean(rootPath),
		filepath.Clean(candidatePath),
	)
	if err != nil {
		return false
	}
	return relative == "." ||
		(relative != ".." &&
			!filepath.IsAbs(relative) &&
			!strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func findExistingAnchorRoot(roots []syncRoot, shared sharedConfig) (string, error) {
	var result string
	for _, root := range roots {
		statePath := shared.explicitStatePath
		if statePath == "" {
			statePath = filepath.Join(
				shared.stateDirectory,
				defaultStateFilename(shared.baseFolderID, root.Path),
			)
		}
		statePath, err := filepath.Abs(statePath)
		if err != nil {
			return "", fmt.Errorf("resolve existing local state: %w", err)
		}
		info, err := os.Stat(statePath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("inspect existing local state: %w", err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("local state path is a directory: %s", statePath)
		}
		if result != "" && result != root.Path {
			return "", errors.New(
				"multiple selected roots have existing state for SYNC_FOLDER_ID; " +
					"start them separately before enabling multi-folder mode",
			)
		}
		result = root.Path
	}
	return result, nil
}

func loadConfig(rootPath string) (config, error) {
	shared, err := loadSharedConfig()
	if err != nil {
		return config{}, err
	}
	return configForRoot(shared, rootPath, shared.baseFolderID)
}

func configForRoot(shared sharedConfig, rootPath, folderID string) (config, error) {
	statePath := shared.explicitStatePath
	if statePath == "" {
		statePath = filepath.Join(
			shared.stateDirectory,
			defaultStateFilename(folderID, rootPath),
		)
	}
	statePath, err := filepath.Abs(statePath)
	if err != nil {
		return config{}, fmt.Errorf("resolve local state path: %w", err)
	}
	return config{
		sharedConfig: shared,
		folderID:     folderID,
		rootPath:     rootPath,
		statePath:    statePath,
	}, nil
}

func rootClientKey(rootPath string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(rootPath)))
	return fmt.Sprintf("root:v1:%x", sum[:])
}

func rootDisplayName(root syncRoot) string {
	label := root.Label
	if label == "" {
		label = filepath.Base(root.Path)
	}
	var result []rune
	bytes := 0
	for _, character := range []rune(label) {
		size := len(string(character))
		if bytes+size > 128 {
			break
		}
		result = append(result, character)
		bytes += size
	}
	if len(result) == 0 {
		return "sync folder"
	}
	return string(result)
}

func defaultStateFilename(folderID, rootPath string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(rootPath)))
	return fmt.Sprintf("%s-%x.db", folderID, sum[:6])
}

func boolEnvironment(name string, defaultValue bool) (bool, error) {
	value := os.Getenv(name)
	if value == "" {
		return defaultValue, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return parsed, nil
}

func durationEnvironment(name string, defaultValue time.Duration) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return defaultValue, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return parsed, nil
}

func environmentOrDefault(name, defaultValue string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return defaultValue
}
