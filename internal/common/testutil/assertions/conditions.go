package assertions

import (
	"context"
	"fmt"
	"time"

	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// defaultPollingInterval is the interval between consecutive polls in
// EventuallyCondition.
const defaultPollingInterval = 250 * time.Millisecond

// AssertCondition asserts that a condition with the given type exists in the
// conditions slice and has the expected status. It produces a descriptive
// failure message if the condition is not found or has a different status.
func AssertCondition(g gomega.Gomega, conditions []metav1.Condition, condType string, status metav1.ConditionStatus) {
	for i := range conditions {
		if conditions[i].Type == condType {
			g.Expect(conditions[i].Status).To(
				gomega.Equal(status),
				fmt.Sprintf("condition %q has status %q, expected %q", condType, conditions[i].Status, status),
			)
			return
		}
	}
	g.Expect(conditions).To(
		gomega.ContainElement(gomega.HaveField("Type", gomega.Equal(condType))),
		fmt.Sprintf("condition %q not found", condType),
	)
}

// EventuallyCondition polls the Kubernetes object until the condition with the
// given type reaches the expected status, or until the timeout elapses. It
// refreshes the object on each poll via c.Get, extracts the
// .status.conditions field using unstructured conversion (so it works with any
// concrete client.Object type), and delegates assertion to AssertCondition.
func EventuallyCondition(ctx context.Context, g gomega.Gomega, c client.Client, obj client.Object, condType string, status metav1.ConditionStatus, timeout time.Duration) {
	g.Eventually(func(eg gomega.Gomega) {
		eg.Expect(c.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(gomega.Succeed())

		unstrMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		eg.Expect(err).NotTo(gomega.HaveOccurred(), "failed to convert object to unstructured")

		rawConditions, found, nestedErr := unstructured.NestedSlice(unstrMap, "status", "conditions")
		eg.Expect(nestedErr).NotTo(gomega.HaveOccurred(), "failed to read .status.conditions")
		eg.Expect(found).To(gomega.BeTrue(), "object has no .status.conditions field")

		conditions := make([]metav1.Condition, 0, len(rawConditions))
		for _, raw := range rawConditions {
			condMap, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			var cond metav1.Condition
			if convErr := runtime.DefaultUnstructuredConverter.FromUnstructured(condMap, &cond); convErr == nil {
				conditions = append(conditions, cond)
			}
		}

		AssertCondition(eg, conditions, condType, status)
	}).WithTimeout(timeout).WithPolling(defaultPollingInterval).Should(gomega.Succeed())
}
