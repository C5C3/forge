package assertions

import (
	"context"
	"testing"
	"time"

	"github.com/onsi/gomega"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestAssertCondition(t *testing.T) {
	tests := []struct {
		name          string
		conditions    []metav1.Condition
		condType      string
		status        metav1.ConditionStatus
		shouldPass    bool
		failureSubstr string
	}{
		{
			name: "condition found with expected status",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue},
			},
			condType:   "Ready",
			status:     metav1.ConditionTrue,
			shouldPass: true,
		},
		{
			name:          "condition not found in empty slice",
			conditions:    []metav1.Condition{},
			condType:      "Ready",
			status:        metav1.ConditionTrue,
			shouldPass:    false,
			failureSubstr: "not found",
		},
		{
			name:          "condition not found in nil slice",
			conditions:    nil,
			condType:      "Ready",
			status:        metav1.ConditionTrue,
			shouldPass:    false,
			failureSubstr: "not found",
		},
		{
			name: "condition found with wrong status",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionFalse},
			},
			condType:      "Ready",
			status:        metav1.ConditionTrue,
			shouldPass:    false,
			failureSubstr: "has status",
		},
		{
			name: "multiple conditions, target found",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue},
				{Type: "Available", Status: metav1.ConditionFalse},
			},
			condType:   "Available",
			status:     metav1.ConditionFalse,
			shouldPass: true,
		},
		{
			name: "multiple conditions, target not found",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue},
			},
			condType:      "Degraded",
			status:        metav1.ConditionTrue,
			shouldPass:    false,
			failureSubstr: "not found",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			failed := false
			var failureMsg string
			testG := gomega.NewGomega(func(message string, callerSkip ...int) {
				failed = true
				failureMsg = message
			})

			AssertCondition(testG, tc.conditions, tc.condType, tc.status)
			g.Expect(failed).To(gomega.Equal(!tc.shouldPass))
			if !tc.shouldPass && tc.failureSubstr != "" {
				g.Expect(failureMsg).To(
					gomega.ContainSubstring(tc.failureSubstr),
					"expected failure message to contain %q, got %q", tc.failureSubstr, failureMsg,
				)
			}
		})
	}
}

func TestEventuallyCondition(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = policyv1.AddToScheme(scheme)

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pdb",
			Namespace: "default",
		},
		Status: policyv1.PodDisruptionBudgetStatus{
			Conditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					Reason:             "AsExpected",
					LastTransitionTime: metav1.Now(),
				},
				{
					Type:               "Available",
					Status:             metav1.ConditionFalse,
					Reason:             "Degraded",
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}

	tests := []struct {
		name       string
		condType   string
		status     metav1.ConditionStatus
		shouldPass bool
	}{
		{
			name:       "condition found with expected status",
			condType:   "Ready",
			status:     metav1.ConditionTrue,
			shouldPass: true,
		},
		{
			name:       "condition found with wrong status",
			condType:   "Ready",
			status:     metav1.ConditionFalse,
			shouldPass: false,
		},
		{
			name:       "condition not found",
			condType:   "Degraded",
			status:     metav1.ConditionTrue,
			shouldPass: false,
		},
		{
			name:       "second condition found with expected status",
			condType:   "Available",
			status:     metav1.ConditionFalse,
			shouldPass: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithRuntimeObjects(pdb).
				Build()

			failed := false
			testG := gomega.NewGomega(func(message string, callerSkip ...int) {
				failed = true
			})

			target := &policyv1.PodDisruptionBudget{}
			target.Name = pdb.Name
			target.Namespace = pdb.Namespace

			EventuallyCondition(context.Background(), testG, c, target, tc.condType, tc.status, 2*time.Second)
			g.Expect(failed).To(gomega.Equal(!tc.shouldPass))
		})
	}
}
