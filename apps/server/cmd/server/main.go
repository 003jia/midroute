// Midroute 自主中间路由平台 —— 唯一后端入口（对齐 PRD US-001 / M1）。
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"midroute/internal/api"
	"midroute/internal/config"
	"midroute/internal/credentials"
	"midroute/internal/db"
	"midroute/internal/httpserver"
	"midroute/internal/repository"
	"midroute/internal/router"
	"midroute/internal/session"
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		slog.Error("midroute exited with error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)
	log.Info("midroute starting", "config", cfg.String(), "version", version())

	// 数据库初始化（空库自动初始化 schema）
	ctx := context.Background()
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	conn, err := db.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	version, err := db.Migrate(ctx, conn)
	if err != nil {
		return err
	}
	log.Info("database ready", "path", cfg.DBPath, "schema_version", version)

	// 凭据库：macOS 用 Keychain，其他平台退化为内存库（凭据不持久，仅开发用）
	var v credentials.Vault
	if kv, err := credentials.NewKeychainVault("midroute"); err == nil {
		v = kv
		log.Info("credential vault", "provider", "macos-keychain")
	} else {
		v = credentials.NewInMemoryVault()
		log.Warn("credential vault degraded to in-memory", "reason", err.Error(), "os", runtime.GOOS)
	}

	// 业务装配
	store := repository.New(conn)
	app := api.NewApp(store, v, nil, log)
	rt := router.New(store, app.ResolveForRouter, log)
	app.Router = rt

	// HTTP 服务
	guard := session.NewGuard(cfg.LocalOnly, cfg.AdminKey)
	srv := httpserver.New(log, httpserver.NewDBStore(version), guard)
	app.Mount(srv)
	handler := srv.Handler()
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return err
	}
	log.Info("midroute listening", "addr", ln.Addr().String(), "local_only", cfg.LocalOnly)

	errCh := make(chan error, 1)
	go func() {
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case sig := <-stop:
		log.Info("shutdown signal received", "signal", sig.String())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}
	log.Info("midroute stopped")
	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

func version() string {
	return "0.1.0-dev"
}
