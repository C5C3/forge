// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Package controller contains the envtest integration test for the ControlPlane
// reconciler. Unlike the fake-client unit tests, this test
// runs the reconciler inside a real controller-runtime manager against a live
// envtest API server (CRDs + validating/defaulting webhook), and drives the full
// sub-reconciler chain — Infrastructure -> Keystone -> KORC -> AdminCredential ->
// Catalog — to the aggregate Ready=True by simulating each external dependency's
// readiness exactly as the production operators would report it.
package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	openbaov1alpha1 "github.com/dc-tec/openbao-operator/api/v1alpha1"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/c5c3/internal/testutil"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
	horizonv1alpha1 "github.com/c5c3/cobaltcore/operators/horizon/api/v1alpha1"
	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
	placementv1alpha1 "github.com/c5c3/cobaltcore/operators/placement/api/v1alpha1"
)

// Integration test timing constants. Polling is generous because every step
// waits on the manager's reconcile loop to observe an externally-simulated
// status transition and requeue (the sub-reconcilers requeue on the order of
// 5-15s, but condition flips are picked up on the next watch-triggered
// reconcile, so the timeouts only bound real stalls).
const (
	itEventuallyTimeout = 60 * time.Second
	itPollInterval      = 500 * time.Millisecond
)

// setupControlPlaneEnvTest wraps testutil.SetupC5c3EnvTestWithController with the
// c5c3 scheme, the ControlPlane webhook, and an INLINE controller builder.
//
// DECISION the controller is registered via an inline
// builder (For/Owns/Watches + the field-indexer registration) rather than by
// calling ControlPlaneReconciler.SetupWithManager directly, and uses
// WithOptions(controller.Options{SkipNameValidation: ptr.To(true)}). This mirrors
// the keystone integration wrapper and keeps the helper reusable if a second
// integration test function ever registers the controller in the same package
// test binary — controller-runtime rejects two controllers with the same name
// unless name validation is skipped. The builder is kept byte-for-byte in step
// with SetupWithManager (same Owns set, same Watches mapper, same indexer) so it
// exercises the real wiring.
//
// The ControlPlane chain runs ALONE here: nothing reconciles the KeystoneService
// children it projects for its built-in services, so those children stay inert.
// A scenario that has to watch one converge takes
// setupRegisteringControlPlaneEnvTest instead.
func setupControlPlaneEnvTest(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return startControlPlaneEnvTest(t, false)
}

// setupRegisteringControlPlaneEnvTest is setupControlPlaneEnvTest with the
// KeystoneService controller running beside the ControlPlane one, on the same
// manager and through its own production wiring (setupWithOptions, the chain
// SetupWithManager applies) with SkipNameValidation set — the pattern
// TestKeystoneServiceSetup_WatchesConverge registers it with.
//
// It is what a scenario driving a built-in service takes. Glance, Placement and
// Barbican hold their projection until the KeystoneService child they project
// reports AccountReady, so without a controller reconciling that child no service
// child is ever written.
func setupRegisteringControlPlaneEnvTest(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return startControlPlaneEnvTest(t, true)
}

// startControlPlaneEnvTest builds the environment both wrappers share.
//
// The KeystoneService ControlPlane-ref field index is registered by exactly ONE of
// the two chains. A field indexer rejects a second registration of the same key,
// and in production the registration controller's own setup is what installs it —
// so the ControlPlane builder installs it only when it runs alone, where
// reconcileRegistrationTenantStores would otherwise resolve its registrations
// through an index nobody registered.
func startControlPlaneEnvTest(
	t testing.TB, withRegistrationController bool,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupC5c3EnvTestWithController(
		t,
		c5c3v1alpha1.AddToScheme,
		func(mgr ctrl.Manager) error {
			// mgr.GetAPIReader() mirrors the production wiring in main.go: webhook
			// admission lookups read the API server directly, never a stale cache.
			if err := (&c5c3v1alpha1.ControlPlaneWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
				return err
			}
			// The webhook manifests installed by envtest carry the KeystoneService
			// entries (failurePolicy=Fail), so the handler must be served here too.
			return (&c5c3v1alpha1.KeystoneServiceWebhook{}).SetupWebhookWithManager(mgr)
		},
		func(mgr ctrl.Manager) error {
			r := &ControlPlaneReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("controlplane-controller"),
			}
			// Register the ControlPlane secret-name field indexer so
			// secretToControlPlaneMapper's MatchingFields lookup works, mirroring
			// what SetupWithManager does in production.
			if err := registerControlPlaneSecretNameIndex(context.Background(), mgr.GetFieldIndexer()); err != nil {
				return err
			}
			if !withRegistrationController {
				if err := registerKeystoneServiceControlPlaneRefIndex(context.Background(), mgr.GetFieldIndexer()); err != nil {
					return err
				}
			}

			memcached := &unstructured.Unstructured{}
			memcached.SetGroupVersionKind(memcachedGVK)

			if err := ctrl.NewControllerManagedBy(mgr).
				For(&c5c3v1alpha1.ControlPlane{}).
				Owns(&mariadbv1alpha1.MariaDB{}).
				Owns(&keystonev1alpha1.Keystone{}).
				Owns(&orcv1alpha1.ApplicationCredential{}).
				Owns(&orcv1alpha1.Service{}).
				Owns(&orcv1alpha1.Endpoint{}).
				Owns(memcached).
				// The registration children of the built-in services: their
				// AccountReady and aggregate Ready are what the Glance, Placement and
				// Barbican gates consume, and a co-located child carries a controller
				// owner reference.
				Owns(&c5c3v1alpha1.KeystoneService{}).
				// Mirror the identity-backend watch SetupWithManager registers, so
				// a backend event wakes the ControlPlane exactly as in production.
				Watches(&keystonev1alpha1.KeystoneIdentityBackend{}, handler.EnqueueRequestsFromMapFunc(
					r.identityBackendToControlPlaneMapper,
				)).
				Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(
					secretToControlPlaneMapper(mgr.GetClient()),
				)).
				// Mirror the cross-namespace watch legs SetupWithManager registers.
				// A child the ControlPlane placed in a service namespace carries no
				// owner reference (Kubernetes forbids one across namespaces), so the
				// Owns() legs above never fire for it: without these, a status
				// transition on such a child would only be picked up on the next
				// periodic requeue, and the harness would not reflect production.
				Watches(&keystonev1alpha1.Keystone{}, handler.EnqueueRequestsFromMapFunc(crossNamespaceChildMapper)).
				Watches(&mariadbv1alpha1.MariaDB{}, handler.EnqueueRequestsFromMapFunc(crossNamespaceChildMapper)).
				Watches(memcached, handler.EnqueueRequestsFromMapFunc(crossNamespaceChildMapper)).
				Watches(&esov1.ExternalSecret{}, handler.EnqueueRequestsFromMapFunc(crossNamespaceChildMapper)).
				Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(crossNamespaceChildMapper)).
				Watches(&c5c3v1alpha1.KeystoneService{},
					handler.EnqueueRequestsFromMapFunc(crossNamespaceChildMapper)).
				// Mirror the registration watch SetupWithManager registers, so a
				// KeystoneService appearing in or leaving an allowlisted namespace
				// moves the tenant-store provisioning set at watch latency here too.
				Watches(&c5c3v1alpha1.KeystoneService{},
					handler.EnqueueRequestsFromMapFunc(keystoneServiceToControlPlaneMapper)).
				WithOptions(controller.Options{SkipNameValidation: ptr.To(true)}).
				Complete(r); err != nil {
				return err
			}
			if !withRegistrationController {
				return nil
			}
			return (&KeystoneServiceReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("keystoneservice-controller"),
			}).setupWithOptions(mgr, controller.Options{SkipNameValidation: ptr.To(true)})
		},
	)
}

// integrationManagedControlPlane returns a valid managed-mode ControlPlane CR:
// database and cache reference managed clusters (clusterRef set), so the
// reconciler projects a MariaDB and a Memcached child. The spec satisfies the
// validating webhook (openStackRelease pattern, database/cache XOR,
// passwordSecretRef.name required); region / cloudCredentialsRef.secretName /
// applicationCredential.restricted / rotation.mode are left for the defaulting
// webhook to fill.
func integrationManagedControlPlane(name, namespace string) *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Infrastructure: &c5c3v1alpha1.InfrastructureSpec{
				Database: commonv1.DatabaseSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "openstack-db"},
					Database:   "keystone",
					SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db"},
				},
				Cache: commonv1.CacheSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "openstack-memcached"},
					Backend:    "dogpile.cache.pymemcache",
					Replicas:   3,
				},
			},
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
					Replicas: ptr.To(int32(3)),
				},
			},
			// One global oslo.policy override so the test can assert the reconciler
			// merges it into the projected Keystone CR's PolicyOverrides.
			GlobalPolicyOverrides: &commonv1.PolicySpec{
				Rules: map[string]string{"identity:list_users": "role:admin"},
			},
			KORC: c5c3v1alpha1.KORCSpec{
				AdminCredential: c5c3v1alpha1.AdminCredentialSpec{
					CloudCredentialsRef: c5c3v1alpha1.CloudCredentialsRef{
						CloudName: "admin",
					},
					// DECISION the spec-level ref is kept at the canonical
					// brownfield default "keystone-admin" (== DefaultAdminPasswordSecretName)
					// rather than renamed to adminPasswordSecretName(cp). In managed mode
					// effectiveAdminPasswordSecretRef ALWAYS overrides to adminPasswordSecretName(cp)
					// regardless of this value, so keeping it distinct makes the projected-child
					// admin-ref assertions below genuinely prove the override (the projected name
					// differs from this spec ref) — exactly mirroring the DB-credential fixture,
					// whose spec ref stays "keystone-db" != dbCredentialSecretName(cp). The
					// pre-created cleartext Secret is named adminPasswordSecretName(cp) (the name
					// readAdminPassword resolves via the effective ref). Reviewer: please verify.
					PasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin", Key: "password"},
				},
			},
		},
	}
}

// integrationMinimalControlPlane returns a ControlPlane with ONLY the two
// genuinely-required user inputs set — openStackRelease and the keystone service
// block — and spec.infrastructure / spec.korc OMITTED (zero structs). The
// defaulting webhook must therefore construct the database, cache, and
// admin-credential blocks from its well-known defaults; TestIntegration_MinimalManagedToReady
// asserts it does and that the CR still converges to Ready=True.
func integrationMinimalControlPlane(name, namespace string) *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
					Replicas: ptr.To(int32(1)),
				},
			},
		},
	}
}

// integrationGlanceService returns a valid services.glance block: a single
// default S3 backend pointed at the in-cluster Garage endpoint. It is shared by
// the full-chain projection test (whose Phase 6 asserts the projected
// GlanceBackend's host and default flag) and the External-mode rejection case,
// so the one curated backend shape is defined in a single place.
func integrationGlanceService() *c5c3v1alpha1.ServiceGlanceSpec {
	return &c5c3v1alpha1.ServiceGlanceSpec{
		Backends: []c5c3v1alpha1.GlanceBackendEntry{{
			Name: "default",
			// The GlanceBackendEntry.Type field is a plain string with a
			// +kubebuilder:validation:Enum=S3 marker — the c5c3 api package defines no
			// Go const for it — so the literal "S3" is the enum value.
			Type:      "S3",
			IsDefault: true,
			S3: &c5c3v1alpha1.GlanceBackendS3Spec{
				Endpoint:             "http://garage.shared-services.svc.cluster.local:3900",
				Bucket:               "glance-images",
				Region:               "garage",
				CredentialsSecretRef: c5c3v1alpha1.SecretNameRef{Name: "garage-s3-credentials"},
			},
		}},
	}
}

// integrationPlacementService returns a valid services.placement block. Unlike
// its Glance sibling it carries no curated sub-block: every ServicePlacementSpec
// field is optional and the projection derives the whole child from the
// ControlPlane, so the empty block is the minimal valid one. It is shared by the
// full-chain projection test, the tail-group test, and the External-mode
// rejection case, so the enabling shape is written once.
func integrationPlacementService() *c5c3v1alpha1.ServicePlacementSpec {
	return &c5c3v1alpha1.ServicePlacementSpec{}
}

// integrationBarbicanService returns a valid services.barbican block: one replica
// on a secret store the ControlPlane provisions for it. Unlike its Placement
// sibling the block cannot be empty — secretStore is required and admits exactly
// one of dedicated/external — and the dedicated mode is the one that puts the whole
// OpenBao ensemble under test. It is shared by the full-chain projection test, the
// tail-group test, the service-account injection test, and the cross-namespace
// teardown test, so the enabling shape is written once.
func integrationBarbicanService() *c5c3v1alpha1.ServiceBarbicanSpec {
	return &c5c3v1alpha1.ServiceBarbicanSpec{
		Replicas: ptr.To(int32(1)),
		SecretStore: c5c3v1alpha1.ServiceBarbicanSecretStoreSpec{
			Dedicated: &c5c3v1alpha1.BarbicanDedicatedSecretStoreSpec{},
		},
	}
}

// integrationNeutronService returns a valid services.neutron block: the required
// OVN control plane reference and a single RPC worker. The block cannot be empty
// the way its Placement sibling can, because the ML2/OVN mechanism driver has no
// logical network model to write to without a central. workerReplicas is pinned
// so the full-chain test can prove the override reaches the child's
// spec.workers.deployment.replicas, while replicas is left unset so the same test
// proves the API pods fall back to the shared operator default.
//
// The centralRef spells no namespace on purpose: the defaulting webhook fills an
// empty one with the ControlPlane's own namespace, and NeutronOVNCentralNamespace()
// resolves it the same way for a CR that bypassed admission. That is where the
// tests create the OVNCentral.
func integrationNeutronService() *c5c3v1alpha1.ServiceNeutronSpec {
	return &c5c3v1alpha1.ServiceNeutronSpec{
		WorkerReplicas: ptr.To(int32(1)),
		OVN: c5c3v1alpha1.NeutronOVNSpec{
			CentralRef: c5c3v1alpha1.NeutronOVNCentralRef{Name: "cp-ovn"},
		},
	}
}

// ensureReadyClusterSecretStore creates the cluster-scoped OpenBao-backed
// ClusterSecretStore the DB-credential, admin-password and admin-credential
// sub-reconcilers gate on (#476) and marks it Ready. It is idempotent across the
// shared envtest cluster: the store is cluster-scoped, so a second test reuses
// the existing object and only refreshes its Ready status. Call it before
// creating a ControlPlane so the first reconcile sees the store Ready and the
// credential gates open; without it the chain stalls at DBCredentialsReady=False
// with reason SecretStoreNotReady. Mirrors the keystone operator's helper.
func ensureReadyClusterSecretStore(t testing.TB, ctx context.Context, c client.Client) {
	t.Helper()
	g := NewGomegaWithT(t)

	store := &esov1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: openBaoClusterStoreName},
	}
	err := c.Get(ctx, client.ObjectKeyFromObject(store), store)
	if apierrors.IsNotFound(err) {
		g.Expect(c.Create(ctx, store)).To(Succeed(), "create ClusterSecretStore")
	} else {
		g.Expect(err).NotTo(HaveOccurred(), "get ClusterSecretStore")
	}

	store.Status = esov1.SecretStoreStatus{
		Conditions: []esov1.SecretStoreStatusCondition{
			{Type: esov1.SecretStoreReady, Status: corev1.ConditionTrue},
		},
	}
	g.Expect(c.Status().Update(ctx, store)).To(Succeed(), "update ClusterSecretStore status")
}

// waitForControlPlaneCondition polls the ControlPlane CR until the named
// condition reaches the expected status, or the timeout is reached. Returns the
// observed condition.
func waitForControlPlaneCondition(
	t testing.TB, ctx context.Context, c client.Client,
	key types.NamespacedName, condType string, expected metav1.ConditionStatus, timeout time.Duration,
) *metav1.Condition {
	t.Helper()
	g := NewGomegaWithT(t)

	var cond *metav1.Condition
	g.Eventually(func() metav1.ConditionStatus {
		cp := &c5c3v1alpha1.ControlPlane{}
		if err := c.Get(ctx, key, cp); err != nil {
			return ""
		}
		cond = meta.FindStatusCondition(cp.Status.Conditions, condType)
		if cond == nil {
			return ""
		}
		return cond.Status
	}, timeout, itPollInterval).Should(Equal(expected),
		fmt.Sprintf("ControlPlane condition %s should reach %s", condType, expected))

	return cond
}

// simulateMariaDBReadyWhenPresent waits for the projected MariaDB child to be
// created by reconcileInfrastructure, then sets its status Ready=True via the
// shared simulator so InfrastructureReady can advance.
func simulateMariaDBReadyWhenPresent(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	g.Eventually(func() error {
		return c.Get(ctx, key, &mariadbv1alpha1.MariaDB{})
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "MariaDB child should be created")
	g.Expect(simulators.SimulateMariaDBReady(ctx, c, key, 3)).To(Succeed(), "simulate MariaDB ready")
}

// simulateMemcachedReadyWhenPresent waits for the projected (unstructured)
// Memcached child, then sets its status Ready=True via the shared simulator
// (which targets the same memcachedGVK the reconciler uses).
func simulateMemcachedReadyWhenPresent(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(memcachedGVK)
	g.Eventually(func() error {
		return c.Get(ctx, key, u)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "Memcached child should be created")
	g.Expect(simulators.SimulateMemcachedReady(ctx, c, key, 3, []string{"openstack-memcached:11211"})).
		To(Succeed(), "simulate Memcached ready")
}

// simulateRabbitmqClusterReadyWhenPresent waits for the projected (unstructured)
// RabbitmqCluster child, then reports what the RabbitMQ Cluster Operator sets on
// a healthy broker via the shared simulator: AllReplicasReady, ClusterAvailable
// and ReconcileSuccess True, plus the default-user secret reference. The operator
// publishes no Ready condition, so AllReplicasReady is the one the infrastructure
// gate reads.
func simulateRabbitmqClusterReadyWhenPresent(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(messaging.RabbitmqClusterGVK)
	g.Eventually(func() error {
		return c.Get(ctx, key, u)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "RabbitmqCluster child should be created")
	g.Expect(simulators.SimulateRabbitmqClusterReady(ctx, c, key, key.Name+"-default-user")).
		To(Succeed(), "simulate RabbitmqCluster ready")
}

// simulateKeystoneReadyWhenPresent waits for the projected Keystone child, then
// sets its Ready condition True inline (there is no Keystone simulator — the
// reconcileKeystone gate mirrors the child Ready condition).
func simulateKeystoneReadyWhenPresent(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	ks := &keystonev1alpha1.Keystone{}
	g.Eventually(func() error {
		return c.Get(ctx, key, ks)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "Keystone child should be created")

	meta.SetStatusCondition(&ks.Status.Conditions, metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionTrue,
		Reason:  "KeystoneReady",
		Message: "simulated ready",
	})
	g.Expect(c.Status().Update(ctx, ks)).To(Succeed(), "set Keystone Ready=True")
}

// simulateHorizonReadyWhenPresent waits for the projected Horizon child, then
// sets its aggregate Ready condition True so reconcileHorizon's mirror flips
// HorizonReady (there is no horizon-operator running in envtest).
func simulateHorizonReadyWhenPresent(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	hz := &horizonv1alpha1.Horizon{}
	g.Eventually(func() error {
		return c.Get(ctx, key, hz)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "Horizon child should be created")

	meta.SetStatusCondition(&hz.Status.Conditions, metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionTrue,
		Reason:  "AllReady",
		Message: "simulated ready",
	})
	g.Expect(c.Status().Update(ctx, hz)).To(Succeed(), "set Horizon Ready=True")
}

// simulateGlanceReadyWhenPresent waits for the projected Glance child, then sets
// its aggregate Ready condition True so reconcileGlance's mirror flips GlanceReady
// (there is no glance-operator running in envtest). Mirrors
// simulateHorizonReadyWhenPresent.
func simulateGlanceReadyWhenPresent(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	gl := &glancev1alpha1.Glance{}
	g.Eventually(func() error {
		return c.Get(ctx, key, gl)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "Glance child should be created")

	meta.SetStatusCondition(&gl.Status.Conditions, metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionTrue,
		Reason:  "AllReady",
		Message: "simulated ready",
	})
	g.Expect(c.Status().Update(ctx, gl)).To(Succeed(), "set Glance Ready=True")
}

// simulatePlacementReadyWhenPresent waits for the projected Placement child, then
// sets its aggregate Ready condition True so reconcilePlacement's mirror flips
// PlacementReady (there is no placement-operator running in envtest). Mirrors
// simulateGlanceReadyWhenPresent.
func simulatePlacementReadyWhenPresent(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	pl := &placementv1alpha1.Placement{}
	g.Eventually(func() error {
		return c.Get(ctx, key, pl)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "Placement child should be created")

	meta.SetStatusCondition(&pl.Status.Conditions, metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionTrue,
		Reason:  "AllReady",
		Message: "simulated ready",
	})
	g.Expect(c.Status().Update(ctx, pl)).To(Succeed(), "set Placement Ready=True")
}

// simulateBarbicanReadyWhenPresent waits for the projected Barbican child, then
// sets its aggregate Ready condition True so reconcileBarbican's mirror flips
// BarbicanReady (there is no barbican-operator running in envtest). Mirrors
// simulatePlacementReadyWhenPresent.
func simulateBarbicanReadyWhenPresent(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	bn := &barbicanv1alpha1.Barbican{}
	g.Eventually(func() error {
		return c.Get(ctx, key, bn)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "Barbican child should be created")

	meta.SetStatusCondition(&bn.Status.Conditions, metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionTrue,
		Reason:  "AllReady",
		Message: "simulated ready",
	})
	g.Expect(c.Status().Update(ctx, bn)).To(Succeed(), "set Barbican Ready=True")
}

// simulateNeutronReadyWhenPresent waits for the projected Neutron child, then
// sets its aggregate Ready condition True so reconcileNeutron's mirror flips
// NeutronReady (there is no neutron-operator running in envtest). Mirrors
// simulateBarbicanReadyWhenPresent.
func simulateNeutronReadyWhenPresent(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	nn := &neutronv1alpha1.Neutron{}
	g.Eventually(func() error {
		return c.Get(ctx, key, nn)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "Neutron child should be created")

	meta.SetStatusCondition(&nn.Status.Conditions, metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionTrue,
		Reason:  "AllReady",
		Message: "simulated ready",
	})
	g.Expect(c.Status().Update(ctx, nn)).To(Succeed(), "set Neutron Ready=True")
}

// simulateOVNCentralReadyWhenPresent waits for the OVNCentral
// services.neutron.ovn.centralRef names, then reports what the ovn-operator
// publishes on a serving control plane: Ready=True plus the three status values
// reconcileOVN reads, the two in-cluster database addresses and the client Secret
// every OVN client authenticates with.
//
// The central is an INPUT rather than a projected child: the ControlPlane never
// creates it, only reads it, so the test creates it and this helper drives it the
// way the external operator would. The condition carries an ObservedGeneration
// because that is what a real operator stamps.
func simulateOVNCentralReadyWhenPresent(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	central := &ovnv1alpha1.OVNCentral{}
	g.Eventually(func() error {
		return c.Get(ctx, key, central)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "the referenced OVNCentral should exist")

	central.Status.Northbound.InternalDbAddress = "ssl:10.0.0.1:6641"
	central.Status.Southbound.InternalDbAddress = "ssl:10.0.0.1:6642"
	central.Status.ClientSecretName = key.Name + "-client"
	meta.SetStatusCondition(&central.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: central.Generation,
		Reason:             "AllReady",
		Message:            "simulated ready",
	})
	g.Expect(c.Status().Update(ctx, central)).To(Succeed(), "set OVNCentral Ready=True")
}

// simulateOpenBaoClusterAvailableWhenPresent waits for the dedicated OpenBao
// instance the Barbican secret store points at, then sets its Available condition
// True — the signal the openbao-operator raises once the instance is initialised,
// unsealed, and serving requests. No openbao-operator runs in envtest, so without
// this the Barbican store and child stay behind
// BarbicanReady=False/WaitingForOpenBaoInstance forever.
func simulateOpenBaoClusterAvailableWhenPresent(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	instance := &openbaov1alpha1.OpenBaoCluster{}
	g.Eventually(func() error {
		return c.Get(ctx, key, instance)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "the dedicated OpenBaoCluster should be created")

	meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
		Type:    string(openbaov1alpha1.ConditionAvailable),
		Status:  metav1.ConditionTrue,
		Reason:  "Available",
		Message: "simulated available",
	})
	g.Expect(c.Status().Update(ctx, instance)).To(Succeed(), "set OpenBaoCluster Available=True")
}

// simulateApplicationCredentialAvailableWhenPresent waits for the owned K-ORC
// ApplicationCredential, then sets its Available condition True and a status.id
// inline (there is no K-ORC simulator — reconcileKORC gates KORCReady on
// orcv1alpha1.IsAvailable and reflects status.id into the ControlPlane status).
func simulateApplicationCredentialAvailableWhenPresent(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	ac := &orcv1alpha1.ApplicationCredential{}
	g.Eventually(func() error {
		return c.Get(ctx, key, ac)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "ApplicationCredential should be minted")

	ac.Status.ID = ptr.To("ac-id-integration")
	meta.SetStatusCondition(&ac.Status.Conditions, metav1.Condition{
		Type:    orcv1alpha1.ConditionAvailable,
		Status:  metav1.ConditionTrue,
		Reason:  orcv1alpha1.ConditionReasonSuccess,
		Message: "simulated available",
	})
	g.Expect(c.Status().Update(ctx, ac)).To(Succeed(), "set ApplicationCredential Available=True")
}

// simulatePushSecretSyncedWhenPresent waits for the named PushSecret to be
// created, then sets its Ready condition True via the shared simulator. There is
// no ESO controller in envtest, so reconcileAdminCredential — which gates
// AdminCredentialReady on the admin app-credential PushSecret actually syncing to
// OpenBao — would otherwise wait forever. SimulatePushSecretSynced
// returns an error until the PushSecret exists, so polling it doubles as the
// "WhenPresent" wait without needing the esov1alpha1 type here.
func simulatePushSecretSyncedWhenPresent(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	g.Eventually(func() error {
		return simulators.SimulatePushSecretSynced(ctx, c, key)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"admin app-credential PushSecret should be created and synced")
}

// simulateCloudsYamlMaterializedWhenPresent performs the ESO round-trip envtest has
// no controller for: it reads the operator-owned app-credential Secret the PushSecret
// mirrors to OpenBao and writes its assembled clouds.yaml into the k-orc-clouds-yaml
// Secret K-ORC authenticates with. reconcileAdminCredential now byte-compares the
// materialized Secret against the freshly assembled clouds.yaml before flipping
// AdminCredentialReady True (closing the post-re-mint stale-credential window), so
// without this materialisation the gate would wait forever.
func simulateCloudsYamlMaterializedWhenPresent(t testing.TB, ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane) {
	t.Helper()
	g := NewGomegaWithT(t)

	name := cp.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName
	if name == "" {
		name = korcCloudsYamlSecretName
	}

	// Wait for the operator-owned Secret to hold the MINTED application-credential
	// clouds.yaml, not the password-based bootstrap seed: reconcileKORC creates the
	// PushSecret (and seeds the password clouds.yaml) before reconcileAdminCredential
	// overwrites it with the app-credential document, so copying too early would
	// materialise the wrong bytes and the byte-compare gate would never match.
	src := &corev1.Secret{}
	g.Eventually(func() error {
		if err := c.Get(ctx, client.ObjectKey{Namespace: childNamespace(cp), Name: adminAppCredentialSecretName(cp)}, src); err != nil {
			return err
		}
		if !strings.Contains(string(src.Data[appCredCloudsYAMLKey]), "application_credential_id") {
			return fmt.Errorf("operator-owned Secret still holds the password seed, not the minted app-credential clouds.yaml")
		}
		return nil
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"operator must assemble the app-credential clouds.yaml before ESO can materialise it back")

	materialized := &corev1.Secret{}
	err := c.Get(ctx, client.ObjectKey{Namespace: childNamespace(cp), Name: name}, materialized)
	switch {
	case apierrors.IsNotFound(err):
		materialized = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: childNamespace(cp)},
			Data:       map[string][]byte{appCredCloudsYAMLKey: src.Data[appCredCloudsYAMLKey]},
		}
		g.Expect(c.Create(ctx, materialized)).To(Succeed(), "materialize the k-orc clouds.yaml Secret")
	case err == nil:
		if materialized.Data == nil {
			materialized.Data = map[string][]byte{}
		}
		materialized.Data[appCredCloudsYAMLKey] = src.Data[appCredCloudsYAMLKey]
		g.Expect(c.Update(ctx, materialized)).To(Succeed(), "refresh the materialized k-orc clouds.yaml Secret")
	default:
		g.Expect(err).NotTo(HaveOccurred(), "get materialized k-orc clouds.yaml Secret")
	}
}

// simulateCatalogServiceEndpointAvailableWhenPresent waits for the owned K-ORC
// identity Service and Endpoint, then sets their Available condition True inline.
// reconcileCatalog now gates CatalogReady on both child CRs reporting Available
// (registering them is not enough — the catalog entry must actually land in
// Keystone), and there is no K-ORC controller in envtest to mark them Available.
func simulateCatalogServiceEndpointAvailableWhenPresent(t testing.TB, ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane) {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := childNamespace(cp)

	svc := &orcv1alpha1.Service{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns, Name: keystoneServiceName(cp)}, svc)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "identity Service should be registered")
	meta.SetStatusCondition(&svc.Status.Conditions, metav1.Condition{
		Type:   orcv1alpha1.ConditionAvailable,
		Status: metav1.ConditionTrue,
		Reason: orcv1alpha1.ConditionReasonSuccess,
		// reconcileCatalog gates on korcAvailableUpToDate, which requires the
		// Available condition's ObservedGeneration to match the object's
		// generation — mirror what the real K-ORC actuator stamps so the gate
		// flips True (the in-cluster apiserver assigns Generation>=1 on create).
		ObservedGeneration: svc.Generation,
		Message:            "simulated available",
	})
	g.Expect(c.Status().Update(ctx, svc)).To(Succeed(), "set identity Service Available=True")

	ep := &orcv1alpha1.Endpoint{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns, Name: keystoneEndpointName(cp)}, ep)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "identity Endpoint should be registered")
	meta.SetStatusCondition(&ep.Status.Conditions, metav1.Condition{
		Type:               orcv1alpha1.ConditionAvailable,
		Status:             metav1.ConditionTrue,
		Reason:             orcv1alpha1.ConditionReasonSuccess,
		ObservedGeneration: ep.Generation,
		Message:            "simulated available",
	})
	g.Expect(c.Status().Update(ctx, ep)).To(Succeed(), "set identity Endpoint Available=True")
}

// waitForBuiltinRegistration polls for the KeystoneService child a built-in
// service's sub-reconciler projects — "<controlplane>-<service>" in the namespace
// that service is assigned to — and returns the live CR. Every child name the two
// simulators below address is derived from it, so a renamed child fails this
// lookup instead of silently changing what is being driven.
func waitForBuiltinRegistration(
	t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey,
) *c5c3v1alpha1.KeystoneService {
	t.Helper()
	g := NewGomegaWithT(t)

	ks := &c5c3v1alpha1.KeystoneService{}
	g.Eventually(func() error {
		return c.Get(ctx, key, ks)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"the KeystoneService child %s should be projected", key)
	return ks
}

// simulateBuiltinRegistrationConvergedWhenPresent drives one projected
// KeystoneService child to its aggregate Ready in envtest, where neither a K-ORC
// nor an ESO controller runs, and returns the child it drove.
//
// Both declared blocks are driven in one call because the registration controller
// runs them independently on every pass and the ControlPlane folds both into the
// service's readiness: the account gates whether a service child is projected at
// all, the catalog gates whether that service is reported ready.
func simulateBuiltinRegistrationConvergedWhenPresent(
	t testing.TB, ctx context.Context, c client.Client,
	cp *c5c3v1alpha1.ControlPlane, key client.ObjectKey,
) *c5c3v1alpha1.KeystoneService {
	t.Helper()

	ks := waitForBuiltinRegistration(t, ctx, c, key)
	simulateRegistrationCatalogAvailableWhenPresent(t, ctx, c, cp, ks)
	simulateRegistrationAccountConvergedWhenPresent(t, ctx, c, cp, ks)
	return ks
}

// simulateRegistrationCatalogAvailableWhenPresent resolves the catalog block of a
// KeystoneService child: the collision probe reports the row ABSENT so the
// registration creates the managed objects, and the managed Service plus one
// Endpoint per declared interface report Available for their current generation
// (korcAvailableUpToDate refuses a stale condition). That is what flips the child's
// CatalogReady.
//
// The K-ORC children live in the ControlPlane's namespace whatever namespace the
// registration itself sits in — they resolve the admin clouds.yaml, which is only
// materialised there.
func simulateRegistrationCatalogAvailableWhenPresent(
	t testing.TB, ctx context.Context, c client.Client,
	cp *c5c3v1alpha1.ControlPlane, ks *c5c3v1alpha1.KeystoneService,
) {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := keystoneServiceChildNamespace(cp)

	// Available=False on the "created externally" marker is what
	// korcImportPendingExternal reads as "no such catalog row", so the registration
	// drops the probe and creates the managed Service.
	probe := &orcv1alpha1.Service{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns, Name: keystoneServiceCatalogServiceProbeRef(ks)}, probe)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "the catalog Service probe should be created")
	meta.SetStatusCondition(&probe.Status.Conditions, metav1.Condition{
		Type:    orcv1alpha1.ConditionAvailable,
		Status:  metav1.ConditionFalse,
		Reason:  orcv1alpha1.ConditionReasonProgressing,
		Message: korcImportPendingExternalMarker,
	})
	g.Expect(c.Status().Update(ctx, probe)).To(Succeed(), "mark the catalog Service probe absent")

	svc := &orcv1alpha1.Service{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns, Name: keystoneServiceCatalogServiceRef(ks)}, svc)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "the managed catalog Service should be registered")
	meta.SetStatusCondition(&svc.Status.Conditions, metav1.Condition{
		Type:               orcv1alpha1.ConditionAvailable,
		Status:             metav1.ConditionTrue,
		Reason:             orcv1alpha1.ConditionReasonSuccess,
		ObservedGeneration: svc.Generation,
		Message:            "simulated available",
	})
	svc.Status.ID = ptr.To("registration-service-id")
	g.Expect(c.Status().Update(ctx, svc)).To(Succeed(), "set the catalog Service Available=True")

	// Both interfaces (D6): a built-in row registers an internal and a public
	// Endpoint from the start.
	for _, entry := range ks.Spec.Catalog.Endpoints {
		ep := &orcv1alpha1.Endpoint{}
		g.Eventually(func() error {
			return c.Get(ctx, client.ObjectKey{
				Namespace: ns, Name: keystoneServiceCatalogEndpointRef(ks, entry.Interface),
			}, ep)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
			"the %q catalog Endpoint should be registered", entry.Interface)
		meta.SetStatusCondition(&ep.Status.Conditions, metav1.Condition{
			Type:               orcv1alpha1.ConditionAvailable,
			Status:             metav1.ConditionTrue,
			Reason:             orcv1alpha1.ConditionReasonSuccess,
			ObservedGeneration: ep.Generation,
			Message:            "simulated available",
		})
		g.Expect(c.Status().Update(ctx, ep)).To(Succeed(),
			"set the %q catalog Endpoint Available=True", entry.Interface)
	}
}

// simulateRegistrationAccountConvergedWhenPresent drives the account block of a
// KeystoneService child through to AccountReady, over the child's own names: the
// collision probes resolve to ABSENT, the managed Project and User (with the
// current generation's password applied), the Role import and the RoleAssignment
// report Available, and the OpenBao round-trip behind the consumer Secret is
// replayed by hand.
//
// The K-ORC children and the password Secret live in the ControlPlane's namespace;
// the delivery objects — source Secret, PushSecret, consumer Secret — stay in the
// registration's own namespace, which for a service placed in a namespace of its
// own is not the same one.
func simulateRegistrationAccountConvergedWhenPresent(
	t testing.TB, ctx context.Context, c client.Client,
	cp *c5c3v1alpha1.ControlPlane, ks *c5c3v1alpha1.KeystoneService,
) {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := keystoneServiceChildNamespace(cp)

	// An account that CREATES its project is probe-gated on the project first:
	// ensureKeystoneServiceProject returns on the pending probe before the user leg
	// runs, so the User probe below does not exist until this one is resolved.
	if ks.Spec.Account.Project.Create {
		projectProbe := &orcv1alpha1.Project{}
		g.Eventually(func() error {
			return c.Get(ctx, client.ObjectKey{Namespace: ns, Name: keystoneServiceProjectProbeRef(ks)}, projectProbe)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "the registration Project probe should be created")
		meta.SetStatusCondition(&projectProbe.Status.Conditions, metav1.Condition{
			Type:    orcv1alpha1.ConditionAvailable,
			Status:  metav1.ConditionFalse,
			Reason:  orcv1alpha1.ConditionReasonProgressing,
			Message: korcImportPendingExternalMarker,
		})
		g.Expect(c.Status().Update(ctx, projectProbe)).To(Succeed(), "mark the Project probe absent")
	}

	probe := &orcv1alpha1.User{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns, Name: keystoneServiceUserProbeRef(ks)}, probe)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "the registration User probe should be created")
	meta.SetStatusCondition(&probe.Status.Conditions, metav1.Condition{
		Type:    orcv1alpha1.ConditionAvailable,
		Status:  metav1.ConditionFalse,
		Reason:  orcv1alpha1.ConditionReasonProgressing,
		Message: korcImportPendingExternalMarker,
	})
	g.Expect(c.Status().Update(ctx, probe)).To(Succeed(), "mark the User probe absent")

	project := &orcv1alpha1.Project{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns, Name: keystoneServiceProjectRef(ks)}, project)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "the registration Project should be created")
	meta.SetStatusCondition(&project.Status.Conditions, metav1.Condition{
		Type:               orcv1alpha1.ConditionAvailable,
		Status:             metav1.ConditionTrue,
		Reason:             orcv1alpha1.ConditionReasonSuccess,
		ObservedGeneration: project.Generation,
		Message:            "simulated available",
	})
	project.Status.ID = ptr.To("registration-project-id")
	g.Expect(c.Status().Update(ctx, project)).To(Succeed(), "resolve the registration Project")

	user := &orcv1alpha1.User{}
	g.Eventually(func() error {
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: keystoneServiceUserRef(ks)}, user); err != nil {
			return err
		}
		if user.Spec.Resource == nil || user.Spec.Resource.PasswordRef == nil {
			return fmt.Errorf("managed User has no passwordRef yet")
		}
		return nil
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"the registration's managed User should be created with a passwordRef")
	meta.SetStatusCondition(&user.Status.Conditions, metav1.Condition{
		Type:    orcv1alpha1.ConditionAvailable,
		Status:  metav1.ConditionTrue,
		Reason:  orcv1alpha1.ConditionReasonSuccess,
		Message: "simulated available",
	})
	user.Status.ID = ptr.To("registration-user-id")
	user.Status.Resource = &orcv1alpha1.UserResourceStatus{AppliedPasswordRef: string(*user.Spec.Resource.PasswordRef)}
	g.Expect(c.Status().Update(ctx, user)).To(Succeed(), "mark the registration's managed User Available")

	for _, role := range ks.Spec.Account.Roles {
		roleImport := &orcv1alpha1.Role{}
		g.Eventually(func() error {
			return c.Get(ctx, client.ObjectKey{Namespace: ns, Name: keystoneServiceRoleImportRef(ks, role)}, roleImport)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "the %q Role import should be created", role)
		meta.SetStatusCondition(&roleImport.Status.Conditions, metav1.Condition{
			Type:               orcv1alpha1.ConditionAvailable,
			Status:             metav1.ConditionTrue,
			Reason:             orcv1alpha1.ConditionReasonSuccess,
			ObservedGeneration: roleImport.Generation,
			Message:            "simulated available",
		})
		roleImport.Status.ID = ptr.To("registration-role-id")
		g.Expect(c.Status().Update(ctx, roleImport)).To(Succeed(), "resolve the %q Role import", role)

		assignment := &orcv1alpha1.RoleAssignment{}
		g.Eventually(func() error {
			return c.Get(ctx, client.ObjectKey{
				Namespace: ns, Name: keystoneServiceRoleAssignmentRef(ks, role),
			}, assignment)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "the %q RoleAssignment should be created", role)
		meta.SetStatusCondition(&assignment.Status.Conditions, metav1.Condition{
			Type:               orcv1alpha1.ConditionAvailable,
			Status:             metav1.ConditionTrue,
			Reason:             orcv1alpha1.ConditionReasonSuccess,
			ObservedGeneration: assignment.Generation,
			Message:            "simulated available",
		})
		g.Expect(c.Status().Update(ctx, assignment)).To(Succeed(), "mark the %q RoleAssignment Available", role)
	}

	// PushSecret sync + consumer-Secret materialisation (the ESO round-trip the
	// account's readiness gate ends on), in the registration's own namespace.
	g.Eventually(func() error {
		return simulators.SimulatePushSecretSynced(ctx, c,
			client.ObjectKey{Namespace: ks.Namespace, Name: keystoneServicePushSecretName(ks)})
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "the registration PushSecret should sync")

	src := &corev1.Secret{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ks.Namespace, Name: keystoneServiceSourceSecretName(ks)}, src)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"the registration must assemble its source Secret")

	name := keystoneServiceCredentialsSecretName(ks)
	materialized := &corev1.Secret{}
	switch err := c.Get(ctx, client.ObjectKey{Namespace: ks.Namespace, Name: name}, materialized); {
	case apierrors.IsNotFound(err):
		materialized = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ks.Namespace},
			Data:       map[string][]byte{serviceAccountPasswordKey: src.Data[serviceAccountPasswordKey]},
		}
		g.Expect(c.Create(ctx, materialized)).To(Succeed(), "materialize the registration's consumer Secret")
	case err == nil:
		if materialized.Data == nil {
			materialized.Data = map[string][]byte{}
		}
		materialized.Data[serviceAccountPasswordKey] = src.Data[serviceAccountPasswordKey]
		g.Expect(c.Update(ctx, materialized)).To(Succeed(), "refresh the registration's materialized Secret")
	default:
		g.Expect(err).NotTo(HaveOccurred(), "get the registration's materialized Secret")
	}
}

// simulateExternalCatalogImportsAvailableWhenPresent waits for the four UNMANAGED
// catalog import CRs an External-mode ControlPlane projects — the identity Service
// plus one Endpoint per interface — and resolves each one inline: Available=True
// with a matching ObservedGeneration (korcAvailableUpToDate refuses a stale
// condition) and a resolved OpenStack id. That is what K-ORC stamps once an import
// matches a live catalog entry, and there is no K-ORC controller in envtest.
func simulateExternalCatalogImportsAvailableWhenPresent(
	t testing.TB, ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane,
) {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := childNamespace(cp)

	svc := &orcv1alpha1.Service{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns, Name: keystoneServiceName(cp)}, svc)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "the identity Service import should be projected")
	meta.SetStatusCondition(&svc.Status.Conditions, metav1.Condition{
		Type:               orcv1alpha1.ConditionAvailable,
		Status:             metav1.ConditionTrue,
		Reason:             orcv1alpha1.ConditionReasonSuccess,
		ObservedGeneration: svc.Generation,
		Message:            "simulated import resolved",
	})
	svc.Status.ID = ptr.To("simulated-identity-service-id")
	g.Expect(c.Status().Update(ctx, svc)).To(Succeed(), "resolve the identity Service import")

	for _, iface := range externalCatalogInterfaces {
		ep := &orcv1alpha1.Endpoint{}
		g.Eventually(func() error {
			return c.Get(ctx, client.ObjectKey{Namespace: ns, Name: keystoneEndpointImportName(cp, iface)}, ep)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "the %q Endpoint import should be projected", iface)
		meta.SetStatusCondition(&ep.Status.Conditions, metav1.Condition{
			Type:               orcv1alpha1.ConditionAvailable,
			Status:             metav1.ConditionTrue,
			Reason:             orcv1alpha1.ConditionReasonSuccess,
			ObservedGeneration: ep.Generation,
			Message:            "simulated import resolved",
		})
		ep.Status.ID = ptr.To("simulated-endpoint-id-" + string(iface))
		g.Expect(c.Status().Update(ctx, ep)).To(Succeed(), "resolve the %q Endpoint import", iface)
	}
}

// simulateAdminPasswordExternalSecretSyncWhenPresent waits for the operator-created
// per-ControlPlane admin-password ExternalSecret (named adminPasswordSecretName(cp)
// in childNamespace(cp)), asserts it reads this CR's keystone-NAME-scoped OpenBao
// path (adminPasswordRemoteKeyFor) and is controller-owned by the ControlPlane, then
// simulates the ESO sync. SimulateExternalSecretSync patches ONLY the ExternalSecret
// .status — it never creates the backing Secret — so the pre-created plain Secret
// (named adminPasswordSecretName(cp)) remains the cleartext source readAdminPassword
// reads. This is the admin-password analog of the inline DB-credential ExternalSecret
// sync, gating the Keystone projection on AdminPasswordReady.
func simulateAdminPasswordExternalSecretSyncWhenPresent(
	t testing.TB, ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane,
) {
	t.Helper()
	g := NewGomegaWithT(t)

	es := &esov1.ExternalSecret{}
	// The admin password is materialised beside the Keystone child, which follows
	// the namespace its service is placed in.
	adminNS := effectiveAdminPasswordSecretNamespace(cp)
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: adminNS, Name: adminPasswordSecretName(cp)}, es)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"operator must create the per-CP admin password ExternalSecret")
	g.Expect(es.Spec.Data).NotTo(BeEmpty(), "admin password ExternalSecret must declare Data entries")
	g.Expect(es.Spec.Data[0].RemoteRef.Key).To(Equal(adminPasswordRemoteKeyFor(cp)),
		"admin password ExternalSecret must read this CR's keystone-name-scoped OpenBao path")
	// Ownership: a controller owner reference at home, the ownership labels when
	// the Keystone service (and with it the admin password) is placed in a
	// namespace of its own — Kubernetes forbids a cross-namespace owner reference.
	if adminNS == cp.Namespace {
		owner := metav1.GetControllerOf(es)
		g.Expect(owner).NotTo(BeNil(), "admin password ExternalSecret must be controller-owned by the ControlPlane")
		g.Expect(owner.Kind).To(Equal("ControlPlane"))
		g.Expect(owner.Name).To(Equal(cp.Name))
	} else {
		g.Expect(es.OwnerReferences).To(BeEmpty())
		g.Expect(es.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, cp.Name))
	}
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Namespace: adminNS, Name: adminPasswordSecretName(cp)})).
		To(Succeed(), "simulate per-CP admin password ExternalSecret sync")
}

// simulateGlanceDBCredentialSyncWhenPresent waits for the operator-created Glance
// DB-credential ExternalSecret, simulates the ESO sync, and materialises the
// Secret behind it with an ENGINE-ISSUED username.
//
// Both halves are required: reconcileGlance gates the Dynamic projection on the
// ExternalSecret being Ready AND on the Secret it targets carrying a username the
// database engine minted, because a Static->Dynamic flip updates the
// ExternalSecret in place and its Ready can otherwise still be the retired Static
// sync's. SimulateExternalSecretSync patches only the ExternalSecret .status, and
// envtest runs no ESO, so the Secret is created here the way ESO would create it.
func simulateGlanceDBCredentialSyncWhenPresent(
	t testing.TB, ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane,
) {
	t.Helper()
	g := NewGomegaWithT(t)

	glanceNS, name := cp.GlanceNamespace(), glanceDBCredentialSecretName(cp)
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: glanceNS, Name: name}, &esov1.ExternalSecret{})
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"operator must create the per-CP Glance DB-credential ExternalSecret")

	g.Expect(c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: glanceNS},
		Data: map[string][]byte{
			"username": []byte(engineIssuedUsernamePrefix + "kubernetes-glance-abc123-1750000000"),
			"password": []byte("engine-issued-password"),
		},
	})).To(Succeed(), "materialise the engine-issued Glance DB credential ESO would have written")

	g.Expect(simulators.SimulateExternalSecretSync(ctx, c, client.ObjectKey{Namespace: glanceNS, Name: name})).
		To(Succeed(), "simulate per-CP Glance DB credential ExternalSecret sync")
}

// simulatePlacementDBCredentialSyncWhenPresent is the Placement twin of
// simulateGlanceDBCredentialSyncWhenPresent: it waits for the operator-created
// Placement DB-credential ExternalSecret, simulates the ESO sync, and materialises
// the Secret behind it with an ENGINE-ISSUED username. reconcilePlacement gates the
// Dynamic projection on both halves for the same reason reconcileGlance does — a
// Static->Dynamic flip updates the ExternalSecret in place, so its Ready can still
// be the retired Static sync's — and the engine-issued username here is
// deliberately not the static seed's "placement".
func simulatePlacementDBCredentialSyncWhenPresent(
	t testing.TB, ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane,
) {
	t.Helper()
	g := NewGomegaWithT(t)

	placementNS, name := cp.PlacementNamespace(), placementDBCredentialSecretName(cp)
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: placementNS, Name: name}, &esov1.ExternalSecret{})
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"operator must create the per-CP Placement DB-credential ExternalSecret")

	g.Expect(c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: placementNS},
		Data: map[string][]byte{
			"username": []byte(engineIssuedUsernamePrefix + "kubernetes-placement-abc123-1750000000"),
			"password": []byte("engine-issued-password"),
		},
	})).To(Succeed(), "materialise the engine-issued Placement DB credential ESO would have written")

	g.Expect(simulators.SimulateExternalSecretSync(ctx, c, client.ObjectKey{Namespace: placementNS, Name: name})).
		To(Succeed(), "simulate per-CP Placement DB credential ExternalSecret sync")
}

// simulateBarbicanDBCredentialSyncWhenPresent is the Barbican twin of
// simulatePlacementDBCredentialSyncWhenPresent: it waits for the operator-created
// Barbican DB-credential ExternalSecret, simulates the ESO sync, and materialises
// the Secret behind it with an ENGINE-ISSUED username. reconcileBarbican gates the
// Dynamic projection on both halves for the same reason reconcilePlacement does — a
// Static->Dynamic flip updates the ExternalSecret in place, so its Ready can still
// be the retired Static sync's — and the engine-issued username here is
// deliberately not the static seed's "barbican".
func simulateBarbicanDBCredentialSyncWhenPresent(
	t testing.TB, ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane,
) {
	t.Helper()
	g := NewGomegaWithT(t)

	barbicanNS, name := cp.BarbicanNamespace(), barbicanDBCredentialSecretName(cp)
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: barbicanNS, Name: name}, &esov1.ExternalSecret{})
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"operator must create the per-CP Barbican DB-credential ExternalSecret")

	g.Expect(c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: barbicanNS},
		Data: map[string][]byte{
			"username": []byte(engineIssuedUsernamePrefix + "kubernetes-barbican-abc123-1750000000"),
			"password": []byte("engine-issued-password"),
		},
	})).To(Succeed(), "materialise the engine-issued Barbican DB credential ESO would have written")

	g.Expect(simulators.SimulateExternalSecretSync(ctx, c, client.ObjectKey{Namespace: barbicanNS, Name: name})).
		To(Succeed(), "simulate per-CP Barbican DB credential ExternalSecret sync")
}

// simulateNeutronDBCredentialSyncWhenPresent is the Neutron twin of
// simulateBarbicanDBCredentialSyncWhenPresent: it waits for the operator-created
// Neutron DB-credential ExternalSecret, simulates the ESO sync, and materialises
// the Secret behind it with an ENGINE-ISSUED username. reconcileNeutron gates the
// Dynamic projection on both halves for the reason its peers do: a Static->Dynamic
// flip updates the ExternalSecret in place, so its Ready can still be the retired
// Static sync's. The engine-issued username here is deliberately not the static
// seed's "neutron".
func simulateNeutronDBCredentialSyncWhenPresent(
	t testing.TB, ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane,
) {
	t.Helper()
	g := NewGomegaWithT(t)

	neutronNS, name := cp.NeutronNamespace(), neutronDBCredentialSecretName(cp)
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: neutronNS, Name: name}, &esov1.ExternalSecret{})
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"operator must create the per-CP Neutron DB-credential ExternalSecret")

	g.Expect(c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: neutronNS},
		Data: map[string][]byte{
			"username": []byte(engineIssuedUsernamePrefix + "kubernetes-neutron-abc123-1750000000"),
			"password": []byte("engine-issued-password"),
		},
	})).To(Succeed(), "materialise the engine-issued Neutron DB credential ESO would have written")

	g.Expect(simulators.SimulateExternalSecretSync(ctx, c, client.ObjectKey{Namespace: neutronNS, Name: name})).
		To(Succeed(), "simulate per-CP Neutron DB credential ExternalSecret sync")
}

// TestIntegration_FullReconcile_ManagedToReady drives a managed-mode ControlPlane
// through every sub-reconciler to the aggregate Ready=True, simulating each
// external dependency's readiness in dependency order. It is the single primary
// end-to-end test for the ControlPlane reconciler.
//
// The KeystoneService controller runs beside it, because the built-in services
// register through the KeystoneService children they project: nothing else
// provisions the Keystone accounts and catalog rows they gate on.
func TestIntegration_FullReconcile_ManagedToReady(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupRegisteringControlPlaneEnvTest(t)

	// The OpenBao-backed ClusterSecretStore must be Ready before the chain
	// reaches the credential gates (#476).
	ensureReadyClusterSecretStore(t, ctx, c)

	// Isolated test namespace per run (namespace-per-test with GenerateName).
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-controlplane-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")
	// Pre-seed the per-tenant SecretStore Ready so the ESOTenantStore gate opens
	// (envtest has no ESO controller); the operator's SSA re-asserts its spec
	// without clobbering the status subresource.
	ensureReadySecretStore(t, ctx, c, esoTenantStoreName, ns.Name)

	// Create the ControlPlane CR (the defaulting webhook fills region etc.).
	// Horizon, Glance, Placement, Barbican and Neutron are enabled HERE (not in the
	// shared fixture) so only this full-chain test — which simulates the Horizon
	// child in Phase 2.5, the four built-in registrations in Phase 5.6, the Glance
	// child (plus its GlanceBackend) in Phase 6, the Placement child in Phase 7, the
	// Barbican child (plus the dedicated OpenBao ensemble behind its secret store)
	// in Phase 8, and the Neutron child (plus the OVN gate and the bus delivery
	// ahead of it) in Phase 9 — carries the extra services; the gate-focused tests
	// reusing the fixture would otherwise wedge at the unsimulated steps.
	cp := integrationManagedControlPlane("cp", ns.Name)
	cp.Spec.Services.Horizon = &c5c3v1alpha1.ServiceHorizonSpec{}
	cp.Spec.Services.Glance = integrationGlanceService()
	cp.Spec.Services.Placement = integrationPlacementService()
	cp.Spec.Services.Barbican = integrationBarbicanService()
	cp.Spec.Services.Neutron = integrationNeutronService()

	// The shared message bus. The network service is what needs it: the Neutron
	// projection derives the child's transport URL from this block, and the
	// validating webhook requires the block beside services.neutron.
	cp.Spec.Infrastructure.Messaging = &commonv1.MessagingSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: "cp-rabbitmq"},
		Replicas:   1,
	}

	// Both halves of the extraConfig merge, so Phase 7 can assert the projected
	// Placement child carries the union: a global-only section plus a
	// placement-only one. Both options are in every enabled service's option
	// catalog and owned by no operator, so admission takes them without a warning.
	cp.Spec.GlobalExtraConfig = map[string]map[string]string{
		"cors": {"allowed_origin": "https://dashboard.example.com"},
	}
	cp.Spec.Services.Placement.ExtraConfig = map[string]map[string]string{
		"placement_database": {"max_pool_size": "10"},
	}

	// Admin password Secret the KORC sub-reconciler hashes to drive the mint. In
	// managed mode readAdminPassword resolves the operator-owned per-CP name
	// (effectiveAdminPasswordSecretRef -> adminPasswordSecretName(cp)), so pre-create
	// the cleartext Secret under that name. ESO would own this Secret in production;
	// envtest has no ESO, and SimulateExternalSecretSync patches only the ES status,
	// so this plain Secret remains the cleartext source readAdminPassword reads
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminPasswordSecretName(cp), Namespace: ns.Name},
		Data:       map[string][]byte{"password": []byte("super-secret-admin-password")},
	}
	g.Expect(c.Create(ctx, adminSecret)).To(Succeed(), "create admin password Secret")

	// The broker's default user. The RabbitMQ Cluster Operator materialises it and
	// points the cluster's status at the Secret; no such operator runs in envtest,
	// so the four keys reconcileNeutronMessaging assembles the transport URL from
	// are seeded here, under the name simulateRabbitmqClusterReadyWhenPresent
	// publishes in status.defaultUser.secretReference.
	g.Expect(c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-rabbitmq-default-user", Namespace: ns.Name},
		Data: map[string][]byte{
			"username": []byte("default-user"),
			"password": []byte("broker-password"),
			"host":     []byte("cp-rabbitmq." + ns.Name + ".svc"),
			"port":     []byte("5672"),
		},
	})).To(Succeed(), "create the broker default-user Secret")

	// The OVNCentral services.neutron.ovn.centralRef names. It is deployed OUTSIDE
	// the ControlPlane: the plane reads it and mirrors its readiness, and never
	// creates, updates or deletes it, so the test creates it here with the one field
	// the CRD requires. It goes in the ControlPlane's own namespace, which is what
	// the ref's empty namespace resolves to.
	g.Expect(c.Create(ctx, &ovnv1alpha1.OVNCentral{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-ovn", Namespace: ns.Name},
		Spec: ovnv1alpha1.OVNCentralSpec{
			TLS: ovnv1alpha1.OVNTLSSpec{IssuerRef: ovnv1alpha1.OVNIssuerRef{Name: "test-issuer"}},
		},
	})).To(Succeed(), "create the referenced OVNCentral")

	g.Expect(c.Create(ctx, cp)).To(Succeed(), "create ControlPlane CR")
	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}

	// --- Phase 1: Infrastructure (MariaDB + Memcached + the shared bus). ---
	simulateMariaDBReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "openstack-db", Namespace: ns.Name})
	simulateMemcachedReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "openstack-memcached", Namespace: ns.Name})
	simulateRabbitmqClusterReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "cp-rabbitmq", Namespace: ns.Name})
	// The OVN step carries no condition gate and owns nothing, so the central's
	// readiness is reported here rather than in Phase 9: the Neutron registration
	// Phase 5.6 drives is not projected until OVNReady is True.
	simulateOVNCentralReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "cp-ovn", Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeInfrastructureReady, metav1.ConditionTrue, itEventuallyTimeout)

	// gate Keystone on the per-CP DB credential ExternalSecret. DECISION:
	// harness sync-simulation lives here to keep this level bisectable (full suite
	// green); the projected-secretRef assertion is made below in the Keystone block
	// Reviewer: please verify.
	// Managed mode defaults to Dynamic (engine-issued) credentials: the operator
	// projects a per-CP VaultDynamicSecret generator plus an ExternalSecret that
	// draws from it via dataFrom.sourceRef.generatorRef (no static Data refs).
	dbCredES := &esov1.ExternalSecret{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)}, dbCredES)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "operator must create the per-CP DB credential ExternalSecret")
	g.Expect(dbCredES.Spec.DataFrom).NotTo(BeEmpty(), "Dynamic DB credential ExternalSecret must declare a generatorRef")
	g.Expect(dbCredES.Spec.DataFrom[0].SourceRef).NotTo(BeNil())
	g.Expect(dbCredES.Spec.DataFrom[0].SourceRef.GeneratorRef).NotTo(BeNil())
	g.Expect(dbCredES.Spec.DataFrom[0].SourceRef.GeneratorRef.Kind).To(Equal("VaultDynamicSecret"))
	g.Expect(dbCredES.Spec.Data).To(BeEmpty(), "Dynamic DB credential ExternalSecret carries no static Data refs")
	// The per-CP VaultDynamicSecret generator reads this tenant's creds path.
	vds := &esgenv1alpha1.VaultDynamicSecret{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)}, vds)).
		To(Succeed(), "operator must create the per-CP VaultDynamicSecret generator")
	g.Expect(vds.Spec.Path).To(Equal(dbDynamicCredsPathFor(cp)))
	g.Expect(metav1.GetControllerOf(vds)).NotTo(BeNil(), "VaultDynamicSecret must be owned by the ControlPlane")
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)})).
		To(Succeed(), "simulate per-CP DB credential ExternalSecret sync")
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeDBCredentialsReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 1.5: AdminPassword (between Infrastructure/DBCredentials and Keystone).
	// The keystone-operator's SecretsReady gate needs the admin Secret backed by a
	// Ready ExternalSecret, so reconcileAdminPassword must create+ready the per-CP
	// admin-password ExternalSecret before the Keystone child is projected. Assert the
	// operator-rendered shape (keystone-name-scoped OpenBao path + controller owner-ref),
	// simulate the ESO sync (status-only — the renamed plain Secret above stays the
	// cleartext source), then AdminPasswordReady flips True. ---
	simulateAdminPasswordExternalSecretSyncWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminPasswordReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 2: Keystone child. ---
	simulateKeystoneReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: keystoneName(cp), Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeKeystoneReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 2.5: Horizon child. Projection is gated on KeystoneReady, so
	// the child only appears now; assert its operator-derived spec before
	// simulating readiness. ---
	horizonKey := client.ObjectKey{Name: horizonName(cp), Namespace: ns.Name}
	projectedHorizon := &horizonv1alpha1.Horizon{}
	g.Eventually(func() error {
		return c.Get(ctx, horizonKey, projectedHorizon)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "Horizon child should be projected once KeystoneReady")
	g.Expect(projectedHorizon.Spec.KeystoneEndpoint).To(
		Equal(fmt.Sprintf("http://%s.%s.svc:5000/v3", keystoneName(cp), ns.Name)),
		"keystoneEndpoint must be the cluster-local convention URL (same as the K-ORC auth_url)",
	)
	g.Expect(projectedHorizon.Spec.Cache.ClusterRef).NotTo(BeNil())
	g.Expect(projectedHorizon.Spec.Cache.ClusterRef.Name).To(Equal("openstack-memcached"))
	simulateHorizonReadyWhenPresent(t, ctx, c, horizonKey)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeHorizonReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 3: K-ORC admin ApplicationCredential. ---
	simulateApplicationCredentialAvailableWhenPresent(t, ctx, c,
		client.ObjectKey{Name: adminAppCredentialName(cp), Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeKORCReady, metav1.ConditionTrue, itEventuallyTimeout)

	// The K-ORC clouds.yaml ExternalSecret AdminCredentialReady gates on is now
	// CREATED BY THE OPERATOR (reconcileKORC -> ensureKORCCloudsYAMLExternalSecret),
	// co-located in the ControlPlane namespace because K-ORC resolves
	// CloudCredentialsRef in the resource's own namespace — it is no longer seeded by
	// write-bootstrap-secrets.sh. reconcileKORC creates it before
	// the AC-Available gate, so it exists by the time KORCReady flips True (above).
	// Assert its operator-rendered shape, then simulate the ESO sync (no ESO
	// controller in envtest) so WaitForExternalSecret reports Ready and Phase 4 can
	// progress.
	cloudsYamlES := &esov1.ExternalSecret{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: korcCloudsYamlSecretName}, cloudsYamlES)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "operator must create the k-orc clouds.yaml ExternalSecret")
	g.Expect(cloudsYamlES.Spec.Data).To(HaveLen(1), "clouds.yaml ExternalSecret must declare exactly one Data entry")
	g.Expect(cloudsYamlES.Spec.Data[0].SecretKey).To(Equal(appCredCloudsYAMLKey))
	g.Expect(cloudsYamlES.Spec.Data[0].RemoteRef.Key).To(Equal(adminAppCredentialRemoteKeyFor(cp)),
		"clouds.yaml ExternalSecret must read the per-CR OpenBao path")
	g.Expect(cloudsYamlES.Spec.Data[0].RemoteRef.Property).To(Equal(appCredCloudsYAMLKey))
	owner := metav1.GetControllerOf(cloudsYamlES)
	g.Expect(owner).NotTo(BeNil(), "clouds.yaml ExternalSecret must be controller-owned by the ControlPlane")
	g.Expect(owner.Kind).To(Equal("ControlPlane"))
	g.Expect(owner.Name).To(Equal(cp.Name))
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Namespace: ns.Name, Name: korcCloudsYamlSecretName})).
		To(Succeed(), "simulate k-orc clouds.yaml ExternalSecret sync")

	// --- Phase 4: AdminCredential push. Gated on the clouds.yaml ES (synced
	// above), on the admin app-credential PushSecret syncing to OpenBao, AND on the
	// materialized k-orc-clouds-yaml Secret matching the assembled credential. The
	// PushSecret sync is status-gated and the materialisation is the ESO round-trip,
	// so simulate both — otherwise AdminCredentialReady never flips in envtest. ---
	simulatePushSecretSyncedWhenPresent(t, ctx, c,
		client.ObjectKey{Name: adminAppCredentialPushSecretName(cp), Namespace: childNamespace(cp)})
	simulateCloudsYamlMaterializedWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminCredentialReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 5: Catalog. The ControlPlane registers the IDENTITY row itself
	// (Service + public Endpoint) and only that one: the image, placement and
	// key-manager rows belong to the KeystoneService children the built-in services
	// project, and are driven in Phase 5.6. CatalogReady gates on the identity row
	// reporting Available, so simulate the K-ORC actuator marking it so before
	// waiting. ---
	simulateCatalogServiceEndpointAvailableWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeCatalogReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 5.6: the built-in registrations. Glance, Placement, Barbican and
	// Neutron each project one KeystoneService child carrying that service's catalog
	// row and its Keystone account. The registration controller running beside the
	// ControlPlane reconciles them, so the same K-ORC and ESO round-trip is replayed
	// once per child — and until each reports AccountReady, no service child is
	// projected at all. The Neutron one appears only because OVNReady is already
	// True and the bus was delivered: both gates sit ahead of the registration. ---
	glanceReg := simulateBuiltinRegistrationConvergedWhenPresent(t, ctx, c, cp,
		client.ObjectKey{Name: glanceName(cp), Namespace: cp.GlanceNamespace()})
	placementReg := simulateBuiltinRegistrationConvergedWhenPresent(t, ctx, c, cp,
		client.ObjectKey{Name: placementName(cp), Namespace: cp.PlacementNamespace()})
	barbicanReg := simulateBuiltinRegistrationConvergedWhenPresent(t, ctx, c, cp,
		client.ObjectKey{Name: barbicanName(cp), Namespace: cp.BarbicanNamespace()})
	neutronReg := simulateBuiltinRegistrationConvergedWhenPresent(t, ctx, c, cp,
		client.ObjectKey{Name: neutronName(cp), Namespace: cp.NeutronNamespace()})

	// The ServiceAccounts member aggregates those four children into
	// ServiceAccountsReady, which is the condition operators alert on: with every
	// registration Ready it reports how many were counted.
	serviceAccountsReady := waitForControlPlaneCondition(t, ctx, c, cpKey,
		conditionTypeServiceAccountsReady, metav1.ConditionTrue, itEventuallyTimeout)
	g.Expect(serviceAccountsReady.Reason).To(Equal(reasonServiceAccountsProvisioned))

	// Each child declares the identity its service authenticates as: the service's
	// user name, a service project of its own, and the "service" role.
	for _, registration := range []struct {
		child   *c5c3v1alpha1.KeystoneService
		user    string
		project string
	}{
		{glanceReg, "glance", "service-glance"},
		{placementReg, "placement", "service-placement"},
		{barbicanReg, "barbican", "service-barbican"},
		{neutronReg, "neutron", "service-neutron"},
	} {
		account := registration.child.Spec.Account
		g.Expect(account).NotTo(BeNil(), "the %q registration must declare a service account", registration.user)
		g.Expect(account.UserName).To(Equal(registration.user))
		g.Expect(account.Project.Name).To(Equal(registration.project),
			"every built-in takes a service project of its own")
		g.Expect(account.Project.Create).To(BeTrue(), "the registration creates that project")
		g.Expect(account.Roles).To(Equal([]string{"service"}))
	}

	// --- Phase 6: Glance child (the last pipeline step, after the registrations).
	// Projection is gated on KeystoneReady AND on the Glance registration reporting
	// AccountReady (driven in Phase 5.6), so the child — and its GlanceBackend and
	// DB-credential ExternalSecret — only appear now. Assert their operator-derived
	// shape before simulating readiness. ---
	//
	// The Dynamic-default credential is a THIRD gate on top of those two: no child
	// is projected until the glance-scoped ExternalSecret has synced and the Secret
	// behind it carries an engine-issued username, so open it first.
	simulateGlanceDBCredentialSyncWhenPresent(t, ctx, c, cp)

	glanceKey := client.ObjectKey{Name: glanceName(cp), Namespace: ns.Name}
	projectedGlance := &glancev1alpha1.Glance{}
	g.Eventually(func() error {
		return c.Get(ctx, glanceKey, projectedGlance)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"Glance child should be projected once KeystoneReady and the Glance registration are ready")

	// Image: the canonical repository with the release-derived tag.
	g.Expect(projectedGlance.Spec.Image.Repository).To(Equal(defaultGlanceRepository))
	g.Expect(projectedGlance.Spec.Image.Tag).To(Equal("2025.2"), "Glance image tag must derive from openStackRelease")

	// Database: the shared managed cluster, the fixed "glance" logical schema, and
	// the operator-owned engine-issued DB credential (Dynamic is the default on the
	// managed shared database, mirroring Keystone with glance-scoped objects).
	g.Expect(projectedGlance.Spec.Database.ClusterRef).NotTo(BeNil(), "Glance database clusterRef must be wired")
	g.Expect(projectedGlance.Spec.Database.ClusterRef.Name).To(Equal("openstack-db"))
	g.Expect(projectedGlance.Spec.Database.Database).To(Equal("glance"))
	g.Expect(projectedGlance.Spec.Database.SecretRef.Name).To(Equal(glanceDBCredentialSecretName(cp)),
		"managed Glance DB secretRef must point at the operator-owned per-CP Glance DB-credential Secret")
	g.Expect(projectedGlance.Spec.Database.SecretRef.Key).To(Equal("password"))
	g.Expect(projectedGlance.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeDynamic),
		"the projected Glance DB credential defaults to Dynamic (engine-issued)")

	// Cache: the shared managed Memcached.
	g.Expect(projectedGlance.Spec.Cache.ClusterRef).NotTo(BeNil(), "Glance cache clusterRef must be wired")
	g.Expect(projectedGlance.Spec.Cache.ClusterRef.Name).To(Equal("openstack-memcached"))

	// Keystone endpoint: the cluster-local convention URL of the projected Keystone
	// child (the same URL K-ORC authenticates against), never the external one.
	g.Expect(projectedGlance.Spec.KeystoneEndpoint).To(
		Equal(fmt.Sprintf("http://%s.%s.svc:5000/v3", keystoneName(cp), ns.Name)),
		"keystoneEndpoint must be the cluster-local Keystone Service URL",
	)

	// Service user: the identity the registration provisions — the glance user in
	// its own service-glance project — and the consumer Secret that registration
	// delivers.
	g.Expect(projectedGlance.Spec.ServiceUser.Username).To(Equal("glance"))
	g.Expect(projectedGlance.Spec.ServiceUser.ProjectName).To(Equal("service-glance"))
	g.Expect(projectedGlance.Spec.ServiceUser.SecretRef.Name).To(Equal(keystoneServiceCredentialsSecretName(glanceReg)),
		"Glance service-user password must read the registration's consumer Secret")
	g.Expect(projectedGlance.Spec.ServiceUser.SecretRef.Key).To(Equal("password"))

	// The child is co-located with the ControlPlane, so it carries a controller
	// owner reference back to it.
	glanceOwner := metav1.GetControllerOf(projectedGlance)
	g.Expect(glanceOwner).NotTo(BeNil(), "Glance child must be controller-owned by the ControlPlane")
	g.Expect(glanceOwner.Kind).To(Equal("ControlPlane"))
	g.Expect(glanceOwner.Name).To(Equal(cp.Name))

	// The projected default GlanceBackend mirrors the curated backends[] entry: it
	// attaches to the Glance child, carries the S3 endpoint as its host, and is the
	// default store.
	projectedBackend := &glancev1alpha1.GlanceBackend{}
	g.Expect(c.Get(ctx, client.ObjectKey{Name: glanceBackendName(cp, "default"), Namespace: ns.Name}, projectedBackend)).
		To(Succeed(), "the default GlanceBackend child must be projected")
	g.Expect(projectedBackend.Spec.GlanceRef.Name).To(Equal(glanceName(cp)),
		"GlanceBackend must reference the projected Glance child by name")
	g.Expect(projectedBackend.Spec.Type).To(Equal(glancev1alpha1.GlanceBackendTypeS3))
	g.Expect(projectedBackend.Spec.S3).NotTo(BeNil())
	g.Expect(projectedBackend.Spec.S3.Host).To(Equal("http://garage.shared-services.svc.cluster.local:3900"),
		"the backends[] endpoint must project onto the child's spec.s3.host")
	g.Expect(projectedBackend.Spec.IsDefault).To(BeTrue(), "the single backends[] entry is the default store")
	backendOwner := metav1.GetControllerOf(projectedBackend)
	g.Expect(backendOwner).NotTo(BeNil(), "GlanceBackend child must be controller-owned by the ControlPlane")
	g.Expect(backendOwner.Name).To(Equal(cp.Name))

	// The Dynamic-mode Glance DB-credential ExternalSecret draws from the per-CP
	// VaultDynamicSecret generator (dataFrom.sourceRef.generatorRef) and carries no
	// static Data refs.
	glanceDBCredES := &esov1.ExternalSecret{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: glanceDBCredentialSecretName(cp)}, glanceDBCredES)).
		To(Succeed(), "operator must create the per-CP Glance DB-credential ExternalSecret")
	g.Expect(glanceDBCredES.Spec.Data).To(BeEmpty(), "the Dynamic Glance DB-credential ExternalSecret carries no static Data refs")
	g.Expect(glanceDBCredES.Spec.DataFrom).NotTo(BeEmpty(), "the Dynamic Glance DB-credential ExternalSecret must declare a generatorRef")
	g.Expect(glanceDBCredES.Spec.DataFrom[0].SourceRef).NotTo(BeNil())
	g.Expect(glanceDBCredES.Spec.DataFrom[0].SourceRef.GeneratorRef).NotTo(BeNil())
	g.Expect(glanceDBCredES.Spec.DataFrom[0].SourceRef.GeneratorRef.Kind).To(Equal("VaultDynamicSecret"))
	glanceESOwner := metav1.GetControllerOf(glanceDBCredES)
	g.Expect(glanceESOwner).NotTo(BeNil(), "Glance DB-credential ExternalSecret must be controller-owned by the ControlPlane")
	g.Expect(glanceESOwner.Name).To(Equal(cp.Name))

	// The per-CP Glance VaultDynamicSecret generator reads this tenant's glance
	// creds path with role glance-db, authenticating with the glance-db-creds SA and
	// the mTLS client Certificate — all in the glance namespace.
	glanceVDS := &esgenv1alpha1.VaultDynamicSecret{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: glanceDBCredentialSecretName(cp)}, glanceVDS)).
		To(Succeed(), "operator must create the per-CP Glance VaultDynamicSecret generator")
	g.Expect(glanceVDS.Spec.Path).To(Equal(glanceDBDynamicCredsPathFor(cp)))
	g.Expect(glanceVDS.Spec.Provider.Auth.Kubernetes.Role).To(Equal(glanceDBDynamicVaultRole))
	g.Expect(metav1.GetControllerOf(glanceVDS)).NotTo(BeNil(), "Glance VaultDynamicSecret must be owned by the ControlPlane")

	glanceDBCredSA := &corev1.ServiceAccount{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: glanceDBCredentialServiceAccountName}, glanceDBCredSA)).
		To(Succeed(), "operator must create the Glance generator's ServiceAccount in the glance namespace")

	glanceDBCredCert := &unstructured.Unstructured{}
	glanceDBCredCert.SetGroupVersionKind(certificateGVK)
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: glanceDBCredentialClientCertName(cp)}, glanceDBCredCert)).
		To(Succeed(), "operator must create the Glance generator's mTLS client Certificate in the glance namespace")

	simulateGlanceReadyWhenPresent(t, ctx, c, glanceKey)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeGlanceReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 7: Placement child (the pipeline tail, after Glance). Projection is
	// gated on KeystoneReady, on the Placement registration reporting AccountReady
	// (driven in Phase 5.6), and on the Dynamic-default credential, so open that
	// third gate first and then assert the operator-derived shape of the child
	// before simulating readiness. ---
	simulatePlacementDBCredentialSyncWhenPresent(t, ctx, c, cp)

	placementKey := client.ObjectKey{Name: placementName(cp), Namespace: ns.Name}
	projectedPlacement := &placementv1alpha1.Placement{}
	g.Eventually(func() error {
		return c.Get(ctx, placementKey, projectedPlacement)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"Placement child should be projected once KeystoneReady and the Placement registration are ready")

	// Release and image: the canonical repository with the release-derived tag.
	g.Expect(projectedPlacement.Spec.OpenStackRelease).To(Equal("2025.2"))
	g.Expect(projectedPlacement.Spec.Image.Repository).To(Equal(defaultPlacementRepository))
	g.Expect(projectedPlacement.Spec.Image.Tag).To(Equal("2025.2"), "Placement image tag must derive from openStackRelease")

	// extraConfig: globalExtraConfig unioned with the per-service block.
	g.Expect(projectedPlacement.Spec.ExtraConfig).To(Equal(map[string]map[string]string{
		"cors":               {"allowed_origin": "https://dashboard.example.com"},
		"placement_database": {"max_pool_size": "10"},
	}), "the projected extraConfig is the merge of globalExtraConfig and services.placement.extraConfig")

	// Database: the shared managed cluster, the fixed "placement" logical schema,
	// and the operator-owned engine-issued DB credential (Dynamic is the default on
	// the managed shared database, mirroring Keystone with placement-scoped objects).
	g.Expect(projectedPlacement.Spec.Database.ClusterRef).NotTo(BeNil(), "Placement database clusterRef must be wired")
	g.Expect(projectedPlacement.Spec.Database.ClusterRef.Name).To(Equal("openstack-db"))
	g.Expect(projectedPlacement.Spec.Database.Database).To(Equal("placement"))
	g.Expect(projectedPlacement.Spec.Database.SecretRef.Name).To(Equal(placementDBCredentialSecretName(cp)),
		"managed Placement DB secretRef must point at the operator-owned per-CP Placement DB-credential Secret")
	g.Expect(projectedPlacement.Spec.Database.SecretRef.Key).To(Equal("password"))
	g.Expect(projectedPlacement.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeDynamic),
		"the projected Placement DB credential defaults to Dynamic (engine-issued)")

	// Cache: the shared managed Memcached.
	g.Expect(projectedPlacement.Spec.Cache.ClusterRef).NotTo(BeNil(), "Placement cache clusterRef must be wired")
	g.Expect(projectedPlacement.Spec.Cache.ClusterRef.Name).To(Equal("openstack-memcached"))

	// Keystone endpoint: derived TOP-DOWN from the naming convention (the same URL
	// K-ORC authenticates against), never read from the Keystone child's status.
	g.Expect(projectedPlacement.Spec.KeystoneEndpoint).To(
		Equal(fmt.Sprintf("http://%s.%s.svc:5000/v3", keystoneName(cp), ns.Name)),
		"keystoneEndpoint must be the cluster-local Keystone Service URL",
	)
	g.Expect(projectedPlacement.Spec.KeystonePublicEndpoint).To(BeEmpty(),
		"this fixture exposes Keystone nowhere externally, so the child falls back to the internal endpoint")

	g.Expect(projectedPlacement.Spec.Region).To(Equal(c5c3v1alpha1.DefaultRegion),
		"the region the defaulting webhook materialized reaches the child")

	// Service user: the identity the Placement registration provisions (its own
	// service-placement project) and the consumer Secret it delivers.
	g.Expect(projectedPlacement.Spec.ServiceUser.Username).To(Equal("placement"))
	g.Expect(projectedPlacement.Spec.ServiceUser.ProjectName).To(Equal("service-placement"))
	g.Expect(projectedPlacement.Spec.ServiceUser.SecretRef.Name).
		To(Equal(keystoneServiceCredentialsSecretName(placementReg)),
			"Placement service-user password must read the registration's consumer Secret")
	g.Expect(projectedPlacement.Spec.ServiceUser.SecretRef.Key).To(Equal("password"))

	// The ControlPlane's RESOLVED store selection, so the child never falls back to
	// its own shared-cluster-store default.
	g.Expect(projectedPlacement.Spec.SecretStoreRef).NotTo(BeNil(), "the resolved store ref must be projected")
	g.Expect(projectedPlacement.Spec.SecretStoreRef.Kind).To(Equal(commonv1.SecretStoreKindNamespaced))
	g.Expect(projectedPlacement.Spec.SecretStoreRef.Name).To(Equal(esoTenantStoreName))

	g.Expect(projectedPlacement.Spec.Deployment.Replicas).To(Equal(commonv1.DefaultReplicas),
		"replicas fall back to the shared operator default when services.placement sets none")
	g.Expect(projectedPlacement.Spec.APIServer).To(BeNil(),
		"spec.apiServer is deliberately unset — the child-side uWSGI defaults stay authoritative")

	// The child is co-located with the ControlPlane, so ownership is a controller
	// owner reference rather than the labels a cross-namespace child carries.
	placementOwner := metav1.GetControllerOf(projectedPlacement)
	g.Expect(placementOwner).NotTo(BeNil(), "Placement child must be controller-owned by the ControlPlane")
	g.Expect(placementOwner.Kind).To(Equal("ControlPlane"))
	g.Expect(placementOwner.Name).To(Equal(cp.Name))

	// The Dynamic-mode Placement DB-credential ExternalSecret draws from the per-CP
	// VaultDynamicSecret generator (dataFrom.sourceRef.generatorRef) and carries no
	// static Data refs.
	placementDBCredES := &esov1.ExternalSecret{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: placementDBCredentialSecretName(cp)}, placementDBCredES)).
		To(Succeed(), "operator must create the per-CP Placement DB-credential ExternalSecret")
	g.Expect(placementDBCredES.Spec.Data).To(BeEmpty(),
		"the Dynamic Placement DB-credential ExternalSecret carries no static Data refs")
	g.Expect(placementDBCredES.Spec.DataFrom).NotTo(BeEmpty(),
		"the Dynamic Placement DB-credential ExternalSecret must declare a generatorRef")
	g.Expect(placementDBCredES.Spec.DataFrom[0].SourceRef).NotTo(BeNil())
	g.Expect(placementDBCredES.Spec.DataFrom[0].SourceRef.GeneratorRef).NotTo(BeNil())
	g.Expect(placementDBCredES.Spec.DataFrom[0].SourceRef.GeneratorRef.Kind).To(Equal("VaultDynamicSecret"))
	placementESOwner := metav1.GetControllerOf(placementDBCredES)
	g.Expect(placementESOwner).NotTo(BeNil(),
		"Placement DB-credential ExternalSecret must be controller-owned by the ControlPlane")
	g.Expect(placementESOwner.Name).To(Equal(cp.Name))

	// The per-CP Placement VaultDynamicSecret generator reads this tenant's
	// placement creds path with role placement-db, authenticating with the
	// placement-db-creds SA and the mTLS client Certificate — all in the placement
	// namespace.
	placementVDS := &esgenv1alpha1.VaultDynamicSecret{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: placementDBCredentialSecretName(cp)}, placementVDS)).
		To(Succeed(), "operator must create the per-CP Placement VaultDynamicSecret generator")
	g.Expect(placementVDS.Spec.Path).To(Equal("database/mariadb/creds/placement-" + ns.Name))
	g.Expect(placementVDS.Spec.Provider.Auth.Kubernetes.Role).To(Equal(placementDBDynamicVaultRole))
	g.Expect(metav1.GetControllerOf(placementVDS)).NotTo(BeNil(),
		"Placement VaultDynamicSecret must be owned by the ControlPlane")

	placementDBCredSA := &corev1.ServiceAccount{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: placementDBCredentialServiceAccountName}, placementDBCredSA)).
		To(Succeed(), "operator must create the Placement generator's ServiceAccount in the placement namespace")

	placementDBCredCert := &unstructured.Unstructured{}
	placementDBCredCert.SetGroupVersionKind(certificateGVK)
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: placementDBCredentialClientCertName(cp)}, placementDBCredCert)).
		To(Succeed(), "operator must create the Placement generator's mTLS client Certificate in the placement namespace")

	simulatePlacementReadyWhenPresent(t, ctx, c, placementKey)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypePlacementReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 8: Barbican child (the pipeline's last member, after Placement).
	// Projection is gated on KeystoneReady, on the Barbican registration reporting
	// AccountReady (driven in Phase 5.6), on the Dynamic-default credential, and —
	// because this fixture takes a dedicated secret store — on the
	// OpenBao instance the ControlPlane provisions for it reporting Available. The
	// whole ensemble behind that instance is projected BEFORE the last gate closes,
	// so open the credential gate, assert the ensemble, prove the instance gate
	// holds, and only then let the instance serve. ---
	simulateBarbicanDBCredentialSyncWhenPresent(t, ctx, c, cp)

	instanceName := barbicanOpenBaoName(cp)
	instanceKey := client.ObjectKey{Name: instanceName, Namespace: ns.Name}
	instance := &openbaov1alpha1.OpenBaoCluster{}
	g.Eventually(func() error {
		return c.Get(ctx, instanceKey, instance)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"the dedicated OpenBao instance must be projected once the Barbican gates ahead of it are open")

	// The instance posture: the pinned OpenBao version, the DeletePVCs policy that
	// keeps a re-created ControlPlane from meeting raft storage sealed under a key
	// that no longer exists, External TLS over the two cert-manager Secrets, and a
	// static seal whose key lives beside it.
	g.Expect(instance.Spec.Version).To(Equal("2.6.2"))
	g.Expect(instance.Spec.Replicas).To(Equal(int32(1)))
	g.Expect(instance.Spec.DeletionPolicy).To(Equal(openbaov1alpha1.DeletionPolicyDeletePVCs))
	g.Expect(instance.Spec.TLS.Enabled).To(BeTrue())
	g.Expect(instance.Spec.TLS.Mode).To(Equal(openbaov1alpha1.TLSModeExternal))
	g.Expect(instance.Spec.Unseal).NotTo(BeNil())
	g.Expect(instance.Spec.Unseal.Type).To(Equal("static"))

	// The API-server egress allowance, resolved from the live cluster rather than
	// hardcoded: without it the operator-rendered NetworkPolicy admits only the
	// in-cluster service VIP on port 443, which a CNI enforcing egress against the
	// post-DNAT destination never matches. envtest's apiserver maintains the
	// well-known EndpointSlice itself, so the addresses asserted here are the ones a
	// real cluster would supply.
	g.Expect(instance.Spec.Network).NotTo(BeNil())
	apiServerSlice := &discoveryv1.EndpointSlice{}
	g.Expect(c.Get(ctx, client.ObjectKey{
		Name: apiServerEndpointSliceName, Namespace: apiServerEndpointSliceNamespace,
	}, apiServerSlice)).To(Succeed(), "envtest must publish the API server's EndpointSlice")
	liveAddresses := []string{}
	for i := range apiServerSlice.Endpoints {
		liveAddresses = append(liveAddresses, apiServerSlice.Endpoints[i].Addresses...)
	}
	g.Expect(liveAddresses).NotTo(BeEmpty())
	g.Expect(instance.Spec.Network.APIServerEndpointIPs).To(ConsistOf(liveAddresses))

	// self-init is one-shot, so the eight requests and their order are frozen at
	// create time: a mount or auth method must be enabled before anything writes
	// under it.
	g.Expect(instance.Spec.SelfInit).NotTo(BeNil())
	g.Expect(instance.Spec.SelfInit.Enabled).To(BeTrue())
	selfInitNames := make([]string, 0, len(instance.Spec.SelfInit.Requests))
	for _, req := range instance.Spec.SelfInit.Requests {
		selfInitNames = append(selfInitNames, req.Name)
	}
	g.Expect(selfInitNames).To(Equal([]string{
		"barbican_kv",
		"barbican_secretstore_policy",
		"approle_auth",
		"barbican_approle_role",
		"kubernetes_auth",
		"kubernetes_auth_config",
		"provisioner_policy",
		"provisioner_k8s_role",
	}))

	// The NetworkPolicy allowlist names the barbican-operator and the Barbican API
	// pods, each pinned to an exact namespace: a wildcard peer would admit any pod
	// carrying the label from anywhere in the cluster.
	g.Expect(instance.Spec.Network).NotTo(BeNil())
	g.Expect(instance.Spec.Network.TrustedIngressPeers).To(HaveLen(2))
	for i, peer := range instance.Spec.Network.TrustedIngressPeers {
		g.Expect(peer.NamespaceSelector).NotTo(BeNil(), "ingress peer %d must pin a namespace", i)
		g.Expect(peer.NamespaceSelector.MatchLabels).NotTo(BeEmpty(),
			"ingress peer %d must not select every namespace", i)
		g.Expect(peer.PodSelector).NotTo(BeNil(), "ingress peer %d must pin a pod label", i)
		g.Expect(peer.PodSelector.MatchLabels).NotTo(BeEmpty(),
			"ingress peer %d must not select every pod", i)
	}

	// The two transport Certificates the External TLS mode consumes. They have no Go
	// type, so they are addressed unstructured exactly as the projection creates them.
	for _, suffix := range []string{barbicanOpenBaoServerCertSuffix, barbicanOpenBaoCACertSuffix} {
		cert := &unstructured.Unstructured{}
		cert.SetGroupVersionKind(certificateGVK)
		g.Expect(c.Get(ctx, client.ObjectKey{Name: instanceName + suffix, Namespace: ns.Name}, cert)).
			To(Succeed(), "the instance's %q Certificate must be projected", suffix)
		secretName, _, serr := unstructured.NestedString(cert.Object, "spec", "secretName")
		g.Expect(serr).NotTo(HaveOccurred())
		g.Expect(secretName).To(Equal(instanceName+suffix),
			"the operator mounts the certificate from this fixed-name Secret")
	}

	// The provisioner account the instance's Kubernetes-auth role binds to. Every
	// consumer mints a token for it explicitly and for the instance audience, so the
	// auto-mounted default-audience token is turned off.
	provisionerSA := &corev1.ServiceAccount{}
	g.Expect(c.Get(ctx, client.ObjectKey{
		Name: instanceName + barbicanOpenBaoProvisionerSuffix, Namespace: ns.Name,
	}, provisionerSA)).To(Succeed(), "the provisioner ServiceAccount must be projected")
	g.Expect(provisionerSA.AutomountServiceAccountToken).NotTo(BeNil())
	g.Expect(*provisionerSA.AutomountServiceAccountToken).To(BeFalse())

	// The cluster-scoped binding that lets the instance run the TokenReview every
	// Kubernetes-auth login needs. No namespace deletion and no owner-reference
	// cascade reaches it, so the ownership labels are its only handle.
	authDelegator := &rbacv1.ClusterRoleBinding{}
	g.Expect(c.Get(ctx, client.ObjectKey{
		Name: barbicanOpenBaoAuthDelegatorName(instanceName, ns.Name),
	}, authDelegator)).To(Succeed(), "the auth-delegator ClusterRoleBinding must be projected")
	g.Expect(authDelegator.RoleRef.Name).To(Equal("system:auth-delegator"))
	g.Expect(authDelegator.Subjects).To(HaveLen(1))
	g.Expect(authDelegator.Subjects[0].Name).To(Equal(instanceName + barbicanOpenBaoPodSASuffix))
	g.Expect(isControlPlaneChild(authDelegator, cp)).To(BeTrue(),
		"a cluster-scoped child lives and dies by the ownership labels")

	// The TokenRequest grant the barbican-operator mints the provisioner token with,
	// scoped by resourceNames to that one account.
	tokenRole := &rbacv1.Role{}
	g.Expect(c.Get(ctx, client.ObjectKey{
		Name: instanceName + barbicanOpenBaoTokenGrantSuffix, Namespace: ns.Name,
	}, tokenRole)).To(Succeed(), "the TokenRequest Role must be projected")
	g.Expect(tokenRole.Rules).To(HaveLen(1))
	g.Expect(tokenRole.Rules[0].Resources).To(Equal([]string{"serviceaccounts/token"}))
	g.Expect(tokenRole.Rules[0].ResourceNames).To(Equal([]string{instanceName + barbicanOpenBaoProvisionerSuffix}))
	tokenBinding := &rbacv1.RoleBinding{}
	g.Expect(c.Get(ctx, client.ObjectKey{
		Name: instanceName + barbicanOpenBaoTokenGrantSuffix, Namespace: ns.Name,
	}, tokenBinding)).To(Succeed(), "the TokenRequest RoleBinding must be projected")
	g.Expect(tokenBinding.Subjects).To(HaveLen(1))
	g.Expect(tokenBinding.Subjects[0].Name).To(Equal(defaultBarbicanOperatorServiceAccount))
	g.Expect(tokenBinding.Subjects[0].Namespace).To(Equal(defaultBarbicanOperatorNamespace))

	// The static-seal key, carrying the INSTANCE's controller owner reference: both
	// live in the same namespace, so deleting the instance reaps its seal key with it
	// and the openbao-operator reads that reference as the proof it may adopt a
	// pre-existing unseal Secret.
	unsealSecret := &corev1.Secret{}
	g.Eventually(func() error {
		if err := c.Get(ctx, client.ObjectKey{
			Name: instanceName + barbicanOpenBaoUnsealSecretSuffix, Namespace: ns.Name,
		}, unsealSecret); err != nil {
			return err
		}
		if metav1.GetControllerOf(unsealSecret) == nil {
			return fmt.Errorf("the unseal Secret does not carry the instance's controller reference yet")
		}
		return nil
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"the unseal Secret must be adopted by the instance once there is an instance to reference")
	g.Expect(unsealSecret.Data).To(HaveKey(barbicanOpenBaoUnsealSecretKey))
	unsealOwner := metav1.GetControllerOf(unsealSecret)
	g.Expect(unsealOwner.Kind).To(Equal("OpenBaoCluster"))
	g.Expect(unsealOwner.Name).To(Equal(instanceName))

	// The tenant that admits the namespace to the openbao-operator. Nothing else in
	// this cluster admits it, so the ControlPlane creates one of its own.
	tenant := &openbaov1alpha1.OpenBaoTenant{}
	g.Expect(c.Get(ctx, client.ObjectKey{
		Name: instanceName + barbicanOpenBaoTenantSuffix, Namespace: ns.Name,
	}, tenant)).To(Succeed(), "the OpenBaoTenant admitting the namespace must be projected")
	g.Expect(tenant.Spec.TargetNamespace).To(Equal(ns.Name))

	// --- The instance gate. Until the instance serves requests, BarbicanReady parks
	// on WaitingForOpenBaoInstance and NEITHER the secret store NOR the child is
	// projected: a store attached to an initialising instance reports
	// ProvisioningDenied and would have to be re-driven from a failure state. ---
	storeKey := client.ObjectKey{Name: barbicanSecretStoreName(cp), Namespace: ns.Name}
	barbicanKey := client.ObjectKey{Name: barbicanName(cp), Namespace: ns.Name}
	g.Eventually(func() string {
		live := &c5c3v1alpha1.ControlPlane{}
		if err := c.Get(ctx, cpKey, live); err != nil {
			return ""
		}
		cond := conditions.GetCondition(live.Status.Conditions, conditionTypeBarbicanReady)
		if cond == nil {
			return ""
		}
		return cond.Reason
	}, itEventuallyTimeout, itPollInterval).Should(Equal("WaitingForOpenBaoInstance"),
		"the Barbican gates ahead of the instance are open, so the only one left is the instance itself")
	g.Expect(apierrors.IsNotFound(c.Get(ctx, storeKey, &barbicanv1alpha1.BarbicanSecretStore{}))).To(BeTrue(),
		"no BarbicanSecretStore may be attached while the instance behind it is still initialising")
	g.Expect(apierrors.IsNotFound(c.Get(ctx, barbicanKey, &barbicanv1alpha1.Barbican{}))).To(BeTrue(),
		"no Barbican child may be projected while its secret store has no server to point at")

	// --- Open the gate: the instance serves requests. ---
	simulateOpenBaoClusterAvailableWhenPresent(t, ctx, c, instanceKey)

	// The secret store attaches the Barbican to that instance, and is its default:
	// it is the only store projected, and a Barbican with no default store never
	// reaches Ready.
	projectedStore := &barbicanv1alpha1.BarbicanSecretStore{}
	g.Eventually(func() error {
		return c.Get(ctx, storeKey, projectedStore)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"the BarbicanSecretStore must be projected once the instance is Available")
	g.Expect(projectedStore.Spec.BarbicanRef.Name).To(Equal(barbicanName(cp)),
		"the store references its Barbican by name (inverted attachment)")
	g.Expect(projectedStore.Spec.IsDefault).To(BeTrue())
	g.Expect(projectedStore.Spec.OpenBao).NotTo(BeNil())
	g.Expect(projectedStore.Spec.OpenBao.InstanceRef).NotTo(BeNil(),
		"a dedicated store names the instance rather than a server URL")
	g.Expect(projectedStore.Spec.OpenBao.InstanceRef.Name).To(Equal(instanceName))
	g.Expect(projectedStore.Spec.OpenBao.KVMountpoint).To(Equal(defaultBarbicanKVMountpoint))

	projectedBarbican := &barbicanv1alpha1.Barbican{}
	g.Eventually(func() error {
		return c.Get(ctx, barbicanKey, projectedBarbican)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"the Barbican child must be projected once its secret store is attached")

	// Release and image: the canonical repository with the release-derived tag.
	g.Expect(projectedBarbican.Spec.OpenStackRelease).To(Equal("2025.2"))
	g.Expect(projectedBarbican.Spec.Image.Repository).To(Equal(defaultBarbicanRepository))
	g.Expect(projectedBarbican.Spec.Image.Tag).To(Equal("2025.2"), "Barbican image tag must derive from openStackRelease")

	// Database: the shared managed cluster, the fixed "barbican" logical schema, and
	// the operator-owned engine-issued DB credential (Dynamic is the default on the
	// managed shared database, mirroring Keystone with barbican-scoped objects).
	g.Expect(projectedBarbican.Spec.Database.ClusterRef).NotTo(BeNil(), "Barbican database clusterRef must be wired")
	g.Expect(projectedBarbican.Spec.Database.ClusterRef.Name).To(Equal("openstack-db"))
	g.Expect(projectedBarbican.Spec.Database.Database).To(Equal("barbican"))
	g.Expect(projectedBarbican.Spec.Database.SecretRef.Name).To(Equal(barbicanDBCredentialSecretName(cp)),
		"managed Barbican DB secretRef must point at the operator-owned per-CP Barbican DB-credential Secret")
	g.Expect(projectedBarbican.Spec.Database.SecretRef.Key).To(Equal("password"))
	g.Expect(projectedBarbican.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeDynamic),
		"the projected Barbican DB credential defaults to Dynamic (engine-issued)")

	// Cache: the shared managed Memcached.
	g.Expect(projectedBarbican.Spec.Cache.ClusterRef).NotTo(BeNil(), "Barbican cache clusterRef must be wired")
	g.Expect(projectedBarbican.Spec.Cache.ClusterRef.Name).To(Equal("openstack-memcached"))

	// Keystone endpoint: derived TOP-DOWN from the naming convention, because
	// Barbican validates every token against it from inside the cluster.
	g.Expect(projectedBarbican.Spec.KeystoneEndpoint).To(
		Equal(fmt.Sprintf("http://%s.%s.svc:5000/v3", keystoneName(cp), ns.Name)),
		"keystoneEndpoint must be the cluster-local Keystone Service URL",
	)
	g.Expect(projectedBarbican.Spec.KeystonePublicEndpoint).To(BeEmpty(),
		"this fixture exposes Keystone nowhere externally, so the child falls back to the internal endpoint")

	g.Expect(projectedBarbican.Spec.Region).To(Equal(c5c3v1alpha1.DefaultRegion),
		"the region the defaulting webhook materialized reaches the child")

	// Service user: the identity the Barbican registration provisions (its own
	// service-barbican project) and the consumer Secret it delivers.
	g.Expect(projectedBarbican.Spec.ServiceUser.Username).To(Equal("barbican"))
	g.Expect(projectedBarbican.Spec.ServiceUser.ProjectName).To(Equal("service-barbican"))
	g.Expect(projectedBarbican.Spec.ServiceUser.SecretRef.Name).
		To(Equal(keystoneServiceCredentialsSecretName(barbicanReg)),
			"Barbican service-user password must read the registration's consumer Secret")
	g.Expect(projectedBarbican.Spec.ServiceUser.SecretRef.Key).To(Equal("password"))

	// The ControlPlane's RESOLVED ESO store selection — the store the child's own
	// ExternalSecrets are routed through, a separate concern from the
	// BarbicanSecretStore holding the tenant key material.
	g.Expect(projectedBarbican.Spec.SecretStoreRef).NotTo(BeNil(), "the resolved store ref must be projected")
	g.Expect(projectedBarbican.Spec.SecretStoreRef.Kind).To(Equal(commonv1.SecretStoreKindNamespaced))
	g.Expect(projectedBarbican.Spec.SecretStoreRef.Name).To(Equal(esoTenantStoreName))

	g.Expect(projectedBarbican.Spec.Gateway).To(BeNil(),
		"this fixture exposes Barbican nowhere externally, so no HTTPRoute is projected")
	g.Expect(projectedBarbican.Spec.Deployment.Replicas).To(Equal(int32(1)),
		"services.barbican.replicas overrides the shared operator default")

	// The child is co-located with the ControlPlane, so ownership is a controller
	// owner reference rather than the labels a cross-namespace child carries.
	barbicanOwner := metav1.GetControllerOf(projectedBarbican)
	g.Expect(barbicanOwner).NotTo(BeNil(), "Barbican child must be controller-owned by the ControlPlane")
	g.Expect(barbicanOwner.Kind).To(Equal("ControlPlane"))
	g.Expect(barbicanOwner.Name).To(Equal(cp.Name))

	simulateBarbicanReadyWhenPresent(t, ctx, c, barbicanKey)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeBarbicanReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 9: Neutron child (the network service, behind Barbican). It carries
	// two gates none of its peers have: the OVNCentral its ML2/OVN mechanism driver
	// programs, whose readiness a step that owns nothing mirrors into OVNReady, and
	// the shared bus, which the ControlPlane resolves itself and delivers into the
	// network service's namespace as a Secret the child references brownfield. Both
	// were opened in Phase 1, so what is left is the Dynamic-default credential and
	// the child. ---
	ovnReady := waitForControlPlaneCondition(t, ctx, c, cpKey,
		conditionTypeOVNReady, metav1.ConditionTrue, itEventuallyTimeout)
	g.Expect(ovnReady.Reason).To(Equal("OVNCentralReady"),
		"the referenced central serves both databases and has published its client Secret")

	simulateNeutronDBCredentialSyncWhenPresent(t, ctx, c, cp)

	// The bus delivery: one Secret beside the Neutron child carrying the transport
	// URL assembled from the broker's default-user credentials, on the root vhost.
	// The neutron operator resolves spec.messaging in the Neutron's own namespace,
	// which is where this lands.
	busSecret := &corev1.Secret{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Name: neutronMessagingSecretName(cp), Namespace: ns.Name}, busSecret)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"the ControlPlane must deliver the shared bus into the network service's namespace")
	g.Expect(string(busSecret.Data[commonv1.DefaultTransportURLSecretKey])).To(
		Equal(fmt.Sprintf("rabbit://default-user:broker-password@cp-rabbitmq.%s.svc:5672/", ns.Name)),
		"the transport URL is assembled from the four keys of the broker's default-user Secret")
	// A plaintext bus declares no TLS, so no CA mirror is written beside it: a trust
	// anchor nothing reads would be residue the teardown has to sweep.
	g.Expect(apierrors.IsNotFound(c.Get(ctx, client.ObjectKey{
		Name: neutronMessagingCASecretName(cp), Namespace: ns.Name,
	}, &corev1.Secret{}))).To(BeTrue(), "a bus without tls leaves no CA mirror in the Neutron namespace")

	neutronKey := client.ObjectKey{Name: neutronName(cp), Namespace: ns.Name}
	projectedNeutron := &neutronv1alpha1.Neutron{}
	g.Eventually(func() error {
		return c.Get(ctx, neutronKey, projectedNeutron)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"Neutron child should be projected once KeystoneReady, OVNReady and the Neutron registration are ready")

	// Release and image: the canonical repository with the release-derived tag.
	g.Expect(projectedNeutron.Spec.OpenStackRelease).To(Equal("2025.2"))
	g.Expect(projectedNeutron.Spec.Image.Repository).To(Equal(defaultNeutronRepository))
	g.Expect(projectedNeutron.Spec.Image.Tag).To(Equal("2025.2"), "Neutron image tag must derive from openStackRelease")

	// extraConfig: this fixture declares no services.neutron.extraConfig, so the
	// merge is the global section alone.
	g.Expect(projectedNeutron.Spec.ExtraConfig).To(Equal(map[string]map[string]string{
		"cors": {"allowed_origin": "https://dashboard.example.com"},
	}), "globalExtraConfig reaches the network service too")

	// Database: the shared managed cluster, the fixed "neutron" logical schema, and
	// the operator-owned engine-issued DB credential (Dynamic is the default on the
	// managed shared database, as it is for every peer).
	g.Expect(projectedNeutron.Spec.Database.ClusterRef).NotTo(BeNil(), "Neutron database clusterRef must be wired")
	g.Expect(projectedNeutron.Spec.Database.ClusterRef.Name).To(Equal("openstack-db"))
	g.Expect(projectedNeutron.Spec.Database.Database).To(Equal("neutron"))
	g.Expect(projectedNeutron.Spec.Database.SecretRef.Name).To(Equal(neutronDBCredentialSecretName(cp)),
		"managed Neutron DB secretRef must point at the operator-owned per-CP Neutron DB-credential Secret")
	g.Expect(projectedNeutron.Spec.Database.SecretRef.Key).To(Equal("password"))
	g.Expect(projectedNeutron.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeDynamic),
		"the projected Neutron DB credential defaults to Dynamic (engine-issued)")

	// Cache: the shared managed Memcached.
	g.Expect(projectedNeutron.Spec.Cache.ClusterRef).NotTo(BeNil(), "Neutron cache clusterRef must be wired")
	g.Expect(projectedNeutron.Spec.Cache.ClusterRef.Name).To(Equal("openstack-memcached"))

	// Keystone endpoint: derived TOP-DOWN from the naming convention, because
	// Neutron validates every token against it from inside the cluster.
	g.Expect(projectedNeutron.Spec.KeystoneEndpoint).To(
		Equal(fmt.Sprintf("http://%s.%s.svc:5000/v3", keystoneName(cp), ns.Name)),
		"keystoneEndpoint must be the cluster-local Keystone Service URL",
	)
	g.Expect(projectedNeutron.Spec.KeystonePublicEndpoint).To(BeEmpty(),
		"this fixture exposes Keystone nowhere externally, so the child falls back to the internal endpoint")

	// Service user: the identity the Neutron registration provisions (its own
	// service-neutron project) and the consumer Secret it delivers, in the admin
	// domain the registration resolves the account in.
	g.Expect(projectedNeutron.Spec.ServiceUser.Username).To(Equal("neutron"))
	g.Expect(projectedNeutron.Spec.ServiceUser.ProjectName).To(Equal("service-neutron"))
	g.Expect(projectedNeutron.Spec.ServiceUser.UserDomainName).To(Equal(adminDomainName(cp)))
	g.Expect(projectedNeutron.Spec.ServiceUser.ProjectDomainName).To(Equal(adminDomainName(cp)))
	g.Expect(projectedNeutron.Spec.ServiceUser.SecretRef.Name).
		To(Equal(keystoneServiceCredentialsSecretName(neutronReg)),
			"Neutron service-user password must read the registration's consumer Secret")
	g.Expect(projectedNeutron.Spec.ServiceUser.SecretRef.Key).To(Equal("password"))

	// The ControlPlane's RESOLVED store selection, so the child never falls back to
	// its own shared-cluster-store default.
	g.Expect(projectedNeutron.Spec.SecretStoreRef).NotTo(BeNil(), "the resolved store ref must be projected")
	g.Expect(projectedNeutron.Spec.SecretStoreRef.Kind).To(Equal(commonv1.SecretStoreKindNamespaced))
	g.Expect(projectedNeutron.Spec.SecretStoreRef.Name).To(Equal(esoTenantStoreName))

	// Two replica counts, one overridden and one not: the RPC workers take the
	// declared count, the API pods the shared operator default.
	g.Expect(projectedNeutron.Spec.Deployment.Replicas).To(Equal(commonv1.DefaultReplicas),
		"replicas fall back to the shared operator default when services.neutron sets none")
	g.Expect(projectedNeutron.Spec.Workers.Deployment.Replicas).To(Equal(int32(1)),
		"services.neutron.workerReplicas sizes both RPC worker Deployments")

	// The bus reaches the child as a brownfield secretRef naming the Secret asserted
	// above, never as the managed clusterRef the ControlPlane resolved it from: the
	// neutron operator would look for that RabbitmqCluster in the Neutron's own
	// namespace on the Neutron's own cluster.
	g.Expect(projectedNeutron.Spec.Messaging.ClusterRef).To(BeNil())
	g.Expect(projectedNeutron.Spec.Messaging.SecretRef).NotTo(BeNil())
	g.Expect(projectedNeutron.Spec.Messaging.SecretRef.Name).To(Equal(neutronMessagingSecretName(cp)))
	g.Expect(projectedNeutron.Spec.Messaging.SecretRef.Key).To(Equal(commonv1.DefaultTransportURLSecretKey))
	g.Expect(projectedNeutron.Spec.Messaging.TLS).To(BeNil(),
		"the bus declares no tls, so the child names no CA mirror")

	// The OVN control plane, with the namespace RESOLVED by the ControlPlane rather
	// than left for the child to default.
	g.Expect(projectedNeutron.Spec.OVN.CentralRef.Name).To(Equal("cp-ovn"))
	g.Expect(projectedNeutron.Spec.OVN.CentralRef.Namespace).To(Equal(ns.Name),
		"an empty ref namespace resolves to the ControlPlane's own namespace")

	g.Expect(projectedNeutron.Spec.APIServer).To(BeNil(),
		"spec.apiServer is deliberately unset: the child-side uWSGI defaults stay authoritative")
	g.Expect(projectedNeutron.Spec.OVNDBSync).To(BeNil(),
		"spec.ovnDBSync is deliberately unset: the child-side schedule stays authoritative")

	// The child is co-located with the ControlPlane, so ownership is a controller
	// owner reference rather than the labels a cross-namespace child carries.
	neutronOwner := metav1.GetControllerOf(projectedNeutron)
	g.Expect(neutronOwner).NotTo(BeNil(), "Neutron child must be controller-owned by the ControlPlane")
	g.Expect(neutronOwner.Kind).To(Equal("ControlPlane"))
	g.Expect(neutronOwner.Name).To(Equal(cp.Name))

	simulateNeutronReadyWhenPresent(t, ctx, c, neutronKey)
	neutronReady := waitForControlPlaneCondition(t, ctx, c, cpKey,
		conditionTypeNeutronReady, metav1.ConditionTrue, itEventuallyTimeout)
	g.Expect(neutronReady.Reason).To(Equal("NeutronReady"))

	// --- Aggregate: Ready=True. ---
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeReady, metav1.ConditionTrue, itEventuallyTimeout)

	// Final assertions on the converged CR.
	final := &c5c3v1alpha1.ControlPlane{}
	g.Expect(c.Get(ctx, cpKey, final)).To(Succeed(), "get converged ControlPlane")

	for _, condType := range []string{
		conditionTypeInfrastructureReady,
		conditionTypeDBCredentialsReady,
		conditionTypeKeystoneReady,
		conditionTypeHorizonReady,
		conditionTypeKORCReady,
		conditionTypeAdminCredentialReady,
		conditionTypeCatalogReady,
		conditionTypeServiceAccountsReady,
		conditionTypeGlanceReady,
		conditionTypePlacementReady,
		conditionTypeBarbicanReady,
		conditionTypeOVNReady,
		conditionTypeNeutronReady,
		conditionTypeReady,
	} {
		cond := meta.FindStatusCondition(final.Status.Conditions, condType)
		g.Expect(cond).NotTo(BeNil(), "condition %s should exist", condType)
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "condition %s should be True", condType)
	}

	// Ready reports the aggregate reason.
	readyCond := meta.FindStatusCondition(final.Status.Conditions, conditionTypeReady)
	g.Expect(readyCond.Reason).To(Equal("AllReady"), "Ready reason should be AllReady")

	// status.observedGeneration tracks the reconciled generation.
	g.Expect(final.Status.ObservedGeneration).To(Equal(final.Generation),
		"status.observedGeneration should match the CR generation")

	// status.services reports one entry per configured service, all ready, in the
	// stable order setServicesStatus produces (keystone, horizon, glance, placement,
	// barbican, neutron).
	g.Expect(final.Status.Services).To(HaveLen(6),
		"six services are configured (keystone, horizon, glance, placement, barbican, neutron)")
	g.Expect(final.Status.Services[0].Name).To(Equal("keystone"))
	g.Expect(final.Status.Services[0].Ready).To(BeTrue())
	g.Expect(final.Status.Services[1].Name).To(Equal("horizon"))
	g.Expect(final.Status.Services[1].Ready).To(BeTrue())
	g.Expect(final.Status.Services[2].Name).To(Equal("glance"))
	g.Expect(final.Status.Services[2].Ready).To(BeTrue())
	g.Expect(final.Status.Services[3].Name).To(Equal("placement"))
	g.Expect(final.Status.Services[3].Ready).To(BeTrue())
	g.Expect(final.Status.Services[3].Release).To(Equal("2025.2"))
	g.Expect(final.Status.Services[4].Name).To(Equal("barbican"))
	g.Expect(final.Status.Services[4].Ready).To(BeTrue())
	g.Expect(final.Status.Services[4].Release).To(Equal("2025.2"))
	g.Expect(final.Status.Services[5].Name).To(Equal("neutron"))
	g.Expect(final.Status.Services[5].Ready).To(BeTrue())
	g.Expect(final.Status.Services[5].Release).To(Equal("2025.2"))

	// Every condition records the generation it was observed against.
	for _, cond := range final.Status.Conditions {
		g.Expect(cond.ObservedGeneration).To(Equal(final.Generation),
			"condition %s ObservedGeneration should match CR generation", cond.Type)
	}

	// The reflected admin application-credential status mirrors the simulated AC.
	g.Expect(final.Status.AdminApplicationCredential).NotTo(BeNil(),
		"status.adminApplicationCredential should be populated")
	g.Expect(final.Status.AdminApplicationCredential.ID).To(Equal("ac-id-integration"))
	catalogCond := meta.FindStatusCondition(final.Status.Conditions, conditionTypeCatalogReady)
	g.Expect(catalogCond).NotTo(BeNil(), "CatalogReady condition should exist")
	g.Expect(catalogCond.Status).To(Equal(metav1.ConditionTrue),
		"CatalogReady condition should be True once the catalog is registered")

	// --- Intermediate projected specs (TE7b). Asserting only the final
	// aggregate condition would not catch a projection regression, so verify the
	// shape of each projected child the chain produced. ---

	// Keystone CR: image tag derived from openStackRelease, clusterRefs wired to
	// the infra CRs, and the global oslo.policy override merged in.
	ks := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: keystoneName(final), Namespace: ns.Name}, ks)).
		To(Succeed(), "get projected Keystone CR")
	g.Expect(ks.Spec.Image.Repository).To(Equal(defaultKeystoneRepository))
	g.Expect(ks.Spec.Image.Tag).To(Equal("2025.2"), "Keystone image tag must derive from openStackRelease")
	g.Expect(ks.Spec.Database.ClusterRef).NotTo(BeNil(), "Keystone database clusterRef must be wired")
	g.Expect(ks.Spec.Database.ClusterRef.Name).To(Equal("openstack-db"))
	g.Expect(ks.Spec.Database.SecretRef.Name).To(Equal(dbCredentialSecretName(final)),
		"managed Keystone DB secretRef must point at the operator-owned per-CP DB-credential Secret")
	g.Expect(ks.Spec.Database.SecretRef.Key).To(Equal("password"))
	// Admin-ref analog in managed mode reconcileKeystone overrides
	// the projected child's bootstrap admin-password ref via effectiveAdminPasswordSecretRef
	// to the operator-owned per-CP Secret. Because the spec ref stays "keystone-admin"
	// (see the fixture DECISION), this differs from the spec ref and genuinely proves
	// the override fired.
	g.Expect(ks.Spec.Bootstrap.AdminPasswordSecretRef.Name).To(Equal(adminPasswordSecretName(final)),
		"managed Keystone admin-password secretRef must point at the operator-owned per-CP admin Secret")
	g.Expect(ks.Spec.Bootstrap.AdminPasswordSecretRef.Key).To(Equal("password"))
	g.Expect(ks.Spec.Cache.ClusterRef).NotTo(BeNil(), "Keystone cache clusterRef must be wired")
	g.Expect(ks.Spec.Cache.ClusterRef.Name).To(Equal("openstack-memcached"))
	g.Expect(ks.Spec.PolicyOverrides).NotTo(BeNil(), "merged policy must be projected")
	g.Expect(ks.Spec.PolicyOverrides.Rules).To(HaveKeyWithValue("identity:list_users", "role:admin"),
		"global oslo.policy override must be merged into the projected Keystone CR")

	// ApplicationCredential CR: the restricted/Unrestricted inversion. restricted
	// defaults to true (least privilege), so K-ORC's Unrestricted must be false.
	ac := &orcv1alpha1.ApplicationCredential{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: adminAppCredentialName(final), Namespace: ns.Name}, ac)).
		To(Succeed(), "get projected ApplicationCredential CR")
	g.Expect(ac.Spec.Resource).NotTo(BeNil())
	g.Expect(ac.Spec.Resource.Unrestricted).NotTo(BeNil())
	g.Expect(*ac.Spec.Resource.Unrestricted).To(BeFalse(),
		"restricted:true (default) MUST project to K-ORC Unrestricted=false (critical inversion)")
	// The AC mints via the operator-owned password-cloud (so a delete+recreate
	// re-mint can always re-authenticate), NOT k-orc-clouds-yaml.
	g.Expect(ac.Spec.CloudCredentialsRef.SecretName).To(Equal(adminPasswordCloudSecretName(final)))

	// Catalog: identity Service + public Endpoint shape.
	svc := &orcv1alpha1.Service{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: keystoneServiceName(final), Namespace: ns.Name}, svc)).
		To(Succeed(), "get projected identity Service CR")
	g.Expect(svc.Spec.Resource).NotTo(BeNil())
	g.Expect(svc.Spec.Resource.Type).To(Equal("identity"), "Service type must be identity")
	// The catalog keeps using k-orc-clouds-yaml (only the AC moves to the
	// password-cloud); this locks in that split.
	g.Expect(svc.Spec.CloudCredentialsRef.SecretName).To(Equal(korcCloudsYamlSecretName))

	ep := &orcv1alpha1.Endpoint{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: keystoneEndpointName(final), Namespace: ns.Name}, ep)).
		To(Succeed(), "get projected identity Endpoint CR")
	g.Expect(ep.Spec.Resource).NotTo(BeNil())
	g.Expect(ep.Spec.Resource.Interface).To(Equal("public"), "Endpoint interface must be public")
	g.Expect(string(ep.Spec.Resource.ServiceRef)).To(Equal(keystoneServiceName(final)),
		"Endpoint serviceRef must reference the identity Service CR")
	g.Expect(ep.Spec.Resource.URL).NotTo(BeEmpty(), "Endpoint URL must be derived")

	// The built-in services' catalog rows: each KeystoneService child registers its
	// own managed Service and one Endpoint per interface, under the child-prefixed
	// names and in the ControlPlane's namespace. They authenticate through the admin
	// PASSWORD cloud rather than k-orc-clouds-yaml, because K-ORC must still reach
	// Keystone to DELETE the row at teardown and the application credential is
	// revoked by its own finalizer while that delete is in flight. Per D6 the
	// internal AND public Endpoints both advertise the in-cluster API URL — this
	// fixture sets no gateway, so the public interface has not yet diverged to an
	// external URL. Each child is Ready, which is what the ControlPlane folds into
	// the service's own readiness.
	for _, registration := range []struct {
		child       *c5c3v1alpha1.KeystoneService
		serviceType string
		url         string
	}{
		{glanceReg, "image", fmt.Sprintf("http://%s.%s.svc:9292", glanceName(final), ns.Name)},
		{placementReg, "placement", fmt.Sprintf("http://%s.%s.svc:8778", placementName(final), ns.Name)},
		{barbicanReg, "key-manager", fmt.Sprintf("http://%s.%s.svc:9311", barbicanName(final), ns.Name)},
	} {
		rowSvc := &orcv1alpha1.Service{}
		g.Expect(c.Get(ctx, types.NamespacedName{
			Name: keystoneServiceCatalogServiceRef(registration.child), Namespace: ns.Name,
		}, rowSvc)).To(Succeed(), "get the projected %q Service CR", registration.serviceType)
		g.Expect(rowSvc.Spec.Resource).NotTo(BeNil())
		g.Expect(rowSvc.Spec.Resource.Type).To(Equal(registration.serviceType),
			"Service type must be %q", registration.serviceType)
		g.Expect(rowSvc.Spec.CloudCredentialsRef.SecretName).To(Equal(adminPasswordCloudSecretName(final)))

		for _, iface := range []c5c3v1alpha1.ExternalEndpointType{
			c5c3v1alpha1.ExternalEndpointTypeInternal,
			c5c3v1alpha1.ExternalEndpointTypePublic,
		} {
			rowEP := &orcv1alpha1.Endpoint{}
			g.Expect(c.Get(ctx, types.NamespacedName{
				Name: keystoneServiceCatalogEndpointRef(registration.child, iface), Namespace: ns.Name,
			}, rowEP)).To(Succeed(), "get the projected %q %q Endpoint CR", registration.serviceType, iface)
			g.Expect(rowEP.Spec.Resource).NotTo(BeNil())
			g.Expect(rowEP.Spec.Resource.Interface).To(Equal(string(iface)))
			g.Expect(string(rowEP.Spec.Resource.ServiceRef)).
				To(Equal(keystoneServiceCatalogServiceRef(registration.child)),
					"the Endpoint must reference its own row's Service CR")
			g.Expect(rowEP.Spec.Resource.URL).To(Equal(registration.url),
				"both interfaces advertise the in-cluster API URL (no gateway in this fixture)")
		}

		liveRegistration := &c5c3v1alpha1.KeystoneService{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(registration.child), liveRegistration)).
			To(Succeed(), "get the converged %q registration", registration.serviceType)
		g.Expect(conditions.AllTrue(liveRegistration.Status.Conditions, conditionTypeReady)).To(BeTrue(),
			"the %q registration must report Ready=True", registration.serviceType)
	}

	// --- Per-CR OpenBao RemoteKey lock. ---
	//
	// On the single-ControlPlane path the admin app-credential PushSecret must
	// already mirror to the per-CR OpenBao path scoped by the CR's Namespace and
	// Name (adminAppCredentialRemoteKeyFor), NOT the legacy flat
	// openstack/keystone/admin/app-credential. Locking this here on the baseline
	// end-to-end test guards the single-CP rendering of the path the multi-CP test
	// asserts is distinct between CRs.
	adminPS := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(ctx, types.NamespacedName{
		Namespace: childNamespace(final), Name: adminAppCredentialPushSecretName(final),
	}, adminPS)).To(Succeed(), "get admin app-credential PushSecret")
	g.Expect(adminPS.Spec.Data).NotTo(BeEmpty(), "admin app-credential PushSecret must declare a Data entry")
	g.Expect(adminPS.Spec.Data[0].Match.RemoteRef.RemoteKey).To(Equal(adminAppCredentialRemoteKeyFor(final)),
		"admin app-credential PushSecret RemoteKey must be the per-CR OpenBao path")
}

// TestIntegration_UnreadyHorizonDoesNotParkBootstrap locks the tail-group
// isolation contract: a dashboard (Horizon) child that never becomes Ready must
// NOT park the identity bootstrap (KORC, AdminCredential, Catalog,
// ServiceAccounts), the image service (Glance), Placement, or Barbican at the
// tail. Under the old short-circuiting chain the reconcile stalled at the unready
// Horizon step and never reached the members behind it; the sequential tail group
// instead attempts every member on each pass and lets each one self-gate, so
// everything except HorizonReady converges while HorizonReady — and with it the
// aggregate Ready — stays False.
//
// The test drives the full phased bring-up exactly as
// TestIntegration_FullReconcile_ManagedToReady does but NEVER simulates the
// Horizon child ready, then asserts the admin ApplicationCredential is still
// minted and every sub-condition except HorizonReady (GlanceReady,
// PlacementReady and BarbicanReady included) reaches True.
func TestIntegration_UnreadyHorizonDoesNotParkBootstrap(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupRegisteringControlPlaneEnvTest(t)

	// The OpenBao-backed ClusterSecretStore must be Ready before the chain
	// reaches the credential gates.
	ensureReadyClusterSecretStore(t, ctx, c)

	// Isolated test namespace per run (namespace-per-test with GenerateName).
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-horizon-unready-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")
	// Pre-seed the per-tenant SecretStore Ready so the ESOTenantStore gate opens
	// (envtest has no ESO controller).
	ensureReadySecretStore(t, ctx, c, esoTenantStoreName, ns.Name)

	// Enable Horizon, Glance, Placement and Barbican on the managed CR exactly as the
	// full-chain test does — but this test deliberately never simulates the Horizon
	// child ready.
	cp := integrationManagedControlPlane("cp", ns.Name)
	cp.Spec.Services.Horizon = &c5c3v1alpha1.ServiceHorizonSpec{}
	cp.Spec.Services.Glance = integrationGlanceService()
	cp.Spec.Services.Placement = integrationPlacementService()
	cp.Spec.Services.Barbican = integrationBarbicanService()

	// Admin password Secret the KORC sub-reconciler hashes to drive the mint (the
	// cleartext source readAdminPassword resolves via the effective per-CP ref).
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminPasswordSecretName(cp), Namespace: ns.Name},
		Data:       map[string][]byte{"password": []byte("super-secret-admin-password")},
	}
	g.Expect(c.Create(ctx, adminSecret)).To(Succeed(), "create admin password Secret")

	g.Expect(c.Create(ctx, cp)).To(Succeed(), "create ControlPlane CR")
	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}

	// --- Phase 1: Infrastructure (MariaDB + Memcached). ---
	simulateMariaDBReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "openstack-db", Namespace: ns.Name})
	simulateMemcachedReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "openstack-memcached", Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeInfrastructureReady, metav1.ConditionTrue, itEventuallyTimeout)

	// DBCredentials: wait for the per-CP DB-credential ExternalSecret, then
	// simulate the ESO sync. The projected-shape assertions live in the full-chain
	// test; here we only need the gate to open.
	dbCredES := &esov1.ExternalSecret{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)}, dbCredES)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "operator must create the per-CP DB credential ExternalSecret")
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)})).
		To(Succeed(), "simulate per-CP DB credential ExternalSecret sync")
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeDBCredentialsReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 1.5: AdminPassword. ---
	simulateAdminPasswordExternalSecretSyncWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminPasswordReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 2: Keystone child (the last member of the blocking prefix). ---
	simulateKeystoneReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: keystoneName(cp), Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeKeystoneReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 2.5: the point of the test. The tail group has been entered: the
	// Horizon child is projected but NEVER simulated ready, so reconcileHorizon
	// settles HorizonReady False. Under the old short-circuiting chain the
	// reconcile would park here; the tail group instead continues to KORC, so the
	// admin ApplicationCredential is minted despite HorizonReady=False. ---
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeHorizonReady, metav1.ConditionFalse, itEventuallyTimeout)

	adminAC := &orcv1alpha1.ApplicationCredential{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: adminAppCredentialName(cp)}, adminAC)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"the admin ApplicationCredential must be minted even though HorizonReady is False")

	// --- Phase 3: K-ORC admin ApplicationCredential. ---
	simulateApplicationCredentialAvailableWhenPresent(t, ctx, c,
		client.ObjectKey{Name: adminAppCredentialName(cp), Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeKORCReady, metav1.ConditionTrue, itEventuallyTimeout)

	cloudsYamlES := &esov1.ExternalSecret{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: korcCloudsYamlSecretName}, cloudsYamlES)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "operator must create the k-orc clouds.yaml ExternalSecret")
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Namespace: ns.Name, Name: korcCloudsYamlSecretName})).
		To(Succeed(), "simulate k-orc clouds.yaml ExternalSecret sync")

	// --- Phase 4: AdminCredential push. ---
	simulatePushSecretSyncedWhenPresent(t, ctx, c,
		client.ObjectKey{Name: adminAppCredentialPushSecretName(cp), Namespace: childNamespace(cp)})
	simulateCloudsYamlMaterializedWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminCredentialReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 5: Catalog (the identity row, the only one the ControlPlane
	// registers itself). ---
	simulateCatalogServiceEndpointAvailableWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeCatalogReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 5.5: the three built-in registrations that carry the remaining
	// catalog rows and service identities, and the condition aggregating them. ---
	simulateBuiltinRegistrationConvergedWhenPresent(t, ctx, c, cp,
		client.ObjectKey{Name: glanceName(cp), Namespace: cp.GlanceNamespace()})
	simulateBuiltinRegistrationConvergedWhenPresent(t, ctx, c, cp,
		client.ObjectKey{Name: placementName(cp), Namespace: cp.PlacementNamespace()})
	simulateBuiltinRegistrationConvergedWhenPresent(t, ctx, c, cp,
		client.ObjectKey{Name: barbicanName(cp), Namespace: cp.BarbicanNamespace()})

	serviceAccountsReady := waitForControlPlaneCondition(t, ctx, c, cpKey,
		conditionTypeServiceAccountsReady, metav1.ConditionTrue, itEventuallyTimeout)
	g.Expect(serviceAccountsReady.Reason).To(Equal(reasonServiceAccountsProvisioned))

	// --- Phase 6: Glance child. ---
	simulateGlanceDBCredentialSyncWhenPresent(t, ctx, c, cp)
	simulateGlanceReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: glanceName(cp), Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeGlanceReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 7: Placement child. ---
	simulatePlacementDBCredentialSyncWhenPresent(t, ctx, c, cp)
	simulatePlacementReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: placementName(cp), Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypePlacementReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 8: Barbican child (the last tail-group member). Its dedicated OpenBao
	// instance is a gate no other service has, so open it before the child can be
	// projected at all. ---
	simulateBarbicanDBCredentialSyncWhenPresent(t, ctx, c, cp)
	simulateOpenBaoClusterAvailableWhenPresent(t, ctx, c,
		client.ObjectKey{Name: barbicanOpenBaoName(cp), Namespace: ns.Name})
	simulateBarbicanReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: barbicanName(cp), Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeBarbicanReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Final assertion: every sub-condition except HorizonReady (GlanceReady,
	// PlacementReady, BarbicanReady, NamespacesReady and ESOTenantStoreReady
	// included) is True, HorizonReady is False, and the aggregate Ready is False.
	// This read is deterministic because updateStatus re-aggregates Ready on every
	// status write, so once BarbicanReady flips True the aggregate has already been
	// recomputed against the still-False HorizonReady. ---
	final := &c5c3v1alpha1.ControlPlane{}
	g.Expect(c.Get(ctx, cpKey, final)).To(Succeed(), "get the ControlPlane after the phased bring-up")

	for _, condType := range subConditionTypes {
		cond := conditions.GetCondition(final.Status.Conditions, condType)
		g.Expect(cond).NotTo(BeNil(), "condition %s should exist", condType)
		if condType == conditionTypeHorizonReady {
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse),
				"HorizonReady must stay False — the dashboard child is never simulated ready")
			continue
		}
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue),
			"sub-condition %s must reach True even though HorizonReady is False", condType)
	}

	readyCond := conditions.GetCondition(final.Status.Conditions, conditionTypeReady)
	g.Expect(readyCond).NotTo(BeNil(), "aggregate Ready condition should exist")
	g.Expect(readyCond.Status).To(Equal(metav1.ConditionFalse),
		"the aggregate Ready must stay False while HorizonReady is False")
}

// TestIntegration_MinimalManagedToReady drives the SMALLEST valid ControlPlane —
// only openStackRelease + services.keystone — to the aggregate Ready=True. The CR
// omits spec.infrastructure and spec.korc entirely, so the defaulting webhook
// must construct the database, cache, and admin-credential blocks from
// its well-known defaults before the validating webhook's required-checks run.
// The test asserts all eight defaults on the converged spec, then drives
// every sub-reconciler to Ready exactly as TestIntegration_FullReconcile_ManagedToReady
// does, and finally asserts the projected Keystone CR's clusterRefs are wired to
// the defaulted managed infra — proving the defaults flow through projection.
func TestIntegration_MinimalManagedToReady(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	// The OpenBao-backed ClusterSecretStore must be Ready before the chain
	// reaches the credential gates (#476).
	ensureReadyClusterSecretStore(t, ctx, c)

	// Isolated test namespace per run (namespace-per-test with GenerateName).
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-minimal-cp-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")

	// Create the MINIMAL ControlPlane CR. Create succeeds because the defaulting
	// webhook fills passwordSecretRef.name (and the whole infra/korc blocks) BEFORE
	// the validating webhook's required-check runs.
	cp := integrationMinimalControlPlane("cp", ns.Name)
	g.Expect(c.Create(ctx, cp)).To(Succeed(),
		"create minimal ControlPlane CR (required fields satisfied by the defaulting webhook)")
	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}

	// --- Core of the test: assert the well-known defaults (plus the cloudCredentialsRef.secretName) on the spec the webhook constructed from the
	// omitted infrastructure/korc blocks. ---
	defaulted := &c5c3v1alpha1.ControlPlane{}
	g.Expect(c.Get(ctx, cpKey, defaulted)).To(Succeed(), "re-fetch defaulted ControlPlane")
	db := defaulted.Spec.Infrastructure.Database
	cache := defaulted.Spec.Infrastructure.Cache
	cred := defaulted.Spec.KORC.AdminCredential
	g.Expect(db.Database).To(Equal(c5c3v1alpha1.DefaultDatabaseName),
		"defaulting webhook must materialize database.database")
	g.Expect(db.SecretRef.Name).To(Equal(c5c3v1alpha1.DefaultDatabaseSecretName),
		"defaulting webhook must materialize database.secretRef.name")
	g.Expect(db.ClusterRef).NotTo(BeNil(), "defaulting webhook must materialize database.clusterRef")
	g.Expect(db.ClusterRef.Name).To(Equal(c5c3v1alpha1.DefaultDatabaseClusterRefName),
		"defaulting webhook must materialize database.clusterRef.name")
	g.Expect(cache.Backend).To(Equal(c5c3v1alpha1.DefaultCacheBackend),
		"defaulting webhook must materialize cache.backend")
	g.Expect(cache.ClusterRef).NotTo(BeNil(), "defaulting webhook must materialize cache.clusterRef")
	g.Expect(cache.ClusterRef.Name).To(Equal(c5c3v1alpha1.DefaultCacheClusterRefName),
		"defaulting webhook must materialize cache.clusterRef.name")
	g.Expect(cred.PasswordSecretRef.Name).To(Equal(c5c3v1alpha1.DefaultAdminPasswordSecretName),
		"defaulting webhook must materialize korc.adminCredential.passwordSecretRef.name")
	g.Expect(cred.PasswordSecretRef.Key).To(Equal(c5c3v1alpha1.DefaultAdminPasswordSecretKey),
		"defaulting webhook must materialize korc.adminCredential.passwordSecretRef.key")
	g.Expect(cred.CloudCredentialsRef.CloudName).To(Equal(c5c3v1alpha1.DefaultCloudName),
		"defaulting webhook must materialize korc.adminCredential.cloudCredentialsRef.cloudName")
	g.Expect(cred.CloudCredentialsRef.SecretName).To(Equal(c5c3v1alpha1.DefaultCloudCredentialsSecretName),
		"defaulting webhook must materialize korc.adminCredential.cloudCredentialsRef.secretName")

	// --- Phases 1-4: provision the per-ControlPlane dependency set (admin Secret,
	// clouds.yaml ExternalSecret) and drive Infrastructure -> Keystone -> KORC ->
	// AdminCredential to Ready. The shared helper provisions those dependencies and
	// the managed infra children at the DEFAULTED well-known names (via the same
	// Default* constants asserted above), so reusing it here still proves the
	// defaults flow through to the reconciler. ---
	driveControlPlaneToAdminCredentialReady(t, ctx, c, cp)

	// --- Phase 5: Catalog. The minimal CR sets no gateway/publicEndpoint, so
	// keystoneCatalogURL falls back to the in-cluster Service URL. CatalogReady is
	// gated on both child CRs reporting Available, so simulate the K-ORC actuator. ---
	simulateCatalogServiceEndpointAvailableWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeCatalogReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Aggregate: Ready=True. ---
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- The defaulted managed infra must flow through to the projected Keystone
	// CR's clusterRefs (proving the webhook defaults are honoured by projection). ---
	final := &c5c3v1alpha1.ControlPlane{}
	g.Expect(c.Get(ctx, cpKey, final)).To(Succeed(), "get converged ControlPlane")
	ks := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: keystoneName(final), Namespace: ns.Name}, ks)).
		To(Succeed(), "get projected Keystone CR")
	g.Expect(ks.Spec.Database.ClusterRef).NotTo(BeNil(), "Keystone database clusterRef must be wired")
	g.Expect(ks.Spec.Database.ClusterRef.Name).To(Equal(c5c3v1alpha1.DefaultDatabaseClusterRefName),
		"Keystone database clusterRef must reference the defaulted managed MariaDB")
	g.Expect(ks.Spec.Database.SecretRef.Name).To(Equal(dbCredentialSecretName(final)),
		"managed Keystone DB secretRef must point at the operator-owned per-CP DB-credential Secret")
	g.Expect(ks.Spec.Database.SecretRef.Key).To(Equal("password"))
	// Admin-ref analog the defaulted managed CR also gets the
	// operator-owned per-CP admin-password ref projected into the Keystone child.
	g.Expect(ks.Spec.Bootstrap.AdminPasswordSecretRef.Name).To(Equal(adminPasswordSecretName(final)),
		"managed Keystone admin-password secretRef must point at the operator-owned per-CP admin Secret")
	g.Expect(ks.Spec.Bootstrap.AdminPasswordSecretRef.Key).To(Equal("password"))
	g.Expect(ks.Spec.Cache.ClusterRef).NotTo(BeNil(), "Keystone cache clusterRef must be wired")
	g.Expect(ks.Spec.Cache.ClusterRef.Name).To(Equal(c5c3v1alpha1.DefaultCacheClusterRefName),
		"Keystone cache clusterRef must reference the defaulted managed Memcached")
}

// TestIntegration_DBCredentialsGate_BlocksKeystoneUntilSecretExists proves the
// DBCredentials gate blocks Keystone projection until the per-CP DB-credential
// ExternalSecret is Ready once Infrastructure is Ready the
// operator creates the DB-credential ExternalSecret, but DBCredentialsReady stays
// False with reason WaitingForDBCredentialSecret and NO Keystone CR is projected
// until the ExternalSecret syncs. Simulating the sync then flips DBCredentialsReady
// True and the Keystone CR appears — pointing at the operator-owned DB-credential
// Secret. This is the negative counterpart to the full-reconcile happy path: it
// pins that the gate genuinely holds Keystone back rather than projecting it early.
func TestIntegration_DBCredentialsGate_BlocksKeystoneUntilSecretExists(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	// The OpenBao-backed ClusterSecretStore must be Ready before the chain
	// reaches the credential gates (#476).
	ensureReadyClusterSecretStore(t, ctx, c)

	// Isolated test namespace per run (namespace-per-test with GenerateName).
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-dbgate-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")
	// Pre-seed the per-tenant SecretStore Ready so the pipeline reaches the
	// DB-credential gate this test exercises (see driveControlPlaneToAdminCredentialReady).
	ensureReadySecretStore(t, ctx, c, esoTenantStoreName, ns.Name)

	// Create the ControlPlane CR (the defaulting webhook fills region etc.).
	cp := integrationManagedControlPlane("cp", ns.Name)

	// Admin password Secret (mirrors driveControlPlaneToAdminCredentialReady) at the
	// operator-owned per-CP name so the later sub-reconcilers don't error — this test
	// stops at the gate, but create it for realism/consistency.
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminPasswordSecretName(cp), Namespace: ns.Name},
		Data:       map[string][]byte{"password": []byte("super-secret-admin-password")},
	}
	g.Expect(c.Create(ctx, adminSecret)).To(Succeed(), "create admin password Secret")

	g.Expect(c.Create(ctx, cp)).To(Succeed(), "create ControlPlane CR")
	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}

	// --- Phase 1: Infrastructure (MariaDB + Memcached) -> InfrastructureReady. ---
	simulateMariaDBReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "openstack-db", Namespace: ns.Name})
	simulateMemcachedReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "openstack-memcached", Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeInfrastructureReady, metav1.ConditionTrue, itEventuallyTimeout)

	// The operator creates the per-CP DB-credential ExternalSecret as soon as
	// Infrastructure is Ready.
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)}, &esov1.ExternalSecret{})
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "operator must create the per-CP DB credential ExternalSecret")

	// --- The gate: BEFORE simulating the ExternalSecret sync, DBCredentialsReady must
	// be False with reason WaitingForDBCredentialSecret, and NO Keystone CR may exist. ---
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeDBCredentialsReady, metav1.ConditionFalse, itEventuallyTimeout)
	gated := &c5c3v1alpha1.ControlPlane{}
	g.Expect(c.Get(ctx, cpKey, gated)).To(Succeed(), "get gated ControlPlane")
	dbCond := meta.FindStatusCondition(gated.Status.Conditions, conditionTypeDBCredentialsReady)
	g.Expect(dbCond).NotTo(BeNil(), "DBCredentialsReady condition must exist while gated")
	g.Expect(dbCond.Reason).To(Equal("WaitingForDBCredentialSecret"),
		"DBCredentialsReady must report it is waiting on the DB credential Secret")

	// No premature/flapping Keystone CR: it must stay NotFound across a short window.
	g.Consistently(func() bool {
		err := c.Get(ctx, types.NamespacedName{Name: keystoneName(cp), Namespace: ns.Name}, &keystonev1alpha1.Keystone{})
		return apierrors.IsNotFound(err)
	}, 2*time.Second, itPollInterval).Should(BeTrue(),
		"Keystone CR must NOT be projected while the DB credential gate is closed")

	// --- Open the gate: simulate the DB-credential ExternalSecret sync. ---
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)})).
		To(Succeed(), "simulate per-CP DB credential ExternalSecret sync")
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeDBCredentialsReady, metav1.ConditionTrue, itEventuallyTimeout)

	// with DBCredentials open the chain reaches the admin-password gate, which
	// ALSO blocks Keystone. Sync the operator-created admin-password ExternalSecret so
	// AdminPasswordReady flips True and the Keystone projection can proceed.
	simulateAdminPasswordExternalSecretSyncWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminPasswordReady, metav1.ConditionTrue, itEventuallyTimeout)

	// Now the Keystone CR is projected, pointing at the operator-owned DB-credential Secret.
	gatedKs := &keystonev1alpha1.Keystone{}
	g.Eventually(func() error {
		return c.Get(ctx, types.NamespacedName{Name: keystoneName(cp), Namespace: ns.Name}, gatedKs)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"Keystone CR must be projected once the DB credential gate opens")
	g.Expect(gatedKs.Spec.Database.SecretRef.Name).To(Equal(dbCredentialSecretName(cp)),
		"projected Keystone DB secretRef must point at the per-CP DB-credential Secret")
}

// TestIntegration_AdminPasswordGate_BlocksKeystoneUntilExternalSecretReady proves the
// AdminPassword gate blocks Keystone projection until the per-CP admin-password
// ExternalSecret is Ready once Infrastructure and the DB-credential
// gate are satisfied the chain reaches reconcileAdminPassword, which creates the
// admin-password ExternalSecret — but AdminPasswordReady stays False with reason
// WaitingForAdminPasswordSecret and NO Keystone CR is projected until the
// ExternalSecret syncs. Simulating the sync then flips AdminPasswordReady True and the
// Keystone CR appears, its bootstrap admin-password ref pointing at the operator-owned
// per-CP admin Secret. This is the admin-password counterpart to
// TestIntegration_DBCredentialsGate_BlocksKeystoneUntilSecretExists: it pins that the
// gate genuinely holds Keystone back rather than projecting it early.
func TestIntegration_AdminPasswordGate_BlocksKeystoneUntilExternalSecretReady(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	// The OpenBao-backed ClusterSecretStore must be Ready before the chain
	// reaches the credential gates (#476).
	ensureReadyClusterSecretStore(t, ctx, c)

	// Isolated test namespace per run (namespace-per-test with GenerateName).
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-adminpwgate-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")
	// Pre-seed the per-tenant SecretStore Ready so the pipeline reaches the
	// admin-password gate this test exercises (see driveControlPlaneToAdminCredentialReady).
	ensureReadySecretStore(t, ctx, c, esoTenantStoreName, ns.Name)

	// Create the ControlPlane CR (the defaulting webhook fills region etc.).
	cp := integrationManagedControlPlane("cp", ns.Name)

	// Admin password Secret at the operator-owned per-CP name. This test stops at the
	// admin-password gate, but create it for realism/consistency with the full path.
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminPasswordSecretName(cp), Namespace: ns.Name},
		Data:       map[string][]byte{"password": []byte("super-secret-admin-password")},
	}
	g.Expect(c.Create(ctx, adminSecret)).To(Succeed(), "create admin password Secret")

	g.Expect(c.Create(ctx, cp)).To(Succeed(), "create ControlPlane CR")
	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}

	// --- Phase 1: Infrastructure (MariaDB + Memcached) -> InfrastructureReady. ---
	simulateMariaDBReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "openstack-db", Namespace: ns.Name})
	simulateMemcachedReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "openstack-memcached", Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeInfrastructureReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Open the DB-credential gate so the chain advances to the admin-password gate. ---
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)}, &esov1.ExternalSecret{})
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "operator must create the per-CP DB credential ExternalSecret")
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)})).
		To(Succeed(), "simulate per-CP DB credential ExternalSecret sync")
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeDBCredentialsReady, metav1.ConditionTrue, itEventuallyTimeout)

	// The operator creates the per-CP admin-password ExternalSecret as soon as the
	// chain reaches reconcileAdminPassword (DB-credential gate open).
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: adminPasswordSecretName(cp)}, &esov1.ExternalSecret{})
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "operator must create the per-CP admin password ExternalSecret")

	// --- The gate: BEFORE simulating the admin-password ExternalSecret sync,
	// AdminPasswordReady must be False with reason WaitingForAdminPasswordSecret, and
	// NO Keystone CR may exist. ---
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminPasswordReady, metav1.ConditionFalse, itEventuallyTimeout)
	gated := &c5c3v1alpha1.ControlPlane{}
	g.Expect(c.Get(ctx, cpKey, gated)).To(Succeed(), "get gated ControlPlane")
	pwCond := meta.FindStatusCondition(gated.Status.Conditions, conditionTypeAdminPasswordReady)
	g.Expect(pwCond).NotTo(BeNil(), "AdminPasswordReady condition must exist while gated")
	g.Expect(pwCond.Reason).To(Equal("WaitingForAdminPasswordSecret"),
		"AdminPasswordReady must report it is waiting on the admin password Secret")

	// No premature/flapping Keystone CR: it must stay NotFound across a short window.
	g.Consistently(func() bool {
		err := c.Get(ctx, types.NamespacedName{Name: keystoneName(cp), Namespace: ns.Name}, &keystonev1alpha1.Keystone{})
		return apierrors.IsNotFound(err)
	}, 2*time.Second, itPollInterval).Should(BeTrue(),
		"Keystone CR must NOT be projected while the admin password gate is closed")

	// --- Open the gate: simulate the admin-password ExternalSecret sync. ---
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Namespace: ns.Name, Name: adminPasswordSecretName(cp)})).
		To(Succeed(), "simulate per-CP admin password ExternalSecret sync")
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminPasswordReady, metav1.ConditionTrue, itEventuallyTimeout)

	// Now the Keystone CR is projected, its bootstrap admin-password ref pointing at
	// the operator-owned per-CP admin Secret.
	gatedKs := &keystonev1alpha1.Keystone{}
	g.Eventually(func() error {
		return c.Get(ctx, types.NamespacedName{Name: keystoneName(cp), Namespace: ns.Name}, gatedKs)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"Keystone CR must be projected once the admin password gate opens")
	g.Expect(gatedKs.Spec.Bootstrap.AdminPasswordSecretRef.Name).To(Equal(adminPasswordSecretName(cp)),
		"projected Keystone admin-password ref must point at the per-CP admin Secret")
}

// driveControlPlaneToAdminCredentialReady provisions the full per-ControlPlane
// dependency set in cp.Namespace and drives the CR through phases 1-4 of the
// sub-reconciler chain (Infrastructure -> Keystone -> KORC -> AdminCredential) to
// conditionTypeAdminCredentialReady=True, simulating each external dependency's
// readiness exactly as TestIntegration_FullReconcile_ManagedToReady does. It
// stops short of the Catalog/aggregate-Ready phases. The namespace and the CR
// must already exist.
//
// The two managed infra clusterRef children use the shared Default*
// constants (which equal the literal names integrationManagedControlPlane sets
// explicitly), while the admin password Secret uses the per-ControlPlane
// operator-owned name adminPasswordSecretName(cp) that effectiveAdminPasswordSecretRef
// resolves in managed mode — derived from cp.Name, so it is
// distinct per CR. This lets both consumers reuse the helper:
//   - TestIntegration_MultiControlPlane_DistinctAdminCredentialPaths, whose CRs set the infra names explicitly, and
//   - TestIntegration_MinimalManagedToReady, whose minimal CR omits the
//     infra/korc blocks so the defaulting webhook materializes the very same infra
//     names — so driving the simulators at the Default* names still proves the
//
// defaults flow through to the reconciler.
func driveControlPlaneToAdminCredentialReady(
	t testing.TB, ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane,
) {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := cp.Namespace

	// The operator provisions the per-tenant SecretStore (openbao-tenant-store)
	// every nil-ref ControlPlane defaults onto (reconcileESOTenantStore) and gates
	// the store-consuming sub-reconcilers on its Ready condition. envtest has no
	// ESO controller to flip that condition, so pre-seed it Ready here; the
	// operator's Server-Side Apply re-asserts the store spec without clobbering the
	// status subresource (apply.EnsureObject strips status from the apply body).
	ensureReadySecretStore(t, ctx, c, esoTenantStoreName, ns)

	// Admin password Secret the KORC sub-reconciler hashes to drive the mint, at the
	// operator-owned per-CP name effectiveAdminPasswordSecretRef resolves in managed
	// mode — readAdminPassword reads the cleartext via that ref.
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminPasswordSecretName(cp), Namespace: ns},
		Data:       map[string][]byte{"password": []byte("super-secret-admin-password")},
	}
	g.Expect(c.Create(ctx, adminSecret)).To(Succeed(), "create admin password Secret")

	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns}

	// --- Phase 1: Infrastructure (MariaDB + Memcached) at the defaulted clusterRef
	// names. ---
	simulateMariaDBReadyWhenPresent(t, ctx, c,
		client.ObjectKey{Name: c5c3v1alpha1.DefaultDatabaseClusterRefName, Namespace: ns})
	simulateMemcachedReadyWhenPresent(t, ctx, c,
		client.ObjectKey{Name: c5c3v1alpha1.DefaultCacheClusterRefName, Namespace: ns})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeInfrastructureReady, metav1.ConditionTrue, itEventuallyTimeout)

	// gate Keystone on the per-CP DB credential ExternalSecret. DECISION:
	// harness sync-simulation lives here to keep this helper's callers bisectable
	// (full suite green). This SHARED helper deliberately does NOT assert the
	// projected Keystone secretRef — that assertion lives in the
	// individual tests that fetch their own converged Keystone CR. Reviewer: please verify.
	dbCredES := &esov1.ExternalSecret{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns, Name: dbCredentialSecretName(cp)}, dbCredES)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "operator must create the per-CP DB credential ExternalSecret")
	// Managed mode defaults to Dynamic: the ExternalSecret draws from the per-CP
	// VaultDynamicSecret generator (no static Data refs). Per-tenant path
	// distinctness is asserted by TestIntegration_MultiControlPlane_DistinctDBCredentialPaths.
	g.Expect(dbCredES.Spec.DataFrom).NotTo(BeEmpty(), "Dynamic DB credential ExternalSecret must declare a generatorRef")
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Namespace: ns, Name: dbCredentialSecretName(cp)})).
		To(Succeed(), "simulate per-CP DB credential ExternalSecret sync")
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeDBCredentialsReady, metav1.ConditionTrue, itEventuallyTimeout)

	// gate Keystone on the per-CP admin-password ExternalSecret.
	// Sync-simulating here keeps this helper's callers bisectable (full suite green),
	// mirroring the DB-credential sync above.
	simulateAdminPasswordExternalSecretSyncWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminPasswordReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 2: Keystone child. ---
	simulateKeystoneReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: keystoneName(cp), Namespace: ns})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeKeystoneReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 3: K-ORC admin ApplicationCredential. ---
	simulateApplicationCredentialAvailableWhenPresent(t, ctx, c,
		client.ObjectKey{Name: adminAppCredentialName(cp), Namespace: ns})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeKORCReady, metav1.ConditionTrue, itEventuallyTimeout)

	// The K-ORC clouds.yaml ExternalSecret is created per-CR BY THE OPERATOR
	// (reconcileKORC -> ensureKORCCloudsYAMLExternalSecret) in the CR's own
	// namespace, no longer seeded by write-bootstrap-secrets.sh.
	// Each CR reads a DISTINCT per-CR OpenBao path (adminAppCredentialRemoteKeyFor) —
	// the meaningful multi-CP check here; full distinctness across CRs is asserted by
	// the caller via the PushSecret RemoteKeys. Assert the per-CR path, then simulate
	// its ESO sync so Phase 4 can progress.
	cloudsYamlES := &esov1.ExternalSecret{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns, Name: korcCloudsYamlSecretName}, cloudsYamlES)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "operator must create the k-orc clouds.yaml ExternalSecret")
	g.Expect(cloudsYamlES.Spec.Data).To(HaveLen(1), "clouds.yaml ExternalSecret must declare exactly one Data entry")
	g.Expect(cloudsYamlES.Spec.Data[0].RemoteRef.Key).To(Equal(adminAppCredentialRemoteKeyFor(cp)),
		"clouds.yaml ExternalSecret must read this CR's per-CR OpenBao path")
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Namespace: ns, Name: korcCloudsYamlSecretName})).
		To(Succeed(), "simulate k-orc clouds.yaml ExternalSecret sync")

	// --- Phase 4: AdminCredential push (gated on the synced clouds.yaml ES, the
	// admin app-credential PushSecret syncing to OpenBao, AND the materialized
	// k-orc-clouds-yaml Secret matching the assembled credential — the byte-compare
	// gate that closes the post-re-mint stale-credential window). ---
	simulatePushSecretSyncedWhenPresent(t, ctx, c,
		client.ObjectKey{Name: adminAppCredentialPushSecretName(cp), Namespace: childNamespace(cp)})
	simulateCloudsYamlMaterializedWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminCredentialReady, metav1.ConditionTrue, itEventuallyTimeout)
}

// TestIntegration_MultiControlPlane_DistinctAdminCredentialPaths brings up TWO
// ControlPlanes and drives both to AdminCredentialReady=True, then asserts each
// CR's admin-credential OpenBao path (the app-credential PushSecret RemoteKey) and
// its imported admin User CR name are scoped per-ControlPlane and distinct, so two
// ControlPlanes never clobber each other's admin credential on the cluster-global
// OpenBao backend.
//
// DECISION the two ControlPlanes use DIFFERENT names (cp-a,
// cp-b) in DIFFERENT namespaces (generated from test-mcp-a- / test-mcp-b-). The
// validating webhook enforces one ControlPlane per namespace,
// so the CRs MUST live in separate namespaces; the distinct names additionally
// make the imported admin User CR names (adminUserRef = "<name>-user-admin")
// differ, which the per-CR-name assertion below requires. The per-CR OpenBao path
// is scoped by BOTH Namespace and Name (adminAppCredentialRemoteKeyFor), so either
// axis alone would distinguish them — using both is the realistic deployment shape.
func TestIntegration_MultiControlPlane_DistinctAdminCredentialPaths(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	// The OpenBao-backed ClusterSecretStore must be Ready before either chain
	// reaches the credential gates (#476).
	ensureReadyClusterSecretStore(t, ctx, c)

	// Two isolated namespaces (namespace-per-CR with GenerateName).
	nsA := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-mcp-a-"}}
	nsB := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-mcp-b-"}}
	g.Expect(c.Create(ctx, nsA)).To(Succeed(), "create namespace A")
	g.Expect(c.Create(ctx, nsB)).To(Succeed(), "create namespace B")

	// Distinct names in distinct namespaces (see DECISION above).
	cpA := integrationManagedControlPlane("cp-a", nsA.Name)
	cpB := integrationManagedControlPlane("cp-b", nsB.Name)
	g.Expect(c.Create(ctx, cpA)).To(Succeed(), "create ControlPlane A")
	g.Expect(c.Create(ctx, cpB)).To(Succeed(), "create ControlPlane B")

	driveControlPlaneToAdminCredentialReady(t, ctx, c, cpA)
	driveControlPlaneToAdminCredentialReady(t, ctx, c, cpB)

	// --- Assert the admin app-credential OpenBao paths are per-CR and distinct. ---
	psA := &esov1alpha1.PushSecret{}
	psB := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(ctx, types.NamespacedName{
		Namespace: childNamespace(cpA), Name: adminAppCredentialPushSecretName(cpA),
	}, psA)).To(Succeed(), "get admin app-credential PushSecret for cp-a")
	g.Expect(c.Get(ctx, types.NamespacedName{
		Namespace: childNamespace(cpB), Name: adminAppCredentialPushSecretName(cpB),
	}, psB)).To(Succeed(), "get admin app-credential PushSecret for cp-b")

	g.Expect(psA.Spec.Data).NotTo(BeEmpty(), "cp-a PushSecret must declare a Data entry")
	g.Expect(psB.Spec.Data).NotTo(BeEmpty(), "cp-b PushSecret must declare a Data entry")
	keyA := psA.Spec.Data[0].Match.RemoteRef.RemoteKey
	keyB := psB.Spec.Data[0].Match.RemoteRef.RemoteKey

	g.Expect(keyA).To(Equal(adminAppCredentialRemoteKeyFor(cpA)),
		"cp-a OpenBao path must be the per-CR path")
	g.Expect(keyB).To(Equal(adminAppCredentialRemoteKeyFor(cpB)),
		"cp-b OpenBao path must be the per-CR path")
	g.Expect(keyA).NotTo(Equal(keyB), "the two ControlPlanes' admin OpenBao paths must be distinct")

	// Each path is scoped by its own ControlPlane's Namespace AND Name.
	g.Expect(keyA).To(ContainSubstring(cpA.Namespace), "cp-a path must contain cp-a's namespace")
	g.Expect(keyA).To(ContainSubstring(cpA.Name), "cp-a path must contain cp-a's name")
	g.Expect(keyB).To(ContainSubstring(cpB.Namespace), "cp-b path must contain cp-b's namespace")
	g.Expect(keyB).To(ContainSubstring(cpB.Name), "cp-b path must contain cp-b's name")

	// --- Assert the imported admin User CRs are per-CR and distinctly named. ---
	userA := &orcv1alpha1.User{}
	userB := &orcv1alpha1.User{}
	g.Expect(c.Get(ctx, types.NamespacedName{
		Namespace: childNamespace(cpA), Name: adminUserRef(cpA),
	}, userA)).To(Succeed(), "get imported admin User CR for cp-a")
	g.Expect(c.Get(ctx, types.NamespacedName{
		Namespace: childNamespace(cpB), Name: adminUserRef(cpB),
	}, userB)).To(Succeed(), "get imported admin User CR for cp-b")

	g.Expect(userA.Name).To(Equal(adminUserRef(cpA)), "cp-a admin User CR must be named per-CR")
	g.Expect(userB.Name).To(Equal(adminUserRef(cpB)), "cp-b admin User CR must be named per-CR")
	g.Expect(userA.Name).NotTo(Equal(userB.Name), "the two ControlPlanes' admin User CR names must be distinct")
}

// TestIntegration_MultiControlPlane_DistinctDBCredentialPaths brings up TWO
// ControlPlanes and drives both to AdminCredentialReady=True, then asserts each
// CR's per-tenant dynamic DB-credential engine path (the per-CP VaultDynamicSecret
// generator's spec.path) and the DB-credential Secret name are scoped
// per-ControlPlane and distinct, so two ControlPlanes never draw from the same
// engine role (AC 4).
//
// DECISION mirroring the admin-credential multi-CP test, the two
// ControlPlanes use DIFFERENT names (cp-a, cp-b) in DIFFERENT namespaces (the
// validating webhook enforces one ControlPlane per namespace), so the CRs MUST live
// in separate namespaces. The per-tenant creds path is keyed on the Namespace
// ALONE (dbDynamicRoleFor): the one-ControlPlane-per-namespace contract makes the
// namespace the unique tenant key, so distinct namespaces are what keep the two
// engine paths distinct.
func TestIntegration_MultiControlPlane_DistinctDBCredentialPaths(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	// The OpenBao-backed ClusterSecretStore must be Ready before either chain
	// reaches the credential gates (#476).
	ensureReadyClusterSecretStore(t, ctx, c)

	// Two isolated namespaces (namespace-per-CR with GenerateName).
	nsA := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-mcpdb-a-"}}
	nsB := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-mcpdb-b-"}}
	g.Expect(c.Create(ctx, nsA)).To(Succeed(), "create namespace A")
	g.Expect(c.Create(ctx, nsB)).To(Succeed(), "create namespace B")

	// Distinct names in distinct namespaces (see DECISION above).
	cpA := integrationManagedControlPlane("cp-a", nsA.Name)
	cpB := integrationManagedControlPlane("cp-b", nsB.Name)
	g.Expect(c.Create(ctx, cpA)).To(Succeed(), "create ControlPlane A")
	g.Expect(c.Create(ctx, cpB)).To(Succeed(), "create ControlPlane B")

	driveControlPlaneToAdminCredentialReady(t, ctx, c, cpA)
	driveControlPlaneToAdminCredentialReady(t, ctx, c, cpB)

	// --- Assert the per-CP dynamic DB-credential engine paths are per-CR and
	// distinct (AC 4). In Dynamic mode each ControlPlane owns a VaultDynamicSecret
	// generator reading its own per-tenant engine role, so a revoke against one
	// tenant's role cannot affect another. ---
	vdsA := &esgenv1alpha1.VaultDynamicSecret{}
	vdsB := &esgenv1alpha1.VaultDynamicSecret{}
	g.Expect(c.Get(ctx, types.NamespacedName{
		Namespace: childNamespace(cpA), Name: dbCredentialSecretName(cpA),
	}, vdsA)).To(Succeed(), "get VaultDynamicSecret for cp-a")
	g.Expect(c.Get(ctx, types.NamespacedName{
		Namespace: childNamespace(cpB), Name: dbCredentialSecretName(cpB),
	}, vdsB)).To(Succeed(), "get VaultDynamicSecret for cp-b")

	pathA := vdsA.Spec.Path
	pathB := vdsB.Spec.Path

	g.Expect(pathA).To(Equal(dbDynamicCredsPathFor(cpA)),
		"cp-a dynamic DB credential path must be the per-tenant engine path")
	g.Expect(pathB).To(Equal(dbDynamicCredsPathFor(cpB)),
		"cp-b dynamic DB credential path must be the per-tenant engine path")
	g.Expect(pathA).NotTo(Equal(pathB), "the two ControlPlanes' dynamic DB credential paths must be distinct")

	// Each path is keyed on its own ControlPlane's Namespace (the tenant key —
	// see dbDynamicRoleFor); the CR name is deliberately NOT part of the role.
	g.Expect(pathA).To(ContainSubstring(cpA.Namespace), "cp-a path must contain cp-a's namespace")
	g.Expect(pathB).To(ContainSubstring(cpB.Namespace), "cp-b path must contain cp-b's namespace")

	// The generator-backed ExternalSecrets exist and carry no static Data refs.
	esA := &esov1.ExternalSecret{}
	esB := &esov1.ExternalSecret{}
	g.Expect(c.Get(ctx, types.NamespacedName{
		Namespace: childNamespace(cpA), Name: dbCredentialSecretName(cpA),
	}, esA)).To(Succeed(), "get DB credential ExternalSecret for cp-a")
	g.Expect(c.Get(ctx, types.NamespacedName{
		Namespace: childNamespace(cpB), Name: dbCredentialSecretName(cpB),
	}, esB)).To(Succeed(), "get DB credential ExternalSecret for cp-b")
	g.Expect(esA.Spec.Data).To(BeEmpty(), "cp-a Dynamic ExternalSecret carries no static Data refs")
	g.Expect(esB.Spec.Data).To(BeEmpty(), "cp-b Dynamic ExternalSecret carries no static Data refs")

	// The DB-credential Secret NAMES are distinct too, so the two CRs never share a
	// materialised Secret in the (separate) namespaces.
	g.Expect(dbCredentialSecretName(cpA)).NotTo(Equal(dbCredentialSecretName(cpB)),
		"the two ControlPlanes' DB credential Secret names must be distinct")
}

// fakeKORCFinalizer mimics the finalizer K-ORC adds to the ApplicationCredential
// it manages. envtest runs no K-ORC controller, so the test injects this
// finalizer to hold the AC Terminating exactly as a real revoke-against-Keystone
// finalizer would, then removes it to let teardown complete.
const fakeKORCFinalizer = "openstack.k-orc.cloud/applicationcredential"

// fakeKORCUserFinalizer mimics the finalizer K-ORC adds to a User it manages. It
// carries korcFinalizerPrefix, so it holds a registration's managed User
// Terminating the way K-ORC's own finalizer would, and the teardown's
// force-release strips it the same way.
const fakeKORCUserFinalizer = "openstack.k-orc.cloud/user"

// fakeOpenBaoFinalizer mimics the finalizer the openbao-operator adds to an
// OpenBaoCluster it manages. envtest runs no openbao-operator, so the
// cross-namespace teardown test injects this finalizer to hold the instance
// Terminating exactly as the real one would while it unmounts and shuts the server
// down, then removes it to let the teardown continue.
const fakeOpenBaoFinalizer = "openbao.org/openbaocluster-finalizer"

// TestIntegration_ControlPlaneDeletion_SequencesORCTeardown proves the
// ORC-teardown finalizer sequences deletion: the operator deletes the owned
// K-ORC ApplicationCredential FIRST and holds the ControlPlane CR (and thus,
// via the deferred owner-ref GC cascade, the projected Keystone child) until the
// AC is gone, then releases the finalizer so the rest can be garbage-collected.
//
// envtest runs no garbage collector, so the owner-ref cascade that tears down
// Keystone/MariaDB once the ControlPlane CR is removed is asserted in the e2e
// test, not here. What this test pins is the sequencing invariant the finalizer
// adds on top of GC: while a K-ORC CR is still Terminating, the ControlPlane CR
// is held and Keystone is NOT yet torn down.
func TestIntegration_ControlPlaneDeletion_SequencesORCTeardown(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	// The OpenBao-backed ClusterSecretStore must be Ready before the chain
	// reaches the credential gates (#476); otherwise reconcileDBCredentials
	// short-circuits at SecretStoreNotReady and never projects the DB-credential
	// ExternalSecret driveControlPlaneToAdminCredentialReady waits for.
	ensureReadyClusterSecretStore(t, ctx, c)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-deletion-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")

	cp := integrationMinimalControlPlane("cp", ns.Name)
	g.Expect(c.Create(ctx, cp)).To(Succeed(), "create ControlPlane CR")
	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}

	// Drive the chain until the K-ORC ApplicationCredential and the projected
	// Keystone child exist.
	driveControlPlaneToAdminCredentialReady(t, ctx, c, cp)

	acKey := types.NamespacedName{Name: adminAppCredentialName(cp), Namespace: ns.Name}
	ksKey := types.NamespacedName{Name: keystoneName(cp), Namespace: ns.Name}
	g.Expect(c.Get(ctx, ksKey, &keystonev1alpha1.Keystone{})).To(Succeed(),
		"projected Keystone child must exist before deletion")

	// The ControlPlane must carry the ORC-teardown finalizer once reconciled.
	g.Eventually(func() bool {
		got := &c5c3v1alpha1.ControlPlane{}
		if err := c.Get(ctx, cpKey, got); err != nil {
			return false
		}
		return controllerutil.ContainsFinalizer(got, controlPlaneORCFinalizer)
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(), "ControlPlane must carry the ORC-teardown finalizer")

	// Inject a fake K-ORC finalizer onto the AC so deleting it leaves it
	// Terminating (as a real revoke-against-Keystone finalizer would), rather
	// than removing it outright in the GC-less envtest.
	g.Eventually(func() error {
		ac := &orcv1alpha1.ApplicationCredential{}
		if err := c.Get(ctx, acKey, ac); err != nil {
			return err
		}
		if controllerutil.ContainsFinalizer(ac, fakeKORCFinalizer) {
			return nil
		}
		controllerutil.AddFinalizer(ac, fakeKORCFinalizer)
		return c.Update(ctx, ac)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "inject fake K-ORC finalizer on the AC")

	// Delete the ControlPlane.
	g.Expect(c.Delete(ctx, cp)).To(Succeed(), "delete ControlPlane CR")

	// Teardown must be initiated: the operator deletes the AC, which is held
	// Terminating by the fake K-ORC finalizer.
	g.Eventually(func() bool {
		ac := &orcv1alpha1.ApplicationCredential{}
		if err := c.Get(ctx, acKey, ac); err != nil {
			return false
		}
		return !ac.DeletionTimestamp.IsZero()
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(), "operator must delete the owned AC first")

	// Sequencing invariant: while the AC is still Terminating, the ControlPlane
	// CR is HELD (finalizer not released) and the Keystone child is NOT torn down.
	g.Consistently(func() bool {
		gotCP := &c5c3v1alpha1.ControlPlane{}
		if err := c.Get(ctx, cpKey, gotCP); err != nil {
			return false
		}
		if !controllerutil.ContainsFinalizer(gotCP, controlPlaneORCFinalizer) {
			return false
		}
		return c.Get(ctx, ksKey, &keystonev1alpha1.Keystone{}) == nil
	}, 3*time.Second, itPollInterval).Should(BeTrue(),
		"ControlPlane finalizer must hold (and Keystone must survive) while the K-ORC CR is Terminating")

	// Release the AC by removing the fake finalizer; the operator then releases
	// the ControlPlane finalizer.
	g.Eventually(func() error {
		ac := &orcv1alpha1.ApplicationCredential{}
		err := c.Get(ctx, acKey, ac)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		controllerutil.RemoveFinalizer(ac, fakeKORCFinalizer)
		return c.Update(ctx, ac)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "remove fake K-ORC finalizer from the AC")

	// Once the AC is gone the operator releases the ControlPlane finalizer, so
	// both objects disappear.
	g.Eventually(func() bool {
		acErr := c.Get(ctx, acKey, &orcv1alpha1.ApplicationCredential{})
		cpErr := c.Get(ctx, cpKey, &c5c3v1alpha1.ControlPlane{})
		return apierrors.IsNotFound(acErr) && apierrors.IsNotFound(cpErr)
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
		"AC and ControlPlane must be removed once the K-ORC finalizer clears")
}

// TestIntegration_ControlPlaneDeletion_SweepsProjectedRegistrationsFirst proves
// the registration-first teardown order with both controllers on one manager:
// deleting a ControlPlane that projects a Placement registration deletes that
// registration BEFORE the admin ApplicationCredential, reports
// KORCReady=False/FinalizingServiceRegistrations on the CR for as long as the
// registration is held, and starts the K-ORC sweep only once the registration and
// its children are gone.
//
// What holds the registration is its own managed User: a K-ORC-shaped finalizer
// keeps the User Terminating, the KeystoneService controller keeps its finalizer
// while that child is still listed, and the ControlPlane waits on both. Removing
// the finalizer releases the whole chain, so the order is observed rather than
// inferred from timing. envtest runs no K-ORC controller and no garbage
// collector, so every deletion here is one an operator issued.
func TestIntegration_ControlPlaneDeletion_SweepsProjectedRegistrationsFirst(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	// Both controllers run on one manager: the ControlPlane projects the
	// registration, and the KeystoneService controller is what tears its children
	// down and holds the registration until they are gone.
	c, ctx, _ := setupRegisteringControlPlaneEnvTest(t)
	ensureReadyClusterSecretStore(t, ctx, c)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-registration-deletion-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")

	// Placement is the lightest built-in (its block carries no sub-spec), so the
	// registration it projects is the one child this teardown has to sequence.
	cp := integrationManagedControlPlane("cp", ns.Name)
	cp.Spec.Services.Placement = integrationPlacementService()
	g.Expect(c.Create(ctx, cp)).To(Succeed(), "create ControlPlane CR")
	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}

	driveControlPlaneToAdminCredentialReady(t, ctx, c, cp)

	// Drive the registration to Ready, so the catalog rows, the Keystone account
	// and the delivery objects the teardown collects all exist.
	reg := simulateBuiltinRegistrationConvergedWhenPresent(t, ctx, c, cp,
		client.ObjectKey{Name: placementName(cp), Namespace: cp.PlacementNamespace()})
	cond := waitForControlPlaneCondition(t, ctx, c, cpKey,
		conditionTypeServiceAccountsReady, metav1.ConditionTrue, itEventuallyTimeout)
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountsProvisioned),
		"the projected registration must be counted as provisioned before the teardown starts")

	childNS := keystoneServiceChildNamespace(cp)
	userKey := client.ObjectKey{Name: keystoneServiceUserRef(reg), Namespace: childNS}
	acKey := client.ObjectKey{Name: adminAppCredentialName(cp), Namespace: ns.Name}
	regKey := client.ObjectKeyFromObject(reg)

	// Hold the registration's managed User Terminating the way K-ORC's own
	// finalizer would while it deletes the Keystone user. That is what keeps the
	// registration itself standing once the ControlPlane deletes it.
	g.Eventually(func() error {
		user := &orcv1alpha1.User{}
		if err := c.Get(ctx, userKey, user); err != nil {
			return err
		}
		if controllerutil.ContainsFinalizer(user, fakeKORCUserFinalizer) {
			return nil
		}
		controllerutil.AddFinalizer(user, fakeKORCUserFinalizer)
		return c.Update(ctx, user)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"inject the fake K-ORC finalizer on the registration's managed User")

	// Hold the admin ApplicationCredential the same way, so the K-ORC sweep is
	// observable as a step of its own instead of completing the instant it runs.
	g.Eventually(func() error {
		ac := &orcv1alpha1.ApplicationCredential{}
		if err := c.Get(ctx, acKey, ac); err != nil {
			return err
		}
		if controllerutil.ContainsFinalizer(ac, fakeKORCFinalizer) {
			return nil
		}
		controllerutil.AddFinalizer(ac, fakeKORCFinalizer)
		return c.Update(ctx, ac)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"inject the fake K-ORC finalizer on the admin ApplicationCredential")

	g.Eventually(func() bool {
		got := &c5c3v1alpha1.ControlPlane{}
		if err := c.Get(ctx, cpKey, got); err != nil {
			return false
		}
		return controllerutil.ContainsFinalizer(got, controlPlaneORCFinalizer)
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
		"the ControlPlane must carry the ORC-teardown finalizer before it is deleted")

	g.Expect(c.Delete(ctx, cp)).To(Succeed(), "delete the ControlPlane")

	// The registration goes first, and the CR says so: the condition is what an
	// operator watching a teardown that is taking its time reads.
	g.Eventually(func() bool {
		gotReg := &c5c3v1alpha1.KeystoneService{}
		if err := c.Get(ctx, regKey, gotReg); err != nil {
			return false
		}
		if gotReg.DeletionTimestamp.IsZero() {
			return false
		}
		gotCP := &c5c3v1alpha1.ControlPlane{}
		if err := c.Get(ctx, cpKey, gotCP); err != nil {
			return false
		}
		korcReady := meta.FindStatusCondition(gotCP.Status.Conditions, conditionTypeKORCReady)
		return korcReady != nil && korcReady.Status == metav1.ConditionFalse &&
			korcReady.Reason == "FinalizingServiceRegistrations"
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
		"the registration must be deleted first, under a persisted KORCReady=False/FinalizingServiceRegistrations")

	// Sequencing invariant: while the registration is held, the ControlPlane
	// finalizer holds with it and the admin ApplicationCredential is untouched: it
	// is the credential the registration's own children authenticate through. The
	// window spans a full korcRequeueAfter, so the teardown is observed re-running
	// against the still-present registration rather than merely not having run
	// again yet.
	g.Consistently(func() bool {
		gotCP := &c5c3v1alpha1.ControlPlane{}
		if err := c.Get(ctx, cpKey, gotCP); err != nil {
			return false
		}
		if !controllerutil.ContainsFinalizer(gotCP, controlPlaneORCFinalizer) {
			return false
		}
		ac := &orcv1alpha1.ApplicationCredential{}
		if err := c.Get(ctx, acKey, ac); err != nil {
			return false
		}
		if !ac.DeletionTimestamp.IsZero() {
			return false
		}
		return c.Get(ctx, regKey, &c5c3v1alpha1.KeystoneService{}) == nil
	}, korcRequeueAfter+5*time.Second, itPollInterval).Should(BeTrue(),
		"the admin credential must outlive a registration that is still finishing its teardown")

	// Release the User; the registration's own teardown completes and takes every
	// child with it.
	g.Eventually(func() error {
		user := &orcv1alpha1.User{}
		err := c.Get(ctx, userKey, user)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		controllerutil.RemoveFinalizer(user, fakeKORCUserFinalizer)
		return c.Update(ctx, user)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"remove the fake K-ORC finalizer from the registration's managed User")

	// The K-ORC children carry ownership labels rather than owner references, so
	// nothing collects them: the registration's teardown is what deletes each one.
	g.Expect(reg.Spec.Catalog.Endpoints).NotTo(BeEmpty(),
		"the built-in registration must declare catalog endpoints for the loop below to assert anything")
	g.Eventually(func() bool {
		if !apierrors.IsNotFound(c.Get(ctx, regKey, &c5c3v1alpha1.KeystoneService{})) {
			return false
		}
		goneInChildNS := func(name string, obj client.Object) bool {
			return apierrors.IsNotFound(c.Get(ctx, client.ObjectKey{Name: name, Namespace: childNS}, obj))
		}
		if !goneInChildNS(keystoneServiceCatalogServiceRef(reg), &orcv1alpha1.Service{}) ||
			!goneInChildNS(keystoneServiceProjectRef(reg), &orcv1alpha1.Project{}) ||
			!goneInChildNS(keystoneServiceRoleImportRef(reg, "service"), &orcv1alpha1.Role{}) ||
			!goneInChildNS(keystoneServiceRoleAssignmentRef(reg, "service"), &orcv1alpha1.RoleAssignment{}) {
			return false
		}
		for _, ep := range reg.Spec.Catalog.Endpoints {
			if !goneInChildNS(keystoneServiceCatalogEndpointRef(reg, ep.Interface), &orcv1alpha1.Endpoint{}) {
				return false
			}
		}
		// The delivery objects stay in the registration's own namespace.
		pushErr := c.Get(ctx, client.ObjectKey{
			Name: keystoneServicePushSecretName(reg), Namespace: reg.Namespace,
		}, &esov1alpha1.PushSecret{})
		sourceErr := c.Get(ctx, client.ObjectKey{
			Name: keystoneServiceSourceSecretName(reg), Namespace: reg.Namespace,
		}, &corev1.Secret{})
		return apierrors.IsNotFound(pushErr) && apierrors.IsNotFound(sourceErr)
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
		"the registration, its K-ORC children and its delivery objects must all be gone")

	// Only now does the K-ORC sweep start: the admin credential is deleted after
	// the registration that resolved it, never before.
	g.Eventually(func() bool {
		ac := &orcv1alpha1.ApplicationCredential{}
		if err := c.Get(ctx, acKey, ac); err != nil {
			return false
		}
		return !ac.DeletionTimestamp.IsZero()
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
		"the K-ORC sweep must delete the admin credential once no registration remains")

	g.Eventually(func() error {
		ac := &orcv1alpha1.ApplicationCredential{}
		err := c.Get(ctx, acKey, ac)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		controllerutil.RemoveFinalizer(ac, fakeKORCFinalizer)
		return c.Update(ctx, ac)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"remove the fake K-ORC finalizer from the admin ApplicationCredential")

	g.Eventually(func() bool {
		acErr := c.Get(ctx, acKey, &orcv1alpha1.ApplicationCredential{})
		cpErr := c.Get(ctx, cpKey, &c5c3v1alpha1.ControlPlane{})
		return apierrors.IsNotFound(acErr) && apierrors.IsNotFound(cpErr)
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
		"the admin credential and the ControlPlane must be removed once the K-ORC finalizer clears")
}

// TestIntegration_ControlPlane_ValidationMarkers pins the validation-marker wave
// on the ControlPlane CRD against the envtest API server (CRD schema + CEL +
// validating webhook). Each rejection case mutates one field of an otherwise
// valid managed ControlPlane in its own namespace (the webhook enforces one
// ControlPlane per namespace); the final case asserts valid non-default
// accessRules, bootstrapResources, and publicEndpoint are accepted.
func TestIntegration_ControlPlane_ValidationMarkers(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	cases := []struct {
		name    string
		mutate  func(*c5c3v1alpha1.ControlPlane)
		wantErr bool
	}{
		{
			name:    "database both clusterRef and host",
			wantErr: true,
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Infrastructure.Database.Host = "db.example.com"
			},
		},
		{
			name:    "cache both clusterRef and servers",
			wantErr: true,
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Infrastructure.Cache.Servers = []string{"mc:11211"}
			},
		},
		{
			name:    "messaging both clusterRef and secretRef",
			wantErr: true,
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Infrastructure.Messaging = &commonv1.MessagingSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "x"},
					SecretRef:  &commonv1.SecretRefSpec{Name: "bus-url"},
				}
			},
		},
		{
			name:    "non-URL keystone publicEndpoint",
			wantErr: true,
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Keystone.PublicEndpoint = "keystone.example.com"
			},
		},
		{
			name:    "accessRule invalid method",
			wantErr: true,
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.KORC.AdminCredential.ApplicationCredential.AccessRules = []c5c3v1alpha1.AccessRule{
					{Service: "compute", Method: "FETCH", Path: "/v2.1/servers"},
				}
			},
		},
		{
			name:    "accessRule non-absolute path",
			wantErr: true,
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.KORC.AdminCredential.ApplicationCredential.AccessRules = []c5c3v1alpha1.AccessRule{
					{Service: "compute", Method: "GET", Path: "v2.1/servers"},
				}
			},
		},
		{
			name:    "bootstrapResource invalid kind",
			wantErr: true,
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.KORC.AdminCredential.BootstrapResources = []c5c3v1alpha1.BootstrapResourceSpec{
					{Kind: "Network", Name: "ext"},
				}
			},
		},
		{
			name:    "valid access rules, bootstrap resources, and public endpoint",
			wantErr: false,
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.com/v3"
				cp.Spec.KORC.AdminCredential.ApplicationCredential.AccessRules = []c5c3v1alpha1.AccessRule{
					{Service: "compute", Method: "GET", Path: "/v2.1/servers"},
				}
				cp.Spec.KORC.AdminCredential.BootstrapResources = []c5c3v1alpha1.BootstrapResourceSpec{
					{Kind: "Project", Name: "service"},
					{Kind: "Role", Name: "admin"},
				}
			},
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-marker-"}}
			g.Expect(c.Create(ctx, ns)).To(Succeed())

			cp := integrationManagedControlPlane(fmt.Sprintf("cp-marker-%d", i), ns.Name)
			tc.mutate(cp)

			err := c.Create(ctx, cp)
			if tc.wantErr {
				g.Expect(err).To(HaveOccurred(), "admission must reject: %s", tc.name)
				g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
					fmt.Sprintf("expected Invalid or Forbidden status error for %q, got: %v", tc.name, err))
			} else {
				g.Expect(err).NotTo(HaveOccurred(), "admission must accept: %s", tc.name)
			}
		})
	}

	// The messaging replica floor cannot be reached through the table above: a Go
	// zero int32 carries json:"replicas,omitempty", so the typed client drops the
	// field and the CRD default of 3 fills it back in. Submitting the ControlPlane
	// as an unstructured object is the only way to put replicas: 0 on the wire.
	t.Run("messaging replicas 0", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-marker-"}}
		g.Expect(c.Create(ctx, ns)).To(Succeed())

		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(
			integrationManagedControlPlane("cp-marker-replicas", ns.Name),
		)
		g.Expect(err).NotTo(HaveOccurred(), "convert the fixture to unstructured")
		obj := &unstructured.Unstructured{Object: raw}
		obj.SetGroupVersionKind(c5c3v1alpha1.GroupVersion.WithKind("ControlPlane"))
		g.Expect(unstructured.SetNestedMap(obj.Object, map[string]interface{}{
			"clusterRef": map[string]interface{}{"name": "x"},
			"replicas":   int64(0),
		}, "spec", "infrastructure", "messaging")).To(Succeed())

		err = c.Create(ctx, obj)
		g.Expect(err).To(HaveOccurred(), "a replica count below the CRD minimum must be rejected")
		g.Expect(apierrors.IsInvalid(err)).To(BeTrue(),
			fmt.Sprintf("expected an Invalid status error, got: %v", err))
	})
}

// TestIntegration_RetiredInlineFieldsArePruned proves the structural schema
// drops the two retired registration stanzas from a stored ControlPlane:
// spec.korc.serviceAccounts on a Managed CR and
// spec.services.keystone.external.catalog.managedEntries on an External one.
// Neither field is in the CRD schema and there is no conversion webhook, so an
// apply that still carries one is admitted (no admission rule names the retired
// paths) and comes back from etcd without it.
func TestIntegration_RetiredInlineFieldsArePruned(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	gvk := c5c3v1alpha1.GroupVersion.WithKind("ControlPlane")
	cases := []struct {
		name  string
		build func(name, namespace string) *c5c3v1alpha1.ControlPlane
		path  []string
		value []any
		// sibling is a LIVE field under the retired one's own parent block, set
		// alongside it and asserted to come back. NestedSlice reports found=false
		// when ANY intermediate key is missing, so a regenerated CRD that dropped
		// the whole parent — the hazard on a branch editing exactly this schema —
		// would otherwise read as "the retired field was pruned".
		sibling      []string
		siblingValue string
	}{
		{
			name:  "spec.korc.serviceAccounts",
			build: integrationMinimalControlPlane,
			path:  []string{"spec", "korc", "serviceAccounts"},
			value: []any{map[string]any{
				"name":    "nova",
				"project": map[string]any{"name": "service"},
			}},
			sibling:      []string{"spec", "korc", "adminCredential", "userName"},
			siblingValue: "admin",
		},
		{
			name:  "spec.services.keystone.external.catalog.managedEntries",
			build: integrationExternalControlPlane,
			path:  []string{"spec", "services", "keystone", "external", "catalog", "managedEntries"},
			value: []any{map[string]any{"type": "image"}},
			sibling: []string{
				"spec", "services", "keystone", "external", "catalog", "identityServiceName",
			},
			siblingValue: "keystone",
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-pruned-"}}
			g.Expect(c.Create(ctx, ns)).To(Succeed())

			raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(
				tc.build(fmt.Sprintf("cp-pruned-%d", i), ns.Name),
			)
			g.Expect(err).NotTo(HaveOccurred(), "convert the fixture to unstructured")
			obj := &unstructured.Unstructured{Object: raw}
			obj.SetGroupVersionKind(gvk)
			g.Expect(unstructured.SetNestedSlice(obj.Object, tc.value, tc.path...)).To(Succeed())
			g.Expect(unstructured.SetNestedField(obj.Object, tc.siblingValue, tc.sibling...)).To(Succeed())

			g.Expect(c.Create(ctx, obj)).To(Succeed(),
				"admission must not reject a CR that still carries %s", tc.name)

			stored := &unstructured.Unstructured{}
			stored.SetGroupVersionKind(gvk)
			g.Expect(c.Get(ctx, client.ObjectKeyFromObject(obj), stored)).To(Succeed())

			_, found, err := unstructured.NestedSlice(stored.Object, tc.path...)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(found).To(BeFalse(), "the structural schema must prune %s", tc.name)

			sibling, sibFound, err := unstructured.NestedString(stored.Object, tc.sibling...)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(sibFound).To(BeTrue(),
				"pruning %s must not take its parent block with it", tc.name)
			g.Expect(sibling).To(Equal(tc.siblingValue))
		})
	}
}

// TestIntegration_CredentialRotation_ServiceAccountValidation pins the two CEL
// rules on the CredentialRotation CRD (keystoneService required exactly when the
// target is serviceAccountPassword) plus the field's object-name pattern against
// the real envtest API server. There is no CredentialRotation webhook, so the
// declarative validation is the only admission gate.
func TestIntegration_CredentialRotation_ServiceAccountValidation(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	cases := []struct {
		name    string
		spec    c5c3v1alpha1.CredentialRotationSpec
		wantErr bool
	}{
		{
			name:    "admin target without keystoneService is accepted",
			spec:    c5c3v1alpha1.CredentialRotationSpec{Target: c5c3v1alpha1.RotationTargetAdminApplicationCredential},
			wantErr: false,
		},
		{
			name: "admin target with keystoneService is rejected",
			spec: c5c3v1alpha1.CredentialRotationSpec{
				Target: c5c3v1alpha1.RotationTargetAdminApplicationCredential, KeystoneService: "nova",
			},
			wantErr: true,
		},
		{
			name:    "service-account target without keystoneService is rejected",
			spec:    c5c3v1alpha1.CredentialRotationSpec{Target: c5c3v1alpha1.RotationTargetServiceAccountPassword},
			wantErr: true,
		},
		{
			name: "service-account target with keystoneService is accepted",
			spec: c5c3v1alpha1.CredentialRotationSpec{
				Target: c5c3v1alpha1.RotationTargetServiceAccountPassword, KeystoneService: "nova",
			},
			wantErr: false,
		},
		{
			name: "service-account target with a non-object-name keystoneService is rejected",
			spec: c5c3v1alpha1.CredentialRotationSpec{
				Target: c5c3v1alpha1.RotationTargetServiceAccountPassword, KeystoneService: "Not_A_Name",
			},
			wantErr: true,
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cr-cel-"}}
			g.Expect(c.Create(ctx, ns)).To(Succeed())

			cr := &c5c3v1alpha1.CredentialRotation{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("rot-%d", i), Namespace: ns.Name},
				Spec:       tc.spec,
			}
			err := c.Create(ctx, cr)
			if tc.wantErr {
				g.Expect(err).To(HaveOccurred(), "admission must reject: %s", tc.name)
				g.Expect(apierrors.IsInvalid(err)).To(BeTrue(),
					fmt.Sprintf("expected Invalid for %q, got: %v", tc.name, err))
			} else {
				g.Expect(err).NotTo(HaveOccurred(), "admission must accept: %s", tc.name)
			}
		})
	}
}

// TestIntegration_KeystoneService_SchemaValidation pins the KeystoneService
// CRD's declarative schema against the real envtest API server: the
// at-least-one-block CEL rule, the identity-type rejection, the K-ORC-mirror
// patterns, the listType=map endpoint key, and the required fields.
//
// It pins the SCHEMA layer specifically, even though the validating webhook
// now mirrors most of these rules: schema validation runs between the mutating
// and the validating webhook, so every rejection below is still the schema's
// own, and its message is the one a user sees. The webhook's twin of each rule
// is covered by the unit tests in the api package.
func TestIntegration_KeystoneService_SchemaValidation(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	ref := c5c3v1alpha1.ControlPlaneRefSpec{Name: "controlplane"}
	account := &c5c3v1alpha1.KeystoneServiceAccountSpec{
		Project: c5c3v1alpha1.ServiceAccountProjectSpec{Name: "service"},
	}

	cases := []struct {
		name       string
		spec       c5c3v1alpha1.KeystoneServiceSpec
		wantErr    bool
		wantErrSub string
	}{
		{
			name:       "neither catalog nor account is rejected",
			spec:       c5c3v1alpha1.KeystoneServiceSpec{ControlPlaneRef: ref},
			wantErr:    true,
			wantErrSub: "at least one of spec.catalog or spec.account must be set",
		},
		{
			name: "identity service type is rejected",
			spec: c5c3v1alpha1.KeystoneServiceSpec{
				ControlPlaneRef: ref,
				Catalog:         &c5c3v1alpha1.KeystoneServiceCatalogSpec{ServiceType: "identity"},
			},
			wantErr:    true,
			wantErrSub: "ControlPlane-owned",
		},
		{
			name: "comma in serviceName is rejected",
			spec: c5c3v1alpha1.KeystoneServiceSpec{
				ControlPlaneRef: ref,
				Catalog: &c5c3v1alpha1.KeystoneServiceCatalogSpec{
					ServiceType: "image", ServiceName: "glance,evil",
				},
			},
			wantErr:    true,
			wantErrSub: "serviceName",
		},
		{
			name: "endpoint url without scheme is rejected",
			spec: c5c3v1alpha1.KeystoneServiceSpec{
				ControlPlaneRef: ref,
				Catalog: &c5c3v1alpha1.KeystoneServiceCatalogSpec{
					ServiceType: "image",
					Endpoints: []c5c3v1alpha1.KeystoneServiceEndpointSpec{
						{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: "keystone.example"},
					},
				},
			},
			wantErr:    true,
			wantErrSub: "url",
		},
		{
			name: "duplicate endpoint interface rows are rejected",
			spec: c5c3v1alpha1.KeystoneServiceSpec{
				ControlPlaneRef: ref,
				Catalog: &c5c3v1alpha1.KeystoneServiceCatalogSpec{
					ServiceType: "image",
					Endpoints: []c5c3v1alpha1.KeystoneServiceEndpointSpec{
						{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: "https://image.example/a"},
						{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: "https://image.example/b"},
					},
				},
			},
			wantErr:    true,
			wantErrSub: "Duplicate value",
		},
		{
			name: "account without project is rejected",
			spec: c5c3v1alpha1.KeystoneServiceSpec{
				ControlPlaneRef: ref,
				Account:         &c5c3v1alpha1.KeystoneServiceAccountSpec{UserName: "glance"},
			},
			wantErr:    true,
			wantErrSub: "project",
		},
		{
			name: "controlPlaneRef namespace outside the DNS-1123 label shape is rejected",
			spec: c5c3v1alpha1.KeystoneServiceSpec{
				ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{
					Name: "controlplane", Namespace: "Not-A-Label",
				},
				Account: account,
			},
			wantErr:    true,
			wantErrSub: "namespace",
		},
		{
			name: "catalog-only registration without endpoints is accepted",
			spec: c5c3v1alpha1.KeystoneServiceSpec{
				ControlPlaneRef: ref,
				Catalog:         &c5c3v1alpha1.KeystoneServiceCatalogSpec{ServiceType: "image"},
			},
			wantErr: false,
		},
		{
			name: "account-only registration with only a project is accepted",
			spec: c5c3v1alpha1.KeystoneServiceSpec{
				ControlPlaneRef: ref,
				Account:         account,
			},
			wantErr: false,
		},
		{
			name: "both blocks with one endpoint row per interface are accepted",
			spec: c5c3v1alpha1.KeystoneServiceSpec{
				ControlPlaneRef: ref,
				Catalog: &c5c3v1alpha1.KeystoneServiceCatalogSpec{
					ServiceType: "image", ServiceName: "glance",
					Endpoints: []c5c3v1alpha1.KeystoneServiceEndpointSpec{
						{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: "https://image.example/public"},
						{Interface: c5c3v1alpha1.ExternalEndpointTypeInternal, URL: "https://image.example/internal"},
						{Interface: c5c3v1alpha1.ExternalEndpointTypeAdmin, URL: "https://image.example/admin"},
					},
				},
				Account: account,
			},
			wantErr: false,
		},
		{
			// The catalog takeover consent decision D6 requires. It is a plain
			// optional bool, so what this pins is that the field reached the CRD
			// schema at all: without it the reconciler's adopt arm is unreachable
			// and every pre-existing catalog row is a permanent collision.
			name: "catalog adopt consent is accepted",
			spec: c5c3v1alpha1.KeystoneServiceSpec{
				ControlPlaneRef: ref,
				Catalog: &c5c3v1alpha1.KeystoneServiceCatalogSpec{
					ServiceType: "image", ServiceName: "glance", Adopt: true,
				},
			},
			wantErr: false,
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-ks-cel-"}}
			g.Expect(c.Create(ctx, ns)).To(Succeed())

			ks := &c5c3v1alpha1.KeystoneService{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("ks-%d", i), Namespace: ns.Name},
				Spec:       tc.spec,
			}
			err := c.Create(ctx, ks)
			if tc.wantErr {
				g.Expect(err).To(HaveOccurred(), "admission must reject: %s", tc.name)
				g.Expect(apierrors.IsInvalid(err)).To(BeTrue(),
					fmt.Sprintf("expected Invalid for %q, got: %v", tc.name, err))
				g.Expect(err.Error()).To(ContainSubstring(tc.wantErrSub))
			} else {
				g.Expect(err).NotTo(HaveOccurred(), "admission must accept: %s", tc.name)
			}
		})
	}
}

// TestIntegration_ControlPlane_ServiceRegistrationsSchemaValidation pins the
// service-registration allowlist against the real envtest API server: the
// per-item RFC-1123 pattern, the listType=set duplicate rejection, and the two
// shapes that must stay legal (an absent block, an empty list).
//
// It pins the SCHEMA layer specifically. The webhook's defense-in-depth twin of
// each rule is covered by the unit tests in the api package, but schema
// validation is what a webhook-bypassed caller still hits, and its message is the
// one the chainsaw rejection fixtures assert on — this is their cluster-free
// counterpart.
func TestIntegration_ControlPlane_ServiceRegistrationsSchemaValidation(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	// wantErrSubs carries every substring the chainsaw rejection fixture asserts
	// on, so a message the apiserver rewords fails here rather than in the
	// cluster-bound e2e job.
	cases := []struct {
		name        string
		allowed     *c5c3v1alpha1.ServiceRegistrationsSpec
		wantErr     bool
		wantErrSubs []string
	}{
		{
			name:        "an entry outside the RFC-1123 label shape is rejected",
			allowed:     &c5c3v1alpha1.ServiceRegistrationsSpec{AllowedNamespaces: []string{"Tenant_A"}},
			wantErr:     true,
			wantErrSubs: []string{"allowedNamespaces[0]", "should match"},
		},
		{
			name: "a duplicate entry is rejected by listType=set",
			allowed: &c5c3v1alpha1.ServiceRegistrationsSpec{
				AllowedNamespaces: []string{"tenant-a", "tenant-a"},
			},
			wantErr:     true,
			wantErrSubs: []string{"allowedNamespaces", "Duplicate value"},
		},
		{
			name:    "an absent block is accepted (the own-plus-dedicated default)",
			allowed: nil,
			wantErr: false,
		},
		{
			name:    "a declared block with no entries is accepted",
			allowed: &c5c3v1alpha1.ServiceRegistrationsSpec{},
			wantErr: false,
		},
		{
			name: "distinct label-shaped entries are accepted",
			allowed: &c5c3v1alpha1.ServiceRegistrationsSpec{
				AllowedNamespaces: []string{"tenant-a", "tenant-b"},
			},
			wantErr: false,
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			// One namespace per case: a ControlPlane is unique per namespace.
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-allowlist-"}}
			g.Expect(c.Create(ctx, ns)).To(Succeed())

			cp := integrationMinimalControlPlane(fmt.Sprintf("cp-allowlist-%d", i), ns.Name)
			cp.Spec.KORC.ServiceRegistrations = tc.allowed

			err := c.Create(ctx, cp)
			if tc.wantErr {
				g.Expect(err).To(HaveOccurred(), "admission must reject: %s", tc.name)
				g.Expect(apierrors.IsInvalid(err)).To(BeTrue(),
					fmt.Sprintf("expected Invalid for %q, got: %v", tc.name, err))
				for _, sub := range tc.wantErrSubs {
					g.Expect(err.Error()).To(ContainSubstring(sub))
				}
			} else {
				g.Expect(err).NotTo(HaveOccurred(), "admission must accept: %s", tc.name)
			}
		})
	}
}

// integrationKeystoneService returns a valid two-block KeystoneService for the
// admission tests below. metadata.name, the catalog service name and the user
// name are three DISTINCT values: every fallback the webhook resolves lands on
// metadata.name, so a fixture that shared them would hide a broken one.
func integrationKeystoneService(namespace string) *c5c3v1alpha1.KeystoneService {
	return &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: "glance-registration", Namespace: namespace},
		Spec: c5c3v1alpha1.KeystoneServiceSpec{
			ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{Name: "controlplane"},
			Catalog: &c5c3v1alpha1.KeystoneServiceCatalogSpec{
				ServiceType: "image",
				ServiceName: "glance",
				Endpoints: []c5c3v1alpha1.KeystoneServiceEndpointSpec{
					{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: "https://image.example.com"},
				},
			},
			Account: &c5c3v1alpha1.KeystoneServiceAccountSpec{
				UserName: "glance-user",
				Project:  c5c3v1alpha1.ServiceAccountProjectSpec{Name: "service"},
				Roles:    []string{"admin"},
			},
		},
	}
}

// TestIntegration_KeystoneService_Defaulting pins the mutating webhook against
// the real envtest API server. The reconciler resolves the effective user name
// defensively (keystoneServiceUserName), so a broken defaulter would not surface
// as a reconcile failure — only the STORED object shows it, which is what this
// test reads back.
func TestIntegration_KeystoneService_Defaulting(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	g := NewGomegaWithT(t)
	c, ctx, _ := setupControlPlaneEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-ks-default-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	// An account block without a user name takes the CR's own name.
	implicit := integrationKeystoneService(ns.Name)
	implicit.Spec.Account.UserName = ""
	g.Expect(c.Create(ctx, implicit)).To(Succeed())

	stored := &c5c3v1alpha1.KeystoneService{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(implicit), stored)).To(Succeed())
	g.Expect(stored.Spec.Account.UserName).To(Equal("glance-registration"),
		"the defaulting webhook must materialize userName from metadata.name")

	// An explicit user name survives admission untouched.
	explicit := integrationKeystoneService(ns.Name)
	explicit.Name = "explicit-registration"
	g.Expect(c.Create(ctx, explicit)).To(Succeed())

	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(explicit), stored)).To(Succeed())
	g.Expect(stored.Spec.Account.UserName).To(Equal("glance-user"))

	// A catalog-only CR gains no account block.
	catalogOnly := integrationKeystoneService(ns.Name)
	catalogOnly.Name = "catalog-only-registration"
	catalogOnly.Spec.Account = nil
	g.Expect(c.Create(ctx, catalogOnly)).To(Succeed())

	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(catalogOnly), stored)).To(Succeed())
	g.Expect(stored.Spec.Account).To(BeNil())
}

// TestIntegration_KeystoneService_Immutability drives the identity freezes over
// the real API server, where BOTH admission layers are live: the CRD's CEL
// transition rules and the validating webhook. Two cases are reachable only
// through the webhook and are marked as such — a CEL transition rule does not
// evaluate when the field is absent on one side of the update, and none can
// read metadata.namespace to resolve the effective ControlPlane namespace.
func TestIntegration_KeystoneService_Immutability(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	cases := []struct {
		name    string
		mutate  func(*c5c3v1alpha1.KeystoneService)
		wantErr bool
		wantSub string
	}{
		{
			name:    "controlPlaneRef name is frozen",
			mutate:  func(ks *c5c3v1alpha1.KeystoneService) { ks.Spec.ControlPlaneRef.Name = "other-plane" },
			wantErr: true, wantSub: "controlPlaneRef.name is immutable",
		},
		{
			// Webhook-only: resolving the effective namespace needs
			// metadata.namespace, which a CEL rule on a spec field cannot read.
			name:    "controlPlaneRef namespace is frozen",
			mutate:  func(ks *c5c3v1alpha1.KeystoneService) { ks.Spec.ControlPlaneRef.Namespace = "other-namespace" },
			wantErr: true, wantSub: "controlPlaneRef.namespace is immutable",
		},
		{
			name:    "serviceType is frozen",
			mutate:  func(ks *c5c3v1alpha1.KeystoneService) { ks.Spec.Catalog.ServiceType = "volume" },
			wantErr: true, wantSub: "serviceType is immutable",
		},
		{
			name:    "serviceName is frozen",
			mutate:  func(ks *c5c3v1alpha1.KeystoneService) { ks.Spec.Catalog.ServiceName = "glance-renamed" },
			wantErr: true, wantSub: "serviceName is immutable",
		},
		{
			// Webhook-only: the new object omits the field, so the CEL transition
			// rule never evaluates — yet dropping it renames the catalog row to
			// metadata.name.
			name:    "serviceName cleared to the fallback is frozen",
			mutate:  func(ks *c5c3v1alpha1.KeystoneService) { ks.Spec.Catalog.ServiceName = "" },
			wantErr: true, wantSub: "serviceName is immutable",
		},
		{
			name:    "userName is frozen",
			mutate:  func(ks *c5c3v1alpha1.KeystoneService) { ks.Spec.Account.UserName = "glance-renamed" },
			wantErr: true, wantSub: "userName is immutable",
		},
		{
			name:    "domainName is frozen",
			mutate:  func(ks *c5c3v1alpha1.KeystoneService) { ks.Spec.Account.DomainName = "corp" },
			wantErr: true, wantSub: "domainName is immutable",
		},
		{
			name:    "project name is frozen",
			mutate:  func(ks *c5c3v1alpha1.KeystoneService) { ks.Spec.Account.Project.Name = "other-project" },
			wantErr: true, wantSub: "project.name is immutable",
		},
		{
			name:    "project create is frozen",
			mutate:  func(ks *c5c3v1alpha1.KeystoneService) { ks.Spec.Account.Project.Create = true },
			wantErr: true, wantSub: "project.create is immutable",
		},
		{
			name:   "adopt consent stays mutable",
			mutate: func(ks *c5c3v1alpha1.KeystoneService) { ks.Spec.Catalog.Adopt, ks.Spec.Account.Adopt = true, true },
		},
		{
			name: "endpoint rows stay mutable",
			mutate: func(ks *c5c3v1alpha1.KeystoneService) {
				ks.Spec.Catalog.Endpoints = []c5c3v1alpha1.KeystoneServiceEndpointSpec{
					{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: "https://image.example.com/v2"},
					{Interface: c5c3v1alpha1.ExternalEndpointTypeInternal, URL: "https://image.internal"},
				}
			},
		},
		{
			name:   "roles stay mutable",
			mutate: func(ks *c5c3v1alpha1.KeystoneService) { ks.Spec.Account.Roles = []string{"admin", "reader"} },
		},
		{
			name:   "a whole block may be removed",
			mutate: func(ks *c5c3v1alpha1.KeystoneService) { ks.Spec.Account = nil },
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-ks-immutable-"}}
			g.Expect(c.Create(ctx, ns)).To(Succeed())

			ks := integrationKeystoneService(ns.Name)
			ks.Name = fmt.Sprintf("ks-immutable-%d", i)
			g.Expect(c.Create(ctx, ks)).To(Succeed())

			stored := &c5c3v1alpha1.KeystoneService{}
			g.Expect(c.Get(ctx, client.ObjectKeyFromObject(ks), stored)).To(Succeed())
			tc.mutate(stored)

			err := c.Update(ctx, stored)
			if !tc.wantErr {
				g.Expect(err).NotTo(HaveOccurred(), "admission must accept: %s", tc.name)
				return
			}
			g.Expect(err).To(HaveOccurred(), "admission must reject: %s", tc.name)
			g.Expect(apierrors.IsInvalid(err)).To(BeTrue(),
				fmt.Sprintf("expected Invalid for %q, got: %v", tc.name, err))
			g.Expect(err.Error()).To(ContainSubstring(tc.wantSub))
		})
	}
}

// integrationExternalControlPlane returns the issue's minimal External-mode
// sketch CR (mode: External + external.authURL + korc.adminCredential.
// passwordSecretRef, no infrastructure block) for the envtest matrix.
func integrationExternalControlPlane(name, namespace string) *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
					Mode: c5c3v1alpha1.KeystoneModeExternal,
					External: &c5c3v1alpha1.ExternalKeystoneSpec{
						AuthURL: "https://keystone.example.com/v3",
					},
				},
			},
			KORC: c5c3v1alpha1.KORCSpec{
				AdminCredential: c5c3v1alpha1.AdminCredentialSpec{
					CloudCredentialsRef: c5c3v1alpha1.CloudCredentialsRef{CloudName: "admin"},
					PasswordSecretRef:   commonv1.SecretRefSpec{Name: "external-admin", Key: "password"},
				},
			},
		},
	}
}

// TestIntegration_ExternalMode_AcceptedAndDefaulted drives the External-mode API
// surface against the real envtest API server (CRD schema + CEL + defaulting and
// validating webhooks). It proves the sketch CR is admitted and stored with the
// External-mode defaults materialized and NO infrastructure block invented.
func TestIntegration_ExternalMode_AcceptedAndDefaulted(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)
	g := NewGomegaWithT(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-external-ok-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	cp := integrationExternalControlPlane("cp-external", ns.Name)
	g.Expect(c.Create(ctx, cp)).To(Succeed(), "the minimal External-mode sketch CR must be admitted")

	fetched := &c5c3v1alpha1.ControlPlane{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(cp), fetched)).To(Succeed())

	g.Expect(fetched.Spec.Infrastructure).To(BeNil(),
		"External mode must persist with no infrastructure block")
	g.Expect(fetched.Spec.Services.Keystone.Mode).To(Equal(c5c3v1alpha1.KeystoneModeExternal))
	g.Expect(fetched.Spec.Services.Keystone.External).NotTo(BeNil())
	g.Expect(fetched.Spec.Services.Keystone.External.EndpointType).
		To(Equal(c5c3v1alpha1.ExternalEndpointTypePublic), "endpointType must default to public")
	// admin identity defaults materialize in External mode too.
	g.Expect(fetched.Spec.KORC.AdminCredential.UserName).To(Equal("admin"))
	g.Expect(fetched.Spec.KORC.AdminCredential.ProjectName).To(Equal("admin"))
	g.Expect(fetched.Spec.KORC.AdminCredential.DomainName).To(Equal("Default"))
}

// TestIntegration_ExternalMode_Rejections exercises the External-mode rejection
// matrix at the real admission chain. The CEL cases prove the schema layer holds
// even if the validating webhook were bypassed; the cross-field cases exercise
// the webhook-only rules.
func TestIntegration_ExternalMode_Rejections(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	cases := []struct {
		name   string
		mutate func(*c5c3v1alpha1.ControlPlane)
	}{
		{
			name: "CEL: managed-only replicas set in External mode",
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Keystone.Replicas = ptr.To(int32(3))
			},
		},
		{
			name: "CEL: external block set in Managed mode",
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Keystone.Mode = c5c3v1alpha1.KeystoneModeManaged
				// Managed mode requires infrastructure; supply it so the ONLY
				// violation under test is external-in-Managed.
				cp.Spec.Infrastructure = &c5c3v1alpha1.InfrastructureSpec{
					Database: commonv1.DatabaseSpec{Host: "db", Database: "d", SecretRef: commonv1.SecretRefSpec{Name: "s"}},
					Cache:    commonv1.CacheSpec{Backend: "b", Servers: []string{"mc:11211"}},
				}
			},
		},
		{
			name: "schema: endpointType outside the enum",
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Keystone.External.EndpointType = "gopher"
			},
		},
		{
			name: "schema: authURL not an http(s) URL",
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Keystone.External.AuthURL = "keystone.example.com"
			},
		},
		{
			// The coarse ^https?:// prefix accepted a scheme-only, hostless URL;
			// the tightened ^https?://[^\s/]+ pattern (and net/url webhook) reject
			// it so the identity consumer never dials a hostless endpoint.
			name: "schema: authURL has no host",
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Keystone.External.AuthURL = "https://"
			},
		},
		{
			name: "webhook: infrastructure set in External mode",
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Infrastructure = &c5c3v1alpha1.InfrastructureSpec{
					Database: commonv1.DatabaseSpec{Host: "db", Database: "d", SecretRef: commonv1.SecretRefSpec{Name: "s"}},
					Cache:    commonv1.CacheSpec{Backend: "b", Servers: []string{"mc:11211"}},
				}
			},
		},
		{
			name: "webhook: external block missing in External mode",
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Keystone.External = nil
			},
		},
		{
			// services.glance has no External-mode design yet (identity is managed
			// against a pre-existing Keystone, so there is no control plane to attach
			// an image service to). validateKeystoneMode forbids it with the message
			// "forbidden when services.keystone.mode is External (Glance needs its own
			// External-mode design)", which the apiserver returns as an Invalid error.
			name: "webhook: services.glance set in External mode",
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Glance = integrationGlanceService()
			},
		},
		{
			// services.placement is forbidden on the same terms as services.glance:
			// validateKeystoneMode rejects it with "forbidden when
			// services.keystone.mode is External (Placement needs its own
			// External-mode design)", which the apiserver returns as an Invalid error.
			name: "webhook: services.placement set in External mode",
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Placement = integrationPlacementService()
			},
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-external-"}}
			g.Expect(c.Create(ctx, ns)).To(Succeed())

			cp := integrationExternalControlPlane(fmt.Sprintf("cp-external-%d", i), ns.Name)
			tc.mutate(cp)

			err := c.Create(ctx, cp)
			g.Expect(err).To(HaveOccurred(), "admission must reject: %s", tc.name)
			g.Expect(apierrors.IsInvalid(err)).To(BeTrue(),
				fmt.Sprintf("expected Invalid status error for %q, got: %v", tc.name, err))
		})
	}
}

// TestIntegration_ExternalMode_TransitionsRejected verifies the keystone-mode
// transition gating on real UPDATEs against the envtest API server: a live
// managed ControlPlane cannot flip to External, and a live External ControlPlane
// cannot flip to Managed.
func TestIntegration_ExternalMode_TransitionsRejected(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	t.Run("managed -> External rejected", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-m2e-"}}
		g.Expect(c.Create(ctx, ns)).To(Succeed())

		cp := integrationManagedControlPlane("cp-m2e", ns.Name)
		g.Expect(c.Create(ctx, cp)).To(Succeed())

		// Get-mutate-update under RetryOnConflict: the live controller updates
		// the freshly created CR concurrently (finalizer, status), and a stale
		// resourceVersion returns 409 Conflict before admission ever sees the
		// transition. The webhook denial itself is deterministic once the
		// update reaches it, so only the Conflict is retried.
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			fetched := &c5c3v1alpha1.ControlPlane{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(cp), fetched); err != nil {
				return err
			}
			fetched.Spec.Services.Keystone.Mode = c5c3v1alpha1.KeystoneModeExternal
			fetched.Spec.Services.Keystone.Replicas = nil
			fetched.Spec.Services.Keystone.External = &c5c3v1alpha1.ExternalKeystoneSpec{
				AuthURL: "https://keystone.example.com/v3",
			}
			fetched.Spec.Infrastructure = nil
			return c.Update(ctx, fetched)
		})
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("cannot be changed to External"))
	})

	t.Run("External -> Managed rejected", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-e2m-"}}
		g.Expect(c.Create(ctx, ns)).To(Succeed())

		cp := integrationExternalControlPlane("cp-e2m", ns.Name)
		g.Expect(c.Create(ctx, cp)).To(Succeed())

		// Same RetryOnConflict rationale as the managed -> External leg above.
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			fetched := &c5c3v1alpha1.ControlPlane{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(cp), fetched); err != nil {
				return err
			}
			fetched.Spec.Services.Keystone.Mode = c5c3v1alpha1.KeystoneModeManaged
			fetched.Spec.Services.Keystone.External = nil
			fetched.Spec.Infrastructure = &c5c3v1alpha1.InfrastructureSpec{
				Database: commonv1.DatabaseSpec{Host: "db", Database: "d", SecretRef: commonv1.SecretRefSpec{Name: "s"}},
				Cache:    commonv1.CacheSpec{Backend: "b", Servers: []string{"mc:11211"}},
			}
			return c.Update(ctx, fetched)
		})
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("phase-3"))
	})
}

// driveExternalControlPlaneToReady creates the user-supplied admin-password
// Secret an External-mode ControlPlane reads, then simulates every external
// dependency the K-ORC chain waits on. The four skipped sub-reconcilers need no
// simulation at all — that is the point of External mode.
func driveExternalControlPlaneToReady(
	t testing.TB, ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane,
) {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := cp.Namespace
	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns}

	// External mode still routes its admin app-credential / service-account pushes
	// through the per-tenant SecretStore the operator provisions, so pre-seed that
	// store Ready (envtest has no ESO controller to flip its Ready condition). See
	// the note in driveControlPlaneToAdminCredentialReady.
	ensureReadySecretStore(t, ctx, c, esoTenantStoreName, ns)

	// In External mode effectiveAdminPasswordSecretRef resolves to the USER's
	// Secret, so the cleartext readAdminPassword reads lives under that name.
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cp.Spec.KORC.AdminCredential.PasswordSecretRef.Name,
			Namespace: ns,
		},
		Data: map[string][]byte{"password": []byte("super-secret-admin-password")},
	}
	g.Expect(c.Create(ctx, adminSecret)).To(Succeed(), "create the user-supplied admin password Secret")

	g.Expect(c.Create(ctx, cp)).To(Succeed(), "create the External-mode ControlPlane CR")

	// The four skipped sub-reconcilers converge with no simulation whatsoever.
	for _, condType := range []string{
		conditionTypeInfrastructureReady,
		conditionTypeDBCredentialsReady,
		conditionTypeAdminPasswordReady,
		conditionTypeKeystoneReady,
	} {
		waitForControlPlaneCondition(t, ctx, c, cpKey, condType, metav1.ConditionTrue, itEventuallyTimeout)
	}
	// services.horizon is forbidden in External mode, so Horizon reports not-managed.
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeHorizonReady, metav1.ConditionTrue, itEventuallyTimeout)

	// K-ORC chain: identical to managed mode, driven against the external Keystone.
	simulateApplicationCredentialAvailableWhenPresent(t, ctx, c,
		client.ObjectKey{Name: adminAppCredentialName(cp), Namespace: ns})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeKORCReady, metav1.ConditionTrue, itEventuallyTimeout)

	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns, Name: korcCloudsYamlSecretName}, &esov1.ExternalSecret{})
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "operator must create the k-orc clouds.yaml ExternalSecret")
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Namespace: ns, Name: korcCloudsYamlSecretName})).To(Succeed())

	simulatePushSecretSyncedWhenPresent(t, ctx, c,
		client.ObjectKey{Name: adminAppCredentialPushSecretName(cp), Namespace: ns})
	simulateCloudsYamlMaterializedWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminCredentialReady, metav1.ConditionTrue, itEventuallyTimeout)

	// The catalog is IMPORTED, not registered: resolve the unmanaged imports.
	simulateExternalCatalogImportsAvailableWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeCatalogReady, metav1.ConditionTrue, itEventuallyTimeout)
}

// TestIntegration_ExternalMode_ConvergesToReadyWithNoWorkloads is the headline
// acceptance criterion: an External-mode ControlPlane whose external dependencies
// are reachable converges Ready=True while creating ZERO MariaDB, Memcached,
// Keystone or Horizon resources — and no operator-owned credential ExternalSecrets.
func TestIntegration_ExternalMode_ConvergesToReadyWithNoWorkloads(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)
	ensureReadyClusterSecretStore(t, ctx, c)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-external-ready-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")

	cp := integrationExternalControlPlane("cp-external", ns.Name)
	driveExternalControlPlaneToReady(t, ctx, c, cp)

	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeReady, metav1.ConditionTrue, itEventuallyTimeout)

	final := &c5c3v1alpha1.ControlPlane{}
	g.Expect(c.Get(ctx, cpKey, final)).To(Succeed())

	readyCond := meta.FindStatusCondition(final.Status.Conditions, conditionTypeReady)
	g.Expect(readyCond.Reason).To(Equal("AllReady"))
	g.Expect(final.Status.ObservedGeneration).To(Equal(final.Generation))

	// Each skipped sub-reconciler reports the dedicated ExternallyManaged reason.
	for _, condType := range []string{
		conditionTypeInfrastructureReady,
		conditionTypeDBCredentialsReady,
		conditionTypeAdminPasswordReady,
		conditionTypeKeystoneReady,
	} {
		cond := meta.FindStatusCondition(final.Status.Conditions, condType)
		g.Expect(cond).NotTo(BeNil(), "condition %s should exist", condType)
		g.Expect(cond.Reason).To(Equal(conditionReasonExternallyManaged),
			"condition %s must report the External-mode skip reason", condType)
		g.Expect(cond.Message).To(ContainSubstring("https://keystone.example.com/v3"),
			"condition %s must name the external endpoint", condType)
	}
	horizonCond := meta.FindStatusCondition(final.Status.Conditions, conditionTypeHorizonReady)
	g.Expect(horizonCond.Reason).To(Equal("HorizonNotManaged"))

	// ZERO workloads. This is the acceptance criterion, so assert absence directly.
	var mariadbList mariadbv1alpha1.MariaDBList
	g.Expect(c.List(ctx, &mariadbList, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(mariadbList.Items).To(BeEmpty(), "External mode must create no MariaDB")

	memcachedList := &unstructured.UnstructuredList{}
	memcachedList.SetGroupVersionKind(memcachedGVK)
	g.Expect(c.List(ctx, memcachedList, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(memcachedList.Items).To(BeEmpty(), "External mode must create no Memcached")

	var keystoneList keystonev1alpha1.KeystoneList
	g.Expect(c.List(ctx, &keystoneList, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(keystoneList.Items).To(BeEmpty(), "External mode must create no Keystone child")

	var horizonList horizonv1alpha1.HorizonList
	g.Expect(c.List(ctx, &horizonList, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(horizonList.Items).To(BeEmpty(), "External mode must create no Horizon child")

	// No operator-owned credential projections either.
	g.Expect(apierrors.IsNotFound(c.Get(ctx,
		client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)}, &esov1.ExternalSecret{}))).
		To(BeTrue(), "External mode must project no DB-credential ExternalSecret")
	g.Expect(apierrors.IsNotFound(c.Get(ctx,
		client.ObjectKey{Namespace: ns.Name, Name: adminPasswordSecretName(cp)}, &esov1.ExternalSecret{}))).
		To(BeTrue(), "External mode must project no admin-password ExternalSecret")

	// status.services reports the single configured service.
	g.Expect(final.Status.Services).To(HaveLen(1))
	g.Expect(final.Status.Services[0].Name).To(Equal(keystoneServiceKey))
	g.Expect(final.Status.Services[0].Ready).To(BeTrue())

	// ZERO catalog entries. Pointed at a populated catalog, the ControlPlane must
	// have created nothing: every K-ORC Service/Endpoint in the namespace is an
	// unmanaged import. This is the import-first acceptance criterion.
	catalogCond := meta.FindStatusCondition(final.Status.Conditions, conditionTypeCatalogReady)
	g.Expect(catalogCond.Reason).To(Equal(conditionReasonCatalogImported))

	var korcServices orcv1alpha1.ServiceList
	g.Expect(c.List(ctx, &korcServices, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(korcServices.Items).To(HaveLen(1))
	g.Expect(korcServices.Items[0].Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged),
		"External mode must create no managed catalog Service")

	var korcEndpoints orcv1alpha1.EndpointList
	g.Expect(c.List(ctx, &korcEndpoints, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(korcEndpoints.Items).To(HaveLen(len(externalCatalogInterfaces)))
	for _, ep := range korcEndpoints.Items {
		g.Expect(ep.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged),
			"External mode must create no managed catalog Endpoint")
	}

	// ... and the existing identity service/endpoints appear as resolved imports.
	// CatalogReady gates only on the REQUIRED imports — the identity Service and the
	// single endpointType interface (public here) — so the two non-gating interface
	// imports resolve on a later reconcile driven by their K-ORC status watch, which
	// can land after Ready flips. Poll for that convergence rather than asserting on
	// the single snapshot taken the instant Ready went True.
	g.Eventually(func() error {
		live := &c5c3v1alpha1.ControlPlane{}
		if err := c.Get(ctx, cpKey, live); err != nil {
			return err
		}
		if live.Status.Catalog == nil {
			return fmt.Errorf("status.catalog not projected yet")
		}
		if got, want := len(live.Status.Catalog.Imports), 1+len(externalCatalogInterfaces); got != want {
			return fmt.Errorf("status.catalog.imports has %d entries, want %d", got, want)
		}
		for _, imp := range live.Status.Catalog.Imports {
			if !imp.Resolved {
				return fmt.Errorf("import %s not reported resolved yet", imp.Name)
			}
			if imp.ID == "" {
				return fmt.Errorf("import %s reports no resolved OpenStack id", imp.Name)
			}
		}
		return nil
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"every catalog import — the identity Service and all %d interfaces — must resolve with an id",
		len(externalCatalogInterfaces))
}

// TestIntegration_ExternalMode_DeletionLeavesUnmanagedImportsAlone is the AC-4
// end-to-end guard: deleting a converged External-mode ControlPlane tears down the
// operator-owned K-ORC CRs and provably nothing else — a foreign K-ORC User in the
// same namespace survives.
func TestIntegration_ExternalMode_DeletionLeavesUnmanagedImportsAlone(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)
	ensureReadyClusterSecretStore(t, ctx, c)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-external-delete-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")

	// A K-ORC User this ControlPlane never created. It shares the namespace and the
	// kind of the operator's own admin import, so only NAME scoping keeps it safe.
	foreign := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "foreign-user", Namespace: ns.Name},
		Spec: orcv1alpha1.UserSpec{
			ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged,
			Import:           &orcv1alpha1.UserImport{Filter: &orcv1alpha1.UserFilter{}},
		},
	}
	g.Expect(c.Create(ctx, foreign)).To(Succeed(), "create an unrelated K-ORC User import")

	cp := integrationExternalControlPlane("cp-external", ns.Name)
	driveExternalControlPlaneToReady(t, ctx, c, cp)

	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeReady, metav1.ConditionTrue, itEventuallyTimeout)

	g.Expect(c.Delete(ctx, cp)).To(Succeed(), "delete the External-mode ControlPlane")

	// Every owned K-ORC CR disappears and the finalizer releases the ControlPlane.
	// envtest runs no garbage collector, so this is the reconcileDelete sweep, not GC.
	g.Eventually(func() bool {
		for _, child := range orcChildObjects(cp) {
			obj := child.newObj()
			key := client.ObjectKey{Name: child.name, Namespace: ns.Name}
			if err := c.Get(ctx, key, obj); !apierrors.IsNotFound(err) {
				return false
			}
		}
		return apierrors.IsNotFound(c.Get(ctx, cpKey, &c5c3v1alpha1.ControlPlane{}))
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
		"the owned K-ORC CRs — including the per-interface identity catalog imports — and the ControlPlane must be gone")

	// ... and the foreign import is untouched.
	g.Expect(c.Get(ctx, client.ObjectKey{Name: "foreign-user", Namespace: ns.Name}, &orcv1alpha1.User{})).
		To(Succeed(), "a K-ORC CR the ControlPlane does not own must survive its deletion")
}

// --- External keystone mode: the generated credentials and their lifecycle ---

// itExternalCABundle is the private-CA bundle the External-mode envtest scenarios
// reference. It is projected verbatim, so the assertions compare bytes.
const itExternalCABundle = "-----BEGIN CERTIFICATE-----\nZW52dGVzdC1jYQ==\n-----END CERTIFICATE-----\n"

// createExternalCABundleSecret provisions the user-supplied CA bundle Secret and
// points the ControlPlane's external block at it. It must run BEFORE the
// ControlPlane is created: reconcileKORC defers with WaitingForCABundle while the
// Secret is absent.
func createExternalCABundleSecret(t testing.TB, ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane) {
	t.Helper()
	g := NewGomegaWithT(t)

	g.Expect(c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "external-keystone-ca", Namespace: cp.Namespace},
		Data:       map[string][]byte{"ca.crt": []byte(itExternalCABundle)},
	})).To(Succeed(), "create the user-supplied CA bundle Secret")

	cp.Spec.Services.Keystone.External.CABundleSecretRef = &commonv1.SecretRefSpec{
		Name: "external-keystone-ca", Key: "ca.crt",
	}
}

// TestIntegration_ExternalMode_GeneratedCloudsYAMLTargetsExternalKeystone is the
// headline acceptance criterion of the clouds.yaml work: once an External-mode
// ControlPlane converges, BOTH generated Secrets — the bootstrap password cloud and
// the minted admin app-cred cloud — carry the external authURL, the configured
// endpoint_type, and the CA bundle.
func TestIntegration_ExternalMode_GeneratedCloudsYAMLTargetsExternalKeystone(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)
	ensureReadyClusterSecretStore(t, ctx, c)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-external-clouds-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")

	cp := integrationExternalControlPlane("cp-external", ns.Name)
	cp.Spec.Region = "eu-de-1"
	cp.Spec.Services.Keystone.External.EndpointType = c5c3v1alpha1.ExternalEndpointTypeInternal
	createExternalCABundleSecret(t, ctx, c, cp)

	driveExternalControlPlaneToReady(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c,
		types.NamespacedName{Name: cp.Name, Namespace: ns.Name}, conditionTypeReady, metav1.ConditionTrue, itEventuallyTimeout)

	// The minted app-credential cloud: the document K-ORC authenticates with once
	// the credential exists, and the PushSecret's source for the OpenBao round-trip.
	appCred := &corev1.Secret{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: adminAppCredentialSecretName(cp)}, appCred)).To(Succeed())
	appCredDoc := string(appCred.Data[appCredCloudsYAMLKey])
	g.Expect(appCredDoc).To(ContainSubstring(`auth_url: "https://keystone.example.com/v3"`))
	g.Expect(appCredDoc).To(ContainSubstring("endpoint_type: internal"))
	g.Expect(appCredDoc).To(ContainSubstring(`region_name: "eu-de-1"`))
	g.Expect(appCredDoc).To(ContainSubstring("application_credential_id"))
	g.Expect(appCredDoc).NotTo(ContainSubstring(".svc:5000"), "External mode must never dial the Service DNS")
	g.Expect(string(appCred.Data[korcCACertKey])).To(Equal(itExternalCABundle))

	// The bootstrap password cloud: the document the ApplicationCredential mints and
	// revokes with.
	pwCloud := &corev1.Secret{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: adminPasswordCloudSecretName(cp)}, pwCloud)).To(Succeed())
	pwDoc := string(pwCloud.Data[appCredCloudsYAMLKey])
	g.Expect(pwDoc).To(ContainSubstring(`auth_url: "https://keystone.example.com/v3"`))
	g.Expect(pwDoc).To(ContainSubstring("endpoint_type: internal"))
	g.Expect(pwDoc).To(ContainSubstring(`region_name: "eu-de-1"`))
	g.Expect(pwDoc).NotTo(ContainSubstring(".svc:5000"))
	g.Expect(string(pwCloud.Data[korcCACertKey])).To(Equal(itExternalCABundle))

	// The clouds.yaml ExternalSecret reads the CA back from the same OpenBao path,
	// so the materialized credentials Secret K-ORC actually reads carries the trust
	// anchor next to clouds.yaml.
	es := &esov1.ExternalSecret{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: korcCloudsYamlSecretName}, es)).To(Succeed())
	g.Expect(es.Spec.Data).To(HaveLen(2))
	g.Expect(es.Spec.Data[1].SecretKey).To(Equal(korcCACertKey))
	g.Expect(es.Spec.Data[1].RemoteRef.Property).To(Equal(korcCACertKey))
}

// TestIntegration_ExternalMode_PasswordRotationReMintsAgainstExternalKeystone is
// the credential-lifecycle acceptance criterion: updating the USER-supplied
// admin-password Secret out-of-band re-mints the application credential (delete +
// recreate, because K-ORC's actuator has no in-place re-mint), and the re-assembled
// clouds.yaml — still carrying the external auth_url — lands in the PushSecret
// source and the materialized Secret.
func TestIntegration_ExternalMode_PasswordRotationReMintsAgainstExternalKeystone(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)
	ensureReadyClusterSecretStore(t, ctx, c)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-external-remint-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")

	cp := integrationExternalControlPlane("cp-external", ns.Name)
	driveExternalControlPlaneToReady(t, ctx, c, cp)

	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}
	acKey := client.ObjectKey{Name: adminAppCredentialName(cp), Namespace: ns.Name}
	appCredKey := client.ObjectKey{Name: adminAppCredentialSecretName(cp), Namespace: ns.Name}

	before := &orcv1alpha1.ApplicationCredential{}
	g.Expect(c.Get(ctx, acKey, before)).To(Succeed())
	beforeSecret := &corev1.Secret{}
	g.Expect(c.Get(ctx, appCredKey, beforeSecret)).To(Succeed())
	beforeValue := string(beforeSecret.Data[appCredSecretValueKey])
	g.Expect(beforeValue).NotTo(BeEmpty())

	// Rotate the admin password OUT-OF-BAND — the only supported rotation path for
	// an external Keystone, since the operator never writes to the installation.
	userSecret := &corev1.Secret{}
	g.Expect(c.Get(ctx, client.ObjectKey{
		Name: cp.Spec.KORC.AdminCredential.PasswordSecretRef.Name, Namespace: ns.Name,
	}, userSecret)).To(Succeed())
	userSecret.Data["password"] = []byte("rotated-admin-password")
	g.Expect(c.Update(ctx, userSecret)).To(Succeed(), "rotate the user-supplied admin password")

	// The hash mismatch drives a delete + recreate, so the AC comes back with a new
	// UID; the secret "value" is regenerated so the recreated AC mints afresh.
	g.Eventually(func() bool {
		after := &orcv1alpha1.ApplicationCredential{}
		if err := c.Get(ctx, acKey, after); err != nil {
			return false
		}
		return after.UID != before.UID
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
		"rotating the admin password must delete and recreate the ApplicationCredential")

	g.Eventually(func() string {
		after := &corev1.Secret{}
		if err := c.Get(ctx, appCredKey, after); err != nil {
			return beforeValue
		}
		return string(after.Data[appCredSecretValueKey])
	}, itEventuallyTimeout, itPollInterval).ShouldNot(Equal(beforeValue),
		"the re-mint must regenerate the application-credential secret value")

	// The password-cloud the re-mint re-authenticates with tracks the rotated
	// password immediately — otherwise K-ORC could not revoke the old credential.
	g.Eventually(func() string {
		pwCloud := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Name: adminPasswordCloudSecretName(cp), Namespace: ns.Name}, pwCloud); err != nil {
			return ""
		}
		return string(pwCloud.Data[appCredCloudsYAMLKey])
	}, itEventuallyTimeout, itPollInterval).Should(ContainSubstring("rotated-admin-password"))

	// Simulate K-ORC minting the replacement credential, then pump the ESO
	// simulators until the chain re-converges: the re-assembled clouds.yaml has to
	// reach the PushSecret source AND the materialized Secret before
	// AdminCredentialReady may report True again (the stale-credential gate).
	reminted := &orcv1alpha1.ApplicationCredential{}
	g.Eventually(func() error { return c.Get(ctx, acKey, reminted) }, itEventuallyTimeout, itPollInterval).Should(Succeed())
	reminted.Status.ID = ptr.To("ac-id-integration-remint")
	meta.SetStatusCondition(&reminted.Status.Conditions, metav1.Condition{
		Type:    orcv1alpha1.ConditionAvailable,
		Status:  metav1.ConditionTrue,
		Reason:  orcv1alpha1.ConditionReasonSuccess,
		Message: "simulated available after re-mint",
	})
	g.Expect(c.Status().Update(ctx, reminted)).To(Succeed())

	g.Eventually(func() error {
		return pumpMaterializedCloudsYAML(ctx, c, cp)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"the re-assembled app-credential clouds.yaml must reach the materialized Secret")

	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminCredentialReady, metav1.ConditionTrue, itEventuallyTimeout)

	// The re-assembled document carries the FRESH credential and still targets the
	// external Keystone. The materialized Secret — what K-ORC reads — agrees.
	afterSecret := &corev1.Secret{}
	g.Expect(c.Get(ctx, appCredKey, afterSecret)).To(Succeed())
	afterDoc := string(afterSecret.Data[appCredCloudsYAMLKey])
	g.Expect(afterDoc).To(ContainSubstring("ac-id-integration-remint"))
	g.Expect(afterDoc).To(ContainSubstring(`auth_url: "https://keystone.example.com/v3"`))
	g.Expect(afterDoc).To(ContainSubstring("endpoint_type: public"))

	materialized := &corev1.Secret{}
	g.Expect(c.Get(ctx, client.ObjectKey{Name: korcCloudsYamlSecretName, Namespace: ns.Name}, materialized)).To(Succeed())
	g.Expect(string(materialized.Data[appCredCloudsYAMLKey])).To(Equal(afterDoc),
		"the credential K-ORC reads must be the freshly minted one, not the revoked predecessor")

	// The PushSecret carries a new content hash, so ESO re-pushes the rotated
	// credential to OpenBao instead of waiting for the hourly refresh.
	ps := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(ctx, client.ObjectKey{Name: adminAppCredentialPushSecretName(cp), Namespace: ns.Name}, ps)).To(Succeed())
	g.Expect(ps.Annotations[adminAppCredentialPushContentHashAnnotation]).NotTo(BeEmpty())
}

// pumpMaterializedCloudsYAML copies the operator-assembled app-credential
// clouds.yaml into the ESO-materialized Secret, but ONLY once the source actually
// holds a minted credential document. There is no ESO controller in envtest, so a
// re-mint would otherwise leave the materialized Secret pinned at the previous
// credential and the stale-credential gate would (correctly) never open. It returns
// an error while the source is not yet re-assembled, so callers can poll it.
func pumpMaterializedCloudsYAML(ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane) error {
	src := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: childNamespace(cp), Name: adminAppCredentialSecretName(cp)}, src); err != nil {
		return err
	}
	assembled := src.Data[appCredCloudsYAMLKey]
	if !strings.Contains(string(assembled), "application_credential_id") {
		return fmt.Errorf("app-credential clouds.yaml not re-assembled yet")
	}

	materialized := &corev1.Secret{}
	key := client.ObjectKey{Namespace: childNamespace(cp), Name: korcCloudsYamlSecretName}
	if err := c.Get(ctx, key, materialized); err != nil {
		return err
	}
	if string(materialized.Data[appCredCloudsYAMLKey]) == string(assembled) {
		return nil
	}
	materialized.Data[appCredCloudsYAMLKey] = assembled
	return c.Update(ctx, materialized)
}

// TestIntegration_FederationBackendWakesReconcileAndProjectsWebSSO drives a
// managed ControlPlane to a projected Horizon child, then attaches an OIDC
// KeystoneIdentityBackend and marks it Ready.
//
// It is the load-bearing proof for the identity-backend watch and its field
// index: nothing else touches the ControlPlane after the Horizon child exists,
// so the websso block can only appear if the backend event woke the reconciler
// and listIdentityBackends resolved through the index. It also asserts the
// Ready gate (a not-Ready backend contributes no choice) and the Keystone-side
// trusted_dashboard projection.
func TestIntegration_FederationBackendWakesReconcileAndProjectsWebSSO(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)
	ensureReadyClusterSecretStore(t, ctx, c)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-controlplane-sso-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")
	// Pre-seed the per-tenant SecretStore Ready so the pipeline reaches the
	// credential and Keystone/Horizon projection stages (envtest has no ESO
	// controller to flip the store's Ready condition).
	ensureReadySecretStore(t, ctx, c, esoTenantStoreName, ns.Name)

	cp := integrationManagedControlPlane("cp", ns.Name)
	cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.com/v3"
	cp.Spec.Services.Horizon = &c5c3v1alpha1.ServiceHorizonSpec{
		PublicEndpoint: "https://horizon.example.com:8443",
	}

	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminPasswordSecretName(cp), Namespace: ns.Name},
		Data:       map[string][]byte{"password": []byte("super-secret-admin-password")},
	}
	g.Expect(c.Create(ctx, adminSecret)).To(Succeed(), "create admin password Secret")
	g.Expect(c.Create(ctx, cp)).To(Succeed(), "create ControlPlane CR")
	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}

	// Drive the chain far enough for the Horizon child to be projected.
	simulateMariaDBReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "openstack-db", Namespace: ns.Name})
	simulateMemcachedReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "openstack-memcached", Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeInfrastructureReady, metav1.ConditionTrue, itEventuallyTimeout)

	g.Eventually(func() error {
		return simulators.SimulateExternalSecretSync(ctx, c,
			client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)})
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "simulate per-CP DB credential ExternalSecret sync")
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeDBCredentialsReady, metav1.ConditionTrue, itEventuallyTimeout)

	simulateAdminPasswordExternalSecretSyncWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminPasswordReady, metav1.ConditionTrue, itEventuallyTimeout)

	keystoneKey := client.ObjectKey{Name: keystoneName(cp), Namespace: ns.Name}
	simulateKeystoneReadyWhenPresent(t, ctx, c, keystoneKey)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeKeystoneReady, metav1.ConditionTrue, itEventuallyTimeout)

	// The Keystone child carries the dashboard's WebSSO origin — derived
	// top-down from cp.Spec, so it is present before any backend attaches.
	projectedKeystone := &keystonev1alpha1.Keystone{}
	g.Eventually(func() []string {
		if err := c.Get(ctx, keystoneKey, projectedKeystone); err != nil || projectedKeystone.Spec.Federation == nil {
			return nil
		}
		return projectedKeystone.Spec.Federation.TrustedDashboards
	}, itEventuallyTimeout, itPollInterval).Should(Equal([]string{"https://horizon.example.com:8443/auth/websso/"}),
		"trusted_dashboard must carry the dashboard's non-default port verbatim")

	horizonKey := client.ObjectKey{Name: horizonName(cp), Namespace: ns.Name}
	projectedHorizon := &horizonv1alpha1.Horizon{}
	g.Eventually(func() error {
		return c.Get(ctx, horizonKey, projectedHorizon)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "Horizon child should be projected once KeystoneReady")
	g.Expect(projectedHorizon.Spec.WebSSO).To(BeNil(), "no backend attached yet, so no SSO choice")

	// Attach an OIDC backend. It starts NOT Ready, so the login page must not
	// gain an SSO button that dead-ends.
	oidcBackend := &keystonev1alpha1.KeystoneIdentityBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "keycloak", Namespace: ns.Name},
		Spec: keystonev1alpha1.KeystoneIdentityBackendSpec{
			KeystoneRef: keystonev1alpha1.KeystoneRefSpec{Name: keystoneName(cp)},
			// The Default domain hosts the SQL-backed service users and the
			// bootstrap admin, so the CRD forbids backing it with an external
			// identity backend — every federated backend gets its own domain.
			Domain: keystonev1alpha1.DomainSpec{Name: "federated"},
			Type:   keystonev1alpha1.IdentityBackendTypeOIDC,
			OIDC: &keystonev1alpha1.OIDCBackendSpec{
				Issuer:          "https://keycloak.example.com/realms/cobaltcore",
				ClientID:        "keystone",
				ClientSecretRef: commonv1.SecretRefSpec{Name: "keycloak-client", Key: "client-secret"},
			},
		},
	}
	g.Expect(c.Create(ctx, oidcBackend)).To(Succeed(), "create OIDC identity backend")

	g.Consistently(func() *horizonv1alpha1.WebSSOSpec {
		h := &horizonv1alpha1.Horizon{}
		if err := c.Get(ctx, horizonKey, h); err != nil {
			return nil
		}
		return h.Spec.WebSSO
	}, 3*time.Second, itPollInterval).Should(BeNil(), "a not-Ready backend must contribute no websso choice")

	// Mark the backend Ready. Nothing else touches the ControlPlane, so the
	// projection below can only happen via the identity-backend watch.
	g.Eventually(func() error {
		fresh := &keystonev1alpha1.KeystoneIdentityBackend{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(oidcBackend), fresh); err != nil {
			return err
		}
		meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "AllBackendsProjected",
			Message:            "simulated",
			ObservedGeneration: fresh.Generation,
		})
		return c.Status().Update(ctx, fresh)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "mark the OIDC backend Ready")

	g.Eventually(func() *horizonv1alpha1.WebSSOSpec {
		h := &horizonv1alpha1.Horizon{}
		if err := c.Get(ctx, horizonKey, h); err != nil {
			return nil
		}
		return h.Spec.WebSSO
	}, itEventuallyTimeout, itPollInterval).ShouldNot(BeNil(),
		"the identity-backend watch must wake the ControlPlane and project the websso block")

	h := &horizonv1alpha1.Horizon{}
	g.Expect(c.Get(ctx, horizonKey, h)).To(Succeed())
	g.Expect(h.Spec.WebSSO.Enabled).To(BeTrue())
	g.Expect(h.Spec.WebSSO.Choices).To(Equal([]horizonv1alpha1.WebSSOChoice{
		{ID: horizonv1alpha1.DefaultWebSSOLocalChoiceID, Label: horizonv1alpha1.DefaultWebSSOLocalChoiceLabel},
		{ID: "keycloak_openid", Label: "keycloak"},
	}))
	g.Expect(h.Spec.WebSSO.IDPMapping).To(HaveKeyWithValue("keycloak_openid",
		horizonv1alpha1.WebSSOIDPTarget{IdentityProvider: "keycloak", Protocol: "openid"}))
	// The browser follows the SSO redirect, so it must target the external
	// Keystone endpoint — never the cluster-local Service URL.
	g.Expect(h.Spec.WebSSO.KeystoneURL).To(Equal("https://keystone.example.com/v3"))
	g.Expect(h.Spec.KeystoneEndpoint).To(HavePrefix("http://" + keystoneName(cp)))

	// Detaching the backend clears the block, so the SSO button disappears.
	g.Expect(c.Delete(ctx, oidcBackend)).To(Succeed())
	g.Eventually(func() *horizonv1alpha1.WebSSOSpec {
		fresh := &horizonv1alpha1.Horizon{}
		if err := c.Get(ctx, horizonKey, fresh); err != nil {
			return &horizonv1alpha1.WebSSOSpec{}
		}
		return fresh.Spec.WebSSO
	}, itEventuallyTimeout, itPollInterval).Should(BeNil(),
		"detaching the last backend must clear the websso block")
}

// ensureReadySecretStore creates or refreshes a namespaced SecretStore with a
// Ready=True condition in namespace — the per-tenant store the store-ref
// integration test selects (issue #605).
func ensureReadySecretStore(t testing.TB, ctx context.Context, c client.Client, name, namespace string) {
	t.Helper()
	g := NewGomegaWithT(t)

	store := &esov1.SecretStore{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	err := c.Get(ctx, client.ObjectKeyFromObject(store), store)
	if apierrors.IsNotFound(err) {
		g.Expect(c.Create(ctx, store)).To(Succeed(), "create SecretStore")
	} else {
		g.Expect(err).NotTo(HaveOccurred(), "get SecretStore")
	}

	store.Status = esov1.SecretStoreStatus{
		Conditions: []esov1.SecretStoreStatusCondition{
			{Type: esov1.SecretStoreReady, Status: corev1.ConditionTrue},
		},
	}
	g.Expect(c.Status().Update(ctx, store)).To(Succeed(), "update SecretStore status")
}

// TestIntegration_SecretStoreRefProjectedAndGated proves the two halves of the
// per-ControlPlane store choice end-to-end: (1) the credential gates follow the
// selected namespaced SecretStore — while it is absent DBCredentialsReady stays
// False with SecretStoreNotReady even though the cluster store is Ready; and
// (2) once the namespaced store is Ready and the credential ExternalSecrets
// sync, the projected Keystone child carries spec.secretStoreRef (issue #605).
func TestIntegration_SecretStoreRefProjectedAndGated(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	// Cluster store Ready — this must NOT satisfy a ControlPlane pinned to a
	// namespaced store.
	ensureReadyClusterSecretStore(t, ctx, c)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-storeref-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")

	cp := integrationManagedControlPlane("cp", ns.Name)
	cp.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced,
		Name: "openbao-tenant-store",
	}
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminPasswordSecretName(cp), Namespace: ns.Name},
		Data:       map[string][]byte{"password": []byte("super-secret-admin-password")},
	}
	g.Expect(c.Create(ctx, adminSecret)).To(Succeed(), "create admin password Secret")
	g.Expect(c.Create(ctx, cp)).To(Succeed(), "create ControlPlane CR")
	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}

	simulateMariaDBReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "openstack-db", Namespace: ns.Name})
	simulateMemcachedReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "openstack-memcached", Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeInfrastructureReady, metav1.ConditionTrue, itEventuallyTimeout)

	// The namespaced store is absent, so the DB-credential gate must stay closed
	// with SecretStoreNotReady even though the cluster store is Ready — proving
	// the gate follows spec.secretStoreRef.
	cond := waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeDBCredentialsReady, metav1.ConditionFalse, itEventuallyTimeout)
	g.Expect(cond.Reason).To(Equal("SecretStoreNotReady"))
	g.Expect(cond.Message).To(ContainSubstring("openbao-tenant-store"))

	// Bring the namespaced store up → the DB-credential ExternalSecret appears
	// and, once synced, DBCredentialsReady flips True.
	ensureReadySecretStore(t, ctx, c, "openbao-tenant-store", ns.Name)
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)}, &esov1.ExternalSecret{})
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "operator must create the per-CP DB credential ExternalSecret once the store is Ready")

	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)})).
		To(Succeed(), "simulate per-CP DB credential ExternalSecret sync")
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeDBCredentialsReady, metav1.ConditionTrue, itEventuallyTimeout)

	// The admin-password ExternalSecret (a static KV-backed ES, unlike the
	// generator-backed DB one) must reference the namespaced tenant store.
	adminES := &esov1.ExternalSecret{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: adminPasswordSecretName(cp)}, adminES)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "operator must create the per-CP admin-password ExternalSecret")
	g.Expect(adminES.Spec.SecretStoreRef.Kind).To(Equal("SecretStore"))
	g.Expect(adminES.Spec.SecretStoreRef.Name).To(Equal("openbao-tenant-store"))

	simulateAdminPasswordExternalSecretSyncWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminPasswordReady, metav1.ConditionTrue, itEventuallyTimeout)

	// The projected Keystone child must carry the namespaced store ref.
	ks := &keystonev1alpha1.Keystone{}
	g.Eventually(func() error {
		return c.Get(ctx, types.NamespacedName{Name: keystoneName(cp), Namespace: ns.Name}, ks)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "Keystone CR must be projected once the gates open")
	g.Expect(ks.Spec.SecretStoreRef).NotTo(BeNil(), "the ControlPlane store ref must be projected onto the Keystone child")
	g.Expect(ks.Spec.SecretStoreRef.Kind).To(Equal(commonv1.SecretStoreKindNamespaced))
	g.Expect(ks.Spec.SecretStoreRef.Name).To(Equal("openbao-tenant-store"))
}

// integrationDedicatedControlPlane returns a ControlPlane whose Keystone service
// opts into a dedicated database AND cache, and whose Horizon dashboard opts into
// a dedicated cache, on top of the shared infrastructure block. It is the opt-in
// shape TestIntegration_DedicatedBackingServices drives end to end against the
// real CRD schema and webhook.
func integrationDedicatedControlPlane(name, namespace string) *c5c3v1alpha1.ControlPlane {
	cp := integrationManagedControlPlane(name, namespace)
	cp.Spec.Services.Keystone.DedicatedBackingServices = &c5c3v1alpha1.KeystoneDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{
			ClusterRef:      &corev1.LocalObjectReference{Name: name + "-keystone-db"},
			CredentialsMode: commonv1.CredentialsModeStatic,
			Database:        "keystone",
			SecretRef:       commonv1.SecretRefSpec{Name: "keystone-db"},
			Replicas:        1,
			StorageSize:     "512Mi",
		},
		Cache: &commonv1.CacheSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: name + "-keystone-cache"},
			Backend:    commonv1.DefaultCacheBackend,
			Replicas:   1,
		},
	}
	cp.Spec.Services.Horizon = &c5c3v1alpha1.ServiceHorizonSpec{
		Replicas: ptr.To(int32(1)),
		DedicatedBackingServices: &c5c3v1alpha1.HorizonDedicatedBackingServicesSpec{
			Cache: &commonv1.CacheSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: name + "-horizon-cache"},
				Backend:    commonv1.DefaultCacheBackend,
				Replicas:   1,
			},
		},
	}
	return cp
}

// TestIntegration_DedicatedBackingServices drives the per-service opt-in through
// the whole chain against a live API server with the real CRD schema and webhook:
//
//   - both services' DEDICATED instances are provisioned and controller-OWNED
//     (ownership is what tears them down with the ControlPlane), while the shared
//     block — which every service here opted out of — has no consumer left and is
//     not provisioned at all;
//   - readiness is gated COLLECTIVELY: with every other instance Ready but the
//     dedicated database still converging, InfrastructureReady stays False and no
//     Keystone child is projected — the consuming service waits for ITS database;
//   - the dedicated managed database takes the STATIC credential branch (no
//     engine role exists for a dedicated instance) and no generator is projected;
//   - each child is pointed at the instance its service actually got.
func TestIntegration_DedicatedBackingServices(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)
	ensureReadyClusterSecretStore(t, ctx, c)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-dedicated-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")
	ensureReadySecretStore(t, ctx, c, esoTenantStoreName, ns.Name)

	cp := integrationDedicatedControlPlane("cp", ns.Name)
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminPasswordSecretName(cp), Namespace: ns.Name},
		Data:       map[string][]byte{"password": []byte("super-secret-admin-password")},
	}
	g.Expect(c.Create(ctx, adminSecret)).To(Succeed(), "create admin password Secret")
	g.Expect(c.Create(ctx, cp)).To(Succeed(), "create ControlPlane CR with dedicated backing services")
	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}

	// --- The dedicated instances are provisioned and owned. ---
	dedicatedDB := &mariadbv1alpha1.MariaDB{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Name: "cp-keystone-db", Namespace: ns.Name}, dedicatedDB)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "the dedicated MariaDB must be provisioned")
	g.Expect(dedicatedDB.Spec.Replicas).To(Equal(int32(1)),
		"the dedicated database is sized from its OWN spec, independently of the shared cluster")
	g.Expect(metav1.IsControlledBy(dedicatedDB, cp)).To(BeTrue(),
		"the dedicated MariaDB must be controller-owned so it is torn down with the ControlPlane")

	for _, cacheName := range []string{"cp-keystone-cache", "cp-horizon-cache"} {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(memcachedGVK)
		g.Eventually(func() error {
			return c.Get(ctx, client.ObjectKey{Name: cacheName, Namespace: ns.Name}, u)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "Memcached %q must be provisioned", cacheName)
		g.Expect(u.GetOwnerReferences()).NotTo(BeEmpty(), "Memcached %q must be owned", cacheName)
	}

	// --- The shared block has NO consumer left: both services opted out of both
	// classes, so nothing resolves to the shared instances and neither is
	// provisioned — an orphan cluster nothing talks to must not be created, nor
	// hold InfrastructureReady back while it converges. ---
	g.Consistently(func() bool {
		dbErr := c.Get(ctx, client.ObjectKey{Name: "openstack-db", Namespace: ns.Name}, &mariadbv1alpha1.MariaDB{})
		sharedCache := &unstructured.Unstructured{}
		sharedCache.SetGroupVersionKind(memcachedGVK)
		cacheErr := c.Get(ctx, client.ObjectKey{Name: "openstack-memcached", Namespace: ns.Name}, sharedCache)
		return apierrors.IsNotFound(dbErr) && apierrors.IsNotFound(cacheErr)
	}, 2*time.Second, itPollInterval).Should(BeTrue(),
		"a shared instance every service opted out of has no consumer and must not be provisioned")

	// --- Collective readiness gate: every other instance Ready, the dedicated
	// database still converging. InfrastructureReady must stay False and no
	// Keystone child may be projected. ---
	simulateMemcachedReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "cp-keystone-cache", Namespace: ns.Name})
	simulateMemcachedReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "cp-horizon-cache", Namespace: ns.Name})

	g.Consistently(func() bool {
		gated := &c5c3v1alpha1.ControlPlane{}
		if err := c.Get(ctx, cpKey, gated); err != nil {
			return false
		}
		cond := meta.FindStatusCondition(gated.Status.Conditions, conditionTypeInfrastructureReady)
		return cond == nil || cond.Status == metav1.ConditionFalse
	}, 2*time.Second, itPollInterval).Should(BeTrue(),
		"a pending DEDICATED database must hold InfrastructureReady False even when every other instance is Ready")
	g.Consistently(func() bool {
		err := c.Get(ctx, types.NamespacedName{Name: keystoneName(cp), Namespace: ns.Name}, &keystonev1alpha1.Keystone{})
		return apierrors.IsNotFound(err)
	}, 2*time.Second, itPollInterval).Should(BeTrue(),
		"no Keystone child may be projected while its dedicated database is still converging")

	// --- Open the gate: the dedicated database becomes Ready. ---
	simulateMariaDBReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "cp-keystone-db", Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeInfrastructureReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- The dedicated managed database takes the STATIC credential branch. ---
	dbES := &esov1.ExternalSecret{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)}, dbES)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "the DB-credential ExternalSecret must be projected")
	g.Expect(dbES.Spec.DataFrom).To(BeEmpty(),
		"a dedicated database must not draw from an engine generator: no per-instance OpenBao engine role exists")
	g.Expect(dbES.Spec.Data).To(HaveLen(2))
	g.Expect(dbES.Spec.Data[0].RemoteRef.Key).To(Equal(dbCredentialRemoteKeyFor(cp)))
	g.Consistently(func() bool {
		err := c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)},
			&esgenv1alpha1.VaultDynamicSecret{})
		return apierrors.IsNotFound(err)
	}, 2*time.Second, itPollInterval).Should(BeTrue(),
		"a dedicated database must not project a VaultDynamicSecret generator")

	// --- Each child is pointed at the instance its service actually got. ---
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)})).To(Succeed())
	simulateAdminPasswordExternalSecretSyncWhenPresent(t, ctx, c, cp)

	ks := &keystonev1alpha1.Keystone{}
	g.Eventually(func() error {
		return c.Get(ctx, types.NamespacedName{Name: keystoneName(cp), Namespace: ns.Name}, ks)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "the Keystone child must be projected")
	g.Expect(ks.Spec.Database.ClusterRef).NotTo(BeNil())
	g.Expect(ks.Spec.Database.ClusterRef.Name).To(Equal("cp-keystone-db"))
	g.Expect(ks.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeStatic))
	g.Expect(ks.Spec.Database.SecretRef.Name).To(Equal(dbCredentialSecretName(cp)))
	g.Expect(ks.Spec.Cache.ClusterRef).NotTo(BeNil())
	g.Expect(ks.Spec.Cache.ClusterRef.Name).To(Equal("cp-keystone-cache"))

	// Horizon is gated on KeystoneReady; once open, the dashboard is pointed at ITS
	// dedicated cache.
	simulateKeystoneReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: keystoneName(cp), Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeKeystoneReady, metav1.ConditionTrue, itEventuallyTimeout)

	hz := &horizonv1alpha1.Horizon{}
	g.Eventually(func() error {
		return c.Get(ctx, types.NamespacedName{Name: horizonName(cp), Namespace: ns.Name}, hz)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "the Horizon child must be projected")
	g.Expect(hz.Spec.Cache.ClusterRef).NotTo(BeNil())
	g.Expect(hz.Spec.Cache.ClusterRef.Name).To(Equal("cp-horizon-cache"),
		"the dashboard must be pointed at its own dedicated cache, not the shared one")
}

// TestIntegration_DedicatedBackingServices_TransitionRejected proves the
// shared<->dedicated freeze holds at the REAL validating webhook, not just in the
// unit tests: a live ControlPlane cannot be moved between shared and dedicated
// backing services in either direction. The freeze is webhook-only (no CEL
// transition rule), so a later transition feature can relax it to a gated
// migration.
func TestIntegration_DedicatedBackingServices_TransitionRejected(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-dedicated-flip-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")

	// A live ControlPlane sharing the ControlPlane-wide instances (the default).
	shared := integrationManagedControlPlane("cp", ns.Name)
	g.Expect(c.Create(ctx, shared)).To(Succeed(), "create shared-backing-services ControlPlane")

	// shared -> dedicated is rejected. Get-mutate-update under RetryOnConflict:
	// the live controller writes to the freshly created CR concurrently (the
	// c5c3.io/orc-teardown finalizer, then status), and a stale resourceVersion
	// returns 409 Conflict before admission ever evaluates the transition. The
	// webhook denial is deterministic once the update reaches it, so only the
	// Conflict is retried.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live := &c5c3v1alpha1.ControlPlane{}
		if err := c.Get(ctx, types.NamespacedName{Name: "cp", Namespace: ns.Name}, live); err != nil {
			return err
		}
		live.Spec.Services.Keystone.DedicatedBackingServices = &c5c3v1alpha1.KeystoneDedicatedBackingServicesSpec{
			Cache: &commonv1.CacheSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-cache"},
				Backend:    commonv1.DefaultCacheBackend,
			},
		}
		return c.Update(ctx, live)
	})
	g.Expect(err).To(HaveOccurred(), "moving a live service onto dedicated backing services must be rejected")
	g.Expect(err.Error()).To(ContainSubstring("switching a service between shared and dedicated backing services"))

	// dedicated -> shared is rejected too. A second ControlPlane needs its own
	// namespace (one ControlPlane per namespace).
	ns2 := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-dedicated-flip-"}}
	g.Expect(c.Create(ctx, ns2)).To(Succeed(), "create second test namespace")
	dedicated := integrationDedicatedControlPlane("cp", ns2.Name)
	g.Expect(c.Create(ctx, dedicated)).To(Succeed(), "create dedicated-backing-services ControlPlane")

	// Same RetryOnConflict rationale as the shared -> dedicated leg above.
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live2 := &c5c3v1alpha1.ControlPlane{}
		if err := c.Get(ctx, types.NamespacedName{Name: "cp", Namespace: ns2.Name}, live2); err != nil {
			return err
		}
		live2.Spec.Services.Keystone.DedicatedBackingServices = nil
		return c.Update(ctx, live2)
	})
	g.Expect(err).To(HaveOccurred(), "moving a live service back onto the shared instances must be rejected")
	g.Expect(err.Error()).To(ContainSubstring("switching a service between shared and dedicated backing services"))
}

// --- per-service namespaces (issue #646) ---

// TestIntegration_DedicatedNamespaces exercises the whole cross-namespace path
// against a real API server: the operator creates the namespace, provisions the
// backing services and the tenant store IN it, materialises the credential
// material there, projects the Keystone and Barbican children (and the dedicated
// OpenBao ensemble behind Barbican's secret store) there — all without an owner
// reference, which the API server would reject across a namespace — and then, on
// deletion, tears every one of them down by hand and deletes the namespace.
//
// Barbican is what puts an ORDER on that teardown. Its OpenBao instance carries the
// openbao-operator's finalizer, and that finalizer runs under the tenant admitting
// this namespace, so the tenant and the namespace must outlive the instance. The
// test holds the instance Terminating behind a fake finalizer to pin the invariant.
//
// Barbican's KeystoneService registration is placed in the same namespace, and it
// comes down ahead of everything else: its managed User is held Terminating behind
// a K-ORC-shaped finalizer, and the Keystone child, the instance and the namespace
// keep a zero DeletionTimestamp until the registration and its children are gone.
//
// envtest runs no namespace controller, so a deleted namespace never actually
// disappears: it is asserted on its DeletionTimestamp, which is what the operator
// is responsible for setting.
func TestIntegration_DedicatedNamespaces(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupRegisteringControlPlaneEnvTest(t)
	ensureReadyClusterSecretStore(t, ctx, c)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-ns-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create the ControlPlane's namespace")

	// The Keystone service is placed in a namespace of its own, under the Managed
	// lifecycle: the operator creates it, and deletes it with the ControlPlane.
	keystoneNS := ns.Name + "-identity"

	cp := integrationManagedControlPlane("cp", ns.Name)
	cp.Spec.Services.Keystone.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name:      keystoneNS,
		Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	// Barbican shares that namespace. Two services in one namespace yield ONE
	// teardown assignment, and Barbican is the service whose teardown has the most to
	// sequence: its dedicated OpenBao instance holds the openbao-operator's finalizer,
	// and the tenant admitting the namespace is what that finalizer runs under.
	cp.Spec.Services.Barbican = integrationBarbicanService()
	cp.Spec.Services.Barbican.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name:      keystoneNS,
		Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	g.Expect(c.Create(ctx, cp)).To(Succeed(), "create ControlPlane CR with a dedicated Keystone namespace")
	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}

	// --- The namespace is created and stamped with the ownership labels. ---
	created := &corev1.Namespace{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Name: keystoneNS}, created)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "the Managed service namespace must be created")
	g.Expect(created.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	g.Expect(created.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, ns.Name))
	g.Expect(created.Labels).To(HaveKeyWithValue(managedByLabel, managedByValue))

	// --- The backing services follow the service into that namespace, carrying
	// the ownership labels and NO owner reference: the API server rejects a
	// cross-namespace controller reference outright. ---
	mariadb := &mariadbv1alpha1.MariaDB{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Name: "openstack-db", Namespace: keystoneNS}, mariadb)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"the database must be provisioned in the Keystone service's namespace")
	g.Expect(mariadb.OwnerReferences).To(BeEmpty(),
		"a cross-namespace child cannot carry an owner reference")
	g.Expect(mariadb.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))

	memcached := &unstructured.Unstructured{}
	memcached.SetGroupVersionKind(memcachedGVK)
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Name: "openstack-memcached", Namespace: keystoneNS}, memcached)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"the cache must be provisioned in the Keystone service's namespace")

	// Nothing is provisioned in the ControlPlane's own namespace: no service
	// resolves there.
	g.Consistently(func() bool {
		err := c.Get(ctx, client.ObjectKey{Name: "openstack-db", Namespace: ns.Name}, &mariadbv1alpha1.MariaDB{})
		return apierrors.IsNotFound(err)
	}, 2*time.Second, itPollInterval).Should(BeTrue(),
		"no backing service may be provisioned in a namespace no service is placed in")

	// The ESOTenantStore sub-reconciler runs after Infrastructure, which
	// short-circuits the pipeline while a backing service is still converging — so
	// the backing services have to reach Ready before the tenant stores are
	// provisioned at all.
	simulateMariaDBReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "openstack-db", Namespace: keystoneNS})
	simulateMemcachedReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "openstack-memcached", Namespace: keystoneNS})

	// --- A tenant SecretStore is provisioned in BOTH namespaces: an ESO store and
	// the Secrets it materialises are namespace-local, so the store at home cannot
	// deliver anything into the service namespace. ---
	for _, storeNS := range []string{ns.Name, keystoneNS} {
		store := &esov1.SecretStore{}
		g.Eventually(func() error {
			return c.Get(ctx, client.ObjectKey{Name: esoTenantStoreName, Namespace: storeNS}, store)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
			"a tenant SecretStore must be provisioned in namespace %q", storeNS)
	}
	// Drive both stores Ready so the store-gated sub-reconcilers proceed.
	ensureReadySecretStore(t, ctx, c, esoTenantStoreName, ns.Name)
	ensureReadySecretStore(t, ctx, c, esoTenantStoreName, keystoneNS)

	// --- The DB credential is issued in the Keystone namespace too: the engine
	// role, the generator's ServiceAccount, and the ExternalSecret all follow the
	// database. Sync it so the pipeline advances to AdminPassword. ---
	dbES := &esov1.ExternalSecret{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Name: dbCredentialSecretName(cp), Namespace: keystoneNS}, dbES)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"the DB-credential ExternalSecret must be projected beside the Keystone child")
	g.Expect(dbES.OwnerReferences).To(BeEmpty())
	vds := &esgenv1alpha1.VaultDynamicSecret{}
	g.Expect(c.Get(ctx, client.ObjectKey{
		Name: dbCredentialSecretName(cp), Namespace: keystoneNS,
	}, vds)).To(Succeed(), "the generator must be projected beside the database it issues against")
	g.Expect(vds.Spec.Path).To(Equal("database/mariadb/creds/keystone-"+keystoneNS),
		"the engine role is keyed on the Keystone service namespace, which the templated OpenBao policy grants")
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Name: dbCredentialSecretName(cp), Namespace: keystoneNS})).To(Succeed())

	// --- The admin-password ExternalSecret is materialised in the Keystone
	// namespace, at the OpenBao path keyed on THAT namespace — the path the
	// keystone-operator's rotation PushSecret writes to. ---
	simulateAdminPasswordExternalSecretSyncWhenPresent(t, ctx, c, cp)
	adminES := &esov1.ExternalSecret{}
	g.Expect(c.Get(ctx, client.ObjectKey{
		Name: adminPasswordSecretName(cp), Namespace: keystoneNS,
	}, adminES)).To(Succeed(), "the admin password must be materialised beside the Keystone child")
	g.Expect(adminES.Spec.Data[0].RemoteRef.Key).To(Equal("bootstrap/" + keystoneNS + "/cp-keystone/admin"))
	g.Expect(adminES.OwnerReferences).To(BeEmpty())

	// --- The Keystone child is projected into the service namespace, and its
	// in-cluster endpoint resolves across the namespace boundary. ---
	keystone := &keystonev1alpha1.Keystone{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Name: "cp-keystone", Namespace: keystoneNS}, keystone)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"the Keystone child must be projected into its assigned namespace")
	g.Expect(keystone.OwnerReferences).To(BeEmpty(),
		"the API server rejects a cross-namespace controller owner reference")
	g.Expect(keystone.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	g.Expect(keystoneEndpointURL(cp)).To(Equal("http://cp-keystone." + keystoneNS + ".svc:5000/v3"))

	// --- NamespacesReady reports the namespace is there. ---
	g.Eventually(func() bool {
		live := &c5c3v1alpha1.ControlPlane{}
		if err := c.Get(ctx, cpKey, live); err != nil {
			return false
		}
		return conditions.AllTrue(live.Status.Conditions, conditionTypeNamespacesReady)
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(), "NamespacesReady must go True")

	// --- Drive the rest of the pipeline so the Barbican registration's delivery leg
	// runs. The admin/K-ORC/catalog machinery lives in the ControlPlane's own
	// namespace (K-ORC children never move); only the delivery follows the service.
	// K-ORC's readAdminPassword reads the cleartext beside the Keystone child, so
	// seed it in the (now-created) Keystone namespace. ---
	adminCleartext := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminPasswordSecretName(cp), Namespace: keystoneNS},
		Data:       map[string][]byte{"password": []byte("super-secret-admin-password")},
	}
	g.Expect(c.Create(ctx, adminCleartext)).To(Succeed(), "seed the cleartext admin password beside the Keystone child")

	simulateKeystoneReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: keystoneName(cp), Namespace: keystoneNS})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeKeystoneReady, metav1.ConditionTrue, itEventuallyTimeout)

	simulateApplicationCredentialAvailableWhenPresent(t, ctx, c,
		client.ObjectKey{Name: adminAppCredentialName(cp), Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeKORCReady, metav1.ConditionTrue, itEventuallyTimeout)

	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Namespace: ns.Name, Name: korcCloudsYamlSecretName})).
		To(Succeed(), "simulate k-orc clouds.yaml ExternalSecret sync")
	simulatePushSecretSyncedWhenPresent(t, ctx, c,
		client.ObjectKey{Name: adminAppCredentialPushSecretName(cp), Namespace: ns.Name})
	simulateCloudsYamlMaterializedWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminCredentialReady, metav1.ConditionTrue, itEventuallyTimeout)

	simulateCatalogServiceEndpointAvailableWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeCatalogReady, metav1.ConditionTrue, itEventuallyTimeout)

	// The Barbican registration splits across the two namespaces: its KeystoneService
	// child sits in the service namespace and delivers there, while the K-ORC children
	// it projects stay beside the admin credential in the ControlPlane's namespace.
	barbicanReg := simulateBuiltinRegistrationConvergedWhenPresent(t, ctx, c, cp,
		client.ObjectKey{Name: barbicanName(cp), Namespace: keystoneNS})

	// --- The Barbican ensemble follows its service into that namespace too. Open the
	// credential gate, then let the dedicated OpenBao instance serve so the secret
	// store and the Barbican child are projected beside it: those three are the wait
	// set the teardown below has to sequence. ---
	simulateBarbicanDBCredentialSyncWhenPresent(t, ctx, c, cp)

	instanceName := barbicanOpenBaoName(cp)
	instanceKey := client.ObjectKey{Name: instanceName, Namespace: keystoneNS}
	simulateOpenBaoClusterAvailableWhenPresent(t, ctx, c, instanceKey)

	tenantKey := client.ObjectKey{Name: instanceName + barbicanOpenBaoTenantSuffix, Namespace: keystoneNS}
	g.Expect(c.Get(ctx, tenantKey, &openbaov1alpha1.OpenBaoTenant{})).To(Succeed(),
		"the OpenBaoTenant admitting the Barbican namespace must be projected into it")

	authDelegatorKey := client.ObjectKey{Name: barbicanOpenBaoAuthDelegatorName(instanceName, keystoneNS)}
	g.Expect(c.Get(ctx, authDelegatorKey, &rbacv1.ClusterRoleBinding{})).To(Succeed(),
		"the auth-delegator ClusterRoleBinding must be projected")

	projectedBarbican := &barbicanv1alpha1.Barbican{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Name: barbicanName(cp), Namespace: keystoneNS}, projectedBarbican)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"the Barbican child must be projected into its assigned namespace")
	g.Expect(projectedBarbican.OwnerReferences).To(BeEmpty(),
		"the API server rejects a cross-namespace controller owner reference")
	g.Expect(projectedBarbican.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))

	// --- Deletion: nothing garbage-collects a cross-namespace child, so the
	// finalizer must delete them by hand and take the namespace down with them.
	//
	// The instance is the one child whose own operator holds a finalizer over it, so
	// inject a fake one and hold it Terminating exactly as the openbao-operator would.
	// It is the wait set's whole point: the tenant that admits this namespace to the
	// openbao-operator — and the namespace itself — must outlive the instance, or the
	// instance's finalizer loses the RBAC it runs under and the namespace wedges. ---
	g.Eventually(func() error {
		instance := &openbaov1alpha1.OpenBaoCluster{}
		if err := c.Get(ctx, instanceKey, instance); err != nil {
			return err
		}
		if controllerutil.ContainsFinalizer(instance, fakeOpenBaoFinalizer) {
			return nil
		}
		controllerutil.AddFinalizer(instance, fakeOpenBaoFinalizer)
		return c.Update(ctx, instance)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"inject the fake openbao-operator finalizer on the dedicated instance")

	// The registration comes down before any of it, so hold its managed User
	// Terminating the way K-ORC would while it deletes the Keystone user. Its K-ORC
	// children stayed in the ControlPlane's namespace whatever namespace the
	// registration itself was placed in.
	barbicanRegUserKey := client.ObjectKey{Name: keystoneServiceUserRef(barbicanReg), Namespace: ns.Name}
	g.Eventually(func() error {
		user := &orcv1alpha1.User{}
		if err := c.Get(ctx, barbicanRegUserKey, user); err != nil {
			return err
		}
		if controllerutil.ContainsFinalizer(user, fakeKORCUserFinalizer) {
			return nil
		}
		controllerutil.AddFinalizer(user, fakeKORCUserFinalizer)
		return c.Update(ctx, user)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"inject the fake K-ORC finalizer on the Barbican registration's managed User")

	live := &c5c3v1alpha1.ControlPlane{}
	g.Expect(c.Get(ctx, cpKey, live)).To(Succeed())
	g.Expect(c.Delete(ctx, live)).To(Succeed(), "delete the ControlPlane")

	// The registration in the dedicated namespace goes FIRST, and the ControlPlane
	// reports the wait it is in on its own status.
	barbicanRegKey := client.ObjectKey{Name: barbicanName(cp), Namespace: keystoneNS}
	g.Eventually(func() bool {
		gotReg := &c5c3v1alpha1.KeystoneService{}
		if err := c.Get(ctx, barbicanRegKey, gotReg); err != nil {
			return false
		}
		if gotReg.DeletionTimestamp.IsZero() {
			return false
		}
		gotCP := &c5c3v1alpha1.ControlPlane{}
		if err := c.Get(ctx, cpKey, gotCP); err != nil {
			return false
		}
		korcReady := meta.FindStatusCondition(gotCP.Status.Conditions, conditionTypeKORCReady)
		return korcReady != nil && korcReady.Status == metav1.ConditionFalse &&
			korcReady.Reason == "FinalizingServiceRegistrations"
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
		"the registration must be deleted first, under KORCReady=False/FinalizingServiceRegistrations")

	// Nothing behind it has started while it is held: the Keystone child, the
	// OpenBao instance and the namespace itself all still carry a zero
	// DeletionTimestamp. The window spans a full korcRequeueAfter, so the teardown is
	// observed re-running against the still-present registration.
	g.Consistently(func() bool {
		gotCP := &c5c3v1alpha1.ControlPlane{}
		if err := c.Get(ctx, cpKey, gotCP); err != nil {
			return false
		}
		if !controllerutil.ContainsFinalizer(gotCP, controlPlaneORCFinalizer) {
			return false
		}
		keystoneChild := &keystonev1alpha1.Keystone{}
		if err := c.Get(ctx, client.ObjectKey{Name: "cp-keystone", Namespace: keystoneNS}, keystoneChild); err != nil {
			return false
		}
		if !keystoneChild.DeletionTimestamp.IsZero() {
			return false
		}
		instance := &openbaov1alpha1.OpenBaoCluster{}
		if err := c.Get(ctx, instanceKey, instance); err != nil {
			return false
		}
		if !instance.DeletionTimestamp.IsZero() {
			return false
		}
		namespace := &corev1.Namespace{}
		if err := c.Get(ctx, client.ObjectKey{Name: keystoneNS}, namespace); err != nil {
			return false
		}
		return namespace.DeletionTimestamp.IsZero()
	}, korcRequeueAfter+5*time.Second, itPollInterval).Should(BeTrue(),
		"the Keystone child, the instance and the namespace must not move while a registration is still finishing")

	// Release the User; the registration finishes and takes its children with it.
	g.Eventually(func() error {
		user := &orcv1alpha1.User{}
		err := c.Get(ctx, barbicanRegUserKey, user)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		controllerutil.RemoveFinalizer(user, fakeKORCUserFinalizer)
		return c.Update(ctx, user)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"remove the fake K-ORC finalizer from the Barbican registration's managed User")

	// The teardown reaches across both namespaces: the delivery objects sat in the
	// service namespace, the K-ORC children in the ControlPlane's.
	g.Eventually(func() bool {
		if !apierrors.IsNotFound(c.Get(ctx, barbicanRegKey, &c5c3v1alpha1.KeystoneService{})) {
			return false
		}
		pushErr := c.Get(ctx, client.ObjectKey{
			Name: keystoneServicePushSecretName(barbicanReg), Namespace: keystoneNS,
		}, &esov1alpha1.PushSecret{})
		sourceErr := c.Get(ctx, client.ObjectKey{
			Name: keystoneServiceSourceSecretName(barbicanReg), Namespace: keystoneNS,
		}, &corev1.Secret{})
		userErr := c.Get(ctx, barbicanRegUserKey, &orcv1alpha1.User{})
		catalogErr := c.Get(ctx, client.ObjectKey{
			Name: keystoneServiceCatalogServiceRef(barbicanReg), Namespace: ns.Name,
		}, &orcv1alpha1.Service{})
		return apierrors.IsNotFound(pushErr) && apierrors.IsNotFound(sourceErr) &&
			apierrors.IsNotFound(userErr) && apierrors.IsNotFound(catalogErr)
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
		"the registration must take its delivery objects and its K-ORC children down across both namespaces")

	g.Eventually(func() bool {
		err := c.Get(ctx, client.ObjectKey{Name: "cp-keystone", Namespace: keystoneNS}, &keystonev1alpha1.Keystone{})
		return apierrors.IsNotFound(err)
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
		"the cross-namespace Keystone child must be deleted explicitly — no GC cascade reaches it")

	// The instance is deleted first and held Terminating by the fake finalizer.
	g.Eventually(func() bool {
		instance := &openbaov1alpha1.OpenBaoCluster{}
		if err := c.Get(ctx, instanceKey, instance); err != nil {
			return false
		}
		return !instance.DeletionTimestamp.IsZero()
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
		"the operator must delete the dedicated OpenBao instance as part of the namespace teardown")

	// Sequencing invariant: while the instance is still Terminating, the tenant it
	// runs under survives, the ControlPlane finalizer holds, and the namespace is NOT
	// deleted. The window spans a full namespaceRequeueAfter, so the sweep is
	// observed re-running against the still-Terminating instance rather than merely
	// not having run again yet.
	g.Consistently(func() bool {
		gotCP := &c5c3v1alpha1.ControlPlane{}
		if err := c.Get(ctx, cpKey, gotCP); err != nil {
			return false
		}
		if !controllerutil.ContainsFinalizer(gotCP, controlPlaneORCFinalizer) {
			return false
		}
		if err := c.Get(ctx, tenantKey, &openbaov1alpha1.OpenBaoTenant{}); err != nil {
			return false
		}
		namespace := &corev1.Namespace{}
		if err := c.Get(ctx, client.ObjectKey{Name: keystoneNS}, namespace); err != nil {
			return false
		}
		return namespace.DeletionTimestamp.IsZero()
	}, namespaceRequeueAfter+5*time.Second, itPollInterval).Should(BeTrue(),
		"the tenant, the ControlPlane finalizer and the namespace must all outlive a Terminating OpenBao instance")

	// Release the instance by removing the fake finalizer.
	g.Eventually(func() error {
		instance := &openbaov1alpha1.OpenBaoCluster{}
		err := c.Get(ctx, instanceKey, instance)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		controllerutil.RemoveFinalizer(instance, fakeOpenBaoFinalizer)
		return c.Update(ctx, instance)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"remove the fake openbao-operator finalizer from the instance")

	// Only now may the tenant follow. The cluster-scoped auth-delegator binding goes
	// with it: no namespace deletion and no owner-reference cascade ever reaches a
	// cluster-scoped object, so the teardown is the only thing that can collect it.
	g.Eventually(func() bool {
		instanceErr := c.Get(ctx, instanceKey, &openbaov1alpha1.OpenBaoCluster{})
		tenantErr := c.Get(ctx, tenantKey, &openbaov1alpha1.OpenBaoTenant{})
		bindingErr := c.Get(ctx, authDelegatorKey, &rbacv1.ClusterRoleBinding{})
		return apierrors.IsNotFound(instanceErr) && apierrors.IsNotFound(tenantErr) && apierrors.IsNotFound(bindingErr)
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
		"the tenant and the cluster-scoped auth-delegator binding must be removed once the instance is gone")

	// envtest runs no namespace controller, so a deleted namespace stays Terminating
	// forever. The DeletionTimestamp is what the operator is responsible for.
	g.Eventually(func() bool {
		terminating := &corev1.Namespace{}
		if err := c.Get(ctx, client.ObjectKey{Name: keystoneNS}, terminating); err != nil {
			return apierrors.IsNotFound(err)
		}
		return !terminating.DeletionTimestamp.IsZero()
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
		"the Managed service namespace must be deleted with the ControlPlane")
}

// TestIntegration_DedicatedNamespaces_RefusesToAdoptForeignNamespace pins the
// never-adopt guard against a real API server. A Managed lifecycle DELETES its
// namespace at teardown, so adopting a pre-existing one would destroy every
// workload in it: the ControlPlane parks on NamespaceNotOwned instead, and
// projects nothing.
func TestIntegration_DedicatedNamespaces_RefusesToAdoptForeignNamespace(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)
	ensureReadyClusterSecretStore(t, ctx, c)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-adopt-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())
	ensureReadySecretStore(t, ctx, c, esoTenantStoreName, ns.Name)

	// A namespace somebody else provisioned, carrying none of our labels.
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		GenerateName: "test-foreign-",
		Labels:       map[string]string{"team": "platform"},
	}}
	g.Expect(c.Create(ctx, foreign)).To(Succeed())

	cp := integrationManagedControlPlane("cp", ns.Name)
	cp.Spec.Services.Keystone.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name:      foreign.Name,
		Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	g.Expect(c.Create(ctx, cp)).To(Succeed())
	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}

	g.Eventually(func() string {
		live := &c5c3v1alpha1.ControlPlane{}
		if err := c.Get(ctx, cpKey, live); err != nil {
			return ""
		}
		cond := conditions.GetCondition(live.Status.Conditions, conditionTypeNamespacesReady)
		if cond == nil {
			return ""
		}
		return cond.Reason
	}, itEventuallyTimeout, itPollInterval).Should(Equal("NamespaceNotOwned"),
		"a pre-existing namespace must never be adopted under the Managed lifecycle")

	// Nothing is projected into it, and it is left exactly as it was.
	g.Consistently(func() bool {
		err := c.Get(ctx, client.ObjectKey{Name: "openstack-db", Namespace: foreign.Name}, &mariadbv1alpha1.MariaDB{})
		return apierrors.IsNotFound(err)
	}, 2*time.Second, itPollInterval).Should(BeTrue(),
		"nothing may be projected into a namespace the operator refuses to own")

	untouched := &corev1.Namespace{}
	g.Expect(c.Get(ctx, client.ObjectKey{Name: foreign.Name}, untouched)).To(Succeed())
	g.Expect(untouched.Labels).NotTo(HaveKey(controlPlaneNameLabel),
		"a namespace the operator refuses to own must never be labelled")
}

// TestIntegration_RegistrationTenantStore_ProvisionedAndCollected drives the
// allowlisted-namespace tenant store over a real API server: it must appear when a
// registration shows up in an admitted namespace and go away again when the last
// one is deleted.
//
// It is the one place the whole loop is exercised end to end — the field index the
// registration List resolves through, the watch leg that re-drives the plane, and
// the cross-namespace ownership labels a foreign trio carries instead of an owner
// reference — none of which a fake client can prove on its own.
func TestIntegration_RegistrationTenantStore_ProvisionedAndCollected(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)
	ensureReadyClusterSecretStore(t, ctx, c)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-regstore-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create the control plane namespace")
	tenant := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-regstore-tenant-"}}
	g.Expect(c.Create(ctx, tenant)).To(Succeed(), "create the registration namespace")

	cp := integrationManagedControlPlane("controlplane", ns.Name)
	cp.Spec.KORC.ServiceRegistrations = &c5c3v1alpha1.ServiceRegistrationsSpec{
		AllowedNamespaces: []string{tenant.Name},
	}
	g.Expect(c.Create(ctx, cp)).To(Succeed(), "create the ControlPlane CR")

	// The sub-reconciler under test is a TAIL-GROUP member, and the blocking prefix
	// ahead of it short-circuits at the first non-zero result. Drive the plane past
	// that prefix, or the tail never runs and this test would prove nothing.
	driveControlPlaneToAdminCredentialReady(t, ctx, c, cp)

	storeKey := client.ObjectKey{Namespace: tenant.Name, Name: esoTenantStoreName}

	// Admitting a namespace provisions nothing by itself: it is worth a store only
	// once something in it actually registers.
	g.Consistently(func() bool {
		return apierrors.IsNotFound(c.Get(ctx, storeKey, &esov1.SecretStore{}))
	}, 2*time.Second, itPollInterval).Should(BeTrue(),
		"an allowlisted namespace with no registration must not receive a tenant store")

	registration := integrationKeystoneService(tenant.Name)
	// An empty reference namespace means the CR's OWN, which is the tenant
	// namespace here, so the plane it registers against has to be named explicitly.
	registration.Spec.ControlPlaneRef.Namespace = ns.Name
	g.Expect(c.Create(ctx, registration)).To(Succeed(), "create the KeystoneService")

	// The trio, label-owned: a cross-namespace child cannot carry an owner reference.
	g.Eventually(func(gm Gomega) {
		store := &esov1.SecretStore{}
		gm.Expect(c.Get(ctx, storeKey, store)).To(Succeed())
		gm.Expect(store.Labels).To(Equal(controlPlaneChildLabels(cp)))
		gm.Expect(metav1.GetControllerOf(store)).To(BeNil())

		sa := &corev1.ServiceAccount{}
		gm.Expect(c.Get(ctx, client.ObjectKey{
			Namespace: tenant.Name, Name: esoTenantServiceAccountName,
		}, sa)).To(Succeed())

		cert := &unstructured.Unstructured{}
		cert.SetGroupVersionKind(certificateGVK)
		gm.Expect(c.Get(ctx, client.ObjectKey{
			Namespace: tenant.Name, Name: esoTenantClientCertName,
		}, cert)).To(Succeed())
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"a registration in an allowlisted namespace must bring the tenant-store trio with it")

	// And it goes away with the last registration.
	g.Expect(c.Delete(ctx, registration)).To(Succeed(), "delete the KeystoneService")

	g.Eventually(func(gm Gomega) {
		gm.Expect(apierrors.IsNotFound(c.Get(ctx, storeKey, &esov1.SecretStore{}))).To(BeTrue())
		gm.Expect(apierrors.IsNotFound(c.Get(ctx, client.ObjectKey{
			Namespace: tenant.Name, Name: esoTenantServiceAccountName,
		}, &corev1.ServiceAccount{}))).To(BeTrue())
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"the trio must be collected once the namespace's last registration is gone")
}

// TestIntegration_KeystoneService_ForeignChildrenLandInThePlaneNamespace walks
// the placement over a real API server, which is the only place two of its
// properties exist at all.
//
// A fake client accepts any ownerReference and any label. An API server does not:
// it is what proves a child written into the ControlPlane's namespace, marked
// only by labels, is admitted and then STAYS — nothing owner-references it, so no
// garbage collection cascade can reach it and the finalizer sweep is the only
// thing that ever removes it. The unit tests can assert the intent; only this can
// assert the contract.
//
// The KeystoneService controller is not among the reconcilers the envtest manager
// runs, so the test drives it directly against the live client, the way the
// production controller would.
func TestIntegration_KeystoneService_ForeignChildrenLandInThePlaneNamespace(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)
	ensureReadyClusterSecretStore(t, ctx, c)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-ksplace-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create the control plane namespace")
	tenant := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-ksplace-tenant-"}}
	g.Expect(c.Create(ctx, tenant)).To(Succeed(), "create the registration namespace")

	cp := integrationManagedControlPlane("controlplane", ns.Name)
	cp.Spec.KORC.ServiceRegistrations = &c5c3v1alpha1.ServiceRegistrationsSpec{
		AllowedNamespaces: []string{tenant.Name},
	}
	g.Expect(c.Create(ctx, cp)).To(Succeed(), "create the ControlPlane CR")
	driveControlPlaneToAdminCredentialReady(t, ctx, c, cp)

	registration := integrationKeystoneService(tenant.Name)
	registration.Spec.Account = nil // catalog-only: the account leg gates on ESO, which envtest has none of.
	registration.Spec.ControlPlaneRef.Namespace = ns.Name
	g.Expect(c.Create(ctx, registration)).To(Succeed(), "create the KeystoneService")

	r := &KeystoneServiceReconciler{Client: c, Scheme: c.Scheme()}
	reconcile := func() {
		t.Helper()
		_, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: client.ObjectKeyFromObject(registration),
		})
		g.Expect(err).NotTo(HaveOccurred())
	}
	reconcile() // installs the finalizer
	reconcile() // projects

	probeKey := client.ObjectKey{
		Namespace: ns.Name, Name: keystoneServiceCatalogServiceProbeRef(registration),
	}
	probe := &orcv1alpha1.Service{}
	g.Expect(c.Get(ctx, probeKey, probe)).To(Succeed(),
		"the API server admits a registration child in the plane's namespace")
	g.Expect(probe.Labels).To(HaveKeyWithValue(keystoneServiceNameLabel, registration.Name))
	g.Expect(probe.Labels).To(HaveKeyWithValue(keystoneServiceNamespaceLabel, registration.Namespace))
	g.Expect(metav1.GetControllerOf(probe)).To(BeNil(),
		"no owner reference is possible across namespaces")

	// Nothing K-ORC in the tenant's namespace, where the admin clouds.yaml the
	// child authenticates with does not exist.
	var tenantServices orcv1alpha1.ServiceList
	g.Expect(c.List(ctx, &tenantServices, client.InNamespace(tenant.Name))).To(Succeed())
	g.Expect(tenantServices.Items).To(BeEmpty())

	// The child outlives its CR's deletion until the finalizer sweep removes it:
	// unreferenced, it is invisible to the garbage collector.
	g.Expect(c.Delete(ctx, registration)).To(Succeed(), "delete the KeystoneService")
	g.Eventually(func() bool {
		reconcile()
		return apierrors.IsNotFound(c.Get(ctx, probeKey, &orcv1alpha1.Service{}))
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
		"the teardown is what reaps a label-owned child, and it must find it")
	g.Eventually(func() bool {
		reconcile()
		return apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(registration),
			&c5c3v1alpha1.KeystoneService{}))
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
		"and then releases the finalizer, once no owned child is listed any more")
}

// --- shared messaging bus (issue #895) ---

// integrationMessagingControlPlane is integrationManagedControlPlane with the
// database and the cache switched to brownfield and a managed messaging block
// added. Neither brownfield block provisions a child, so the RabbitmqCluster is
// the only managed instance left and InfrastructureReady gates on it alone.
func integrationMessagingControlPlane(name, namespace string) *c5c3v1alpha1.ControlPlane {
	cp := integrationManagedControlPlane(name, namespace)
	cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		Host:      "db.invalid",
		Database:  "keystone",
		SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"},
	}
	cp.Spec.Infrastructure.Cache = commonv1.CacheSpec{
		Servers: []string{"mc.invalid:11211"},
		Backend: "dogpile.cache.pymemcache",
	}
	cp.Spec.Infrastructure.Messaging = &commonv1.MessagingSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: "openstack-rabbitmq"},
		Replicas:   1,
	}
	return cp
}

// TestIntegration_Messaging_ManagedProjectsRabbitmqCluster drives the managed
// message bus against a live API server: the reconciler writes an owned
// RabbitmqCluster into the ControlPlane's own namespace at the declared replica
// count, holds InfrastructureReady False with reason WaitingForMessaging while
// the broker converges, and flips it True once the RabbitMQ Cluster Operator's
// AllReplicasReady condition is reported.
func TestIntegration_Messaging_ManagedProjectsRabbitmqCluster(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-messaging-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")

	cp := integrationMessagingControlPlane("cp", ns.Name)
	g.Expect(c.Create(ctx, cp)).To(Succeed(), "create the messaging ControlPlane CR")
	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}
	busKey := client.ObjectKey{Name: "openstack-rabbitmq", Namespace: ns.Name}

	bus := &unstructured.Unstructured{}
	bus.SetGroupVersionKind(messaging.RabbitmqClusterGVK)
	g.Eventually(func() error {
		return c.Get(ctx, busKey, bus)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"the RabbitmqCluster child must be created in the ControlPlane's own namespace")

	live := &c5c3v1alpha1.ControlPlane{}
	g.Expect(c.Get(ctx, cpKey, live)).To(Succeed(), "get the live ControlPlane")
	g.Expect(metav1.IsControlledBy(bus, live)).To(BeTrue(),
		"the bus must be controller-owned so it is torn down with the ControlPlane")
	replicas, found, err := unstructured.NestedInt64(bus.Object, "spec", "replicas")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue(), "spec.replicas must be projected")
	g.Expect(replicas).To(Equal(int64(1)), "the declared replica count must reach the child")

	cond := waitForControlPlaneCondition(t, ctx, c, cpKey,
		conditionTypeInfrastructureReady, metav1.ConditionFalse, itEventuallyTimeout)
	g.Expect(cond.Reason).To(Equal("WaitingForMessaging"),
		"a broker that has not reported AllReplicasReady holds the infrastructure gate")

	simulateRabbitmqClusterReadyWhenPresent(t, ctx, c, busKey)

	waitForControlPlaneCondition(t, ctx, c, cpKey,
		conditionTypeInfrastructureReady, metav1.ConditionTrue, itEventuallyTimeout)
}

// TestIntegration_Messaging_Defaulting pins what admission does to a messaging
// block declared empty: the mutating webhook invents the managed clusterRef, the
// CRD default supplies the replica count, and the block cannot be dropped again
// once the ControlPlane is live.
func TestIntegration_Messaging_Defaulting(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-messaging-default-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")

	cp := integrationManagedControlPlane("cp", ns.Name)
	cp.Spec.Infrastructure.Messaging = &commonv1.MessagingSpec{}
	g.Expect(c.Create(ctx, cp)).To(Succeed(),
		"an empty messaging block must be admitted: the defaulting webhook resolves the XOR")
	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}

	defaulted := &c5c3v1alpha1.ControlPlane{}
	g.Expect(c.Get(ctx, cpKey, defaulted)).To(Succeed(), "re-fetch the defaulted ControlPlane")
	m := defaulted.Spec.Infrastructure.Messaging
	g.Expect(m).NotTo(BeNil(), "the declared block must survive defaulting")
	g.Expect(m.SecretRef).To(BeNil(), "an empty block resolves to managed mode, not brownfield")
	g.Expect(m.ClusterRef).NotTo(BeNil(), "the defaulting webhook must materialize messaging.clusterRef")
	g.Expect(m.ClusterRef.Name).To(Equal(c5c3v1alpha1.DefaultMessagingClusterRefName))
	g.Expect(m.Replicas).To(Equal(int32(3)), "the CRD default supplies the replica count")

	// Dropping the block from a live ControlPlane is rejected: the owned
	// RabbitmqCluster keeps the queues. Get-mutate-update under RetryOnConflict,
	// on the rationale TestIntegration_DedicatedBackingServices_TransitionRejected
	// states: the live controller writes to the CR concurrently, so a stale
	// resourceVersion returns 409 before admission evaluates the removal.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &c5c3v1alpha1.ControlPlane{}
		if err := c.Get(ctx, cpKey, latest); err != nil {
			return err
		}
		latest.Spec.Infrastructure.Messaging = nil
		return c.Update(ctx, latest)
	})
	g.Expect(err).To(HaveOccurred(), "removing a declared messaging block must be rejected")
	g.Expect(err.Error()).To(ContainSubstring("cannot be removed once declared"))
}
