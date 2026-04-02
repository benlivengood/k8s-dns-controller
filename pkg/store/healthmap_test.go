package store

import (
	"context"
	"net"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

func TestHealthStore_RoundTrip(t *testing.T) {
	client := fake.NewSimpleClientset()
	hs := NewHealthStore(client, "default")
	ctx := context.Background()

	results := map[string]bool{
		"node-a": true,
		"node-b": false,
		"node-c": true,
	}
	if err := hs.SetZoneResults(ctx, "us-east-1a", results); err != nil {
		t.Fatal(err)
	}

	health, err := hs.GetAllHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}

	zone, ok := health["us-east-1a"]
	if !ok {
		t.Fatal("expected us-east-1a zone data")
	}

	if _, ok := zone["node-a"]; !ok {
		t.Error("expected node-a timestamp (was successful)")
	}
	if _, ok := zone["node-b"]; ok {
		t.Error("expected no node-b timestamp (was unsuccessful)")
	}
	if _, ok := zone["node-c"]; !ok {
		t.Error("expected node-c timestamp (was successful)")
	}
}

func TestHealthStore_MergesWithExisting(t *testing.T) {
	client := fake.NewSimpleClientset()
	hs := NewHealthStore(client, "default")
	ctx := context.Background()

	// First probe: node-a and node-b succeed.
	if err := hs.SetZoneResults(ctx, "us-east-1a", map[string]bool{
		"node-a": true,
		"node-b": true,
	}); err != nil {
		t.Fatal(err)
	}

	// Second probe: node-a succeeds, node-b fails.
	if err := hs.SetZoneResults(ctx, "us-east-1a", map[string]bool{
		"node-a": true,
		"node-b": false,
	}); err != nil {
		t.Fatal(err)
	}

	health, err := hs.GetAllHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}

	zone := health["us-east-1a"]
	if _, ok := zone["node-a"]; !ok {
		t.Error("expected node-a to have timestamp")
	}
	// node-b should still have its old timestamp (not cleared on failure).
	if _, ok := zone["node-b"]; !ok {
		t.Error("expected node-b to retain previous success timestamp")
	}
}

func TestHealthStore_MultipleZones(t *testing.T) {
	client := fake.NewSimpleClientset()
	hs := NewHealthStore(client, "default")
	ctx := context.Background()

	if err := hs.SetZoneResults(ctx, "us-east-1a", map[string]bool{"node-a": true}); err != nil {
		t.Fatal(err)
	}
	if err := hs.SetZoneResults(ctx, "us-west-2a", map[string]bool{"node-a": true, "node-b": true}); err != nil {
		t.Fatal(err)
	}

	health, err := hs.GetAllHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(health) != 2 {
		t.Fatalf("expected 2 zones, got %d", len(health))
	}
	if len(health["us-east-1a"]) != 1 {
		t.Errorf("us-east-1a: expected 1 entry, got %d", len(health["us-east-1a"]))
	}
	if len(health["us-west-2a"]) != 2 {
		t.Errorf("us-west-2a: expected 2 entries, got %d", len(health["us-west-2a"]))
	}
}

func TestApplyHealthFilter_NoHealthData(t *testing.T) {
	ips := map[string]net.IP{
		"node-a": net.ParseIP("1.1.1.1"),
		"node-b": net.ParseIP("2.2.2.2"),
	}

	result := ApplyHealthFilter(ips, nil, 5*time.Minute, 0.5)
	if len(result) != 2 {
		t.Fatalf("expected all IPs to pass with no health data, got %d", len(result))
	}
}

func TestApplyHealthFilter_EmptyHealthData(t *testing.T) {
	ips := map[string]net.IP{
		"node-a": net.ParseIP("1.1.1.1"),
	}

	result := ApplyHealthFilter(ips, map[string]map[string]time.Time{}, 5*time.Minute, 0.5)
	if len(result) != 1 {
		t.Fatalf("expected all IPs to pass with empty health data, got %d", len(result))
	}
}

func TestApplyHealthFilter_AllHealthy(t *testing.T) {
	now := time.Now()
	ips := map[string]net.IP{
		"node-a": net.ParseIP("1.1.1.1"),
		"node-b": net.ParseIP("2.2.2.2"),
	}
	health := map[string]map[string]time.Time{
		"us-east-1a": {"node-a": now, "node-b": now},
		"us-west-2a": {"node-a": now, "node-b": now},
	}

	result := ApplyHealthFilter(ips, health, 5*time.Minute, 0.5)
	if len(result) != 2 {
		t.Fatalf("expected 2 IPs, got %d", len(result))
	}
}

func TestApplyHealthFilter_OneNodeStale(t *testing.T) {
	now := time.Now()
	stale := now.Add(-10 * time.Minute)
	ips := map[string]net.IP{
		"node-a": net.ParseIP("1.1.1.1"),
		"node-b": net.ParseIP("2.2.2.2"),
	}
	health := map[string]map[string]time.Time{
		"us-east-1a": {"node-a": now, "node-b": stale},
		"us-west-2a": {"node-a": now, "node-b": stale},
	}

	result := ApplyHealthFilter(ips, health, 5*time.Minute, 0.5)
	if len(result) != 1 {
		t.Fatalf("expected 1 IP (node-b stale), got %d", len(result))
	}
	if _, ok := result["node-a"]; !ok {
		t.Error("expected node-a to pass")
	}
}

func TestApplyHealthFilter_ThresholdPartialZones(t *testing.T) {
	now := time.Now()
	stale := now.Add(-10 * time.Minute)
	ips := map[string]net.IP{
		"node-a": net.ParseIP("1.1.1.1"),
	}
	// 2 of 3 zones report fresh data -- meets 0.5 threshold.
	health := map[string]map[string]time.Time{
		"zone-1": {"node-a": now},
		"zone-2": {"node-a": now},
		"zone-3": {"node-a": stale},
	}

	result := ApplyHealthFilter(ips, health, 5*time.Minute, 0.5)
	if len(result) != 1 {
		t.Fatalf("expected node-a to pass (2/3 >= 0.5), got %d", len(result))
	}
}

func TestApplyHealthFilter_ThresholdNotMet(t *testing.T) {
	now := time.Now()
	stale := now.Add(-10 * time.Minute)
	ips := map[string]net.IP{
		"node-a": net.ParseIP("1.1.1.1"),
	}
	// Only 1 of 3 zones report fresh data -- fails 0.5 threshold.
	health := map[string]map[string]time.Time{
		"zone-1": {"node-a": now},
		"zone-2": {"node-a": stale},
		"zone-3": {"node-a": stale},
	}

	result := ApplyHealthFilter(ips, health, 5*time.Minute, 0.5)
	if len(result) != 0 {
		t.Fatalf("expected node-a to fail (1/3 < 0.5), got %d", len(result))
	}
}

func TestApplyHealthFilter_NodeMissingFromZone(t *testing.T) {
	now := time.Now()
	ips := map[string]net.IP{
		"node-a": net.ParseIP("1.1.1.1"),
		"node-b": net.ParseIP("2.2.2.2"),
	}
	// node-b only reported by 1 of 2 zones.
	health := map[string]map[string]time.Time{
		"zone-1": {"node-a": now, "node-b": now},
		"zone-2": {"node-a": now},
	}

	result := ApplyHealthFilter(ips, health, 5*time.Minute, 0.5)
	if len(result) != 2 {
		t.Fatalf("expected 2 IPs (1/2 >= 0.5), got %d", len(result))
	}

	// With threshold=1.0, node-b should fail.
	result = ApplyHealthFilter(ips, health, 5*time.Minute, 1.0)
	if len(result) != 1 {
		t.Fatalf("expected 1 IP with threshold=1.0, got %d", len(result))
	}
	if _, ok := result["node-a"]; !ok {
		t.Error("expected node-a to pass")
	}
}
