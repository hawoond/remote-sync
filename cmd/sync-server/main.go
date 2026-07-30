package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	syncv1 "github.com/hawoond/remote-sync/api/sync/v1"
	"github.com/hawoond/remote-sync/internal/auth"
	"github.com/hawoond/remote-sync/internal/blob"
	"github.com/hawoond/remote-sync/internal/buildinfo"
	"github.com/hawoond/remote-sync/internal/domain"
	"github.com/hawoond/remote-sync/internal/garbage"
	"github.com/hawoond/remote-sync/internal/metadata"
	"github.com/hawoond/remote-sync/internal/syncengine"
	"github.com/hawoond/remote-sync/internal/transport/grpcserver"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
)

const (
	defaultListenAddress = ":8443"
	defaultMaxFileSize   = int64(10 << 30)
	defaultMaxFolderSize = int64(1 << 40)
	defaultMaxUserSize   = int64(5 << 40)
	defaultPendingSize   = int64(20 << 30)
	defaultMaxChunkSize  = int64(4 << 20)
	defaultGCInterval    = time.Hour
	defaultGCBatchSize   = int64(100)
)

type config struct {
	databaseURL     string
	blobRoot        string
	listenAddress   string
	deviceID        string
	deviceToken     string
	tlsCertFile     string
	tlsKeyFile      string
	allowInsecure   bool
	devBootstrap    bool
	bootstrapUser   string
	bootstrapFolder string
	gcEnabled       bool
	gcInterval      time.Duration
	gcBatchSize     int
	limits          metadata.Limits
}

func main() {
	if buildinfo.Requested(os.Args[1:]) {
		fmt.Println(buildinfo.String("sync-server"))
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config, logger *slog.Logger) error {
	pool, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	blobStore, err := blob.NewLocal(cfg.blobRoot)
	if err != nil {
		return fmt.Errorf("open blob store: %w", err)
	}
	defer blobStore.Close()
	metadataStore := metadata.NewPostgres(pool, cfg.limits)
	if cfg.devBootstrap {
		credentialDigest := domain.Hash(sha256.Sum256([]byte(cfg.deviceToken)))
		if err := metadataStore.BootstrapDevelopment(
			ctx,
			cfg.bootstrapUser,
			cfg.deviceID,
			cfg.bootstrapFolder,
			credentialDigest,
		); err != nil {
			return fmt.Errorf("bootstrap development records: %w", err)
		}
	} else if cfg.deviceID != "" && cfg.deviceToken != "" {
		credentialDigest := domain.Hash(sha256.Sum256([]byte(cfg.deviceToken)))
		if err := metadataStore.SetDeviceCredential(ctx, cfg.deviceID, credentialDigest); err != nil {
			return fmt.Errorf("register configured device credential: %w", err)
		}
	}

	resolver, err := auth.NewDatabaseResolver(metadataStore)
	if err != nil {
		return fmt.Errorf("configure device authentication: %w", err)
	}
	engine := syncengine.New(metadataStore, blobStore, cfg.limits)
	if cfg.gcEnabled {
		collector, err := garbage.New(metadataStore, blobStore, cfg.gcBatchSize)
		if err != nil {
			return fmt.Errorf("configure garbage collector: %w", err)
		}
		go func() {
			_ = collector.Run(ctx, cfg.gcInterval, func(
				report domain.GarbageCollectionReport,
				runErr error,
			) {
				if runErr != nil {
					logger.Error("garbage collection failed", "error", runErr)
					return
				}
				logger.Info(
					"garbage collection completed",
					"expired_uploads", report.ExpiredUploads,
					"pruned_versions", report.PrunedVersions,
					"marked_objects", report.MarkedObjects,
					"deleted_objects", report.DeletedObjects,
				)
			})
		}()
	}

	options, err := serverOptions(cfg)
	if err != nil {
		return err
	}
	rpcServer := grpc.NewServer(options...)
	syncv1.RegisterSyncServiceServer(rpcServer, grpcserver.New(engine, resolver))

	healthServer := health.NewServer()
	healthv1.RegisterHealthServer(rpcServer, healthServer)
	healthServer.SetServingStatus("", healthv1.HealthCheckResponse_SERVING)
	healthServer.SetServingStatus(
		syncv1.SyncService_ServiceDesc.ServiceName,
		healthv1.HealthCheckResponse_SERVING,
	)

	listener, err := net.Listen("tcp", cfg.listenAddress)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.listenAddress, err)
	}
	defer listener.Close()

	logger.Info("server listening", "address", listener.Addr().String(), "tls", !cfg.allowInsecure)
	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- rpcServer.Serve(listener)
	}()

	select {
	case err := <-serveErrors:
		if err != nil {
			return fmt.Errorf("serve gRPC: %w", err)
		}
		return nil
	case <-ctx.Done():
		healthServer.SetServingStatus("", healthv1.HealthCheckResponse_NOT_SERVING)
		healthServer.SetServingStatus(
			syncv1.SyncService_ServiceDesc.ServiceName,
			healthv1.HealthCheckResponse_NOT_SERVING,
		)
		return stopServer(rpcServer, serveErrors)
	}
}

func stopServer(server *grpc.Server, serveErrors <-chan error) error {
	stopped := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		server.Stop()
		<-stopped
	}

	select {
	case err := <-serveErrors:
		if err != nil {
			return fmt.Errorf("serve gRPC: %w", err)
		}
	default:
	}
	return nil
}

func serverOptions(cfg config) ([]grpc.ServerOption, error) {
	if cfg.allowInsecure {
		return nil, nil
	}
	certificate, err := tls.LoadX509KeyPair(cfg.tlsCertFile, cfg.tlsKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS certificate: %w", err)
	}
	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
	}
	return []grpc.ServerOption{grpc.Creds(credentials.NewTLS(tlsConfig))}, nil
}

func loadConfig() (config, error) {
	allowInsecure, err := boolEnvironment("ALLOW_INSECURE", false)
	if err != nil {
		return config{}, err
	}
	devBootstrap, err := boolEnvironment("DEV_BOOTSTRAP", false)
	if err != nil {
		return config{}, err
	}
	gcEnabled, err := boolEnvironment("GC_ENABLED", true)
	if err != nil {
		return config{}, err
	}
	gcInterval, err := durationEnvironment("GC_INTERVAL", defaultGCInterval)
	if err != nil {
		return config{}, err
	}
	gcBatchSize, err := positiveInt64Environment("GC_BATCH_SIZE", defaultGCBatchSize)
	if err != nil {
		return config{}, err
	}
	if gcBatchSize > 1000 {
		return config{}, errors.New("GC_BATCH_SIZE must not exceed 1000")
	}
	maxFileSize, err := positiveInt64Environment("MAX_FILE_SIZE_BYTES", defaultMaxFileSize)
	if err != nil {
		return config{}, err
	}
	maxFolderSize, err := positiveInt64Environment("MAX_FOLDER_LIVE_SIZE_BYTES", defaultMaxFolderSize)
	if err != nil {
		return config{}, err
	}
	maxUserSize, err := positiveInt64Environment("MAX_USER_LIVE_SIZE_BYTES", defaultMaxUserSize)
	if err != nil {
		return config{}, err
	}
	maxPendingSize, err := positiveInt64Environment("MAX_PENDING_UPLOAD_SIZE_BYTES", defaultPendingSize)
	if err != nil {
		return config{}, err
	}
	maxChunkSize, err := positiveInt64Environment("MAX_CHUNK_SIZE_BYTES", defaultMaxChunkSize)
	if err != nil {
		return config{}, err
	}
	if maxChunkSize > 4<<20 {
		return config{}, fmt.Errorf("MAX_CHUNK_SIZE_BYTES must not exceed %d", 4<<20)
	}

	cfg := config{
		databaseURL:     os.Getenv("DATABASE_URL"),
		blobRoot:        os.Getenv("BLOB_ROOT"),
		listenAddress:   environmentOrDefault("LISTEN_ADDR", defaultListenAddress),
		deviceID:        os.Getenv("SYNC_DEVICE_ID"),
		deviceToken:     os.Getenv("SYNC_DEVICE_TOKEN"),
		tlsCertFile:     os.Getenv("TLS_CERT_FILE"),
		tlsKeyFile:      os.Getenv("TLS_KEY_FILE"),
		allowInsecure:   allowInsecure,
		devBootstrap:    devBootstrap,
		bootstrapUser:   os.Getenv("SYNC_USER_ID"),
		bootstrapFolder: os.Getenv("SYNC_FOLDER_ID"),
		gcEnabled:       gcEnabled,
		gcInterval:      gcInterval,
		gcBatchSize:     int(gcBatchSize),
		limits: metadata.Limits{
			MaxFileSize:                 maxFileSize,
			MaxFolderLiveSize:           maxFolderSize,
			MaxUserLiveSize:             maxUserSize,
			MaxPendingUploadSizePerUser: maxPendingSize,
			MaxChunkSize:                int32(maxChunkSize),
			UploadSessionTTL:            24 * time.Hour,
		},
	}
	for name, value := range map[string]string{
		"DATABASE_URL": cfg.databaseURL,
		"BLOB_ROOT":    cfg.blobRoot,
	} {
		if value == "" {
			return config{}, fmt.Errorf("%s is required", name)
		}
	}
	if !cfg.allowInsecure && (cfg.tlsCertFile == "" || cfg.tlsKeyFile == "") {
		return config{}, errors.New("TLS_CERT_FILE and TLS_KEY_FILE are required unless ALLOW_INSECURE=true")
	}
	if (cfg.deviceID == "") != (cfg.deviceToken == "") {
		return config{}, errors.New("SYNC_DEVICE_ID and SYNC_DEVICE_TOKEN must be configured together")
	}
	if cfg.deviceToken != "" && len(cfg.deviceToken) < 32 {
		return config{}, errors.New("SYNC_DEVICE_TOKEN must contain at least 32 characters")
	}
	if cfg.devBootstrap &&
		(cfg.bootstrapUser == "" || cfg.bootstrapFolder == "" ||
			cfg.deviceID == "" || cfg.deviceToken == "") {
		return config{}, errors.New(
			"SYNC_USER_ID, SYNC_DEVICE_ID, SYNC_FOLDER_ID, and SYNC_DEVICE_TOKEN are required when DEV_BOOTSTRAP=true",
		)
	}
	return cfg, nil
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

func positiveInt64Environment(name string, defaultValue int64) (int64, error) {
	value := os.Getenv(name)
	if value == "" {
		return defaultValue, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
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
