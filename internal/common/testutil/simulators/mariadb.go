package simulators

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SimulateMariaDBReady creates a MariaDB custom resource (if it does not already
// exist) and patches its status sub-resource so that ready=true and a "Ready"
// condition with status "True" is present.  This simulates the behaviour of the
// mariadb-operator controller in envtest environments where the operator is not
// running.
func SimulateMariaDBReady(ctx context.Context, c client.Client, name, namespace string) error {
	return simulateUnstructuredReady(ctx, c, schema.GroupVersionKind{
		Group:   "k8s.mariadb.com",
		Version: "v1alpha1",
		Kind:    "MariaDB",
	}, name, namespace, "Ready", "MariaDB is ready")
}
