package simulators

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// createOrGet creates the given object in the cluster. If the object already
// exists, it fetches the existing version instead. This makes the operation
// idempotent — calling it multiple times with the same object is safe.
func createOrGet(ctx context.Context, c client.Client, obj client.Object, kind string) error {
	createErr := c.Create(ctx, obj)
	if createErr != nil && !apierrors.IsAlreadyExists(createErr) {
		return fmt.Errorf("creating %s: %w", kind, createErr)
	}

	if apierrors.IsAlreadyExists(createErr) {
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			return fmt.Errorf("getting %s: %w", kind, err)
		}
	}

	return nil
}

// simulateUnstructuredReady creates an unstructured custom resource with the
// given GVK (if it does not already exist) and patches its status sub-resource
// to set ready=true and a "Ready" condition with status "True". This is the
// shared implementation for simulators of operators that follow the standard
// ready+condition pattern (e.g. MariaDB, Memcached).
func simulateUnstructuredReady(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, name, namespace, condReason, condMessage string) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName(name)
	obj.SetNamespace(namespace)

	if err := createOrGet(ctx, c, obj, gvk.Kind); err != nil {
		return err
	}

	patch := client.MergeFrom(obj.DeepCopy())

	if err := unstructured.SetNestedField(obj.Object, true, "status", "ready"); err != nil {
		return fmt.Errorf("setting %s status.ready: %w", gvk.Kind, err)
	}

	conditions := []interface{}{
		map[string]interface{}{
			"type":               "Ready",
			"status":             "True",
			"reason":             condReason,
			"message":            condMessage,
			"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
		},
	}
	if err := unstructured.SetNestedSlice(obj.Object, conditions, "status", "conditions"); err != nil {
		return fmt.Errorf("setting %s status.conditions: %w", gvk.Kind, err)
	}

	if err := c.Status().Patch(ctx, obj, patch); err != nil {
		return fmt.Errorf("patching %s status: %w", gvk.Kind, err)
	}

	return nil
}
