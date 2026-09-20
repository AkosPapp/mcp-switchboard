// Command mcp-switchboard-hub terminates client tunnels, aggregates the MCP
// servers behind them, and serves them to consumers.
//
// Two listeners, for a deliberate reason: only the tunnel endpoint is meant to
// be reachable from the internet, and it always requires a token. The console,
// the API, the MCP scopes and /metrics live on a second listener that binds
// loopback, so they are not protected by a password - they are simply not
// reachable. That is a stronger guarantee than a shared secret, and it is why
// the private listener is unauthenticated by default.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/api"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/calls"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/config"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/console"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/loki"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/mcpserver"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/metrics"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/protocol"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/tunnel"
	"github.com/AkosPapp/mcp-switchboard/hub/web"
)

// version is stamped at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags)"
var version = "0.0.0+dev"

// shutdownGrace bounds how long in-flight work has to finish once a signal
// arrives (spec.md G3).
const shutdownGrace = 10 * time.Second

// purgeInterval is how often retention runs. Daily is plenty for a log bounded
// by both age and row count.
const purgeInterval = 24 * time.Hour

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mcp-switchboard-hub:", err)
		os.Exit(1)
	}
}

// defaultEnvFile is read from the working directory when --env-file is not
// given. Running the hub in a checkout therefore picks up the repo's .env
// without being told to, which is how the hub this replaces behaved and what
// every editor task and shell alias assumes.
const defaultEnvFile = ".env"

func run() error {
	envFile := flag.String("env-file", defaultEnvFile,
		"read MCP_SWITCHBOARD_* settings from this file first (default ./.env, skipped if absent)")
	showVersion := flag.Bool("version", false, "print the version and exit")

	// Flags win over the environment, which wins over the env file. Each is a
	// pointer so "not given" stays distinguishable from "given as empty".
	tunnelHost := flag.String("tunnel-host", "", "override MCP_SWITCHBOARD_TUNNEL_HOST")
	tunnelPort := flag.Int("tunnel-port", 0, "override MCP_SWITCHBOARD_TUNNEL_PORT")
	privateHost := flag.String("private-host", "", "override MCP_SWITCHBOARD_PRIVATE_HOST")
	privatePort := flag.Int("private-port", 0, "override MCP_SWITCHBOARD_PRIVATE_PORT")
	dataDir := flag.String("data-dir", "", "override MCP_SWITCHBOARD_DATA_DIR")
	logLevel := flag.String("log-level", "", "override MCP_SWITCHBOARD_LOG_LEVEL")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}

	// A file the operator named explicitly has to be there: silently ignoring a
	// typo'd path would start the hub with none of the settings they meant.
	// The default is missing-ok, because not every deployment has a .env.
	explicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "env-file" {
			explicit = true
		}
	})
	if explicit && *envFile != "" {
		if _, err := os.Stat(*envFile); err != nil {
			return fmt.Errorf("env file not found: %s", *envFile)
		}
	}

	settings, err := config.Load(*envFile)
	if err != nil {
		return err
	}

	if *tunnelHost != "" {
		settings.TunnelHost = *tunnelHost
	}
	if *tunnelPort != 0 {
		settings.TunnelPort = *tunnelPort
	}
	if *privateHost != "" {
		settings.PrivateHost = *privateHost
	}
	if *privatePort != 0 {
		settings.PrivatePort = *privatePort
	}
	if *dataDir != "" {
		settings.DataDir = *dataDir
	}
	if *logLevel != "" {
		settings.LogLevel = strings.ToUpper(*logLevel)
	}

	logger := newLogger(settings.LogLevel)
	slog.SetDefault(logger)

	// The root context every request, session and run hangs off. Cancelling it
	// is what a signal does, and what makes shutdown orderly rather than abrupt.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(settings.DataDir, 0o750); err != nil {
		return fmt.Errorf("cannot create the data directory %s: %w", settings.DataDir, err)
	}

	db, err := store.Open(ctx, store.Options{
		Path:          settings.DBPath(),
		RetentionDays: settings.RetentionDays,
		MaxRows:       settings.MaxRows,
	})
	if err != nil {
		return err
	}
	defer db.Close()

	collectors := metrics.New()
	bus := events.NewBus()
	reg := registry.New(bus)

	exporter := loki.New(loki.Config{
		URL:     settings.LokiURL,
		Labels:  settings.LokiLabels,
		Enabled: settings.LokiEnabled,
		Logger:  logger,
	}, collectors)
	exporter.Start()

	dispatcher := calls.NewDispatcher(calls.Options{
		Store:   db,
		Bus:     bus,
		Metrics: collectors,
		Loki:    exporter,
		Timeout: time.Duration(settings.CallTimeout * float64(time.Second)),
		Logger:  logger,
	})

	mcpEndpoint := mcpserver.New(reg, dispatcher, mcpserver.Options{
		Version: version,
		Logger:  logger,
	})

	apiHandler := api.New(api.Options{
		Settings:   settings,
		Registry:   reg,
		Store:      db,
		Dispatcher: dispatcher,
		Bus:        bus,
		Logger:     logger,
	})

	tunnelHandler := tunnel.NewHandler(reg, tunnel.Options{
		Token:        settings.TunnelToken,
		ToolsTimeout: time.Duration(settings.ToolsTimeout * float64(time.Second)),
		SettleDelay:  time.Duration(settings.ServerSettleDelay * float64(time.Second)),
		HubVersion:   version,
		Metrics:      collectors,
		Logger:       logger,
	})

	// The registry's counts feed two gauges that nothing else updates, so they
	// are refreshed whenever the topology changes rather than on a timer.
	go watchTopology(ctx, bus, reg, collectors)
	go purgeLoop(ctx, db, logger)

	tunnelSrv := &http.Server{
		Addr:              net.JoinHostPort(settings.TunnelHost, strconv.Itoa(settings.TunnelPort)),
		Handler:           tunnelMux(tunnelHandler),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	privateSrv := &http.Server{
		Addr:              net.JoinHostPort(settings.PrivateHost, strconv.Itoa(settings.PrivatePort)),
		Handler:           privateMux(settings, apiHandler, mcpEndpoint, collectors),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	logger.Info("hub starting",
		"version", version,
		"tunnel", tunnelSrv.Addr,
		"private", privateSrv.Addr,
		"data", settings.DataDir,
	)

	errc := make(chan error, 2)
	for _, srv := range []*http.Server{tunnelSrv, privateSrv} {
		go func(srv *http.Server) {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("listener %s: %w", srv.Addr, err)
				return
			}
			errc <- nil
		}(srv)
	}

	select {
	case err := <-errc:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		logger.Info("shutting down")
	}

	// Drain in-flight work rather than cutting it off: a tool call that has
	// already reached a machine should be allowed to answer (G3).
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	_ = tunnelSrv.Shutdown(shutdownCtx)
	_ = privateSrv.Shutdown(shutdownCtx)
	exporter.Close(shutdownCtx)

	return nil
}

// tunnelMux serves only what the public listener is allowed to expose.
func tunnelMux(handler http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(protocol.Path, handler)
	mux.HandleFunc("/health", health)
	return mux
}

func privateMux(
	settings config.Settings,
	apiHandler http.Handler,
	mcpEndpoint *mcpserver.Endpoint,
	collectors *metrics.Metrics,
) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/", apiHandler)
	mux.Handle("/metrics", collectors.Handler())

	// Both spellings: the scope is read off the full path, so one handler
	// covers "/mcp" and everything below it.
	mux.Handle("/mcp", mcpEndpoint)
	mux.Handle("/mcp/", mcpEndpoint)

	// Everything that is not an API, MCP or metrics path is the console, which
	// owns its own routing in the browser (spec.md 8).
	mux.Handle("/", console.Handler(web.FS()))

	var handler http.Handler = mux
	if settings.PrivateToken != "" {
		handler = api.RequireToken(settings.PrivateToken, handler)
	}
	// /health stays outside the token check: a health probe that needs a secret
	// is a health probe nobody configures correctly.
	outer := http.NewServeMux()
	outer.HandleFunc("/health", health)
	outer.Handle("/", handler)
	return outer
}

func health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintln(w, `{"status":"ok"}`)
}

// watchTopology keeps the connection and server gauges in step with the
// registry. They are derived state, so deriving them on change is both cheaper
// and more accurate than sampling on a timer.
func watchTopology(ctx context.Context, bus *events.Bus, reg *registry.Registry, collectors *metrics.Metrics) {
	changes, unsubscribe := bus.Subscribe(ctx)
	defer unsubscribe()

	update := func() {
		connections, byState := reg.Counts()
		collectors.SetConnections(connections)
		collectors.SetServers(byState)
	}
	update()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-changes:
			if !ok {
				return
			}
			if event.Type == events.TypeConnections {
				update()
			}
		}
	}
}

func purgeLoop(ctx context.Context, db *store.SQLiteStore, logger *slog.Logger) {
	ticker := time.NewTicker(purgeInterval)
	defer ticker.Stop()

	for {
		if deleted, err := db.PurgeCalls(ctx); err != nil {
			logger.Warn("could not purge the call log", "error", err)
		} else if deleted > 0 {
			logger.Info("purged call records", "rows", deleted)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func newLogger(level string) *slog.Logger {
	parsed := parseLevel(level)
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: parsed}))
}

// parseLevel accepts slog's own names plus Python's, because the hub this
// replaces took WARNING and CRITICAL and there are deployments (and a NixOS
// module option) configured with them. Falling back to INFO on an unknown value
// would quietly discard the operator's intent, which is the opposite of what
// raising the level is for.
func parseLevel(level string) slog.Level {
	normalised := strings.ToUpper(strings.TrimSpace(level))
	switch normalised {
	case "WARNING":
		return slog.LevelWarn
	case "CRITICAL", "FATAL":
		return slog.LevelError
	}

	var parsed slog.Level
	if err := parsed.UnmarshalText([]byte(normalised)); err != nil {
		return slog.LevelInfo
	}
	return parsed
}
