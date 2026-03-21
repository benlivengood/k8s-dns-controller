package store

import (
	"context"
	"encoding/json"
	"net"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes/fake"
)

func seedConfigMap(t *testing.T, s *Store, data map[string]string) {
	t.Helper()
	ctx := context.Background()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigMapName,
			Namespace: s.namespace,
		},
		Data: data,
	}
	_, err := s.client.CoreV1().ConfigMaps(s.namespace).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, entry NodeEntry) string {
	t.Helper()
	b, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustSelector(t *testing.T, expr string) labels.Selector {
	t.Helper()
	sel, err := labels.Parse(expr)
	if err != nil {
		t.Fatalf("bad selector %q: %v", expr, err)
	}
	return sel
}

func TestParseEntry_JSON(t *testing.T) {
	raw := `{"ip":"10.0.0.1","labels":{"role":"worker"}}`
	entry, err := parseEntry(raw)
	if err != nil {
		t.Fatal(err)
	}
	if entry.IP != "10.0.0.1" {
		t.Errorf("IP = %q, want 10.0.0.1", entry.IP)
	}
	if entry.Labels["role"] != "worker" {
		t.Errorf("Labels[role] = %q, want worker", entry.Labels["role"])
	}
}

func TestParseEntry_LegacyPlainIP(t *testing.T) {
	entry, err := parseEntry("203.0.113.5")
	if err != nil {
		t.Fatal(err)
	}
	if entry.IP != "203.0.113.5" {
		t.Errorf("IP = %q, want 203.0.113.5", entry.IP)
	}
	if entry.Labels != nil {
		t.Errorf("Labels = %v, want nil", entry.Labels)
	}
}

func TestParseEntry_Garbage(t *testing.T) {
	_, err := parseEntry("not-an-ip-or-json")
	if err == nil {
		t.Fatal("expected error for garbage input")
	}
}

func TestGetFilteredIPs_NoSelector(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := New(client, "default")

	seedConfigMap(t, s, map[string]string{
		"node-a": mustJSON(t, NodeEntry{IP: "1.1.1.1", Labels: map[string]string{"role": "cp"}}),
		"node-b": mustJSON(t, NodeEntry{IP: "2.2.2.2", Labels: map[string]string{"role": "worker"}}),
	})

	ips, err := s.GetFilteredIPs(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 2 {
		t.Fatalf("got %d IPs, want 2", len(ips))
	}
}

func TestGetFilteredIPs_EmptySelector(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := New(client, "default")

	seedConfigMap(t, s, map[string]string{
		"node-a": mustJSON(t, NodeEntry{IP: "1.1.1.1", Labels: map[string]string{"role": "cp"}}),
		"node-b": mustJSON(t, NodeEntry{IP: "2.2.2.2", Labels: map[string]string{"role": "worker"}}),
	})

	sel := mustSelector(t, "")
	ips, err := s.GetFilteredIPs(context.Background(), sel)
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 2 {
		t.Fatalf("got %d IPs, want 2", len(ips))
	}
}

func TestGetFilteredIPs_MatchByLabel(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := New(client, "default")

	seedConfigMap(t, s, map[string]string{
		"cp-1":     mustJSON(t, NodeEntry{IP: "10.0.0.1", Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}}),
		"cp-2":     mustJSON(t, NodeEntry{IP: "10.0.0.2", Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}}),
		"worker-1": mustJSON(t, NodeEntry{IP: "10.0.1.1", Labels: map[string]string{"node-role.kubernetes.io/worker": ""}}),
		"worker-2": mustJSON(t, NodeEntry{IP: "10.0.1.2", Labels: map[string]string{"node-role.kubernetes.io/worker": ""}}),
	})

	sel := mustSelector(t, "node-role.kubernetes.io/control-plane=")
	ips, err := s.GetFilteredIPs(context.Background(), sel)
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 2 {
		t.Fatalf("got %d IPs, want 2", len(ips))
	}
	for name := range ips {
		if name != "cp-1" && name != "cp-2" {
			t.Errorf("unexpected node %q in results", name)
		}
	}
}

func TestGetFilteredIPs_ExcludeByLabel(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := New(client, "default")

	seedConfigMap(t, s, map[string]string{
		"cp-1":     mustJSON(t, NodeEntry{IP: "10.0.0.1", Labels: map[string]string{"node-role.kubernetes.io/control-plane": "", "os": "linux"}}),
		"worker-1": mustJSON(t, NodeEntry{IP: "10.0.1.1", Labels: map[string]string{"os": "linux"}}),
	})

	sel := mustSelector(t, "!node-role.kubernetes.io/control-plane")
	ips, err := s.GetFilteredIPs(context.Background(), sel)
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 1 {
		t.Fatalf("got %d IPs, want 1", len(ips))
	}
	if _, ok := ips["worker-1"]; !ok {
		t.Error("expected worker-1 in results")
	}
}

func TestGetFilteredIPs_MultipleRequirements(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := New(client, "default")

	seedConfigMap(t, s, map[string]string{
		"node-a": mustJSON(t, NodeEntry{IP: "1.1.1.1", Labels: map[string]string{"env": "prod", "zone": "us-east-1a"}}),
		"node-b": mustJSON(t, NodeEntry{IP: "2.2.2.2", Labels: map[string]string{"env": "prod", "zone": "us-west-2a"}}),
		"node-c": mustJSON(t, NodeEntry{IP: "3.3.3.3", Labels: map[string]string{"env": "staging", "zone": "us-east-1a"}}),
	})

	sel := mustSelector(t, "env=prod,zone=us-east-1a")
	ips, err := s.GetFilteredIPs(context.Background(), sel)
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 1 {
		t.Fatalf("got %d IPs, want 1", len(ips))
	}
	if ips["node-a"].String() != "1.1.1.1" {
		t.Errorf("expected node-a=1.1.1.1, got %v", ips["node-a"])
	}
}

func TestGetFilteredIPs_NoMatches(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := New(client, "default")

	seedConfigMap(t, s, map[string]string{
		"node-a": mustJSON(t, NodeEntry{IP: "1.1.1.1", Labels: map[string]string{"env": "staging"}}),
	})

	sel := mustSelector(t, "env=prod")
	ips, err := s.GetFilteredIPs(context.Background(), sel)
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 0 {
		t.Fatalf("got %d IPs, want 0", len(ips))
	}
}

func TestGetFilteredIPs_LegacyEntriesMatchAllSelectors(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := New(client, "default")

	seedConfigMap(t, s, map[string]string{
		"old-node": "192.168.1.1",
		"new-node": mustJSON(t, NodeEntry{IP: "10.0.0.1", Labels: map[string]string{"role": "worker"}}),
	})

	// With no selector, legacy entries are included.
	ips, err := s.GetFilteredIPs(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 2 {
		t.Fatalf("no selector: got %d IPs, want 2", len(ips))
	}

	// With a selector, legacy entries (nil labels) don't match.
	sel := mustSelector(t, "role=worker")
	ips, err = s.GetFilteredIPs(context.Background(), sel)
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 1 {
		t.Fatalf("with selector: got %d IPs, want 1", len(ips))
	}
	if _, ok := ips["new-node"]; !ok {
		t.Error("expected new-node in results")
	}
}

func TestSetNode_RoundTrip(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := New(client, "default")

	ctx := context.Background()
	nodeLabels := map[string]string{
		"node-role.kubernetes.io/control-plane": "",
		"topology.kubernetes.io/zone":           "us-east-1a",
	}

	if err := s.SetNode(ctx, "my-node", net.ParseIP("1.2.3.4"), nodeLabels); err != nil {
		t.Fatal(err)
	}

	nodes, err := s.GetNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := nodes["my-node"]
	if !ok {
		t.Fatal("my-node not found")
	}
	if entry.IP != "1.2.3.4" {
		t.Errorf("IP = %q, want 1.2.3.4", entry.IP)
	}
	if entry.Labels["topology.kubernetes.io/zone"] != "us-east-1a" {
		t.Errorf("zone label = %q, want us-east-1a", entry.Labels["topology.kubernetes.io/zone"])
	}

	sel := mustSelector(t, "node-role.kubernetes.io/control-plane=")
	ips, err := s.GetFilteredIPs(ctx, sel)
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 1 || ips["my-node"].String() != "1.2.3.4" {
		t.Errorf("filtered IPs = %v, want {my-node: 1.2.3.4}", ips)
	}
}
