package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const maxConflictRetries = 5

const (
	ConfigMapName  = "node-public-ips"
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "k8s-dns-controller"
)

// NodeEntry is the per-node value stored as JSON in the ConfigMap.
type NodeEntry struct {
	IP     string            `json:"ip"`
	Labels map[string]string `json:"labels,omitempty"`
}

// Store manages node→IP mappings backed by a Kubernetes ConfigMap.
type Store struct {
	client    kubernetes.Interface
	namespace string
	mu        sync.Mutex
}

func New(client kubernetes.Interface, namespace string) *Store {
	return &Store{
		client:    client,
		namespace: namespace,
	}
}

// SetNode writes or updates the public IP and labels for a given node.
// It retries on conflict since multiple agents update the same ConfigMap.
func (s *Store) SetNode(ctx context.Context, nodeName string, ip net.IP, nodeLabels map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry := NodeEntry{IP: ip.String(), Labels: nodeLabels}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshaling node entry: %w", err)
	}
	value := string(encoded)

	for attempt := range maxConflictRetries {
		cm, err := s.getOrCreate(ctx)
		if err != nil {
			return err
		}

		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}

		if cm.Data[nodeName] == value {
			return nil
		}

		cm.Data[nodeName] = value

		_, err = s.client.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return fmt.Errorf("updating configmap: %w", err)
		}

		if err := backoff(ctx, attempt); err != nil {
			return fmt.Errorf("updating configmap: %w", err)
		}
	}

	return fmt.Errorf("updating configmap: conflict after %d retries", maxConflictRetries)
}

// GetNodes returns all node entries from the ConfigMap.
func (s *Store) GetNodes(ctx context.Context) (map[string]NodeEntry, error) {
	cm, err := s.getOrCreate(ctx)
	if err != nil {
		return nil, err
	}

	result := make(map[string]NodeEntry, len(cm.Data))
	for node, raw := range cm.Data {
		entry, err := parseEntry(raw)
		if err != nil {
			continue
		}
		result[node] = entry
	}
	return result, nil
}

// GetFilteredIPs returns IPs for nodes whose labels match the selector.
// A nil/empty selector matches all nodes.
func (s *Store) GetFilteredIPs(ctx context.Context, sel labels.Selector) (map[string]net.IP, error) {
	nodes, err := s.GetNodes(ctx)
	if err != nil {
		return nil, err
	}

	result := make(map[string]net.IP, len(nodes))
	for name, entry := range nodes {
		if sel != nil && !sel.Empty() && !sel.Matches(labels.Set(entry.Labels)) {
			continue
		}
		ip := net.ParseIP(entry.IP)
		if ip != nil {
			result[name] = ip
		}
	}
	return result, nil
}

// parseEntry handles both the current JSON format and the legacy plain-IP format.
func parseEntry(raw string) (NodeEntry, error) {
	var entry NodeEntry
	if err := json.Unmarshal([]byte(raw), &entry); err == nil && entry.IP != "" {
		return entry, nil
	}
	if ip := net.ParseIP(raw); ip != nil {
		return NodeEntry{IP: raw}, nil
	}
	return NodeEntry{}, fmt.Errorf("unrecognized entry: %q", raw)
}

func (s *Store) getOrCreate(ctx context.Context) (*corev1.ConfigMap, error) {
	cm, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, ConfigMapName, metav1.GetOptions{})
	if err == nil {
		return cm, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("getting configmap: %w", err)
	}

	cm = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigMapName,
			Namespace: s.namespace,
			Labels: map[string]string{
				ManagedByLabel: ManagedByValue,
			},
		},
		Data: make(map[string]string),
	}
	cm, err = s.client.CoreV1().ConfigMaps(s.namespace).Create(ctx, cm, metav1.CreateOptions{})
	if err == nil {
		return cm, nil
	}

	// Another agent may have created it between our Get and Create.
	if apierrors.IsAlreadyExists(err) {
		return s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, ConfigMapName, metav1.GetOptions{})
	}
	return nil, fmt.Errorf("creating configmap: %w", err)
}

// backoff sleeps for an exponentially increasing duration with jitter.
// Base delay doubles each attempt: 50ms, 100ms, 200ms, 400ms, ...
func backoff(ctx context.Context, attempt int) error {
	base := 50 * time.Millisecond
	for range attempt {
		base *= 2
	}
	jitter := time.Duration(rand.Int64N(int64(base)))
	delay := base + jitter

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		return nil
	}
}
