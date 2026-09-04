// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package testutil provides c5c3-specific test utilities for envtest integration
// tests of the ControlPlane reconciler.
package testutil

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	openbaov1alpha1 "github.com/dc-tec/openbao-operator/api/v1alpha1"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonenvtest "github.com/c5c3/cobaltcore/internal/common/testutil/envtest"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
	horizonv1alpha1 "github.com/c5c3/cobaltcore/operators/horizon/api/v1alpha1"
	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
	placementv1alpha1 "github.com/c5c3/cobaltcore/operators/placement/api/v1alpha1"
)

// SkipIfEnvTestUnavailable re-exports the common skip guard for envtest-based
// integration tests. Call as the first statement in each integration test
// function.
var SkipIfEnvTestUnavailable = commonenvtest.SkipIfEnvTestUnavailable

// SetupC5c3EnvTestWithController starts an envtest API server with the c5c3 CRDs,
// the Keystone CRD (the reconciler Owns a Keystone child), fake CRDs for the
// external operators the ControlPlane reconciler talks to (MariaDB, Memcached,
// external-secrets, cert-manager, K-ORC), the ControlPlane webhook
// configurations, and a controller-runtime Manager running the
// ControlPlaneReconciler. It returns a direct (non-caching) client, a context,
// and its cancel function. The environment is torn down automatically via
// t.Cleanup().
//
// Parameters:
//   - addToScheme registers the c5c3 API types with the runtime scheme. Callers
//     pass c5c3v1alpha1.AddToScheme to avoid an import cycle between the testutil
//     package and the v1alpha1 package.
//   - registerWebhooks sets up webhook handlers with the manager. Callers pass a
//     closure that calls ControlPlaneWebhook.SetupWebhookWithManager(mgr).
//   - registerController registers the ControlPlaneReconciler via
//     SetupWithManager (or an inline builder for multi-test setups).
//
// The scheme is built fresh per test — internal/common's SharedScheme() is NOT
// modified, mirroring the keystone testutil discipline.
func SetupC5c3EnvTestWithController(
	t testing.TB,
	addToScheme func(*k8sruntime.Scheme) error,
	registerWebhooks func(ctrl.Manager) error,
	registerController func(ctrl.Manager) error,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	return SetupC5c3EnvTestWithControllerAndCRDs(
		t,
		CRDDirectoryPaths(),
		addToScheme,
		registerWebhooks,
		registerController,
	)
}

// SetupC5c3EnvTestWithControllerAndCRDs is the caller-supplied-dirs variant of
// SetupC5c3EnvTestWithController: it is identical except the CRD directories
// envtest loads come from crdDirs rather than the full built-in set.
// SetupC5c3EnvTestWithController delegates here with CRDDirectoryPaths() (the
// full CRD set). Integration tests pass BaselineCRDDirectoryPaths() to model a
// cluster missing the sibling service-operator CRDs (Keystone, Horizon, Glance,
// Placement), proving the ControlPlane controller starts when those kinds are
// unserved.
func SetupC5c3EnvTestWithControllerAndCRDs(
	t testing.TB,
	crdDirs []string,
	addToScheme func(*k8sruntime.Scheme) error,
	registerWebhooks func(ctrl.Manager) error,
	registerController func(ctrl.Manager) error,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	return commonenvtest.StartManagedEnvTest(t, commonenvtest.ManagedEnvTestConfig{
		Name:               "c5c3",
		Scheme:             BuildControllerScheme(addToScheme),
		CRDDirectoryPaths:  crdDirs,
		WebhookDir:         C5c3WebhookDir(),
		RegisterWebhooks:   registerWebhooks,
		RegisterController: registerController,
	})
}

// CRDDirectoryPaths returns the absolute CRD directories envtest loads for a
// ControlPlane integration test, resolved relative to this
// source file via runtime.Caller(0):
//   - the sibling service-operator CRDs (Keystone, Horizon, Glance, Placement,
//     Barbican, Neutron — and with them GlanceBackend, KeystoneIdentityBackend
//     and BarbicanSecretStore) the reconciler Owns as children.
//   - the OVNCentral CRD, which the reconciler only reads and watches: it
//     mirrors the referenced central's readiness into OVNReady.
//   - BaselineCRDDirectoryPaths(): the c5c3 CRDs plus every shared fake CRD dir
//     (mariadb-operator, memcached-operator, external-secrets, cert-manager,
//     k-orc, openbao-operator, ...) so the external operator kinds the reconciler
//     create-or-updates resolve in the apiserver RESTMapper.
func CRDDirectoryPaths() []string {
	base := callerDir()
	keystoneCRDDir := filepath.Join(base, "..", "..", "..", "keystone", "config", "crd", "bases")
	horizonCRDDir := filepath.Join(base, "..", "..", "..", "horizon", "config", "crd", "bases")
	glanceCRDDir := filepath.Join(base, "..", "..", "..", "glance", "config", "crd", "bases")
	placementCRDDir := filepath.Join(base, "..", "..", "..", "placement", "config", "crd", "bases")
	barbicanCRDDir := filepath.Join(base, "..", "..", "..", "barbican", "config", "crd", "bases")
	neutronCRDDir := filepath.Join(base, "..", "..", "..", "neutron", "config", "crd", "bases")
	ovnCRDDir := filepath.Join(base, "..", "..", "..", "ovn", "config", "crd", "bases")

	dirs := []string{
		keystoneCRDDir, horizonCRDDir, glanceCRDDir, placementCRDDir, barbicanCRDDir,
		neutronCRDDir, ovnCRDDir,
	}
	return append(dirs, BaselineCRDDirectoryPaths()...)
}

// BaselineCRDDirectoryPaths returns the absolute CRD directories for an envtest
// environment carrying only the c5c3 CRDs and the shared fake CRDs for the hard
// infrastructure baseline (MariaDB, Memcached, external-secrets, cert-manager,
// K-ORC), resolved relative to this source file via runtime.Caller(0):
//   - c5c3 CRDs (controlplanes, credentialrotations, secretaggregates).
//   - every shared fake CRD dir under internal/common/testutil/fake_crds/*.
//
// The sibling service-operator CRDs (Keystone, Horizon, Glance, Placement, Barbican,
// Neutron — and with them GlanceBackend, KeystoneIdentityBackend and
// BarbicanSecretStore) plus the OVNCentral CRD are DELIBERATELY absent, so tests can
// prove the ControlPlane controller starts when those kinds are unserved. K-ORC IS
// served — its fake CRDs are part of the common fake dirs — because the K-ORC kinds
// are Owned unconditionally as hard dependencies (see optionalWatchObjects in
// crd_presence.go): the manager would fail to start without them. The two openbao.org kinds ARE served too, since their fake
// CRDs ship in those same common dirs, so the baseline also covers the mixed case a
// guarded leg has to survive: some optional CRDs installed, others not. Tests
// therefore exercise the intended partial state — the guarded service-operator legs
// are skipped while every unconditional infrastructure leg, K-ORC included, stays
// wired.
func BaselineCRDDirectoryPaths() []string {
	c5c3CRDDir := filepath.Join(callerDir(), "..", "..", "config", "crd", "bases")

	dirs := []string{c5c3CRDDir}
	return append(dirs, commonenvtest.CommonFakeCRDDirs()...)
}

// C5c3WebhookDir returns the absolute path to the c5c3 webhook configuration
// directory, resolved relative to this source file via runtime.Caller(0).
func C5c3WebhookDir() string {
	return filepath.Join(callerDir(), "..", "..", "config", "webhook")
}

// callerDir returns the directory containing this source file, resolved via
// runtime.Caller(0) so the absolute CRD/webhook paths are independent of the
// process working directory.
func callerDir() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("testutil: runtime.Caller failed to determine source file path")
	}
	return filepath.Dir(thisFile)
}

// BuildControllerScheme creates a runtime.Scheme that includes all types needed
// by the ControlPlaneReconciler: the c5c3 API types, core K8s types,
// apiextensions, and the external operator types the reconciler uses as TYPED
// client objects — MariaDB, Keystone, external-secrets (v1 + v1alpha1), and
// K-ORC. It is built fresh per test.
//
// It is exported for the dual-envtest multicluster test, which needs the scheme
// before StartManagedEnvTest runs: the manager, the target cluster's own client,
// and the kubeconfig provider's per-cluster clients all have to agree on it, and
// a client built on a scheme that does not know a CRD kind fails its first write
// with "no kind is registered".
//
// DECISION Memcached (memcached.c5c3.io) is deliberately NOT
// registered — it ships no Go module, so the reconciler handles it as an
// *unstructured.Unstructured carrying memcachedGVK (see reconcile_infrastructure.go).
// Its CRD is still loaded into envtest via the memcached-operator fake CRD dir so
// the apiserver can serve the unstructured object, but no scheme registration is
// required.
//
// DECISION cert-manager is NOT registered either — the
// ControlPlane reconciler references no cert-manager types (unlike the keystone
// reconciler), so adding certmanagerv1 to the scheme would promote an otherwise
// indirect dependency for no benefit. Its fake CRD remains loaded for parity with
// the shared fake_crds tree but needs no scheme entry.
func BuildControllerScheme(addToScheme func(*k8sruntime.Scheme) error) *k8sruntime.Scheme {
	return commonenvtest.BuildScheme(
		// External operator types the reconciler manipulates as typed objects.
		mariadbv1alpha1.AddToScheme,
		keystonev1alpha1.AddToScheme,
		horizonv1alpha1.AddToScheme,
		glancev1alpha1.AddToScheme,
		placementv1alpha1.AddToScheme,
		barbicanv1alpha1.AddToScheme,
		neutronv1alpha1.AddToScheme,
		ovnv1alpha1.AddToScheme,
		openbaov1alpha1.AddToScheme,
		esov1.AddToScheme,
		esov1alpha1.AddToScheme,
		esgenv1alpha1.AddToScheme,
		orcv1alpha1.AddToScheme,
		// c5c3 API types (ControlPlane, CredentialRotation, ...).
		addToScheme,
	)
}
