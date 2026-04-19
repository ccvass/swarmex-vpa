package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/client"
	vpa "github.com/ccvass/swarmex/swarmex-vpa"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	promURL := os.Getenv("PROMETHEUS_URL")
	if promURL == "" { promURL = "http://prometheus:9090" }
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil { logger.Error("docker failed", "error", err); os.Exit(1) }
	defer cli.Close()
	ctrl := vpa.New(cli, promURL, logger)
	go func() { http.Handle("/metrics", promhttp.Handler())
		http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }); http.ListenAndServe(":8080", nil) }()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	logger.Info("swarmex-vpa starting", "prometheus", promURL)
	go ctrl.RunLoop(ctx, 30*time.Second)
	msgCh, errCh := cli.Events(ctx, events.ListOptions{})
	for { select { case e := <-msgCh: ctrl.HandleEvent(ctx, e); case err := <-errCh: if ctx.Err() != nil { return }; logger.Error("error", "err", err); return; case <-ctx.Done(): return } }
}
