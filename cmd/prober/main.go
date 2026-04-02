package main

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/yourorg/k8s-dns-controller/pkg/health"
	"github.com/yourorg/k8s-dns-controller/pkg/store"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const zoneLabel = "topology.kubernetes.io/zone"

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

	interval := 30 * time.Second
	if v := os.Getenv("PROBE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			interval = d
		}
	}

	port := 443
	if v := os.Getenv("PROBE_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			port = p
		}
	}

	timeout := 5 * time.Second
	if v := os.Getenv("PROBE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			timeout = d
		}
	}

	serverName := os.Getenv("PROBE_TLS_SERVERNAME")

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

	node, err := clientset.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if err != nil {
		logger.Error("fetching node", "error", err)
		os.Exit(1)
	}
	zone := node.Labels[zoneLabel]
	if zone == "" {
		logger.Error("node has no zone label", "label", zoneLabel, "node", nodeName)
		os.Exit(1)
	}

	ipStore := store.New(clientset, namespace)
	healthStore := store.NewHealthStore(clientset, namespace)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	prober := &health.Prober{Port: port, Timeout: timeout, ServerName: serverName}

	logger.Info("prober starting",
		"node", nodeName,
		"zone", zone,
		"interval", interval,
		"port", port,
		"timeout", timeout,
	)

	runProbe(ctx, logger, ipStore, healthStore, prober, zone)

	for {
		jitter := interval / 4
		sleep := interval - jitter + time.Duration(rand.Int64N(int64(2*jitter)))

		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			return
		case <-time.After(sleep):
			runProbe(ctx, logger, ipStore, healthStore, prober, zone)
		}
	}
}

func runProbe(ctx context.Context, logger *slog.Logger, ipStore *store.Store, healthStore *store.HealthStore, prober *health.Prober, zone string) {
	nodes, err := ipStore.GetNodes(ctx)
	if err != nil {
		logger.Error("reading node IPs", "error", err)
		return
	}

	ips := make(map[string]string, len(nodes))
	for name, entry := range nodes {
		ips[name] = entry.IP
	}

	if len(ips) == 0 {
		logger.Warn("no node IPs to probe")
		return
	}

	results := prober.CheckAll(ctx, ips)

	healthy := 0
	for _, ok := range results {
		if ok {
			healthy++
		}
	}
	logger.Info("probe complete", "zone", zone, "total", len(results), "healthy", healthy)

	if err := healthStore.SetZoneResults(ctx, zone, results); err != nil {
		logger.Error("failed to store health results", "error", err)
	}
}
