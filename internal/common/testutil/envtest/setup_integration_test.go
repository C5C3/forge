//go:build integration

package envtest

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Shared test infrastructure: a single envtest environment for all tests in
// this file, started once in TestMain and torn down when the suite completes.
var (
	testCfg      *rest.Config
	testClient   client.Client
	testTeardown func()
)

func TestMain(m *testing.M) {
	// SetupEnvTest automatically enumerates fake_crds/ subdirectories.
	var err error
	testCfg, testClient, testTeardown, err = SetupEnvTest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup envtest: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	testTeardown()
	os.Exit(code)
}

// ---------------------------------------------------------------------------
// REQ-001: Verify that SetupEnvTest starts and stops cleanly.
// ---------------------------------------------------------------------------

func TestSetupEnvTest_StartsAndStops(t *testing.T) {
	if testCfg == nil {
		t.Fatal("expected rest.Config to be non-nil after SetupEnvTest")
	}
	if testClient == nil {
		t.Fatal("expected client.Client to be non-nil after SetupEnvTest")
	}

	// Verify the config points at a reachable API server.
	if testCfg.Host == "" {
		t.Fatal("expected rest.Config.Host to be non-empty")
	}

	t.Logf("envtest API server running at %s", testCfg.Host)
}

// ---------------------------------------------------------------------------
// REQ-002: Verify that all bundled CRDs are installable and usable.
// ---------------------------------------------------------------------------

func TestSetupEnvTest_CRDsInstallable(t *testing.T) {
	ctx := context.Background()

	// Use a dynamic client so we can interact with CRD-based resources
	// without needing them registered in the client scheme.
	dynClient, err := dynamic.NewForConfig(testCfg)
	if err != nil {
		t.Fatalf("failed to create dynamic client: %v", err)
	}

	// Create a dedicated namespace for this test.
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-crds-",
		},
	}
	if err := testClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create test namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = testClient.Delete(ctx, ns)
	})
	t.Logf("using namespace %s", ns.Name)

	// Each entry describes a custom resource to create in order to verify its
	// CRD was installed by SetupEnvTest.
	crdTests := []struct {
		name     string
		gvr      schema.GroupVersionResource
		gvk      schema.GroupVersionKind
		specData map[string]interface{}
	}{
		{
			name: "MariaDB",
			gvr:  schema.GroupVersionResource{Group: "k8s.mariadb.com", Version: "v1alpha1", Resource: "mariadbs"},
			gvk:  schema.GroupVersionKind{Group: "k8s.mariadb.com", Version: "v1alpha1", Kind: "MariaDB"},
			specData: map[string]interface{}{
				"replicas": int64(1),
				"image":    "mariadb:10.11",
			},
		},
		{
			name: "ExternalSecret",
			gvr:  schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1beta1", Resource: "externalsecrets"},
			gvk:  schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1beta1", Kind: "ExternalSecret"},
			specData: map[string]interface{}{
				"refreshInterval": "1h",
				"secretStoreRef": map[string]interface{}{
					"name": "test-store",
					"kind": "ClusterSecretStore",
				},
			},
		},
		{
			name: "Certificate",
			gvr:  schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"},
			gvk:  schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"},
			specData: map[string]interface{}{
				"secretName": "test-cert-tls",
				"issuerRef": map[string]interface{}{
					"name":  "test-issuer",
					"kind":  "ClusterIssuer",
					"group": "cert-manager.io",
				},
				"dnsNames": []interface{}{"example.com"},
			},
		},
		{
			name: "Memcached",
			gvr:  schema.GroupVersionResource{Group: "opsv1.memcached.com", Version: "v1alpha1", Resource: "memcacheds"},
			gvk:  schema.GroupVersionKind{Group: "opsv1.memcached.com", Version: "v1alpha1", Kind: "Memcached"},
			specData: map[string]interface{}{
				"replicas": int64(1),
			},
		},
		{
			name: "RabbitmqCluster",
			gvr:  schema.GroupVersionResource{Group: "rabbitmq.com", Version: "v1beta1", Resource: "rabbitmqclusters"},
			gvk:  schema.GroupVersionKind{Group: "rabbitmq.com", Version: "v1beta1", Kind: "RabbitmqCluster"},
			specData: map[string]interface{}{
				"replicas": int64(1),
			},
		},
	}

	for _, tc := range crdTests {
		t.Run(tc.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": tc.gvk.Group + "/" + tc.gvk.Version,
					"kind":       tc.gvk.Kind,
					"metadata": map[string]interface{}{
						"generateName": "test-" + strings.ToLower(tc.name) + "-",
						"namespace":    ns.Name,
					},
					"spec": tc.specData,
				},
			}

			created, err := dynClient.Resource(tc.gvr).Namespace(ns.Name).Create(
				ctx, obj, metav1.CreateOptions{},
			)
			if err != nil {
				t.Fatalf("failed to create %s CR: %v (this likely means the CRD was not installed)", tc.name, err)
			}
			t.Logf("successfully created %s CR: %s/%s", tc.name, created.GetNamespace(), created.GetName())

			// Clean up the created resource.
			t.Cleanup(func() {
				_ = dynClient.Resource(tc.gvr).Namespace(ns.Name).Delete(
					ctx, created.GetName(), metav1.DeleteOptions{},
				)
			})
		})
	}
}

// ---------------------------------------------------------------------------
// REQ-001 (supplementary): Verify that the scheme includes core API types.
// ---------------------------------------------------------------------------

func TestSetupEnvTest_SchemeRegistered(t *testing.T) {
	ctx := context.Background()

	// Create a dedicated namespace for this test.
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-scheme-",
		},
	}
	if err := testClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create test namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = testClient.Delete(ctx, ns)
	})
	t.Logf("using namespace %s", ns.Name)

	t.Run("core/v1 Namespace", func(t *testing.T) {
		// List namespaces to confirm the scheme handles core/v1.
		nsList := &corev1.NamespaceList{}
		if err := testClient.List(ctx, nsList); err != nil {
			t.Fatalf("failed to list Namespaces (core/v1): %v", err)
		}
		if len(nsList.Items) == 0 {
			t.Fatal("expected at least one namespace, got zero")
		}
		t.Logf("listed %d namespaces", len(nsList.Items))
	})

	t.Run("apps/v1 Deployment", func(t *testing.T) {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-deploy-",
				Namespace:    ns.Name,
			},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "test"},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{"app": "test"},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test",
								Image: "busybox:latest",
							},
						},
					},
				},
			},
		}
		if err := testClient.Create(ctx, dep); err != nil {
			t.Fatalf("failed to create Deployment (apps/v1): %v", err)
		}
		t.Logf("created deployment %s/%s", dep.Namespace, dep.Name)
		t.Cleanup(func() {
			_ = testClient.Delete(ctx, dep)
		})
	})

	t.Run("batch/v1 Job", func(t *testing.T) {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-job-",
				Namespace:    ns.Name,
			},
			Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:    "test",
								Image:   "busybox:latest",
								Command: []string{"echo", "hello"},
							},
						},
						RestartPolicy: corev1.RestartPolicyNever,
					},
				},
			},
		}
		if err := testClient.Create(ctx, job); err != nil {
			t.Fatalf("failed to create Job (batch/v1): %v", err)
		}
		t.Logf("created job %s/%s", job.Namespace, job.Name)
		t.Cleanup(func() {
			_ = testClient.Delete(ctx, job)
		})
	})
}
