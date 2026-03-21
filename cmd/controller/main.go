package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
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
	)

	// Reconcile immediately, then on a ticker.
	reconcile(ctx, logger, s, reconciler, nodeSelector)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			return
		case <-ticker.C:
			reconcile(ctx, logger, s, reconciler, nodeSelector)
		}
	}
}

func reconcile(ctx context.Context, logger *slog.Logger, s *store.Store, r *dns.Reconciler, sel labels.Selector) {
	ips, err := s.GetFilteredIPs(ctx, sel)
	if err != nil {
		logger.Error("reading IP store", "error", err)
		return
	}

	logger.Info("current node IPs", "count", len(ips))

	changed, err := r.Reconcile(ctx, ips)
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
