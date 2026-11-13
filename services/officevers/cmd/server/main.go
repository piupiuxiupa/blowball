// Command server starts the office-version-service HTTP API. It loads
// configuration from the environment, bootstraps the MinIO bucket (creating it
// and enabling Object Versioning if needed), and serves the /documents API.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lush/blowball/services/officevers/internal/config"
	"github.com/lush/blowball/services/officevers/internal/minio"
	"github.com/lush/blowball/services/officevers/internal/router"
)

func main() {
	cfg, err := config.Load(nil)
	if err != nil {
		// --help / -h: usage was already printed; exit cleanly.
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		log.Fatalf("config: %v", err)
	}

	store, err := minio.New(cfg)
	if err != nil {
		log.Fatalf("minio client: %v", err)
	}

	// Bootstrap the bucket before serving any request: versioning must be on
	// before the first object is written.
	bootCtx, bootCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer bootCancel()
	if err := store.Bootstrap(bootCtx); err != nil {
		log.Fatalf("bootstrap: %v", err)
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           router.New(store),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("office-version-service listening on %s (bucket=%s)", cfg.ListenAddr, cfg.MinIOBucket)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutdown: stopping server...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
