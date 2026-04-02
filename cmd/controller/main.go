package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/route53"

	"github.com/yourorg/k8s-dns-controller/pkg/dns"
	"github.com/yourorg/k8s-dns-controller/pkg/store"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	hostedZoneID := requireEnv(logger, "HOSTED_ZONE_ID")
	recordNames := strings.Split(requireEnv(logger, "RECORD_NAMES"), ",")

	namespace := os.Getenv("NAMESPACE")
	if namespace == "" {
		namespace = "kube-system"
	}

	var nodeSelector labels.Selector
	if v := os.Getenv("NODE_SELECTOR"); v != "" {
		var err error
		nodeSelector, err = labels.Parse(v)
		if err != nil {
			logger.Error("invalid NODE_SELECTOR", "value", v, "error", err)
			os.Exit(1)
		}
	}

	ttl := int64(60)
	if v := os.Getenv("DNS_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			ttl = int64(d.Seconds())
		}
	}

	interval := 30 * time.Second
	if v := os.Getenv("RECONCILE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			interval = d
		}
	}

	var healthMaxAge time.Duration
	if v := os.Getenv("HEALTH_MAX_AGE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			healthMaxAge = d
		} else {
			logger.Error("invalid HEALTH_MAX_AGE", "value", v, "error", err)
			os.Exit(1)
		}
	}

	healthThreshold := 0.5
	if v := os.Getenv("HEALTH_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			healthThreshold = f
		} else {
			logger.Error("invalid HEALTH_THRESHOLD", "value", v, "error", err)
			os.Exit(1)
		}
	}

	// Kubernetes client.
	k8sConfig, err := rest.InClusterConfig()
	if err != nil {
		logger.Error("building k8s config", "error", err)
		os.Exit(1)
	}
	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		logger.Error("building k8s client", "error", err)
		os.Exit(1)
	}

	s := store.New(clientset, namespace)

	var hs *store.HealthStore
	if healthMaxAge > 0 {
		hs = store.NewHealthStore(clientset, namespace)
	}

	// AWS client — uses IRSA, env creds, or instance profile automatically.
	awsCfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		logger.Error("loading AWS config", "error", err)
		os.Exit(1)
	}
	r53Client := route53.NewFromConfig(awsCfg)

	reconciler := dns.NewReconciler(r53Client, dns.Config{
		HostedZoneID: hostedZoneID,
		RecordNames:  trimAll(recordNames),
		TTL:          ttl,
	}, logger)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	logger.Info("controller starting",
		"hosted_zone", hostedZoneID,
		"records", recordNames,
		"ttl", ttl,
		"interval", interval,
		"node_selector", nodeSelector,
		"health_max_age", healthMaxAge,
		"health_threshold", healthThreshold,
	)

	rc := &reconcileConfig{
		store:           s,
		healthStore:     hs,
		reconciler:      reconciler,
		nodeSelector:    nodeSelector,
		healthMaxAge:    healthMaxAge,
		healthThreshold: healthThreshold,
	}

	// Reconcile immediately, then on a ticker.
	reconcile(ctx, logger, rc)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			return
		case <-ticker.C:
			reconcile(ctx, logger, rc)
		}
	}
}

type reconcileConfig struct {
	store           *store.Store
	healthStore     *store.HealthStore
	reconciler      *dns.Reconciler
	nodeSelector    labels.Selector
	healthMaxAge    time.Duration
	healthThreshold float64
}

func reconcile(ctx context.Context, logger *slog.Logger, rc *reconcileConfig) {
	ips, err := rc.store.GetFilteredIPs(ctx, rc.nodeSelector)
	if err != nil {
		logger.Error("reading IP store", "error", err)
		return
	}

	logger.Info("current node IPs", "count", len(ips))

	if rc.healthStore != nil {
		allHealth, err := rc.healthStore.GetAllHealth(ctx)
		if err != nil {
			logger.Error("reading health store", "error", err)
			return
		}
		before := len(ips)
		ips = store.ApplyHealthFilter(ips, allHealth, rc.healthMaxAge, rc.healthThreshold)
		if len(ips) != before {
			logger.Info("health filter applied", "before", before, "after", len(ips))
		}
	}

	changed, err := rc.reconciler.Reconcile(ctx, ips)
	if err != nil {
		logger.Error("reconciliation failed", "error", err)
		return
	}
	if changed {
		logger.Info("DNS records updated")
	}
}

func requireEnv(logger *slog.Logger, key string) string {
	v := os.Getenv(key)
	if v == "" {
		logger.Error("required environment variable not set", "key", key)
		os.Exit(1)
	}
	return v
}

func trimAll(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
