package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	freereps "github.com/claude/freereps"
	"github.com/claude/freereps/internal/alerts"
	"github.com/claude/freereps/internal/config"
	"github.com/claude/freereps/internal/demo"
	"github.com/claude/freereps/internal/hevy"
	"github.com/claude/freereps/internal/ingest/alpha"
	"github.com/claude/freereps/internal/ingest/health"
	freerepsmcp "github.com/claude/freereps/internal/mcp"
	"github.com/claude/freereps/internal/notify"
	"github.com/claude/freereps/internal/opengym"
	"github.com/claude/freereps/internal/oura"
	"github.com/claude/freereps/internal/server"
	"github.com/claude/freereps/internal/storage"
	"github.com/claude/freereps/internal/withings"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"tailscale.com/tsnet"
)

// Version is set at build time via -ldflags.
var Version = "dev"

// BuildSHA is the commit the binary was built from, set at build time via
// -ldflags from the BUILD_SHA build argument. /healthz reports it.
var BuildSHA = "dev"

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	migrateOnly := flag.Bool("migrate-only", false, "run migrations and exit")
	rederiveSleep := flag.Bool("rederive-sleep", false, "re-derive sleep stages from the category samples already stored, then exit")
	rederiveFrom := flag.String("rederive-from", "", "first day to re-derive (2006-01-02, UTC); empty means the beginning of the data")
	rederiveTo := flag.String("rederive-to", "", "last day to re-derive (2006-01-02, UTC, inclusive); empty means today")
	rederiveUser := flag.Int("rederive-user", 0, "user ID to re-derive sleep stages for, required with -rederive-sleep")
	mcpMode := flag.Bool("mcp", false, "run as MCP server over stdio (for Claude Code integration)")
	demoMode := flag.Bool("demo", false, "seed database with demo data for testing")
	flag.Parse()

	// In MCP stdio mode, logs go to stderr to keep stdout clean for JSON-RPC.
	logOutput := os.Stdout
	if *mcpMode {
		logOutput = os.Stderr
	}
	log := slog.New(slog.NewTextHandler(logOutput, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log.Info("FreeReps starting", "version", Version)

	// Load config
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Run migrations (skip in MCP stdio mode — DB is managed by the server)
	dsn := cfg.Database.DSN()
	if !*mcpMode {
		if err := storage.RunMigrations(dsn, "migrations"); err != nil {
			log.Error("migration failed", "error", err)
			os.Exit(1)
		}
		log.Info("migrations applied")
	}

	if *migrateOnly {
		log.Info("migrate-only: exiting")
		return
	}

	// Connect database
	ctx := context.Background()
	db, err := storage.New(ctx, dsn)
	if err != nil {
		log.Error("failed to connect database", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	db.SetSourcePriority(cfg.SourcePriority)
	log.Info("database connected")

	// Seed demo data if requested (via -demo flag or FREEREPS_DEMO=true env var)
	if *demoMode || os.Getenv("FREEREPS_DEMO") == "true" {
		if err := demo.Seed(ctx, db, log); err != nil {
			log.Error("demo seed failed", "error", err)
			os.Exit(1)
		}
	}

	// Backfill sleep sessions from stages (idempotent — ON CONFLICT DO NOTHING).
	//
	// After the seed, not before it: the seed writes sleep stages, and a
	// backfill that ran first leaves a fresh demo database without the
	// sleep_analysis metric until the next start. The front page then shows
	// three hero numbers and an empty fourth cell. For a real deployment the
	// order makes no difference, because nothing seeds.
	if err := db.BackfillSleepSessions(ctx, log); err != nil {
		log.Warn("sleep session backfill failed", "error", err)
	}

	// Re-derive sleep stages from the category samples already stored.
	//
	// The ingest path derives stages from what a client sends, and the iOS app
	// syncs incrementally: its anchor query delivers what was added since the
	// last run plus the last 24 hours, and even a full sync resumes from the
	// server's checkpoints minus a week. Nights stored before a change to the
	// derivation are therefore never re-sent, and this is how they are rebuilt
	// without asking the phone for its history. Idempotent — see
	// health.ReDeriveSleepStages.
	if *rederiveSleep {
		if *rederiveUser <= 0 {
			log.Error("-rederive-sleep needs -rederive-user")
			os.Exit(1)
		}
		from, to, err := rederiveRange(*rederiveFrom, *rederiveTo)
		if err != nil {
			log.Error("invalid -rederive range", "error", err)
			os.Exit(1)
		}
		report, err := health.NewProvider(db, log).ReDeriveSleepStages(ctx, *rederiveUser, from, to)
		if err != nil {
			log.Error("re-deriving sleep stages failed", "error", err)
			os.Exit(1)
		}
		if report.Skipped != "" {
			log.Info("sleep stages left alone", "reason", report.Skipped)
			return
		}
		log.Info("sleep stages re-derived",
			"user_id", *rederiveUser,
			"from", from.Format("2006-01-02"), "to", to.Format("2006-01-02"),
			"source", report.Source, "samples_used", report.SamplesUsed,
			"samples_filtered", report.SamplesFiltered,
			"stages", report.Stages, "stages_inserted", report.StagesInserted)
		// sleep_sessions are built from the stages, so they are rebuilt from
		// the result rather than left to the next start.
		if err := db.BackfillSleepSessions(ctx, log); err != nil {
			log.Error("sleep session backfill failed", "error", err)
			os.Exit(1)
		}
		return
	}

	// MCP stdio mode: serve MCP protocol over stdin/stdout, then exit
	if *mcpMode {
		log.Info("starting MCP stdio server")
		mcpSrv := freerepsmcp.New(db, Version, log)
		if err := mcpserver.ServeStdio(mcpSrv,
			mcpserver.WithStdioContextFunc(func(ctx context.Context) context.Context {
				return freerepsmcp.WithUserID(ctx, 1)
			}),
		); err != nil {
			log.Error("MCP stdio server error", "error", err)
			os.Exit(1)
		}
		return
	}

	// Create providers
	healthProvider := health.NewProvider(db, log)
	// Logged because a changed zone silently relocates every session imported
	// from here on, relative to the sessions already stored.
	log.Info("alpha ingest ready", "session_timezone", cfg.Ingest.Location.String())
	alphaProvider := alpha.NewProvider(db, log, cfg.Ingest.Location)

	// Create server
	server.Version = Version
	server.BuildSHA = BuildSHA
	srv := server.New(db, healthProvider, alphaProvider, log)
	// Empty by default: the OAuth redirect URIs are then derived from the request
	// that starts the flow (internal/server.callbackURL).
	srv.SetBaseURL(cfg.Server.BaseURL)

	// Start Oura sync (always runs; no-ops if no users have Oura tokens)
	ouraClient := oura.NewClient()
	tokenMgr := oura.NewTokenManager(db)
	ouraSyncer := oura.NewSyncer(ouraClient, tokenMgr, db, cfg.Oura, log)

	syncCtx, syncCancel := context.WithCancel(ctx)
	defer syncCancel()
	go ouraSyncer.Run(syncCtx)

	srv.SetOura(tokenMgr, ouraSyncer)
	log.Info("oura sync started", "interval", cfg.Oura.SyncInterval)

	// Start Hevy sync (always runs; no-ops if no users have an API key)
	hevySyncer := hevy.NewSyncer(hevy.NewClient(), db, cfg.Hevy, log)
	go hevySyncer.Run(syncCtx)

	srv.SetHevy(hevySyncer)
	log.Info("hevy sync started", "interval", cfg.Hevy.SyncInterval)

	// Start openGym sync (always runs; no-ops if no user has paired an instance)
	openGymSyncer := opengym.NewSyncer(db, cfg.OpenGym.SyncInterval, cfg.Ingest.Location, log)
	go openGymSyncer.Run(syncCtx)

	srv.SetOpenGym(openGymSyncer)
	log.Info("openGym sync started", "interval", cfg.OpenGym.SyncInterval)

	// Start Withings sync (always runs; no-ops if no users have authorized)
	withingsTokenMgr := withings.NewTokenManager(db)
	withingsSyncer := withings.NewSyncer(withings.NewClient(), withingsTokenMgr, db, cfg.Withings, log)
	go withingsSyncer.Run(syncCtx)

	srv.SetWithings(withingsTokenMgr, withingsSyncer)
	log.Info("withings sync started", "interval", cfg.Withings.SyncInterval)

	// Start the alert watcher. It reads import_logs rather than hooking into the
	// syncers above, so a syncer that stopped running is also covered
	// (internal/alerts), and it reads its configuration from the database on
	// every cycle, so an edit in the Settings UI takes effect without a restart.
	//
	// config.yaml only seeds the row: a redeploy must not overwrite what was
	// entered in the UI, and a fresh database must not come up silent.
	seeded, err := db.SeedAlertSettings(ctx, storage.AlertSettings{
		Enabled:          cfg.Alerts.Enabled,
		NtfyURL:          cfg.Alerts.NtfyURL,
		Format:           cfg.Alerts.Format,
		Hostname:         cfg.Alerts.Hostname,
		CheckInterval:    cfg.Alerts.CheckInterval,
		FailureThreshold: cfg.Alerts.FailureThreshold,
		AppleSilence:     cfg.Alerts.AppleSilence,
	})
	if err != nil {
		log.Error("seeding alert settings failed", "error", err)
		os.Exit(1)
	}
	if seeded {
		log.Info("alert settings seeded from config",
			"enabled", cfg.Alerts.Enabled, "target", cfg.Alerts.NtfyURL, "format", cfg.Alerts.Format)
	}

	alertWatcher := alerts.NewWatcher(db, notify.New(log), log)
	go alertWatcher.Run(syncCtx)
	srv.SetAlerts(alertWatcher)
	log.Info("alert watcher started")

	// Mount MCP SSE server
	mcpSrv := freerepsmcp.New(db, Version, log)
	srv.SetMCP(mcpSrv)

	// Serve embedded frontend
	webDist, err := fs.Sub(freereps.WebFS, "web/dist")
	if err != nil {
		log.Error("failed to load embedded frontend", "error", err)
		os.Exit(1)
	}
	srv.SetFrontend(webDist)

	// Start server — tsnet or plain HTTP
	var listener net.Listener
	var tsServer *tsnet.Server

	if cfg.Tailscale.Enabled {
		tsServer = &tsnet.Server{
			Hostname: cfg.Tailscale.Hostname,
			Dir:      cfg.Tailscale.StateDir,
		}
		if err := tsServer.Start(); err != nil {
			log.Error("tsnet start failed", "error", err)
			os.Exit(1)
		}
		defer func() { _ = tsServer.Close() }()

		lc, err := tsServer.LocalClient()
		if err != nil {
			log.Error("tsnet local client failed", "error", err)
			os.Exit(1)
		}
		srv.SetTailscale(lc)

		listener, err = tsServer.ListenTLS("tcp", ":443")
		if err != nil {
			log.Error("tsnet listen failed", "error", err)
			os.Exit(1)
		}
		log.Info("tsnet server starting", "hostname", cfg.Tailscale.Hostname, "tls", true)
	} else {
		addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
		listener, err = net.Listen("tcp", addr)
		if err != nil {
			log.Error("listen failed", "addr", addr, "error", err)
			os.Exit(1)
		}
		log.Info("server starting", "addr", addr, "mode", "dev (no tailscale)")
	}

	httpSrv := &http.Server{Handler: srv}

	go func() {
		if err := httpSrv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	log.Info("shutting down", "signal", sig)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown error", "error", err)
	}
	log.Info("server stopped")
}

// rederiveRange turns the -rederive-from and -rederive-to flags into the
// half-open range the category sample query reads. Dates are UTC calendar
// days, and -rederive-to is inclusive: the whole day it names is read, so a
// range given as the two days a night touches covers that night.
//
// An empty flag leaves that end open — the beginning of the data below, the
// end of today above — which is what a first run over all stored samples
// wants.
func rederiveRange(from, to string) (time.Time, time.Time, error) {
	start := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	if from != "" {
		day, err := time.Parse("2006-01-02", from)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("-rederive-from %q is not a date (2006-01-02): %w", from, err)
		}
		start = day
	}

	end := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	if to != "" {
		day, err := time.Parse("2006-01-02", to)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("-rederive-to %q is not a date (2006-01-02): %w", to, err)
		}
		end = day.AddDate(0, 0, 1)
	}

	if !end.After(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("-rederive-to %s ends before -rederive-from %s",
			end.AddDate(0, 0, -1).Format("2006-01-02"), start.Format("2006-01-02"))
	}
	return start, end, nil
}
