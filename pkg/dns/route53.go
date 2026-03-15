package dns

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"
)

// Config holds the Route53 reconciler configuration.
type Config struct {
	HostedZoneID string   // e.g. "Z1234567890"
	RecordNames  []string // e.g. ["*.k8s.example.com", "ingress.example.com"]
	TTL          int64    // default 60
}

// Reconciler keeps Route53 A records in sync with the desired set of IPs.
type Reconciler struct {
	client *route53.Client
	config Config
	logger *slog.Logger
}

func NewReconciler(client *route53.Client, config Config, logger *slog.Logger) *Reconciler {
	if config.TTL == 0 {
		config.TTL = 60
	}
	return &Reconciler{
		client: client,
		config: config,
		logger: logger,
	}
}

// Reconcile compares the current Route53 state with the desired IPs and
// applies changes if needed. Returns true if any changes were made.
func (r *Reconciler) Reconcile(ctx context.Context, desiredIPs map[string]net.IP) (bool, error) {
	if len(desiredIPs) == 0 {
		r.logger.Warn("no IPs to reconcile — skipping (will not remove all records as a safety measure)")
		return false, nil
	}

	ips := uniqueSortedIPs(desiredIPs)
	r.logger.Info("reconciling", "desired_ips", ips, "records", r.config.RecordNames)

	var changes []r53types.Change
	for _, name := range r.config.RecordNames {
		fqdn := ensureTrailingDot(name)

		current, err := r.getCurrentIPs(ctx, fqdn)
		if err != nil {
			return false, fmt.Errorf("fetching current records for %s: %w", name, err)
		}

		if ipsEqual(current, ips) {
			r.logger.Info("no change needed", "record", name)
			continue
		}

		r.logger.Info("change detected",
			"record", name,
			"current", current,
			"desired", ips,
		)

		var records []r53types.ResourceRecord
		for _, ip := range ips {
			records = append(records, r53types.ResourceRecord{
				Value: aws.String(ip),
			})
		}

		changes = append(changes, r53types.Change{
			Action: r53types.ChangeActionUpsert,
			ResourceRecordSet: &r53types.ResourceRecordSet{
				Name:            aws.String(fqdn),
				Type:            r53types.RRTypeA,
				TTL:             aws.Int64(r.config.TTL),
				ResourceRecords: records,
			},
		})
	}

	if len(changes) == 0 {
		return false, nil
	}

	_, err := r.client.ChangeResourceRecordSets(ctx, &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(r.config.HostedZoneID),
		ChangeBatch: &r53types.ChangeBatch{
			Comment: aws.String("k8s-dns-controller automatic update"),
			Changes: changes,
		},
	})
	if err != nil {
		return false, fmt.Errorf("applying Route53 changes: %w", err)
	}

	r.logger.Info("Route53 updated", "changes", len(changes))
	return true, nil
}

func (r *Reconciler) getCurrentIPs(ctx context.Context, fqdn string) ([]string, error) {
	out, err := r.client.ListResourceRecordSets(ctx, &route53.ListResourceRecordSetsInput{
		HostedZoneId:    aws.String(r.config.HostedZoneID),
		StartRecordName: aws.String(fqdn),
		StartRecordType: r53types.RRTypeA,
		MaxItems:        aws.Int32(1),
	})
	if err != nil {
		return nil, err
	}

	for _, rrs := range out.ResourceRecordSets {
		// Route53 returns wildcards as \052 (octal for *) in its API responses.
		name := strings.ReplaceAll(aws.ToString(rrs.Name), `\052`, "*")
		if name == fqdn && rrs.Type == r53types.RRTypeA {
			var ips []string
			for _, rr := range rrs.ResourceRecords {
				ips = append(ips, aws.ToString(rr.Value))
			}
			sort.Strings(ips)
			return ips, nil
		}
	}

	return nil, nil // record doesn't exist yet
}

func uniqueSortedIPs(m map[string]net.IP) []string {
	seen := make(map[string]bool)
	var out []string
	for _, ip := range m {
		s := ip.String()
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func ipsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func ensureTrailingDot(name string) string {
	if !strings.HasSuffix(name, ".") {
		return name + "."
	}
	return name
}
