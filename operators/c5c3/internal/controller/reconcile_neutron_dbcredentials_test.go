// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Neutron DB-credentials concern
// (reconcile_neutron_dbcredentials.go): the target the service-agnostic builders
// in reconcile_dbcredentials.go consume, the two OpenBao handles it carries, and
// the per-service databaseCredentialsMode override plus the Static opt-out that
// decide the mode. The concern is pure data — no Neutron sub-reconciler consumes
// it yet, so the reconcile-driven twins of the Placement tests (the projected
// generator objects and their Static teardown) land with that sub-reconciler —
// and the fixture is a bare ControlPlane rather than a fake client.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

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
