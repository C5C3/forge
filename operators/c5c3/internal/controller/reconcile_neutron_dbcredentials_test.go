// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Neutron DB-credentials concern
// (reconcile_neutron_dbcredentials.go): the target the service-agnostic builders
// in reconcile_dbcredentials.go consume, the two OpenBao handles it carries, the
// per-service databaseCredentialsMode override plus the Static opt-out that decide
// the mode, and the objects reconcileNeutron projects from them. The handle tests
// run against a bare ControlPlane; the reconcile-driven ones reuse
// neutronControlPlane / newNeutronTestReconciler from reconcile_neutron_test.go.
package controller

import (
	"context"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// getNeutronVDS fetches the projected Neutron VaultDynamicSecret generator at its
// derived name/namespace.
func getNeutronVDS(t *testing.T, r *ControlPlaneReconciler, cp *c5c3v1alpha1.ControlPlane) (*esgenv1alpha1.VaultDynamicSecret, error) {
	t.Helper()
	vds := &esgenv1alpha1.VaultDynamicSecret{}
	err := r.Get(context.Background(),
		types.NamespacedName{Namespace: cp.NeutronNamespace(), Name: neutronDBCredentialSecretName(cp)}, vds)
	return vds, err
}

// getNeutronDBCredES fetches the projected Neutron DB-credential ExternalSecret at
// its derived name/namespace.
func getNeutronDBCredES(t *testing.T, r *ControlPlaneReconciler, cp *c5c3v1alpha1.ControlPlane) (*esov1.ExternalSecret, error) {
	t.Helper()
	es := &esov1.ExternalSecret{}
	err := r.Get(context.Background(),
		types.NamespacedName{Namespace: cp.NeutronNamespace(), Name: neutronDBCredentialSecretName(cp)}, es)
	return es, err
}

// neutronLeftoverClientCert builds the Neutron mTLS client Certificate at its
// derived name/namespace, as a prior Dynamic deployment left it.
func neutronLeftoverClientCert(cp *c5c3v1alpha1.ControlPlane) *unstructured.Unstructured {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	cert.SetName(neutronDBCredentialClientCertName(cp))
	cert.SetNamespace(cp.NeutronNamespace())
	return cert
}

// neutronDBCredentialsControlPlane builds a ControlPlane on the managed SHARED
// database with a Neutron service block, the shape the Dynamic default applies
// to. The OVN central ref is required on the block; the DB-credential concern
// reads it nowhere.
func neutronDBCredentialsControlPlane() *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "default", Generation: 1},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Region:           "RegionOne",
			Infrastructure: &c5c3v1alpha1.InfrastructureSpec{
				Database: commonv1.DatabaseSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "openstack-db"},
					Database:   "keystone",
					SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db"},
				},
			},
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{},
				Neutron: &c5c3v1alpha1.ServiceNeutronSpec{
					OVN: c5c3v1alpha1.NeutronOVNSpec{
						CentralRef: c5c3v1alpha1.NeutronOVNCentralRef{Name: "ovn"},
					},
				},
			},
		},
	}
}

// TestNeutronDBDynamicKeys_FollowTheNeutronNamespace pins the re-keying of the
// two OpenBao handles onto the NEUTRON service namespace: the engine role name,
// the dynamic creds path, and the static KV path all track it, so neutron's
// engine plumbing is keystone-independent. The role name MUST stay in sync with
// setup-database-tenant.sh. It also pins every field the service-agnostic
// builders read off the target, including the fixed ServiceAccount name the
// neutron-db auth role binds in setup-auth.sh.
func TestNeutronDBDynamicKeys_FollowTheNeutronNamespace(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := neutronDBCredentialsControlPlane() // no namespace assignment; neutron shares "default"
	g.Expect(neutronDBDynamicRoleFor(cp)).To(Equal("neutron-default"),
		"an unassigned Neutron keeps the ControlPlane-namespace-derived role name")
	g.Expect(neutronDBDynamicCredsPathFor(cp)).To(Equal("database/mariadb/creds/neutron-default"))
	g.Expect(neutronDBCredentialRemoteKeyFor(cp)).To(Equal("openstack/neutron/default/cp/db"))

	// The names the neutron-db role binds in setup-auth.sh, and the two
	// per-ControlPlane object names tracking the projected Neutron child.
	g.Expect(neutronDBCredentialServiceAccountName).To(Equal("neutron-db-creds"))
	g.Expect(neutronDBDynamicVaultRole).To(Equal("neutron-db"))
	g.Expect(neutronDBCredentialSecretName(cp)).To(Equal("cp-neutron-db-credentials"))
	g.Expect(neutronDBCredentialClientCertName(cp)).To(Equal("cp-neutron-db-openbao-client"))

	target := neutronDBCredentialTarget(cp)
	g.Expect(target.qualifier).To(Equal("Neutron"))
	g.Expect(target.prefix()).To(Equal("Neutron "), "the target must name Neutron in the messages it emits")
	g.Expect(target.namespace).To(Equal("default"))
	g.Expect(target.secretName).To(Equal(neutronDBCredentialSecretName(cp)))
	g.Expect(target.certName).To(Equal(neutronDBCredentialClientCertName(cp)))
	g.Expect(target.saName).To(Equal(neutronDBCredentialServiceAccountName))
	g.Expect(target.vaultRole).To(Equal(neutronDBDynamicVaultRole))
	g.Expect(target.credsPath).To(Equal("database/mariadb/creds/neutron-default"))
	g.Expect(target.kvPath).To(Equal(neutronDBCredentialRemoteKeyFor(cp)))
	g.Expect(target.storeRef).To(Equal(effectiveControlPlaneStoreRef(cp)))

	// A services.neutron.namespace assignment moves the whole concern with the
	// service: both OpenBao handles re-key onto it and every object is built in
	// the namespace the generator's policy grants.
	cp.Spec.Services.Neutron.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "network", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	g.Expect(neutronDBDynamicRoleFor(cp)).To(Equal("neutron-network"))
	g.Expect(neutronDBDynamicCredsPathFor(cp)).To(Equal("database/mariadb/creds/neutron-network"))
	g.Expect(neutronDBCredentialRemoteKeyFor(cp)).To(Equal("openstack/neutron/network/cp/db"))

	moved := neutronDBCredentialTarget(cp)
	g.Expect(moved.namespace).To(Equal("network"), "every dynamic object lands beside the Neutron child")
	g.Expect(moved.credsPath).To(Equal("database/mariadb/creds/neutron-network"))
	g.Expect(moved.kvPath).To(Equal("openstack/neutron/network/cp/db"))
	// The object names are ControlPlane-derived, so the move leaves them untouched.
	g.Expect(moved.secretName).To(Equal("cp-neutron-db-credentials"))
	g.Expect(moved.certName).To(Equal("cp-neutron-db-openbao-client"))
	g.Expect(moved.saName).To(Equal("neutron-db-creds"),
		"the generator's SA keeps the fixed name the neutron-db role binds in any namespace")
}

// TestNeutronDBCredentialsDynamicEnabled_DedicatedIsStaticEvenWhenModeBypassed
// is the fail-safe twin: the validating webhook rejects a Dynamic override / mode
// on a dedicated neutron database, but a webhook-bypassed CR must still fall
// closed onto Static rather than projecting a generator that could never sync (no
// engine role exists for a dedicated instance). The other two closed branches are
// pinned beside it: an explicit Static override, and a CR whose database does not
// resolve at all.
func TestNeutronDBCredentialsDynamicEnabled_DedicatedIsStaticEvenWhenModeBypassed(t *testing.T) {
	g := NewGomegaWithT(t)

	dedicated := neutronDBCredentialsControlPlane()
	dedicated.Spec.Services.Neutron.DedicatedBackingServices = &c5c3v1alpha1.NeutronDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{
			ClusterRef:      &corev1.LocalObjectReference{Name: "cp-neutron-db"},
			CredentialsMode: commonv1.CredentialsModeDynamic,
			Database:        "neutron",
			SecretRef:       commonv1.SecretRefSpec{Name: "neutron-db"},
		},
	}
	g.Expect(neutronDBCredentialsDynamicEnabled(dedicated)).To(BeFalse(),
		"a dedicated neutron database is never Dynamic, even with the mode written directly into the spec")

	static := neutronDBCredentialsControlPlane()
	static.Spec.Services.Neutron.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
	g.Expect(neutronDBCredentialsDynamicEnabled(static)).To(BeFalse(),
		"a per-service Static override opts Neutron out of the engine-issued credential")

	unresolvable := neutronDBCredentialsControlPlane()
	unresolvable.Spec.Infrastructure = nil
	unresolvable.Spec.Services.Neutron = nil
	g.Expect(neutronDBCredentialsDynamicEnabled(unresolvable)).To(BeFalse(),
		"with no neutron block and no infrastructure nothing resolves, so the predicate falls closed")

	shared := neutronDBCredentialsControlPlane()
	shared.Spec.Infrastructure.Database.CredentialsMode = commonv1.CredentialsModeDynamic
	g.Expect(neutronDBCredentialsDynamicEnabled(shared)).To(BeTrue(),
		"a managed shared database with no per-service override inherits the ControlPlane-wide Dynamic mode")
}

// TestReconcileNeutron_DynamicDefaultProjectsEngineObjects verifies a managed
// shared neutron database (default Dynamic) projects the generator-backed
// ExternalSecret (no static Data), the VaultDynamicSecret with role neutron-db and
// the per-tenant creds path, the neutron-db-creds ServiceAccount, and the mTLS
// client Certificate named <neutron-name>-db-openbao-client.
func TestReconcileNeutron_DynamicDefaultProjectsEngineObjects(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	r := newNeutronTestReconciler(t, cp)
	ctx := context.Background()

	g.Expect(neutronDBCredentialsDynamicEnabled(cp)).To(BeTrue(),
		"a managed shared neutron database defaults to Dynamic")

	_, err := r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	// ExternalSecret: generator-backed, no static KV Data, no SecretStoreRef.
	es, err := getNeutronDBCredES(t, r, cp)
	g.Expect(err).NotTo(HaveOccurred(), "operator must create the Neutron DB-credential ExternalSecret")
	g.Expect(es.Spec.Data).To(BeEmpty(), "the Dynamic ExternalSecret must carry no static Data refs")
	g.Expect(es.Spec.SecretStoreRef.Name).To(BeEmpty(),
		"a generator-backed ExternalSecret must not reference a SecretStore")
	g.Expect(es.Spec.DataFrom).To(HaveLen(1))
	g.Expect(es.Spec.DataFrom[0].SourceRef).NotTo(BeNil())
	g.Expect(es.Spec.DataFrom[0].SourceRef.GeneratorRef).NotTo(BeNil())
	g.Expect(es.Spec.DataFrom[0].SourceRef.GeneratorRef.Kind).To(Equal("VaultDynamicSecret"))
	g.Expect(es.Spec.DataFrom[0].SourceRef.GeneratorRef.Name).To(Equal(neutronDBCredentialSecretName(cp)))

	// VaultDynamicSecret: role neutron-db, per-tenant creds path, same-namespace refs.
	vds, err := getNeutronVDS(t, r, cp)
	g.Expect(err).NotTo(HaveOccurred(), "operator must create the Neutron VaultDynamicSecret generator")
	g.Expect(vds.Spec.Path).To(Equal("database/mariadb/creds/neutron-default"))
	g.Expect(vds.Spec.Method).To(Equal("GET"))
	g.Expect(vds.Spec.Provider).NotTo(BeNil())
	g.Expect(vds.Spec.Provider.Auth.Kubernetes.Role).To(Equal(neutronDBDynamicVaultRole))
	g.Expect(vds.Spec.Provider.Auth.Kubernetes.Role).To(Equal("neutron-db"))
	g.Expect(vds.Spec.Provider.Auth.Kubernetes.ServiceAccountRef.Name).To(Equal(neutronDBCredentialServiceAccountName))
	g.Expect(vds.Spec.Provider.CAProvider.Name).To(Equal(neutronDBCredentialClientCertName(cp)))
	g.Expect(vds.Spec.Provider.ClientTLS.CertSecretRef.Name).To(Equal(neutronDBCredentialClientCertName(cp)))
	g.Expect(vds.Spec.Provider.ClientTLS.KeySecretRef.Name).To(Equal(neutronDBCredentialClientCertName(cp)))

	// ServiceAccount neutron-db-creds, the name the neutron-db auth role binds.
	g.Expect(neutronDBCredentialServiceAccountName).To(Equal("neutron-db-creds"))
	sa := &corev1.ServiceAccount{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: cp.NeutronNamespace(), Name: neutronDBCredentialServiceAccountName,
	}, sa)).To(Succeed())

	// Certificate <neutron-name>-db-openbao-client with the CA issuer.
	g.Expect(neutronDBCredentialClientCertName(cp)).To(Equal("cp-neutron-db-openbao-client"))
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: cp.NeutronNamespace(), Name: neutronDBCredentialClientCertName(cp),
	}, cert)).To(Succeed())
	issuer, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "name")
	g.Expect(issuer).To(Equal(openBaoCAIssuerName))

	// The projected child carries Dynamic.
	nn := getProjectedNeutron(t, r.Client, cp)
	g.Expect(nn.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeDynamic))
}

// TestReconcileNeutron_StaticOptOutProjectsKVAndTearsDownDynamic verifies that
// both opt-out routes, the shared credentialsMode: Static and the per-service
// services.neutron.databaseCredentialsMode: Static, project the KV-backed
// ExternalSecret, tear down any leftover generator objects, and stamp the child
// Static.
func TestReconcileNeutron_StaticOptOutProjectsKVAndTearsDownDynamic(t *testing.T) {
	for _, tt := range []struct {
		name  string
		apply func(cp *c5c3v1alpha1.ControlPlane)
	}{
		{
			name: "shared credentialsMode Static",
			apply: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Infrastructure.Database.CredentialsMode = commonv1.CredentialsModeStatic
			},
		},
		{
			name: "per-service databaseCredentialsMode Static",
			apply: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Neutron.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := neutronControlPlane()
			tt.apply(cp)
			g.Expect(neutronDBCredentialsDynamicEnabled(cp)).To(BeFalse())

			// Pre-seed the leftover generator, SA and mTLS client Certificate from a
			// prior Dynamic deployment, each carrying the ownership a live projection
			// stamps: the teardown is gated on it.
			s := neutronTestScheme(t)
			target := neutronDBCredentialTarget(cp)
			leftovers := []client.Object{
				dbCredentialVaultDynamicSecret(target, openBaoDefaultServer, openBaoDefaultKubernetesMount),
				dbCredentialServiceAccount(target),
				neutronLeftoverClientCert(cp),
			}
			for _, obj := range leftovers {
				g.Expect(claimChildOwnership(localWriter(), cp, obj, s)).To(Succeed())
			}
			r := newNeutronTestReconciler(t, append([]client.Object{cp}, leftovers...)...)
			ctx := context.Background()

			_, err := r.reconcileNeutron(ctx, cp)
			g.Expect(err).NotTo(HaveOccurred())

			es, err := getNeutronDBCredES(t, r, cp)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(es.Spec.DataFrom).To(BeEmpty(), "the Static opt-out must project the KV ExternalSecret")
			g.Expect(es.Spec.Data).To(HaveLen(2))
			g.Expect(es.Spec.Data[0].RemoteRef.Key).To(Equal(neutronDBCredentialRemoteKeyFor(cp)))

			_, vdsErr := getNeutronVDS(t, r, cp)
			g.Expect(apierrors.IsNotFound(vdsErr)).To(BeTrue(),
				"the Static opt-out must delete the leftover VaultDynamicSecret")

			saErr := r.Get(ctx, types.NamespacedName{
				Namespace: cp.NeutronNamespace(), Name: neutronDBCredentialServiceAccountName,
			}, &corev1.ServiceAccount{})
			g.Expect(apierrors.IsNotFound(saErr)).To(BeTrue(),
				"the Static opt-out must delete the generator's ServiceAccount")

			sweptCert := &unstructured.Unstructured{}
			sweptCert.SetGroupVersionKind(certificateGVK)
			certErr := r.Get(ctx, types.NamespacedName{
				Namespace: cp.NeutronNamespace(), Name: neutronDBCredentialClientCertName(cp),
			}, sweptCert)
			g.Expect(apierrors.IsNotFound(certErr)).To(BeTrue(),
				"the Static opt-out must delete the leftover mTLS client Certificate")

			nn := getProjectedNeutron(t, r.Client, cp)
			g.Expect(nn.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeStatic))
		})
	}
}

// TestReconcileNeutron_DynamicObjectsLandInTheNeutronNamespace verifies every
// dynamic object lands beside the Neutron child in a namespace of its own: the
// ServiceAccount whose token OpenBao authenticates, the mTLS client Certificate,
// the generator, and the ExternalSecret, carrying the ownership labels rather than
// an owner reference, and nothing is left in the ControlPlane's namespace.
func TestReconcileNeutron_DynamicObjectsLandInTheNeutronNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronControlPlane()
	cp.Spec.Services.Neutron.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "network", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	r := newNeutronTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileNeutron(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	es := &esov1.ExternalSecret{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: "network", Name: neutronDBCredentialSecretName(cp),
	}, es)).To(Succeed())
	g.Expect(es.OwnerReferences).To(BeEmpty(), "a cross-namespace object cannot carry an owner reference")
	g.Expect(es.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))

	vds := &esgenv1alpha1.VaultDynamicSecret{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: "network", Name: neutronDBCredentialSecretName(cp),
	}, vds)).To(Succeed())
	g.Expect(vds.Spec.Path).To(Equal("database/mariadb/creds/neutron-network"),
		"the generator's per-tenant path follows the neutron namespace")

	sa := &corev1.ServiceAccount{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: "network", Name: neutronDBCredentialServiceAccountName,
	}, sa)).To(Succeed(), "the generator's SA must authenticate from the namespace the policy grants")
	g.Expect(sa.Name).To(Equal("neutron-db-creds"))

	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: "network", Name: neutronDBCredentialClientCertName(cp),
	}, cert)).To(Succeed())

	// Nothing may be left in the ControlPlane's own namespace.
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: "default", Name: neutronDBCredentialSecretName(cp),
	}, &esov1.ExternalSecret{})).NotTo(Succeed())
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: "default", Name: neutronDBCredentialSecretName(cp),
	}, &esgenv1alpha1.VaultDynamicSecret{})).NotTo(Succeed())
}
