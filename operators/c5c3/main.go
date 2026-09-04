// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package main is the entrypoint for the C5C3 operator.
//
// DEVIATION from the original project-setup design:
// Hand-crafted instead of `operator-sdk init` — the SDK scaffolds config/,
// internal/controller/, Dockerfile, and a per-module Makefile that would be
// immediately deleted for this minimal scaffolding phase. The manager setup
// follows kubebuilder v4 / controller-runtime v0.23+ patterns directly.
package main

import (
	"os"
	"strings"

	openbaov1alpha1 "github.com/dc-tec/openbao-operator/api/v1alpha1"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/c5c3/internal/controller"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
	horizonv1alpha1 "github.com/c5c3/cobaltcore/operators/horizon/api/v1alpha1"
	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
	placementv1alpha1 "github.com/c5c3/cobaltcore/operators/placement/api/v1alpha1"

	ctrl "sigs.k8s.io/controller-runtime"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
)

// leaderElectionID is the unique leader-election lock identifier for the c5c3
// operator. KEEP this exact value: it is referenced by the deploy stack RBAC and
// is asserted by main_test.go so a rename cannot silently break leader election.
const leaderElectionID = "c5c3.openstack.c5c3.io"

// barbicanOperatorNamespaceEnv and barbicanOperatorServiceAccountEnv name the
// environment the deploy stack sets to tell the c5c3 operator where the
// barbican-operator runs and which ServiceAccount it runs as. A dedicated
// Barbican secret store grants that identity a TokenRequest RoleBinding and a
// NetworkPolicy ingress peer on the OpenBao instance it provisions.
//
// A blank or unset value is passed to the reconciler as the empty string, which
// resolves to the barbican-operator chart defaults (barbican-system /
// barbican-operator) — the same fallback a programmatically constructed
// reconciler gets, so the default lives in exactly one place.
const (
	barbicanOperatorNamespaceEnv      = "BARBICAN_OPERATOR_NAMESPACE"
	barbicanOperatorServiceAccountEnv = "BARBICAN_OPERATOR_SERVICE_ACCOUNT"
)

var scheme = bootstrap.NewScheme(
	// c5c3 own API types (ControlPlane, CredentialRotation).
	c5c3v1alpha1.AddToScheme,
	// Keystone CR — the ControlPlane reconciler projects and Owns a Keystone child.
	keystonev1alpha1.AddToScheme,
	horizonv1alpha1.AddToScheme,
	// Glance CR (and GlanceBackend) — the ControlPlane reconciler projects and
	// Owns a Glance child plus its GlanceBackend children.
	glancev1alpha1.AddToScheme,
	// Placement CR — the ControlPlane reconciler projects and Owns a Placement
	// child.
	placementv1alpha1.AddToScheme,
	// Barbican CR (and BarbicanSecretStore) — the ControlPlane reconciler
	// projects and Owns a Barbican child plus the secret store attached to it.
	barbicanv1alpha1.AddToScheme,
	// Neutron CR — projected and Owned by the ControlPlane reconciler.
	neutronv1alpha1.AddToScheme,
	// OVNCentral — read and watched by reconcileOVN, never projected: the OVN
	// control plane is deployed outside the plane and only referenced.
	ovnv1alpha1.AddToScheme,
	// OpenBaoCluster and OpenBaoTenant — projected and Owned by the dedicated
	// Barbican secret store.
	openbaov1alpha1.AddToScheme,
	// MariaDB CR — projected and Owned by reconcileInfrastructure.
	mariadbv1alpha1.AddToScheme,
	// ESO PushSecret (v1alpha1) and ClusterSecretStore/ExternalSecret (v1) — the
	// admin-credential push and the K-ORC clouds.yaml gate read/write these.
	esov1alpha1.SchemeBuilder.AddToScheme,
	esov1.SchemeBuilder.AddToScheme,
	// ESO VaultDynamicSecret generator — projected and Owned by
	// reconcileDBCredentials to issue short-lived DB credentials in Dynamic mode.
	esgenv1alpha1.AddToScheme,
	// K-ORC CRs (ApplicationCredential, Service, Endpoint, ...) — minted/Owned by
	// reconcileKORC and reconcileCatalog. This same group registration also
	// covers the Role/RoleAssignment kinds used for the service-account role
	// projection.
	orcv1alpha1.AddToScheme,
	// Memcached (memcached.c5c3.io) is deliberately NOT registered: it ships no Go
	// module, so SetupWithManager's Owns(unstructured) resolves the GVK via the
	// cluster RESTMapper at runtime — no scheme registration is required.
	// +kubebuilder:scaffold:scheme
)

func main() {
	if err := bootstrap.Run(bootstrap.ManagerConfig{
		Scheme:           scheme,
		LeaderElectionID: leaderElectionID,
		// The ControlPlane reconciler resolves the per-service targetClusterRef,
		// so the binary engages the clusters registered in --clusters-namespace.
		// The provider watches that namespace for registration Secrets and
		// engages nothing while it holds none, which is the single-cluster
		// default every existing install keeps.
		TargetClusters: true,
		SetupFunc: func(mcMgr mcmanager.Manager, webhooks bool, maxConcurrentReconciles int) error {
			// The ControlPlane reconciler completes through the multicluster
			// builder, so it takes mcMgr. The local manager is what the
			// CredentialRotation reconciler and the webhook run on: both only
			// ever touch the management cluster.
			mgr := mcMgr.GetLocalManager()
			// Register the operator's Prometheus collectors on the
			// controller-runtime registry before wiring controllers, so a
			// duplicate-registration fails startup cleanly instead of panicking
			// mid-reconcile.
			if err := controller.RegisterMetrics(); err != nil {
				return err
			}
			// +kubebuilder:scaffold:builder — register controllers here
			if err := (&controller.ControlPlaneReconciler{
				Client:                         mgr.GetClient(),
				Scheme:                         mgr.GetScheme(),
				Recorder:                       mgr.GetEventRecorderFor("controlplane-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
				MaxConcurrentReconciles:        maxConcurrentReconciles,
				BarbicanOperatorNamespace:      strings.TrimSpace(os.Getenv(barbicanOperatorNamespaceEnv)),
				BarbicanOperatorServiceAccount: strings.TrimSpace(os.Getenv(barbicanOperatorServiceAccountEnv)),
				Resolver:                       mcMgr,
			}).SetupWithManager(mcMgr); err != nil {
				return err
			}
			if err := (&controller.CredentialRotationReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("credentialrotation-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
			}).SetupWithManager(mgr); err != nil {
				return err
			}
			// The KeystoneService reconciler also runs on the local manager: a
			// registration delivers into its own namespace on the management
			// cluster, so it carries no cluster Resolver.
			if err := (&controller.KeystoneServiceReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				Recorder:                mgr.GetEventRecorderFor("keystoneservice-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
				MaxConcurrentReconciles: maxConcurrentReconciles,
			}).SetupWithManager(mgr); err != nil {
				return err
			}
			if webhooks {
				// DECISION Client must be non-nil for the
				// one-ControlPlane-per-namespace ValidateCreate check. It reads
				// through mgr.GetAPIReader() (direct, uncached) rather than
				// mgr.GetClient(): two concurrent CREATEs must not both see an
				// empty informer cache and both be admitted, and even sequential
				// creates within the cache-sync window would both pass against
				// the cached client.
				if err := (&c5c3v1alpha1.ControlPlaneWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
					return err
				}
				// No Client here: every KeystoneService rule is decidable from the
				// object under admission alone. The cross-object checks are left to
				// the reconciler's collision probes, which fail loudly (see
				// KeystoneServiceWebhook).
				if err := (&c5c3v1alpha1.KeystoneServiceWebhook{}).SetupWebhookWithManager(mgr); err != nil {
					return err
				}
			}
			return nil
		},
	}); err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to run manager")
		os.Exit(1)
	}
}
