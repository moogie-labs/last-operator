package sandbox_test

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/moogie-labs/last-operator/internal/sandbox"
)

const (
	kubernetesService = "kubernetes"
	registryNS        = "last-registry"
)

func TestParseEgress(t *testing.T) {
	ok := map[string]string{
		"10.1.2.3:443":    "10.1.2.3/32:443",
		"10.0.0.0/8:443":  "10.0.0.0/8:443",
		"10.1.2.3/8:80":   "10.0.0.0/8:80",
		"[fd00::1]:443":   "fd00::1/128:443",
		"[fd00::/8]:6443": "fd00::/8:6443",
	}
	for in, want := range ok {
		r, err := sandbox.ParseEgress(in)
		if err != nil || r.String() != want {
			t.Errorf("ParseEgress(%q) = %v, %v; want %s", in, r, err, want)
		}
	}
	for _, in := range []string{"example.com:443", "*.amazonaws.com:443"} {
		if _, err := sandbox.ParseEgress(in); !errors.Is(err, sandbox.ErrHostEgressUnsupported) {
			t.Errorf("ParseEgress(%q) err = %v, want ErrHostEgressUnsupported", in, err)
		}
	}
	for _, in := range []string{"10.1.2.3", "10.1.2.3:0", "10.1.2.3:70000", "10.1.2.3:http", ""} {
		if _, err := sandbox.ParseEgress(in); err == nil || errors.Is(err, sandbox.ErrHostEgressUnsupported) {
			t.Errorf("ParseEgress(%q) err = %v, want a format error", in, err)
		}
	}
}

func TestEgressPolicySelectsOnlyTheRun(t *testing.T) {
	rules := []sandbox.EgressRule{{CIDR: "10.1.2.3/32", Port: 443}, {CIDR: "10.0.0.0/8", Port: 80}}
	np := sandbox.EgressPolicy("last-sandbox", "run1", rules)
	if np.Namespace != "last-sandbox" || np.Spec.PodSelector.MatchLabels[sandbox.LabelRun] != "run1" || len(np.Spec.PodSelector.MatchLabels) != 1 {
		t.Fatalf("selector = %+v", np.Spec.PodSelector)
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
		t.Fatalf("policyTypes = %v", np.Spec.PolicyTypes)
	}
	if len(np.Spec.Egress) != 2 || np.Spec.Egress[0].To[0].IPBlock.CIDR != "10.1.2.3/32" ||
		*np.Spec.Egress[0].Ports[0].Protocol != corev1.ProtocolTCP || np.Spec.Egress[0].Ports[0].Port.IntVal != 443 {
		t.Fatalf("egress = %+v", np.Spec.Egress)
	}
	if np.Labels[sandbox.LabelManagedBy] != sandbox.ManagedByValue || np.Labels[sandbox.LabelRun] != "run1" {
		t.Fatalf("labels = %v", np.Labels)
	}
	if sandbox.EgressPolicy("last-sandbox", "run1", nil) != nil {
		t.Fatal("expected no policy for an empty rule set")
	}
}

func TestAPIServerEgressIncludesClusterIPAndEndpoints(t *testing.T) {
	kube := fake.NewClientset(
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: kubernetesService, Namespace: "default"},
			Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.1", Ports: []corev1.ServicePort{{Name: "https", Port: 443}}}},
		&discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{Name: kubernetesService, Namespace: "default", Labels: map[string]string{discoveryv1.LabelServiceName: kubernetesService}},
			Endpoints:  []discoveryv1.Endpoint{{Addresses: []string{"172.18.0.2"}}},
			Ports:      []discoveryv1.EndpointPort{{Name: ptr.To("https"), Port: ptr.To(int32(6443))}}, //nolint:modernize
		},
	)
	ip, rules, err := sandbox.APIServerEgress(context.Background(), kube)
	if err != nil || ip != "10.96.0.1" {
		t.Fatalf("ip=%s err=%v", ip, err)
	}
	got := map[string]bool{}
	for _, r := range rules {
		got[r.String()] = true
	}
	if !got["10.96.0.1/32:443"] || !got["172.18.0.2/32:6443"] || len(rules) != 2 {
		t.Fatalf("rules = %v", rules)
	}
}

func TestServiceEgressCoversClusterIPAndEndpoints(t *testing.T) {
	kube := fake.NewClientset(
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "front", Namespace: registryNS},
			Spec: corev1.ServiceSpec{ClusterIP: "10.96.5.5", Ports: []corev1.ServicePort{{Port: 5000}}}},
		&discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{Name: "front-abc", Namespace: registryNS, Labels: map[string]string{discoveryv1.LabelServiceName: "front"}},
			Endpoints:  []discoveryv1.Endpoint{{Addresses: []string{"10.244.1.7"}}},
			Ports:      []discoveryv1.EndpointPort{{Port: ptr.To(int32(8080))}}, //nolint:modernize
		},
		// Another service's slice in the same namespace must not leak in.
		&discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{Name: "other-abc", Namespace: registryNS, Labels: map[string]string{discoveryv1.LabelServiceName: "other"}},
			Endpoints:  []discoveryv1.Endpoint{{Addresses: []string{"10.244.9.9"}}},
			Ports:      []discoveryv1.EndpointPort{{Port: ptr.To(int32(80))}}, //nolint:modernize
		},
	)
	rules, err := sandbox.ServiceEgress(context.Background(), kube, registryNS, "front")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rules {
		got[r.String()] = true
	}
	if !got["10.96.5.5/32:5000"] || !got["10.244.1.7/32:8080"] || len(rules) != 2 {
		t.Fatalf("rules = %v", rules)
	}
	if _, err := sandbox.ServiceEgress(context.Background(), kube, registryNS, "missing"); err == nil {
		t.Fatal("expected an error for a missing service")
	}
}
