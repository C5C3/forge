//go:build integration

package simulators_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	testenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
	"github.com/c5c3/forge/internal/common/testutil/simulators"
)

var (
	k8sClient client.Client

	mariadbGVK = schema.GroupVersionKind{
		Group:   "k8s.mariadb.com",
		Version: "v1alpha1",
		Kind:    "MariaDB",
	}
	memcachedGVK = schema.GroupVersionKind{
		Group:   "opsv1.memcached.com",
		Version: "v1alpha1",
		Kind:    "Memcached",
	}
	externalSecretGVK = schema.GroupVersionKind{
		Group:   "external-secrets.io",
		Version: "v1beta1",
		Kind:    "ExternalSecret",
	}
)

func TestMain(m *testing.M) {
	// SetupEnvTest automatically enumerates fake_crds/ subdirectories.
	_, c, teardown, err := testenvtest.SetupEnvTest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup envtest: %v\n", err)
		os.Exit(1)
	}
	k8sClient = c

	ctx := context.Background()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-simulators",
		},
	}
	if err := k8sClient.Create(ctx, ns); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create test namespace: %v\n", err)
		teardown()
		os.Exit(1)
	}

	code := m.Run()
	teardown()
	os.Exit(code)
}

func TestSimulateMariaDBReady(t *testing.T) {
	ctx := context.Background()
	name := "test-mariadb-ready"
	namespace := "test-simulators"

	if err := simulators.SimulateMariaDBReady(ctx, k8sClient, name, namespace); err != nil {
		t.Fatalf("SimulateMariaDBReady returned error: %v", err)
	}

	assertUnstructuredReady(t, ctx, k8sClient, mariadbGVK, name, namespace)
}

func TestSimulateMariaDBReady_Idempotent(t *testing.T) {
	ctx := context.Background()
	name := "test-mariadb-idempotent"
	namespace := "test-simulators"

	if err := simulators.SimulateMariaDBReady(ctx, k8sClient, name, namespace); err != nil {
		t.Fatalf("first call to SimulateMariaDBReady returned error: %v", err)
	}

	if err := simulators.SimulateMariaDBReady(ctx, k8sClient, name, namespace); err != nil {
		t.Fatalf("second call to SimulateMariaDBReady returned error: %v", err)
	}

	assertUnstructuredReady(t, ctx, k8sClient, mariadbGVK, name, namespace)
}

func TestSimulateMemcachedReady(t *testing.T) {
	ctx := context.Background()
	name := "test-memcached-ready"
	namespace := "test-simulators"

	if err := simulators.SimulateMemcachedReady(ctx, k8sClient, name, namespace); err != nil {
		t.Fatalf("SimulateMemcachedReady returned error: %v", err)
	}

	assertUnstructuredReady(t, ctx, k8sClient, memcachedGVK, name, namespace)
}

func TestSimulateMemcachedReady_Idempotent(t *testing.T) {
	ctx := context.Background()
	name := "test-memcached-idempotent"
	namespace := "test-simulators"

	if err := simulators.SimulateMemcachedReady(ctx, k8sClient, name, namespace); err != nil {
		t.Fatalf("first call to SimulateMemcachedReady returned error: %v", err)
	}

	if err := simulators.SimulateMemcachedReady(ctx, k8sClient, name, namespace); err != nil {
		t.Fatalf("second call to SimulateMemcachedReady returned error: %v", err)
	}

	assertUnstructuredReady(t, ctx, k8sClient, memcachedGVK, name, namespace)
}

func TestSimulateExternalSecretSync(t *testing.T) {
	ctx := context.Background()
	name := "test-externalsecret-sync"
	namespace := "test-simulators"

	targetData := map[string][]byte{
		"username": []byte("admin"),
		"password": []byte("s3cret"),
	}

	if err := simulators.SimulateExternalSecretSync(ctx, k8sClient, name, namespace, targetData); err != nil {
		t.Fatalf("SimulateExternalSecretSync returned error: %v", err)
	}

	assertExternalSecretConditions(t, ctx, k8sClient, name, namespace)
	assertSecretData(t, ctx, k8sClient, name, namespace, targetData)
}

func TestSimulateExternalSecretSync_Idempotent(t *testing.T) {
	ctx := context.Background()
	name := "test-externalsecret-idempotent"
	namespace := "test-simulators"

	targetData := map[string][]byte{
		"username": []byte("admin"),
		"password": []byte("s3cret"),
	}

	if err := simulators.SimulateExternalSecretSync(ctx, k8sClient, name, namespace, targetData); err != nil {
		t.Fatalf("first call to SimulateExternalSecretSync returned error: %v", err)
	}

	// Second invocation should also succeed and leave the ExternalSecret/Secret
	// in the same Ready state with the same data (idempotency).
	if err := simulators.SimulateExternalSecretSync(ctx, k8sClient, name, namespace, targetData); err != nil {
		t.Fatalf("second call to SimulateExternalSecretSync returned error: %v", err)
	}

	assertExternalSecretConditions(t, ctx, k8sClient, name, namespace)
	assertSecretData(t, ctx, k8sClient, name, namespace, targetData)
}

func TestSimulateJobComplete(t *testing.T) {
	ctx := context.Background()
	name := "test-job-complete"
	namespace := "test-simulators"

	if err := simulators.SimulateJobComplete(ctx, k8sClient, name, namespace); err != nil {
		t.Fatalf("SimulateJobComplete returned error: %v", err)
	}

	assertJobComplete(t, ctx, k8sClient, name, namespace)
}

func TestSimulateJobComplete_Idempotent(t *testing.T) {
	ctx := context.Background()
	name := "test-job-idempotent"
	namespace := "test-simulators"

	if err := simulators.SimulateJobComplete(ctx, k8sClient, name, namespace); err != nil {
		t.Fatalf("first call to SimulateJobComplete returned error: %v", err)
	}

	if err := simulators.SimulateJobComplete(ctx, k8sClient, name, namespace); err != nil {
		t.Fatalf("second call to SimulateJobComplete returned error: %v", err)
	}

	assertJobComplete(t, ctx, k8sClient, name, namespace)
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// assertUnstructuredReady fetches an unstructured CR by GVK and verifies that
// status.ready is true and a Ready=True condition is present.
func assertUnstructuredReady(t *testing.T, ctx context.Context, c client.Client, gvk schema.GroupVersionKind, name, namespace string) {
	t.Helper()

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, obj); err != nil {
		t.Fatalf("failed to get %s %s/%s: %v", gvk.Kind, namespace, name, err)
	}

	ready, found, err := unstructured.NestedBool(obj.Object, "status", "ready")
	if err != nil {
		t.Fatalf("error reading %s status.ready: %v", gvk.Kind, err)
	}
	if !found {
		t.Fatalf("%s status.ready field not found", gvk.Kind)
	}
	if !ready {
		t.Fatalf("expected %s status.ready to be true, got false", gvk.Kind)
	}

	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil {
		t.Fatalf("error reading %s status.conditions: %v", gvk.Kind, err)
	}
	if !found {
		t.Fatalf("%s status.conditions field not found", gvk.Kind)
	}
	if len(conditions) == 0 {
		t.Fatalf("expected at least one %s condition, got none", gvk.Kind)
	}

	assertCondition(t, conditions, "Ready", "True")
}

// assertExternalSecretConditions fetches an ExternalSecret CR and verifies that
// a Ready=True condition is present in status.conditions.
func assertExternalSecretConditions(t *testing.T, ctx context.Context, c client.Client, name, namespace string) {
	t.Helper()

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(externalSecretGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, obj); err != nil {
		t.Fatalf("failed to get ExternalSecret %s/%s: %v", namespace, name, err)
	}

	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil {
		t.Fatalf("error reading ExternalSecret status.conditions: %v", err)
	}
	if !found {
		t.Fatal("ExternalSecret status.conditions field not found")
	}
	if len(conditions) == 0 {
		t.Fatal("expected at least one ExternalSecret condition, got none")
	}

	assertCondition(t, conditions, "Ready", "True")
}

// assertSecretData fetches a Secret and verifies that its Data matches the expected map.
func assertSecretData(t *testing.T, ctx context.Context, c client.Client, name, namespace string, expectedData map[string][]byte) {
	t.Helper()

	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, secret); err != nil {
		t.Fatalf("failed to get Secret %s/%s: %v", namespace, name, err)
	}

	for key, expectedVal := range expectedData {
		actualVal, ok := secret.Data[key]
		if !ok {
			t.Fatalf("expected key %q in Secret data, but not found", key)
		}
		if string(actualVal) != string(expectedVal) {
			t.Fatalf("Secret data[%q]: expected %q, got %q", key, expectedVal, actualVal)
		}
	}
}

// assertJobComplete fetches a Job and verifies that status.succeeded is 1 and
// a Complete=True condition is present.
func assertJobComplete(t *testing.T, ctx context.Context, c client.Client, name, namespace string) {
	t.Helper()

	job := &batchv1.Job{}
	if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, job); err != nil {
		t.Fatalf("failed to get Job %s/%s: %v", namespace, name, err)
	}

	if job.Status.Succeeded != 1 {
		t.Fatalf("expected Job status.succeeded=1, got %d", job.Status.Succeeded)
	}

	foundComplete := false
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
			foundComplete = true
			break
		}
	}
	if !foundComplete {
		t.Fatalf("expected Complete=True condition on Job %s/%s, not found", namespace, name)
	}
}

// assertCondition checks that the given conditions slice (from an unstructured
// object) contains a condition with the specified type and status values.
func assertCondition(t *testing.T, conditions []interface{}, condType, condStatus string) {
	t.Helper()
	for _, c := range conditions {
		condMap, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if condMap["type"] == condType && condMap["status"] == condStatus {
			return
		}
	}
	t.Fatalf("expected condition type=%q status=%q not found in conditions: %v", condType, condStatus, conditions)
}
