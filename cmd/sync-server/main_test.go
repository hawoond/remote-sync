package main

import (
	"strings"
	"testing"
)

func TestLoadConfigAllowsDatabaseAuthenticationWithoutBootstrapCredential(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example.invalid/remote_sync")
	t.Setenv("BLOB_ROOT", t.TempDir())
	t.Setenv("ALLOW_INSECURE", "true")
	t.Setenv("DEV_BOOTSTRAP", "false")
	t.Setenv("SYNC_DEVICE_ID", "")
	t.Setenv("SYNC_DEVICE_TOKEN", "")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.devBootstrap {
		t.Fatal("development bootstrap unexpectedly enabled")
	}
	if !cfg.gcEnabled || cfg.gcInterval <= 0 || cfg.gcBatchSize <= 0 {
		t.Fatalf("garbage collector configuration = %+v", cfg)
	}
}

func TestLoadConfigRequiresCompleteBootstrapIdentity(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example.invalid/remote_sync")
	t.Setenv("BLOB_ROOT", t.TempDir())
	t.Setenv("ALLOW_INSECURE", "true")
	t.Setenv("DEV_BOOTSTRAP", "true")
	t.Setenv("SYNC_USER_ID", "7ccf9e76-8a97-4883-8faf-d9ead627699c")
	t.Setenv("SYNC_FOLDER_ID", "1b7c7e04-d897-4cef-a61a-9e6041d968bc")
	t.Setenv("SYNC_DEVICE_ID", "")
	t.Setenv("SYNC_DEVICE_TOKEN", "")

	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "required when DEV_BOOTSTRAP=true") {
		t.Fatalf("loadConfig() error = %v", err)
	}
}
