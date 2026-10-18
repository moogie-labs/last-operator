package gateway

import (
	"bytes"
	"context"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

// registryPullSecret is the copy of registry.external.pullSecretRef in the sandbox
// namespace that sandbox pods pull with (design §6.2).
const registryPullSecret = "last-registry-pull"

// ensurePullSecret copies the pull Secret that ext names, from the gateway namespace,
// to last-registry-pull in the sandbox namespace: it creates the copy, or deletes and
// recreates it when it differs. It runs before every sandbox run in external mode,
// so a rotated pull Secret reaches the next run. The copy carries no sandbox labels,
// so the reaper leaves it alone. Errors: FailedPrecondition when the source is
// missing or has no .dockerconfigjson; Unavailable for Kubernetes errors.
func (g *Gateway) ensurePullSecret(ctx context.Context, ext *lastv1alpha1.ExternalRegistry) error {
	config, err := g.dockerConfig(ctx, "pullSecretRef", ext.PullSecretRef.Name)
	if err != nil {
		return err
	}
	g.pullMu.Lock()
	defer g.pullMu.Unlock()
	secrets := g.kube.CoreV1().Secrets(g.cfg.SandboxNamespace)
	cur, err := secrets.Get(ctx, registryPullSecret, metav1.GetOptions{})
	switch {
	case err == nil:
		if cur.Type == corev1.SecretTypeDockerConfigJson && len(cur.Data) == 1 &&
			bytes.Equal(cur.Data[corev1.DockerConfigJsonKey], config) {
			return nil
		}
		// The sandbox Role has no update on Secrets: replace the copy instead.
		if err := secrets.Delete(ctx, registryPullSecret, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return status.Errorf(codes.Unavailable, "replace the registry pull Secret %s/%s: %v", g.cfg.SandboxNamespace, registryPullSecret, err)
		}
	case !apierrors.IsNotFound(err):
		return status.Errorf(codes.Unavailable, "read the registry pull Secret %s/%s: %v", g.cfg.SandboxNamespace, registryPullSecret, err)
	}
	copied := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: registryPullSecret, Namespace: g.cfg.SandboxNamespace},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: config},
	}
	if _, err := secrets.Create(ctx, copied, metav1.CreateOptions{}); err != nil {
		return status.Errorf(codes.Unavailable, "copy the registry pull Secret to %s/%s: %v", g.cfg.SandboxNamespace, registryPullSecret, err)
	}
	slog.Info("Copied the registry pull Secret", "from", g.cfg.Namespace+"/"+ext.PullSecretRef.Name,
		"to", g.cfg.SandboxNamespace+"/"+registryPullSecret)
	return nil
}
