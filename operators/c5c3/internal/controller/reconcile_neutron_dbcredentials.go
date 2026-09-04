// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// The Neutron DB-credential concern mirrors the Keystone one in
// reconcile_dbcredentials.go, neutron-scoped and keystone-independent: Dynamic
// (engine-issued) is the default on a managed SHARED database, and every dynamic
// object — the ServiceAccount, the mTLS client Certificate, the VaultDynamicSecret
// generator, and the generator-backed ExternalSecret — lands in the NEUTRON
// service namespace, beside the database it issues against and the child that
// consumes it.
//
// Only the diverging inputs live here: the names, the OpenBao role and paths, and
// the mode predicate. The object builders and the ensure/delete pair are the
// service-agnostic ones in reconcile_dbcredentials.go, driven by the
// dbCredentialTarget this file constructs.

const (
	// neutronDBDynamicVaultRole is the OpenBao Kubernetes-auth role the Neutron
	// generator authenticates against (see deploy/openbao/bootstrap/setup-auth.sh);
	// it is bound to the neutron-db-dynamic policy scoping reads to the per-tenant
	// creds path.
	neutronDBDynamicVaultRole = "neutron-db"
	// neutronDBCredentialServiceAccountName is the fixed name of the
	// per-ControlPlane ServiceAccount whose token the Neutron VaultDynamicSecret
	// generator presents to OpenBao. It is the name the neutron-db role binds
	// (bound_service_account_names, setup-auth.sh), so the two MUST STAY IN SYNC. A
	// fixed name is safe because a namespace belongs to at most one ControlPlane:
	// the one-ControlPlane-per-namespace webhook guarantees it for the
	// ControlPlane's own namespace, and the namespace-claim webhook guarantees it
	// for every service namespace.
	neutronDBCredentialServiceAccountName = "neutron-db-creds" //nolint:gosec // G101 false positive: ServiceAccount name, not a credential.
)

// neutronDBCredentialSecretName returns the deterministic name of the
// per-ControlPlane Neutron DB-credential Secret/ExternalSecret. It tracks the
// projected Neutron CR, mirroring dbCredentialSecretName's derivation from the
// Keystone child.
func neutronDBCredentialSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return neutronName(cp) + dbCredentialSecretNameSuffix
}

// neutronDBCredentialClientCertName returns the name of the per-ControlPlane
// cert-manager Certificate and the Secret it materialises (client mTLS keypair
// plus the CA under ca.crt) for the Neutron generator, mirroring
// dbCredentialClientCertName.
func neutronDBCredentialClientCertName(cp *c5c3v1alpha1.ControlPlane) string {
	return neutronName(cp) + dbCredentialClientCertSuffix
}

// neutronDBCredentialRemoteKeyFor returns the per-ControlPlane, namespace-scoped
// OpenBao KV path the STATIC Neutron DB credential is read from (keys username,
// password). It is retained only for the Static opt-out / brownfield-migration
// path; the default managed mode reads engine-issued credentials from
// neutronDBDynamicCredsPathFor instead. The eso-tenant.hcl policy already grants
// this path shape; nothing seeds it, so a ControlPlane on the Static branch must
// have the path seeded out-of-band before ESO can sync the credential.
func neutronDBCredentialRemoteKeyFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "openstack/neutron/" + cp.NeutronNamespace() + "/" + cp.Name + "/db"
}

// neutronDBDynamicRoleFor returns the per-tenant OpenBao database-engine role
// name for this ControlPlane's Neutron service. It is keyed on the NEUTRON
// SERVICE NAMESPACE alone — the namespace the database and the generator that
// reads from it actually live in — so it is collision-free (namespaces are
// cluster-unique) and the neutron-db-dynamic policy can scope reads by the
// caller's service_account_namespace with an EXACT match. It MUST stay in sync
// with the role-name derivation in
// deploy/openbao/bootstrap/setup-database-tenant.sh: the operator reads
// credentials from the engine role that script provisions per service, which for
// Neutron is neutron-<neutron-ns>. Both OpenBao halves are pre-wired — the
// neutron-db auth role and the neutron-db-dynamic policy cluster-wide, the engine
// pair per ControlPlane onboarding.
func neutronDBDynamicRoleFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "neutron-" + cp.NeutronNamespace()
}

// neutronDBDynamicCredsPathFor returns the OpenBao path the Neutron
// VaultDynamicSecret generator reads short-lived credentials from
// (database/mariadb/creds/<role>).
func neutronDBDynamicCredsPathFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "database/mariadb/creds/" + neutronDBDynamicRoleFor(cp)
}

// neutronDBCredentialTarget describes the Neutron DB-credential concern for the
// service-agnostic builders and the ensure/delete pair in
// reconcile_dbcredentials.go.
func neutronDBCredentialTarget(cp *c5c3v1alpha1.ControlPlane) dbCredentialTarget {
	return dbCredentialTarget{
		qualifier:  "Neutron",
		namespace:  cp.NeutronNamespace(),
		secretName: neutronDBCredentialSecretName(cp),
		certName:   neutronDBCredentialClientCertName(cp),
		saName:     neutronDBCredentialServiceAccountName,
		vaultRole:  neutronDBDynamicVaultRole,
		credsPath:  neutronDBDynamicCredsPathFor(cp),
		kvPath:     neutronDBCredentialRemoteKeyFor(cp),
		storeRef:   effectiveControlPlaneStoreRef(cp),
	}
}

// neutronDBCredentialsDynamicEnabled reports the effective credentials mode of
// the database Neutron actually connects to: Dynamic (engine-issued) is the
// default for a managed SHARED database; a ControlPlane opts out by setting
// credentialsMode: Static (migration staging / brownfield). A non-empty
// per-service services.neutron.databaseCredentialsMode override wins over the
// shared credentialsMode, so a staged migration can run Neutron on one mode while
// another service stays on the other.
//
// A DEDICATED neutron database is never Dynamic. The OpenBao database engine
// carries one connection and one role per NAMESPACE bootstrapped against the
// SHARED cluster, so no engine role exists that could issue credentials for a
// dedicated instance — it takes the Static branch. Neutron is never External-mode
// (services.neutron is forbidden in External ControlPlanes), so no External
// short-circuit is needed here; keying the decision on the dedicated declaration
// rather than only on the stored mode keeps a webhook-bypassed CR failing closed
// onto Static rather than projecting a generator that could never sync.
func neutronDBCredentialsDynamicEnabled(cp *c5c3v1alpha1.ControlPlane) bool {
	var override string
	if nn := cp.Spec.Services.Neutron; nn != nil {
		override = nn.DatabaseCredentialsMode
	}
	return dbCredentialModeIsDynamic(cp.DedicatedNeutronDatabase(), effectiveNeutronDatabase(cp), override)
}
