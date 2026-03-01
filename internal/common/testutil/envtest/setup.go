package envtest

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// schemeAdders lists the core API groups that SetupEnvTest registers
// automatically. Add entries here when new API groups are needed.
var schemeAdders = []struct {
	name string
	add  func(*k8sruntime.Scheme) error
}{
	{"corev1", corev1.AddToScheme},
	{"appsv1", appsv1.AddToScheme},
	{"batchv1", batchv1.AddToScheme},
}

// FakeCRDsPath returns the absolute path to the fake_crds/ directory bundled
// alongside this package. The path is resolved relative to the source file at
// compile time using runtime.Caller so it remains correct regardless of the
// working directory at test execution time.
func FakeCRDsPath() (string, error) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("envtest: runtime.Caller(0) failed; cannot determine source file path")
	}
	return filepath.Join(filepath.Dir(filename), "..", "fake_crds"), nil
}

// fakeCRDSubDirs returns the absolute paths of all immediate subdirectories
// under fake_crds/. controller-runtime's envtest reads CRD manifests only from
// the top level of each listed directory (no recursion), so each subdirectory
// must be listed individually.
func fakeCRDSubDirs() ([]string, error) {
	base, err := FakeCRDsPath()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, fmt.Errorf("envtest: cannot read fake_crds directory %s: %w", base, err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(base, e.Name()))
		}
	}
	return dirs, nil
}

// SetupEnvTest starts a local Kubernetes API server and etcd using
// controller-runtime's envtest machinery and returns a configured rest.Config,
// a ready-to-use client.Client, and a teardown function.
//
// crdPaths optionally specifies additional directories containing CRD manifests
// to install. All subdirectories of the bundled fake_crds/ directory are always
// included automatically (envtest does not recurse into subdirectories, so each
// subdirectory is enumerated individually).
//
// The teardown function must be called (typically via defer) to stop the
// environment after the test completes. It logs any stop error to stderr but
// does not panic, so the test binary can still exit cleanly.
//
// SetupEnvTest returns an error if the environment cannot be started.
func SetupEnvTest(crdPaths ...string) (*rest.Config, client.Client, func(), error) {
	crdSubDirs, err := fakeCRDSubDirs()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("envtest: enumerating CRD subdirectories: %w", err)
	}
	allCRDPaths := make([]string, 0, len(crdSubDirs)+len(crdPaths))
	allCRDPaths = append(allCRDPaths, crdSubDirs...)
	allCRDPaths = append(allCRDPaths, crdPaths...)

	testEnv := &envtest.Environment{
		CRDDirectoryPaths: allCRDPaths,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("envtest: failed to start environment: %w", err)
	}

	s := k8sruntime.NewScheme()

	for _, sa := range schemeAdders {
		if err := sa.add(s); err != nil {
			// Stop the already-started environment before returning the error.
			_ = testEnv.Stop()
			return nil, nil, nil, fmt.Errorf("envtest: failed to register %s scheme: %w", sa.name, err)
		}
	}

	k8sClient, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		_ = testEnv.Stop()
		return nil, nil, nil, fmt.Errorf("envtest: failed to create client: %w", err)
	}

	teardown := func() {
		if err := testEnv.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "envtest: error stopping environment: %v\n", err)
		}
	}

	return cfg, k8sClient, teardown, nil
}
