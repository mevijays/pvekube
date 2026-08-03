// Command pvekube is the entire application: a single binary that serves a
// web UI for building Kubernetes-ready Proxmox VM templates and launching
// Cluster API-managed Kubernetes clusters, with no CLI work required from
// the operator.
package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"pvekube/internal/auth"
	"pvekube/internal/config"
	"pvekube/internal/jobs"
	"pvekube/internal/secrets"
	"pvekube/internal/server"
	"pvekube/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer st.Close()

	sealer, err := secrets.LoadOrCreateSealer(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("loading secret sealer: %w", err)
	}
	redactor := secrets.NewRedactor()

	jobEngine := jobs.NewEngine(st.DB, cfg.LogDir, redactor.Redact)
	if err := jobEngine.ReconcileOnStartup(); err != nil {
		return fmt.Errorf("reconciling jobs on startup: %w", err)
	}

	authMgr := auth.New(st.DB)

	srv := server.New(server.Deps{
		DB:       st.DB,
		AuthMgr:  authMgr,
		Jobs:     jobEngine,
		Sealer:   sealer,
		Redactor: redactor,
		BinDir:   cfg.BinDir,
		DataDir:  cfg.DataDir,
	})

	slog.Info("pvekube starting", "listen", cfg.Listen, "data_dir", cfg.DataDir)
	fmt.Printf("\nPVEKube is running. Open http://%s in your browser.\n\n", displayAddr(cfg.Listen))
	return http.ListenAndServe(cfg.Listen, srv.Routes())
}

func displayAddr(listen string) string {
	if len(listen) > 0 && listen[0] == ':' {
		return "localhost" + listen
	}
	return listen
}
