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
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/hawoond/remote-sync/internal/agent"
	"github.com/hawoond/remote-sync/internal/localdb"
	"github.com/hawoond/remote-sync/internal/transport/grpcclient"
)

const defaultServerAddress = "127.0.0.1:8443"

type config struct {
	serverAddress string
	deviceToken   string
	folderID      string
	rootPath      string
	statePath     string
	tlsServerName string
	tlsCAFile     string
	allowInsecure bool
	scanInterval  time.Duration
	debounce      time.Duration
}

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func realMain(arguments []string, input *os.File, output, errorOutput io.Writer) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if len(arguments) > 0 {
		var commandErr error
		switch arguments[0] {
		case "discover":
			commandErr = runDiscoverCommand(ctx, arguments[1:], output, errorOutput)
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

	rootPath, err := resolveSyncRoot(ctx, input, errorOutput)
	if err != nil {
		_, _ = fmt.Fprintln(errorOutput, "sync-agent:", err)
		return 2
	}
	cfg, err := loadConfig(rootPath)
	if err != nil {
		_, _ = fmt.Fprintln(errorOutput, "sync-agent: invalid configuration:", err)
		return 2
	}

	logger := slog.New(slog.NewJSONHandler(output, nil))
	slog.SetDefault(logger)
	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("agent stopped", "error", err)
		return 1
	}
	return 0
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

	tlsConfig, err := clientTLSConfig(cfg)
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

func clientTLSConfig(cfg config) (*tls.Config, error) {
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

func loadConfig(rootPath string) (config, error) {
	allowInsecure, err := boolEnvironment("ALLOW_INSECURE", false)
	if err != nil {
		return config{}, err
	}
	scanInterval, err := durationEnvironment("SCAN_INTERVAL", 15*time.Minute)
	if err != nil {
		return config{}, err
	}
	debounce, err := durationEnvironment("WATCH_DEBOUNCE", 500*time.Millisecond)
	if err != nil {
		return config{}, err
	}

	folderID := os.Getenv("SYNC_FOLDER_ID")
	if _, err := uuid.Parse(folderID); err != nil {
		return config{}, errors.New("SYNC_FOLDER_ID must be a UUID")
	}
	statePath := os.Getenv("SYNC_STATE_PATH")
	if statePath == "" {
		configDirectory, err := os.UserConfigDir()
		if err != nil {
			return config{}, fmt.Errorf("resolve user configuration directory: %w", err)
		}
		statePath = filepath.Join(
			configDirectory,
			"remote-sync",
			defaultStateFilename(folderID, rootPath),
		)
	}
	statePath, err = filepath.Abs(statePath)
	if err != nil {
		return config{}, fmt.Errorf("resolve SYNC_STATE_PATH: %w", err)
	}

	cfg := config{
		serverAddress: environmentOrDefault("SYNC_SERVER_ADDRESS", defaultServerAddress),
		deviceToken:   os.Getenv("SYNC_DEVICE_TOKEN"),
		folderID:      folderID,
		rootPath:      rootPath,
		statePath:     statePath,
		tlsServerName: os.Getenv("TLS_SERVER_NAME"),
		tlsCAFile:     os.Getenv("TLS_CA_FILE"),
		allowInsecure: allowInsecure,
		scanInterval:  scanInterval,
		debounce:      debounce,
	}
	if cfg.deviceToken == "" {
		return config{}, errors.New("SYNC_DEVICE_TOKEN is required")
	}
	if len(cfg.deviceToken) < 32 {
		return config{}, errors.New("SYNC_DEVICE_TOKEN must contain at least 32 characters")
	}
	return cfg, nil
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
