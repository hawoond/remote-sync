package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/hawoond/remote-sync/internal/migrate"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	var directory string
	flag.StringVar(&directory, "dir", "migrations", "migration directory")
	flag.Parse()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		slog.Error("DATABASE_URL is required")
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		slog.Error("ping database", "error", err)
		os.Exit(1)
	}
	if err := migrate.Up(ctx, pool, directory); err != nil {
		slog.Error("apply migrations", "error", err)
		os.Exit(1)
	}
	fmt.Println("migrations applied")
}
