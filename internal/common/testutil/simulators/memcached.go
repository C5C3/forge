package simulators

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SimulateMemcachedReady creates a Memcached custom resource (if it does not
// already exist) and patches its status sub-resource so that ready=true and a
// "Ready" condition with status "True" is present.  This simulates the behaviour
// of the memcached operator controller in envtest environments where the operator
// is not running.
//
// Note: the API group "opsv1.memcached.com" is a fabricated placeholder for
// testing purposes. The group name unusually encodes the API version ("opsv1")
// in the group field; this does not correspond to a real-world operator.
func SimulateMemcachedReady(ctx context.Context, c client.Client, name, namespace string) error {
	return simulateUnstructuredReady(ctx, c, schema.GroupVersionKind{
		Group:   "opsv1.memcached.com",
		Version: "v1alpha1",
		Kind:    "Memcached",
	}, name, namespace, "Ready", "Memcached is ready")
}
