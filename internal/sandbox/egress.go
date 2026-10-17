package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

// ErrHostEgressUnsupported: host-name egress needs an FQDN-aware CNI (spec §11.1).
var ErrHostEgressUnsupported = errors.New("sandbox: host-name egress is not supported (it needs Cilium); use an IP or CIDR destination")

// EgressRule allows TCP to one CIDR and port.
type EgressRule struct {
	CIDR string
	Port int32
}

func (r EgressRule) String() string { return fmt.Sprintf("%s:%d", r.CIDR, r.Port) }

// ParseEgress parses "IP:port", "CIDR:port", "[IPv6]:port" or "[IPv6/len]:port".
func ParseEgress(s string) (EgressRule, error) {
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return EgressRule{}, fmt.Errorf("sandbox: egress %q: %w", s, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return EgressRule{}, fmt.Errorf("sandbox: egress %q: port must be 1-65535", s)
	}
	var cidr string
	if _, network, cerr := net.ParseCIDR(host); cerr == nil {
		cidr = network.String()
	} else if ip := net.ParseIP(host); ip != nil {
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		cidr = fmt.Sprintf("%s/%d", ip.String(), bits)
	} else {
		return EgressRule{}, fmt.Errorf("%w: %q", ErrHostEgressUnsupported, s)
	}
	return EgressRule{CIDR: cidr, Port: int32(port)}, nil
}

// EgressPolicy allows the run's pod to reach exactly the given destinations. The
// namespace's default-deny policy blocks everything else. Nil means "nothing to allow".
func EgressPolicy(namespace, runID string, rules []EgressRule) *networkingv1.NetworkPolicy {
	if len(rules) == 0 {
		return nil
	}
	tcp := corev1.ProtocolTCP
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "last-run-" + runID, Namespace: namespace, Labels: RunLabels(runID)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{LabelRun: runID}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		},
	}
	for _, r := range rules {
		port := intstr.FromInt32(r.Port)
		np.Spec.Egress = append(np.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: r.CIDR}}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
		})
	}
	return np
}

// APIServerEgress returns the in-cluster API server's ClusterIP and egress rules for
// both the ClusterIP and its endpoint addresses (see ServiceEgress).
func APIServerEgress(ctx context.Context, kube kubernetes.Interface) (string, []EgressRule, error) {
	return serviceEgress(ctx, kube, metav1.NamespaceDefault, "kubernetes")
}

// ServiceEgress returns egress rules for a Service's ClusterIP and its EndpointSlice
// addresses on each port: depending on the CNI, NetworkPolicy is evaluated before or
// after service DNAT, so both must be allowed.
func ServiceEgress(ctx context.Context, kube kubernetes.Interface, namespace, name string) ([]EgressRule, error) {
	_, rules, err := serviceEgress(ctx, kube, namespace, name)
	return rules, err
}

func serviceEgress(ctx context.Context, kube kubernetes.Interface, namespace, name string) (string, []EgressRule, error) {
	svc, err := kube.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", nil, fmt.Errorf("sandbox: read service %s/%s: %w", namespace, name, err)
	}
	seen := map[string]bool{}
	var rules []EgressRule
	add := func(ip string, port int32) error {
		r, err := ParseEgress(net.JoinHostPort(ip, strconv.Itoa(int(port))))
		if err != nil {
			return err
		}
		if !seen[r.String()] {
			seen[r.String()] = true
			rules = append(rules, r)
		}
		return nil
	}
	if svc.Spec.ClusterIP != "" && svc.Spec.ClusterIP != corev1.ClusterIPNone {
		for _, p := range svc.Spec.Ports {
			if err := add(svc.Spec.ClusterIP, p.Port); err != nil {
				return "", nil, err
			}
		}
	}
	slices, err := kube.DiscoveryV1().EndpointSlices(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: discoveryv1.LabelServiceName + "=" + name,
	})
	if err != nil {
		return "", nil, fmt.Errorf("sandbox: list endpoints of service %s/%s: %w", namespace, name, err)
	}
	for _, s := range slices.Items {
		for _, ep := range s.Endpoints {
			for _, addr := range ep.Addresses {
				for _, p := range s.Ports {
					if p.Port != nil {
						if err := add(addr, *p.Port); err != nil {
							return "", nil, err
						}
					}
				}
			}
		}
	}
	return svc.Spec.ClusterIP, rules, nil
}
