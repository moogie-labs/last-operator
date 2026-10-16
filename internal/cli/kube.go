package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

// KubeFlags are the kubectl connection flags the Intent commands take.
type KubeFlags struct {
	Kubeconfig, Context, Namespace string
}

func (f *KubeFlags) addTo(cmd *cobra.Command) {
	fs := cmd.PersistentFlags()
	fs.StringVar(&f.Kubeconfig, "kubeconfig", "", "path to the kubeconfig file (default: $KUBECONFIG or ~/.kube/config)")
	fs.StringVar(&f.Context, "context", "", "the kubeconfig context to use")
	fs.StringVarP(&f.Namespace, "namespace", "n", "", "the Intent's namespace (default: the context's namespace)")
}

// Kube is the cluster as the user sees it: every call acts with their
// kubeconfig identity, never the gateway's (M3 spec §5).
type Kube struct {
	Client    client.Client
	Namespace string
	// WhoAmI returns the user's name as the API server knows it.
	WhoAmI func(context.Context) (string, error)
}

// NewKube connects with the user's kubeconfig.
func NewKube(f KubeFlags) (*Kube, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = f.Kubeconfig
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: f.Context})
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load the kubeconfig: %w", err)
	}
	ns := f.Namespace
	if ns == "" {
		if ns, _, err = cc.Namespace(); err != nil {
			return nil, fmt.Errorf("load the kubeconfig: %w", err)
		}
	}
	c, err := client.New(cfg, client.Options{Scheme: newScheme()})
	if err != nil {
		return nil, err
	}
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Kube{Client: c, Namespace: ns, WhoAmI: func(ctx context.Context) (string, error) {
		r, err := kc.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authv1.SelfSubjectReview{}, metav1.CreateOptions{})
		if err != nil {
			return "", fmt.Errorf("ask the API server who you are (SelfSubjectReview): %w", err)
		}
		return r.Status.UserInfo.Username, nil
	}}, nil
}

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(lastv1alpha1.AddToScheme(s))
	return s
}

// getIntent reads one Intent in the user's namespace.
func getIntent(ctx context.Context, k *Kube, name string) (*lastv1alpha1.Intent, error) {
	var in lastv1alpha1.Intent
	if err := k.Client.Get(ctx, client.ObjectKey{Namespace: k.Namespace, Name: name}, &in); err != nil {
		return nil, err
	}
	return &in, nil
}
