package store

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const maxConflictRetries = 5

const (
	ConfigMapName = "node-public-ips"
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "k8s-dns-controller"
)

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

// SetNodeIP writes or updates the public IP for a given node.
// It retries on conflict since multiple agents update the same ConfigMap.
func (s *Store) SetNodeIP(ctx context.Context, nodeName string, ip net.IP) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for attempt := range maxConflictRetries {
		cm, err := s.getOrCreate(ctx)
		if err != nil {
			return err
		}

		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}

		if cm.Data[nodeName] == ip.String() {
			return nil
		}

		cm.Data[nodeName] = ip.String()

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

// GetAllIPs returns the current set of node→IP mappings.
func (s *Store) GetAllIPs(ctx context.Context) (map[string]net.IP, error) {
	cm, err := s.getOrCreate(ctx)
	if err != nil {
		return nil, err
	}

	result := make(map[string]net.IP, len(cm.Data))
	for node, ipStr := range cm.Data {
		ip := net.ParseIP(ipStr)
		if ip != nil {
			result[node] = ip
		}
	}
	return result, nil
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
