// Command counter is an API gateway in front of OpenAI-compatible model servers.
// It checks API keys, limits concurrent requests per model without queueing, counts
// what passes and streams responses through untouched. It stores no request or
// response content.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const usageText = `usage:
  counter serve --config <file> --keys <file> [flags]
  counter genkey --name <name>
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	switch args[0] {
	case "serve":
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return runServe(ctx, args[1:], stdout, stderr)
	case "genkey":
		return runGenkey(args[1:], stdout, stderr)
	default:
		fmt.Fprint(stderr, usageText)
		return 2
	}
}

func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "config file (required)")
	keysPath := flags.String("keys", "", "key file (required)")
	addr := flags.String("addr", ":8080", "API listen address")
	metricsAddr := flags.String("metrics-addr", ":9090", "metrics listen address")
	reloadInterval := flags.Duration("keys-reload-interval", 10*time.Second, "how often to re-read the key file")
	shutdownDelay := flags.Duration("shutdown-delay", 5*time.Second,
		"how long to keep accepting new connections after SIGTERM, until load balancing has stopped sending them")
	shutdownTimeout := flags.Duration("shutdown-timeout", 25*time.Second,
		"how long running requests may finish after SIGTERM; keep it below the pod's termination grace period")
	headerTimeout := flags.Duration("upstream-response-header-timeout", 10*time.Minute,
		"how long to wait for response headers from a model, 0 for no limit; "+
			"a non-streaming response sends headers only after generating everything")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *configPath == "" || *keysPath == "" || flags.NArg() != 0 || *reloadInterval <= 0 {
		fmt.Fprint(stderr, usageText)
		return 2
	}

	logger := slog.New(slog.NewJSONHandler(stdout, nil))
	if err := serve(ctx, serveOptions{
		configPath:            *configPath,
		keysPath:              *keysPath,
		addr:                  *addr,
		metricsAddr:           *metricsAddr,
		reloadInterval:        *reloadInterval,
		shutdownDelay:         *shutdownDelay,
		shutdownTimeout:       *shutdownTimeout,
		upstreamHeaderTimeout: *headerTimeout,
	}, logger); err != nil {
		logger.Error("counter stopped", "error", err)
		return 1
	}
	return 0
}

type serveOptions struct {
	configPath            string
	keysPath              string
	addr                  string
	metricsAddr           string
	reloadInterval        time.Duration
	shutdownDelay         time.Duration
	shutdownTimeout       time.Duration
	upstreamHeaderTimeout time.Duration
}

func serve(ctx context.Context, opts serveOptions, logger *slog.Logger) error {
	cfg, err := loadConfig(opts.configPath)
	if err != nil {
		return err
	}
	m := newMetrics()
	keys, err := newKeyStore(opts.keysPath, logger, m.keysReloadErrors)
	if err != nil {
		return fmt.Errorf("read key file: %w", err)
	}
	handler, err := newServer(cfg, keys, m, logger, opts.upstreamHeaderTimeout)
	if err != nil {
		return err
	}

	apiListener, err := net.Listen("tcp", opts.addr)
	if err != nil {
		return err
	}
	metricsListener, err := net.Listen("tcp", opts.metricsAddr)
	if err != nil {
		_ = apiListener.Close()
		return err
	}

	serverLog := slog.NewLogLogger(logger.Handler(), slog.LevelError)
	api := newPublicServer(handler, serverLog)
	metricsServer := &http.Server{
		Handler:           m.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		ErrorLog:          serverLog,
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go keys.watch(ctx, opts.reloadInterval)

	failed := make(chan error, 2)
	serveOn := func(srv *http.Server, l net.Listener) {
		if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
			failed <- err
		}
	}
	go serveOn(api, apiListener)
	go serveOn(metricsServer, metricsListener)
	logger.Info("counter started", "addr", apiListener.Addr().String(),
		"metrics_addr", metricsListener.Addr().String(), "models", len(cfg.Models))

	var serveErr error
	select {
	case <-ctx.Done():
		// Endpoints are updated after SIGTERM arrives, not before. Closing the listener at once
		// would refuse the connections still being routed here during a rolling update.
		select {
		case <-time.After(opts.shutdownDelay):
		case serveErr = <-failed:
		}
	case serveErr = <-failed:
	}

	// Running streams may finish until the timeout, then they are cut. Closing a
	// connection cancels its request, which cancels the upstream request too.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), opts.shutdownTimeout)
	defer cancelShutdown()
	if err := api.Shutdown(shutdownCtx); err != nil {
		logger.Warn("requests still running at the shutdown timeout, closing them", "error", err)
		_ = api.Close()
	}
	_ = metricsServer.Close()
	logger.Info("counter stopped")
	return serveErr
}
