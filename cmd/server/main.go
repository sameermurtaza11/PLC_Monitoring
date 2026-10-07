// PLC Monitoring — Version 01
//
// One process runs two things:
//  1. the acquisition manager (PLC → PostgreSQL), in the background
//  2. the Gin web server (PostgreSQL → browser)
//
// Run from the project folder:  go run ./cmd/server
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/gin-gonic/gin"

	"PLC_Monitoring/internal/acquisition"
	"PLC_Monitoring/internal/config"
	"PLC_Monitoring/internal/store"
	"PLC_Monitoring/internal/web"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	cfg := config.Load()

	// Ctrl+C cancels ctx → acquisition and HTTP server stop cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("PostgreSQL: %v", err) // nothing works without the DB
	}
	defer db.Close()
	log.Println("connected to PostgreSQL")

	// PLCs are NOT connected here. Each PLC worker connects (and reconnects)
	// on its own, so an offline PLC never stops the application.
	acqDone := make(chan struct{})
	onConfigChange := func() {} // called by the web UI after a config save
	if cfg.AcquireEnabled {
		mgr := acquisition.NewManager(db, cfg.ConfigReload)
		onConfigChange = mgr.Reload
		go func() {
			defer close(acqDone)
			mgr.Run(ctx)
		}()
	} else {
		close(acqDone)
		log.Println("acquisition disabled (ACQUISITION=off)")
	}

	gin.SetMode(gin.ReleaseMode)
	httpSrv := &http.Server{Addr: cfg.HTTPAddr, Handler: web.NewRouter(db, onConfigChange)}
	go func() {
		log.Printf("web UI on http://localhost%s", cfg.HTTPAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpSrv.Shutdown(shutdownCtx)
	<-acqDone
}
