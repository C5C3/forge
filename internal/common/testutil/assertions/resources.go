package assertions

import (
	"context"

	"github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// AssertResourceExists asserts that the Kubernetes resource identified by key
// exists in the API server. It uses the provided client to perform a Get
// operation and fails if the resource is not found.
func AssertResourceExists(ctx context.Context, g gomega.Gomega, c client.Client, key types.NamespacedName, obj client.Object) {
	err := c.Get(ctx, key, obj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// AssertResourceNotExists asserts that the Kubernetes resource identified by
// key does not exist in the API server. It uses the provided client to perform
// a Get operation and expects a NotFound error.
func AssertResourceNotExists(ctx context.Context, g gomega.Gomega, c client.Client, key types.NamespacedName, obj client.Object) {
	err := c.Get(ctx, key, obj)
	g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue(), "expected NotFound error, got: %v", err)
}
