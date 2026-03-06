package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cdx.cc/claude-bridge/internal/config"
	"cdx.cc/claude-bridge/internal/logging"
	"cdx.cc/claude-bridge/internal/server"
)

const (
	shutdownTimeout   = 10 * time.Second
	readHeaderTimeout = 10 * time.Second
	configFilePath    = "runtime_config.json"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config error", slog.Any("err", err))
		os.Exit(1)
	}

	logger := logging.NewLogger(cfg.LogLevel)

	// 创建运行时配置（支持热更新 + JSON 持久化）
	rtCfg, err := config.NewRuntimeConfig(cfg, configFilePath)
	if err != nil {
		logger.Error("runtime config error", slog.Any("err", err))
		os.Exit(1)
	}

	srv, err := server.New(cfg, rtCfg, logger)
	if err != nil {
		logger.Error("server init error", slog.Any("err", err))
		os.Exit(1)
	}
	defer func() {
		_ = srv.Close()
	}()

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Router(),
		ReadHeaderTimeout: readHeaderTimeout,
	}

	go func() {
		logger.Info("server listening",
			slog.String("addr", cfg.ListenAddr),
			slog.String("admin", "http://localhost"+cfg.ListenAddr+"/admin"),
		)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", slog.Any("err", err))
			os.Exit(1)
		}
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		logger.Error("shutdown error", slog.Any("err", err))
	}
}
