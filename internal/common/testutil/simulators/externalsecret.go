package simulators

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SimulateExternalSecretSync creates an ExternalSecret custom resource (if it
// does not already exist), patches its status sub-resource to reflect a
// successful sync, and also creates the target Kubernetes Secret populated with
// targetSecretData.
//
// In a real cluster the external-secrets operator would watch ExternalSecret
// objects and create the target Secret automatically.  In envtest the operator
// is absent, so this simulator performs both actions to put the cluster in the
// expected terminal state.
func SimulateExternalSecretSync(ctx context.Context, c client.Client, name, namespace string, targetSecretData map[string][]byte) error {
	// NOTE: Unlike MariaDB and Memcached, this does not use
	// simulateUnstructuredReady because ExternalSecret has a different status
	// shape — no status.ready boolean field, only status.conditions.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "external-secrets.io",
		Version: "v1beta1",
		Kind:    "ExternalSecret",
	})
	obj.SetName(name)
	obj.SetNamespace(namespace)

	if err := createOrGet(ctx, c, obj, "ExternalSecret"); err != nil {
		return err
	}

	patch := client.MergeFrom(obj.DeepCopy())

	conditions := []interface{}{
		map[string]interface{}{
			"type":               "Ready",
			"status":             "True",
			"reason":             "SecretSynced",
			"message":            "Secret was synced",
			"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
		},
	}
	if err := unstructured.SetNestedSlice(obj.Object, conditions, "status", "conditions"); err != nil {
		return fmt.Errorf("setting ExternalSecret status.conditions: %w", err)
	}

	if err := c.Status().Patch(ctx, obj, patch); err != nil {
		return fmt.Errorf("patching ExternalSecret status: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: targetSecretData,
	}
	if err := c.Create(ctx, secret); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating target Secret: %w", err)
		}
		// Secret already exists — update its Data to match targetSecretData so
		// the helper is idempotent for content changes.
		existing := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(secret), existing); err != nil {
			return fmt.Errorf("getting existing target Secret: %w", err)
		}
		existing.Data = targetSecretData
		if err := c.Update(ctx, existing); err != nil {
			return fmt.Errorf("updating existing target Secret data: %w", err)
		}
	}

	return nil
}
