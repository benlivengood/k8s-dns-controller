package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yourorg/k8s-dns-controller/pkg/ipcheck"
	"github.com/yourorg/k8s-dns-controller/pkg/store"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		logger.Error("NODE_NAME environment variable is required")
		os.Exit(1)
	}

	namespace := os.Getenv("NAMESPACE")
	if namespace == "" {
		namespace = "kube-system"
	}

	interval := 60 * time.Second
	if v := os.Getenv("CHECK_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil {
			interval = d
		}
	}

	// Build provider list. Supports adding a self-hosted whoami endpoint.
	providers := ipcheck.DefaultProviders
	if extra := os.Getenv("EXTRA_IP_PROVIDERS"); extra != "" {
		for _, url := range strings.Split(extra, ",") {
			url = strings.TrimSpace(url)
			if url != "" {
				providers = append([]ipcheck.Provider{{Name: "self-hosted", URL: url}}, providers...)
			}
		}
	}

	quorum := 2

	// In-cluster Kubernetes client.
	config, err := rest.InClusterConfig()
	if err != nil {
		logger.Error("building k8s config", "error", err)
		os.Exit(1)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		logger.Error("building k8s client", "error", err)
		os.Exit(1)
	}

	s := store.New(clientset, namespace)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	logger.Info("agent starting",
		"node", nodeName,
		"namespace", namespace,
		"interval", interval,
		"providers", fmt.Sprintf("%d", len(providers)),
	)

	// Run immediately, then on a ticker.
	run(ctx, logger, s, providers, quorum, nodeName)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			// Best-effort removal so stale IPs don't linger.
			rmCtx, rmCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := s.RemoveNode(rmCtx, nodeName); err != nil {
				logger.Warn("failed to remove node entry on shutdown", "error", err)
			}
			rmCancel()
			return
		case <-ticker.C:
			run(ctx, logger, s, providers, quorum, nodeName)
		}
	}
}

func run(ctx context.Context, logger *slog.Logger, s *store.Store, providers []ipcheck.Provider, quorum int, nodeName string) {
	ip, err := ipcheck.Discover(ctx, providers, quorum)
	if err != nil {
		// Discover returns best-effort IP even on quorum failure.
		logger.Warn("ip discovery issue", "error", err)
	}
	if ip == nil {
		logger.Error("could not determine public IP")
		return
	}

	logger.Info("discovered public IP", "node", nodeName, "ip", ip.String())

	if err := s.SetNodeIP(ctx, nodeName, ip); err != nil {
		logger.Error("failed to store IP", "error", err)
	}
}
