package assertions

import (
	"context"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestAssertResourceExists(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: "default",
		},
	}

	tests := []struct {
		name       string
		objects    []runtime.Object
		key        types.NamespacedName
		shouldPass bool
	}{
		{
			name:    "resource exists",
			objects: []runtime.Object{secret},
			key: types.NamespacedName{
				Name:      "test-secret",
				Namespace: "default",
			},
			shouldPass: true,
		},
		{
			name:    "resource not found",
			objects: []runtime.Object{},
			key: types.NamespacedName{
				Name:      "missing-secret",
				Namespace: "default",
			},
			shouldPass: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			builder := fake.NewClientBuilder().WithScheme(scheme)
			for _, obj := range tc.objects {
				builder = builder.WithRuntimeObjects(obj)
			}
			c := builder.Build()

			failed := false
			testG := gomega.NewGomega(func(message string, callerSkip ...int) {
				failed = true
			})

			AssertResourceExists(context.Background(), testG, c, tc.key, &corev1.Secret{})
			g.Expect(failed).To(gomega.Equal(!tc.shouldPass))
		})
	}
}

func TestAssertResourceNotExists(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: "default",
		},
	}

	tests := []struct {
		name       string
		objects    []runtime.Object
		key        types.NamespacedName
		shouldPass bool
	}{
		{
			name:    "resource not found",
			objects: []runtime.Object{},
			key: types.NamespacedName{
				Name:      "missing-secret",
				Namespace: "default",
			},
			shouldPass: true,
		},
		{
			name:    "resource exists",
			objects: []runtime.Object{secret},
			key: types.NamespacedName{
				Name:      "test-secret",
				Namespace: "default",
			},
			shouldPass: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			builder := fake.NewClientBuilder().WithScheme(scheme)
			for _, obj := range tc.objects {
				builder = builder.WithRuntimeObjects(obj)
			}
			c := builder.Build()

			failed := false
			testG := gomega.NewGomega(func(message string, callerSkip ...int) {
				failed = true
			})

			AssertResourceNotExists(context.Background(), testG, c, tc.key, &corev1.Secret{})
			g.Expect(failed).To(gomega.Equal(!tc.shouldPass))
		})
	}
}
