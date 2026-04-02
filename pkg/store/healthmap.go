package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const HealthConfigMapName = "node-health-probes"

// HealthStore manages zone→{node→last-success} mappings in a ConfigMap.
type HealthStore struct {
	client    kubernetes.Interface
	namespace string
	mu        sync.Mutex
}

func NewHealthStore(client kubernetes.Interface, namespace string) *HealthStore {
	return &HealthStore{
		client:    client,
		namespace: namespace,
	}
}

// SetZoneResults writes the probe results for a zone. It merges with existing
// data: successful probes update the timestamp, failed probes leave the
// previous timestamp untouched so it ages out naturally.
func (h *HealthStore) SetZoneResults(ctx context.Context, zone string, successes map[string]bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now().UTC()

	for attempt := range maxConflictRetries {
		cm, err := h.getOrCreate(ctx)
		if err != nil {
			return err
		}

		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}

		existing := make(map[string]time.Time)
		if raw, ok := cm.Data[zone]; ok {
			_ = json.Unmarshal([]byte(raw), &existing)
		}

		for node, ok := range successes {
			if ok {
				existing[node] = now
			}
			// On failure, leave existing timestamp (if any) so it ages out.
		}

		encoded, err := json.Marshal(existing)
		if err != nil {
			return fmt.Errorf("marshaling health results: %w", err)
		}

		cm.Data[zone] = string(encoded)

		_, err = h.client.CoreV1().ConfigMaps(h.namespace).Update(ctx, cm, metav1.UpdateOptions{})
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return fmt.Errorf("updating health configmap: %w", err)
		}

		if err := backoff(ctx, attempt); err != nil {
			return fmt.Errorf("updating health configmap: %w", err)
		}
	}

	return fmt.Errorf("updating health configmap: conflict after %d retries", maxConflictRetries)
}

// GetAllHealth reads all zone health data.
// Returns zone -> node -> last-success-timestamp.
func (h *HealthStore) GetAllHealth(ctx context.Context) (map[string]map[string]time.Time, error) {
	cm, err := h.getOrCreate(ctx)
	if err != nil {
		return nil, err
	}

	result := make(map[string]map[string]time.Time, len(cm.Data))
	for zone, raw := range cm.Data {
		var timestamps map[string]time.Time
		if err := json.Unmarshal([]byte(raw), &timestamps); err != nil {
			continue
		}
		result[zone] = timestamps
	}
	return result, nil
}

// ApplyHealthFilter removes IPs that don't have fresh-enough health data
// from enough zones. If no health data exists at all, all IPs pass through.
func ApplyHealthFilter(ips map[string]net.IP, health map[string]map[string]time.Time, maxAge time.Duration, threshold float64) map[string]net.IP {
	totalZones := len(health)
	if totalZones == 0 {
		return ips
	}

	now := time.Now()
	result := make(map[string]net.IP, len(ips))
	for node, ip := range ips {
		freshCount := 0
		for _, zoneData := range health {
			if ts, ok := zoneData[node]; ok && now.Sub(ts) <= maxAge {
				freshCount++
			}
		}
		if float64(freshCount)/float64(totalZones) >= threshold {
			result[node] = ip
		}
	}
	return result
}

func (h *HealthStore) getOrCreate(ctx context.Context) (*corev1.ConfigMap, error) {
	cm, err := h.client.CoreV1().ConfigMaps(h.namespace).Get(ctx, HealthConfigMapName, metav1.GetOptions{})
	if err == nil {
		return cm, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("getting health configmap: %w", err)
	}

	cm = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      HealthConfigMapName,
			Namespace: h.namespace,
			Labels: map[string]string{
				ManagedByLabel: ManagedByValue,
			},
		},
		Data: make(map[string]string),
	}
	cm, err = h.client.CoreV1().ConfigMaps(h.namespace).Create(ctx, cm, metav1.CreateOptions{})
	if err == nil {
		return cm, nil
	}

	if apierrors.IsAlreadyExists(err) {
		return h.client.CoreV1().ConfigMaps(h.namespace).Get(ctx, HealthConfigMapName, metav1.GetOptions{})
	}
	return nil, fmt.Errorf("creating health configmap: %w", err)
}
