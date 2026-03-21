package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yourorg/k8s-dns-controller/pkg/ipcheck"
	"github.com/yourorg/k8s-dns-controller/pkg/store"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	interval := 5 * time.Minute
	if v := os.Getenv("CHECK_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil {
			interval = d
		}
	}

	providers := ipcheck.DefaultProviders
	if extra := os.Getenv("EXTRA_IP_PROVIDERS"); extra != "" {
		var custom []ipcheck.Provider
		for i, url := range strings.Split(extra, ",") {
			url = strings.TrimSpace(url)
			if url != "" {
				custom = append(custom, ipcheck.Provider{
					Name: fmt.Sprintf("custom-%d", i),
					URL:  url,
				})
			}
		}
		providers = append(custom, providers...)
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

	// Stagger the first check so agents started simultaneously don't all
	// hit the IP providers and ConfigMap at the same instant.
	initialJitter := time.Duration(rand.Int64N(int64(interval / 2)))
	logger.Info("waiting before first check", "jitter", initialJitter)
	select {
	case <-ctx.Done():
	case <-time.After(initialJitter):
	}

	run(ctx, logger, clientset, s, providers, quorum, nodeName)

	for {
		// ±25% jitter around the configured interval to avoid thundering herd.
		jitter := interval / 4
		sleep := interval - jitter + time.Duration(rand.Int64N(int64(2*jitter)))

		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			return
		case <-time.After(sleep):
			run(ctx, logger, clientset, s, providers, quorum, nodeName)
		}
	}
}

func run(ctx context.Context, logger *slog.Logger, clientset kubernetes.Interface, s *store.Store, providers []ipcheck.Provider, quorum int, nodeName string) {
	ip, err := ipcheck.Discover(ctx, providers, quorum)
	if err != nil {
		logger.Warn("ip discovery issue", "error", err)
	}
	if ip == nil {
		logger.Error("could not determine public IP")
		return
	}

	node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		logger.Error("failed to get node labels", "error", err)
		return
	}

	logger.Info("discovered public IP", "node", nodeName, "ip", ip.String())

	if err := s.SetNode(ctx, nodeName, ip, node.Labels); err != nil {
		logger.Error("failed to store node entry", "error", err)
	}
}
