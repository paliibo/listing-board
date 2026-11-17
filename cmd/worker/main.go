package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"listingboard/internal/store"
	"listingboard/internal/worker"
)

const defaultPause = 2 * time.Second

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required")
	}
	dataDir := env("DATA_DIR", "data")
	pause, err := stepPause(os.Getenv("STEP_PAUSE"))
	if err != nil {
		log.Printf("warning: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	st, err := store.Wait(ctx, dsn, 30*time.Second)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Fatal(err)
	}
	defer st.Close()

	log.Printf("worker started, step pause %s", pause)
	if err := worker.New(st, dataDir, pause).Run(ctx); err != nil {
		log.Fatal(err)
	}
	log.Print("worker stopped")
}

// stepPause reads STEP_PAUSE. A bad value falls back to the default with an
// error to log, never a fatal one: compose does not restart the worker, so a
// typo would otherwise leave every sheet running for good.
func stepPause(raw string) (time.Duration, error) {
	if raw == "" {
		return defaultPause, nil
	}
	pause, err := time.ParseDuration(raw)
	if err != nil {
		return defaultPause, fmt.Errorf("STEP_PAUSE %q: %v; using %s", raw, err, defaultPause)
	}
	if pause < 0 {
		return defaultPause, fmt.Errorf("STEP_PAUSE %q is negative; using %s", raw, defaultPause)
	}
	return pause, nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
