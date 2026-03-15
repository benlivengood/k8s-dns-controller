package store

import (
	"context"
	"fmt"
	"net"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

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
func (s *Store) SetNodeIP(ctx context.Context, nodeName string, ip net.IP) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cm, err := s.getOrCreate(ctx)
	if err != nil {
		return err
	}

	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}

	current := cm.Data[nodeName]
	if current == ip.String() {
		return nil // no change
	}

	cm.Data[nodeName] = ip.String()

	_, err = s.client.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("updating configmap: %w", err)
	}
	return nil
}

// RemoveNode removes a node's entry from the store.
func (s *Store) RemoveNode(ctx context.Context, nodeName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cm, err := s.getOrCreate(ctx)
	if err != nil {
		return err
	}

	if _, ok := cm.Data[nodeName]; !ok {
		return nil
	}

	delete(cm.Data, nodeName)

	_, err = s.client.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
	return err
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

	// Create if not found.
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
	if err != nil {
		return nil, fmt.Errorf("creating configmap: %w", err)
	}
	return cm, nil
}
