// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller implements the ControlPlane reconciler.
package controller

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
	horizonv1alpha1 "github.com/c5c3/cobaltcore/operators/horizon/api/v1alpha1"
	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
	placementv1alpha1 "github.com/c5c3/cobaltcore/operators/placement/api/v1alpha1"
	openbaov1alpha1 "github.com/dc-tec/openbao-operator/api/v1alpha1"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// ControlPlaneSecretNameIndexKey is the field-indexer key under which ControlPlane
// CRs are indexed by the union of their referenced Secret names. Today that is the
// single EFFECTIVE admin-password Secret name in managed mode
// (Database.ClusterRef != nil) the operator-owned per-ControlPlane Secret
// adminPasswordSecretName(cp), and in brownfield mode the user-supplied
// spec.korc.adminCredential.passwordSecretRef.name. SetupWithManager registers the
// indexer and secretToControlPlaneMapper uses it for an O(1) reverse lookup from a
// Secret event to the referencing ControlPlane(s), mirroring the keystone operator's
// KeystoneSecretNameIndexKey. The constant's string value remains
// the spec passwordSecretRef field path because it is only an index-key identifier.
// #nosec G101 -- field-indexer key (a JSONPath-like field selector), not a credential.
const ControlPlaneSecretNameIndexKey = "spec.korc.adminCredential.passwordSecretRef.name"

// Condition types set by the ControlPlane controller. These constants are the
// single source of truth for the status contract: call sites (sub-reconcilers,
// setReadyCondition, the instrumentation map) MUST reference these constants
// rather than inline string literals so a rename is caught by the compiler and
// the no-inline-literals drift guard.
const (
	conditionTypeNamespacesReady     = "NamespacesReady"
	conditionTypeInfrastructureReady = "InfrastructureReady"
	conditionTypeESOTenantStoreReady = "ESOTenantStoreReady" //nolint:gosec // G101 false positive: condition type name, not a credential.
	conditionTypeDBCredentialsReady  = "DBCredentialsReady"  //nolint:gosec // G101 false positive: condition type name, not a credential.
	conditionTypeKeystoneReady       = "KeystoneReady"
	conditionTypeHorizonReady        = "HorizonReady"
	conditionTypeGlanceReady         = "GlanceReady"
	conditionTypePlacementReady      = "PlacementReady"
	conditionTypeBarbicanReady       = "BarbicanReady"
	// conditionTypeOVNReady mirrors the readiness of the OVNCentral named by
	// services.neutron.ovn.centralRef. The ControlPlane owns nothing of the OVN
	// layer: the central is deployed outside the plane and only referenced, so
	// this condition reports what the plane observes, never what it drives.
	conditionTypeOVNReady = "OVNReady"
	// conditionTypeNeutronReady covers the network service the ControlPlane does
	// drive: the projected Neutron child and the material it consumes, the shared
	// bus delivered into the Neutron namespace among it. It is separate from
	// conditionTypeOVNReady, which reports a central this plane only reads.
	conditionTypeNeutronReady         = "NeutronReady"
	conditionTypeKORCReady            = "KORCReady"
	conditionTypeAdminCredentialReady = "AdminCredentialReady" //nolint:gosec // G101 false positive: condition type name, not a credential.
	conditionTypeAdminPasswordReady   = "AdminPasswordReady"   //nolint:gosec // G101 false positive: condition type name, not a credential.
	conditionTypeCatalogReady         = "CatalogReady"
	conditionTypeServiceAccountsReady = "ServiceAccountsReady" //nolint:gosec // G101 false positive: condition type name, not a credential.
	// conditionTypeRegistrationTenantStoresReady covers the per-tenant stores
	// provisioned in the ALLOWLISTED namespaces standalone KeystoneService CRs
	// register from, which conditionTypeESOTenantStoreReady deliberately does not:
	// that one gates the blocking prefix, and a namespace the control plane does not
	// occupy must never park the plane's own credential material behind it.
	conditionTypeRegistrationTenantStoresReady = "RegistrationTenantStoresReady" //nolint:gosec // G101 false positive: condition type name, not a credential.
	conditionTypeReady                         = "Ready"
)

// controlPlaneORCFinalizer blocks the ControlPlane CR from leaving etcd until
// the operator has torn down the K-ORC CRs it owns
// (ApplicationCredential/Service/Endpoint/User/Domain). Those CRs carry K-ORC
// finalizers that revoke/delete against the Keystone API; holding the
// ControlPlane CR in etcd defers the owner-reference GC cascade that would
// otherwise tear Keystone (and its MariaDB) down concurrently, keeping Keystone
// reachable so K-ORC can finish. Defined once as the single source of truth for
// Reconcile, reconcileDelete, tests, and docs.
const controlPlaneORCFinalizer = "c5c3.io/orc-teardown"

// subConditionTypes lists the condition types set by individual sub-reconcilers.
// The Ready condition is True only when all of these are True.
var subConditionTypes = []string{
	conditionTypeNamespacesReady,
	conditionTypeInfrastructureReady,
	conditionTypeESOTenantStoreReady,
	conditionTypeDBCredentialsReady,
	conditionTypeKeystoneReady,
	conditionTypeHorizonReady,
	conditionTypeGlanceReady,
	conditionTypePlacementReady,
	conditionTypeBarbicanReady,
	conditionTypeOVNReady,
	conditionTypeNeutronReady,
	conditionTypeKORCReady,
	conditionTypeAdminCredentialReady,
	conditionTypeAdminPasswordReady,
	conditionTypeCatalogReady,
	conditionTypeServiceAccountsReady,
	conditionTypeRegistrationTenantStoresReady,
}

// ControlPlaneReconciler reconciles a ControlPlane object.
type ControlPlaneReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// APIReader is the manager's DIRECT, uncached reader (mgr.GetAPIReader(),
	// wired by SetupWithManager). It exists for the get-by-exact-name reads on
	// kinds the operator manages a handful of objects of but never watches — the
	// three RBAC kinds behind a dedicated Barbican secret store (Role,
	// RoleBinding, and the cluster-scoped ClusterRoleBinding) among them.
	// Reading those through the cached client would have controller-runtime
	// start an unfiltered CLUSTER-WIDE informer for each of them: every Role and
	// RoleBinding in the cluster held in memory, in an operator the chart caps
	// at 128Mi, to track at most three objects per ControlPlane whose names the
	// reconciler already derives. A nil value falls back to Client, so a
	// programmatically constructed reconciler (unit tests, envtest fixtures)
	// keeps working unchanged.
	APIReader client.Reader

	// Resolver resolves the target cluster a ControlPlane names in a service's
	// targetClusterRef into the client that service's children are read and
	// written with. Nil means always-local: every service keeps its children on
	// the management cluster, which is what single-cluster tests and deployments
	// want.
	Resolver commonmulticluster.ClusterResolver

	// MaxConcurrentReconciles bounds how many ControlPlane CRs reconcile
	// concurrently. It is threaded from the --max-concurrent-reconciles flag
	// (see internal/common/bootstrap) and applied to the controller's
	// controller.Options in SetupWithManager. A value <= 0 falls back to
	// bootstrap.DefaultMaxConcurrentReconciles inside
	// bootstrap.ControllerOptions, so the zero value is safe for
	// programmatically constructed reconcilers.
	MaxConcurrentReconciles int

	// BarbicanOperatorNamespace and BarbicanOperatorServiceAccount identify the
	// barbican-operator's own Pod identity. The dedicated OpenBao instance grants
	// it two things a ControlPlane-derived name cannot supply: the TokenRequest
	// RoleBinding subject, and the NetworkPolicy peer that admits its pods to the
	// instance's API port. Both are threaded from the BARBICAN_OPERATOR_NAMESPACE
	// and BARBICAN_OPERATOR_SERVICE_ACCOUNT environment (see main.go); an empty
	// value falls back to the barbican-operator chart defaults, so the zero value
	// is safe for programmatically constructed reconcilers.
	BarbicanOperatorNamespace      string
	BarbicanOperatorServiceAccount string
}

// apiReader returns the uncached reader described on APIReader, falling back to
// the cached client when none was injected.
func (r *ControlPlaneReconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// controlPlaneRemoteChildKinds are the kinds the ControlPlane projects into a
// service namespace on a target cluster, and the kinds teardownDedicatedNamespaces
// sweeps by ownership label when the CR is deleted. Neither an owner reference nor
// a garbage collection cascade crosses a cluster boundary, so a kind missing from
// this list is a kind that keeps running on the target after the ControlPlane is
// gone.
//
// The list is cross-checked against the create verbs of the kubebuilder RBAC
// markers below: the operator can only leave behind what it is allowed to create.
// Three groups of marker are deliberately not in it:
//
//   - The service CRs (Keystone, Horizon, Glance, GlanceBackend, Placement,
//     Barbican, BarbicanSecretStore) and the K-ORC kinds. Both stay on the
//     management cluster whatever a service names — a placed service's CR carries
//     the ref and its own operator projects onto the target, and the K-ORC CRs live
//     in the ControlPlane's own namespace — so deleteServiceChildrenIn and
//     deleteORCResources delete them there, by name.
//   - The cluster-scoped auth-delegator ClusterRoleBinding behind a dedicated
//     OpenBao instance. The sweep lists namespaced objects in one namespace, so it
//     can never see a cluster-scoped one; deleteBarbicanAuthDelegatorBinding
//     deletes it by name, through that same cluster's client.
//   - Namespace, cluster-scoped as well and deleted by name under the Managed
//     lifecycle alone (deleteManagedNamespace).
//
// Role and RoleBinding are in the list although the markers below grant them no
// list verb, and that is not an oversight: the sweep runs exclusively through the
// credentials of the registered target cluster's kubeconfig, never through the
// management cluster's ClusterRole. The markers describe what this operator may do
// at home, where those two kinds are read and written by exact name only.
//
// The order carries no ESO sequencing. A PushSecret purges its OpenBao path
// through the tenant SecretStore in its own namespace, but every owned PushSecret
// is deleted — and waited for — by deleteOwnedPushSecrets before this sweep runs,
// so no PushSecret is left for the SecretStore entry to outrun.
var controlPlaneRemoteChildKinds = []schema.GroupVersionKind{
	mariadbv1alpha1.GroupVersion.WithKind("MariaDB"),
	memcachedGVK,
	esov1.SchemeGroupVersion.WithKind("SecretStore"),
	certificateGVK,
	corev1.SchemeGroupVersion.WithKind("ServiceAccount"),
	rbacv1.SchemeGroupVersion.WithKind("Role"),
	rbacv1.SchemeGroupVersion.WithKind("RoleBinding"),
	corev1.SchemeGroupVersion.WithKind("Secret"),
	esov1.SchemeGroupVersion.WithKind("ExternalSecret"),
	esov1alpha1.SchemeGroupVersion.WithKind("PushSecret"),
	esgenv1alpha1.SchemeGroupVersion.WithKind("VaultDynamicSecret"),
	openbaov1alpha1.GroupVersion.WithKind("OpenBaoTenant"),
	openbaov1alpha1.GroupVersion.WithKind("OpenBaoCluster"),
}

// +kubebuilder:rbac:groups=c5c3.io,resources=controlplanes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=c5c3.io,resources=controlplanes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=controlplanes/finalizers,verbs=update
// +kubebuilder:rbac:groups=c5c3.io,resources=credentialrotations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=c5c3.io,resources=credentialrotations/status,verbs=get;update;patch
// The ControlPlane reconciler only observes SecretAggregate CRs; it never
// creates or mutates them, so the rule is intentionally read-only.
// +kubebuilder:rbac:groups=c5c3.io,resources=secretaggregates,verbs=get;list;watch
// The ControlPlane projects one KeystoneService per built-in service it manages,
// resets a spec field another field manager wrote on one
// (reclaimBuiltinRegistrationFields, an ordinary Update because field ownership
// constrains apply requests only), and sweeps it on the deletion opt-in. Every
// verb it writes with is named HERE rather than borrowed from the union
// controller-gen builds with the KeystoneService controller's own block: narrowing
// that block would otherwise drop one silently, and the reclaim would 403 on every
// pass while the tampered catalog row stays published in Keystone.
// The KeystoneService reconciler reads the CRs and updates them to install and
// release its teardown finalizer. The create/update/patch/delete verbs belong
// to the ControlPlane reconciler, which projects one registration per built-in
// service, resets foreign spec fields on one, and sweeps it on the deletion
// opt-in.
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneservices,verbs=create;update;patch;delete
// Projected and Owned by reconcileInfrastructure.
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=mariadbs,verbs=get;list;watch;create;update;patch;delete
// Projected and Owned by reconcileInfrastructure (resolved via the cluster
// RESTMapper at runtime; no Go scheme registration required).
// +kubebuilder:rbac:groups=memcached.c5c3.io,resources=memcacheds,verbs=get;list;watch;create;update;patch;delete
// Projected and Owned by reconcileInfrastructure for spec.infrastructure.messaging
// (resolved via the cluster RESTMapper at runtime; no Go scheme registration
// required). The watch is registered only when the CRD is served.
// +kubebuilder:rbac:groups=rabbitmq.com,resources=rabbitmqclusters,verbs=get;list;watch;create;update;patch;delete
// The ControlPlane reconciler projects and Owns a Keystone child.
// +kubebuilder:rbac:groups=keystone.openstack.c5c3.io,resources=keystones,verbs=get;list;watch;create;update;patch;delete
// READ-ONLY: the ControlPlane reconciler watches the federation/domain backends
// attached to its Keystone child to project the Horizon websso choices and the
// Keystone trusted_dashboard. The backends themselves are authored by the
// operator and reconciled by the keystone-operator, never written here.
// +kubebuilder:rbac:groups=keystone.openstack.c5c3.io,resources=keystoneidentitybackends,verbs=get;list;watch
// The ControlPlane reconciler projects and Owns a Horizon child.
// +kubebuilder:rbac:groups=horizon.openstack.c5c3.io,resources=horizons,verbs=get;list;watch;create;update;patch;delete
// glancebackends, glances:
// The ControlPlane reconciler projects and Owns a Glance child plus one
// GlanceBackend child per services.glance.backends entry. Both are
// operator-written children, so both get full verbs.
// +kubebuilder:rbac:groups=glance.openstack.c5c3.io,resources=glances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=glance.openstack.c5c3.io,resources=glancebackends,verbs=get;list;watch;create;update;patch;delete
// The ControlPlane reconciler projects and Owns a Placement child. Placement has
// no satellite kind, so placements is the single operator-written child kind.
// +kubebuilder:rbac:groups=placement.openstack.c5c3.io,resources=placements,verbs=get;list;watch;create;update;patch;delete
// barbicans, barbicansecretstores:
// The ControlPlane reconciler projects and Owns a Barbican child plus the
// BarbicanSecretStore that points it at its secret backend. Both are
// operator-written children, so both get full verbs.
// +kubebuilder:rbac:groups=barbican.openstack.c5c3.io,resources=barbicans,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=barbican.openstack.c5c3.io,resources=barbicansecretstores,verbs=get;list;watch;create;update;patch;delete
// The ControlPlane reconciler projects and Owns a Neutron child, the network
// service the OVN control plane below carries the logical model for.
// +kubebuilder:rbac:groups=neutron.openstack.c5c3.io,resources=neutrons,verbs=get;list;watch;create;update;patch;delete
// The OVNCentral is deployed outside the plane and only REFERENCED by
// services.neutron.ovn.centralRef, so the reconciler reads and watches it but
// never writes it: read-only verbs.
// +kubebuilder:rbac:groups=ovn.openstack.c5c3.io,resources=ovncentrals,verbs=get;list;watch
// A managed Barbican secret store gets a dedicated OpenBao instance: the
// OpenBaoCluster the store reads and writes through, and the OpenBaoTenant that
// admits the Barbican service namespace to it.
// +kubebuilder:rbac:groups=openbao.org,resources=openbaoclusters;openbaotenants,verbs=get;list;watch;create;update;patch;delete
// rolebindings, roles:
// The dedicated OpenBao instance comes with its own RBAC: the Role/RoleBinding
// pair that lets the barbican-operator mint a bound token for the instance's
// provisioner ServiceAccount.
//
// No list or watch: the reconciler reads these by exact name through the
// manager's UNCACHED API reader (ControlPlaneReconciler.APIReader), so
// controller-runtime never starts a cluster-wide Role/RoleBinding informer for
// the two objects a ControlPlane owns. No update either: every write is a
// Server-Side Apply (patch), and the teardown deletes.
// clusterrolebindings:
// The one binding that lets the instance pods issue the TokenReview every
// Kubernetes-auth login is validated with. Same verb set and the same uncached
// read as the namespaced pair above.
//
// ACCEPTED RISK: delete on a cluster-scoped binding is not covered by RBAC
// escalation prevention, and the object names are derived per ControlPlane, so
// resourceNames cannot bound them. A compromised operator identity can therefore
// delete any ClusterRoleBinding in the cluster — an authorization OUTAGE, not an
// escalation. The code path never can: deleteBarbicanAuthDelegatorBinding and
// deleteBarbicanEnsembleIn both re-check isControlPlaneChild against the live
// object before deleting. Removing the grant entirely needs the binding to move
// to the openbao-operator, which already creates the ServiceAccount it binds.
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings;clusterrolebindings,verbs=get;create;patch;delete
// Kubernetes refuses a binding to a ClusterRole whose permissions the author
// does not hold. bind on that single name is the narrow exception that lets the
// reconciler grant the OpenBao instance its TokenReview without holding
// TokenReview itself; no other ClusterRole is bindable.
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,resourceNames="system:auth-delegator",verbs=bind
// domains, projects, roles, roleassignments. Minted/owned by reconcileKORC and
// reconcileCatalog; users + domains are imported (unmanaged) so the admin
// ApplicationCredential's UserRef resolves (ensureKORCAdminImports); users +
// projects are also managed/owned by the KeystoneService registration projection
// (registration_projection.go). Roles are imported and RoleAssignments minted for
// the registrations' role projection.
// +kubebuilder:rbac:groups=openstack.k-orc.cloud,resources=applicationcredentials;services;endpoints;users;domains;projects;roles;roleassignments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets;pushsecrets,verbs=get;list;watch;create;update;patch;delete
// Required so the operator can observe the shared cluster store's Ready condition
// and reflect upstream secret-backend outages. A ControlPlane that sets an
// explicit cluster-scoped spec.secretStoreRef reaches OpenBao through it.
// +kubebuilder:rbac:groups=external-secrets.io,resources=clustersecretstores,verbs=get;list;watch
// The operator PROVISIONS the per-tenant namespaced SecretStore (openbao-tenant-store)
// it defaults every ControlPlane onto (reconcileESOTenantStore), and observes its
// Ready condition, so it needs the write verbs in addition to the read verbs.
// +kubebuilder:rbac:groups=external-secrets.io,resources=secretstores,verbs=get;list;watch;create;update;patch;delete
// Required so reconcileDBCredentials can project the per-ControlPlane
// VaultDynamicSecret generator that issues short-lived DB credentials in
// Dynamic credentials mode.
// +kubebuilder:rbac:groups=generators.external-secrets.io,resources=vaultdynamicsecrets,verbs=get;list;watch;create;update;patch;delete
// Required so reconcileDBCredentials can project the per-ControlPlane mTLS client
// Certificate the VaultDynamicSecret generator presents to the OpenBao listener.
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// Required so reconcileDBCredentials can project (and clean up on a Static flip)
// the per-ControlPlane ServiceAccount whose token the VaultDynamicSecret
// generator presents to OpenBao. delete is used by the Dynamic->Static teardown.
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// Required so the reconciler may author the TokenRequest Role that lets the
// barbican-operator mint a bound token for a dedicated OpenBao instance:
// Kubernetes only lets an author grant permissions it holds itself.
//
// ACCEPTED RISK, and why it cannot be narrowed. The Role the reconciler writes
// IS resourceNames-scoped to the one "<instance>-provisioner" account (see
// barbicanOpenBaoTokenRole) — the scoping the barbican-operator chart refuses to
// render without. But the escalation-prevention check covers a requested rule
// only from a granted rule whose resourceNames are absent or contain the exact
// name, and the instance name is derived from the ControlPlane, so no static
// resourceNames list can cover every ControlPlane a cluster will ever hold. The
// grant is therefore unrestricted, and anyone reaching this operator's identity
// can mint a bearer token for any ServiceAccount in the cluster.
//
// Two alternatives were rejected. Dropping resourceNames from the projected Role
// and binding a shipped ClusterRole with `bind` moves the verb off this identity,
// but hands the barbican-operator TokenRequest for EVERY account in the Barbican
// namespace — including eso-tenant-auth and the <service>-db-creds accounts that
// read tenant secrets out of OpenBao — which the barbican-operator chart, its
// values schema, and hack/ci-deploy-operator.sh all refuse by design. And
// `escalate` on roles is a strictly wider primitive than the verb it would
// replace. It does not widen what this identity already reaches either: the
// cluster-wide secrets rule above lets it create a legacy
// kubernetes.io/service-account-token Secret for any account and read the token
// out of it. Removing it for real needs the grant to be authored by the
// openbao-operator, which owns the instance the account belongs to.
// +kubebuilder:rbac:groups=core,resources=serviceaccounts/token,verbs=create
// Read-only on the single well-known EndpointSlice default/kubernetes, which
// carries the addresses the API server answers on. A dedicated Barbican secret
// store projects them into the OpenBaoCluster's
// spec.network.apiServerEndpointIPs, without which the operator-rendered
// NetworkPolicy denies the instance its API-server egress on a CNI that enforces
// against the post-DNAT destination. No list or watch: the name is well known and
// the read goes through the uncached reader.
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// Required so reconcileNamespaces can ensure the namespaces a service is placed
// in via spec.services.<svc>.namespace: create for the Managed lifecycle, delete
// for the teardown that follows it, get/list/watch for both lifecycles (an
// External namespace is only ever verified, never mutated). A ControlPlane with
// no namespace assignments never exercises create or delete.
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch;create;delete

// Reconcile is the main reconciliation loop for the ControlPlane CR.
func (r *ControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Fetch the ControlPlane CR.
	var cp c5c3v1alpha1.ControlPlane
	if err := r.Get(ctx, req.NamespacedName, &cp); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("ControlPlane resource not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching ControlPlane: %w", err)
	}

	// Snapshot the persisted status so updateStatus can skip the write when a
	// pass leaves status unchanged (no write → no watch event → no
	// resourceVersion churn). Taken before any sub-reconciler or finalizer
	// mutates conditions.
	statusBefore := cp.Status.DeepCopy()

	// Handle deletion via the ORC-teardown finalizer: delete the operator-owned
	// K-ORC CRs first and hold the ControlPlane CR (which defers the owner-ref GC
	// cascade so Keystone/MariaDB stay reachable) until they disappear, then
	// release the finalizer so GC tears down the rest. reconcileDelete requeues
	// while ORC CRs are still Terminating; route that path through updateStatus so
	// the KORCReady=False/FinalizingORC condition is persisted. On the terminal
	// release path it returns a zero result and removes the finalizer, so skip the
	// status write — the CR is about to be garbage-collected. Deletion is handled
	// before the duplicate guard so a Terminating ControlPlane that carries the
	// finalizer always releases it (reconcileDelete is a no-op when the finalizer
	// is absent), instead of being parked and wedged.
	if !cp.DeletionTimestamp.IsZero() {
		if result, err := r.reconcileDelete(ctx, &cp); !result.IsZero() || err != nil {
			return r.updateStatus(ctx, &cp, statusBefore, result, err)
		}
		return ctrl.Result{}, nil
	}

	// Defense-in-depth for the one-ControlPlane-per-namespace contract
	// the validating webhook rejects duplicate CREATEs,
	// but CRs that predate the guard, raced through the API server, or were
	// written with the webhook bypassed can still coexist. Park every
	// ControlPlane except the oldest so two reconcilers never operate on the
	// namespace's shared credential paths and child resources at once —
	// mirroring the CredentialRotation reconciler's AmbiguousControlPlane
	// handling.
	incumbent, err := r.duplicateControlPlaneIncumbent(ctx, &cp)
	if err != nil {
		return ctrl.Result{}, err
	}
	if incumbent != "" {
		return r.parkDuplicateControlPlane(ctx, &cp, incumbent)
	}

	// Ensure the ORC-teardown finalizer is installed before any sub-reconciler
	// projects a K-ORC CR, so a deletion issued between now and the next pass
	// still funnels through reconcileDelete. Installed after the duplicate guard
	// so only the active incumbent — the ControlPlane that actually projects K-ORC
	// CRs — carries the finalizer; parked duplicates return above and never need
	// it. Requeuing after the Update guarantees the next reconcile observes the
	// persisted finalizer.
	if added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, &cp, controlPlaneORCFinalizer); err != nil {
		return ctrl.Result{}, err
	} else if added {
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
	}

	// The remote-children finalizer goes on only when the ControlPlane places a
	// service on a target cluster, and only once at least one cluster it names
	// resolves: what it holds the CR open for is the sweep of the namespaces on
	// those clusters, and a ControlPlane that keeps every child at home has nothing
	// for it to hold. ANY resolving cluster is enough, because reconcileNamespaces
	// writes per namespace and will create the namespaces of that cluster on this
	// very pass, whatever a sibling ref does; a CR with no resolvable cluster at all
	// skips the install WITHOUT failing the pass — reconcileNamespaces is the first
	// blocking step below and reports that failure under the shared
	// TargetClusterUnavailable reason, while nothing has been written anywhere that
	// a finalizer would have to reclaim.
	//
	// Once installed the finalizer stays. A cluster that stops resolving later
	// still holds children this CR is responsible for, and the deletion path is
	// what decides between waiting for it and abandoning them.
	if !controllerutil.ContainsFinalizer(&cp, commonmulticluster.RemoteChildrenFinalizer) &&
		r.anyTargetClusterResolves(ctx, &cp) {
		if added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, &cp,
			commonmulticluster.RemoteChildrenFinalizer); err != nil {
			return ctrl.Result{}, err
		} else if added {
			return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
		}
	}

	// Run the sub-reconcilers in two phases via the shared table-driven chain.
	//
	// The blocking prefix — Namespaces → Infrastructure → ESOTenantStore →
	// DBCredentials → AdminPassword → Keystone — runs through RunPipeline and
	// short-circuits at the first non-zero result or error, because each step
	// genuinely feeds the next: a later step applying before its predecessor
	// converged would fail or wedge.
	//
	// The tail is a RunSequentialGroup of nine independent projections (Horizon,
	// KORC, AdminCredential, Catalog, Glance, Placement, Barbican, ServiceAccounts,
	// RegistrationTenantStores). Running every member on every pass is safe: every
	// member runs each pass, its condition always persists, the members' requeues
	// aggregate to the shortest member interval, and one member's failure no longer
	// suppresses its peers (member errors are joined). A still-converging Horizon
	// therefore no longer parks KORC, the AdminCredential/Catalog identity
	// bootstrap, Glance, Placement, or Barbican.
	//
	// Correctness rests on each member gating itself on the conditions it
	// consumes rather than on its position in the chain — the prefix's
	// short-circuit is gone, so a member that assumed an unreachable peer had
	// blocked it would now run against unverified state. Every member except
	// KORC, ServiceAccounts and RegistrationTenantStores opens with an in-memory
	// conditions.AllTrue gate and returns before touching the API; those three have
	// no condition gate and run their full body every pass, which is what dominates
	// the group's steady-state API cost. ServiceAccounts is ungated because it only
	// reads the KeystoneService children the service legs applied earlier in the
	// same pass, so there is no projection it could defer.
	//
	// Onboarding rule: a future service whose projection is independent of the
	// others joins the tail group rather than the blocking prefix — and MUST
	// carry its own condition gate, following the six gated members rather than
	// KORC. RegistrationTenantStores is ungated for the same reason KORC is, not
	// as an exemption from that rule: it consumes no condition this chain
	// produces, because the tenant-store trio depends on cert-manager and OpenBao
	// alone.
	//
	// Either phase's outcome funnels through updateStatus, so conditions and
	// the requeue/error are persisted by construction on every exit path. Every
	// named step is routed through instrumenter.Instrument so that duration
	// samples and error counters are emitted under a stable sub_reconciler
	// label; the group members self-instrument, which is why the group step
	// itself is bare/unnamed. AdminPassword runs BEFORE Keystone: the
	// keystone-operator's SecretsReady gate needs the admin-password
	// ExternalSecret to exist before the projected Keystone child references it.
	pipeline := []commonreconcile.Step{
		// Namespaces runs FIRST: every later sub-reconciler projects into a
		// service namespace, and applying into one that does not exist fails with
		// an error naming neither the ControlPlane nor the assignment behind it.
		// A ControlPlane without namespace assignments (the default) short-circuits
		// to True immediately, so the step costs nothing.
		{Name: "Namespaces", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileNamespaces(ctx, &cp)
		}},
		{Name: "Infrastructure", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileInfrastructure(ctx, &cp)
		}},
		// ESOTenantStore runs before every store-consuming sub-reconciler
		// (DBCredentials, AdminPassword, Keystone, ...): it provisions the
		// per-tenant SecretStore they default onto, so the store exists — and is
		// gated Ready — before they route ExternalSecrets/PushSecrets through it.
		// Placed after Infrastructure so MariaDB/Memcached provisioning is not
		// blocked behind tenant-store cert issuance.
		{Name: "ESOTenantStore", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileESOTenantStore(ctx, &cp)
		}},
		{Name: "DBCredentials", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDBCredentials(ctx, &cp)
		}},
		{Name: "AdminPassword", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileAdminPassword(ctx, &cp)
		}},
		{Name: "Keystone", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileKeystone(ctx, &cp)
		}},
		// The member order is same-pass convergence ordering, not a
		// short-circuiting dependency chain.
		{Fn: func(ctx context.Context) (ctrl.Result, error) {
			return commonreconcile.RunSequentialGroup(ctx, instrumenter.Instrument, []commonreconcile.Step{
				// Horizon is gated on KeystoneReady (the dashboard
				// authenticates against the Keystone child).
				{Name: "Horizon", Fn: func(ctx context.Context) (ctrl.Result, error) {
					return r.reconcileHorizon(ctx, &cp)
				}},
				{Name: "KORC", Fn: func(ctx context.Context) (ctrl.Result, error) {
					return r.reconcileKORC(ctx, &cp)
				}},
				// AdminCredential is ordered after KORC because it reads the
				// KORCReady condition KORC wrote earlier in this same pass.
				{Name: "AdminCredential", Fn: func(ctx context.Context) (ctrl.Result, error) {
					return r.reconcileAdminCredential(ctx, &cp)
				}},
				{Name: "Catalog", Fn: func(ctx context.Context) (ctrl.Result, error) {
					return r.reconcileCatalog(ctx, &cp)
				}},
				// Glance is gated on KeystoneReady (Glance validates tokens
				// against the Keystone child) and on the KeystoneService child
				// it projects for itself, whose AccountReady reports the
				// Keystone user; the child's aggregate Ready folds into
				// GlanceReady.
				{Name: "Glance", Fn: func(ctx context.Context) (ctrl.Result, error) {
					return r.reconcileGlance(ctx, &cp)
				}},
				// Placement is gated exactly like Glance: on KeystoneReady —
				// Placement validates tokens against the Keystone child — and
				// on the AccountReady of the KeystoneService child it projects,
				// whose aggregate Ready folds into PlacementReady.
				{Name: "Placement", Fn: func(ctx context.Context) (ctrl.Result, error) {
					return r.reconcilePlacement(ctx, &cp)
				}},
				// Barbican is gated exactly like Glance and Placement: on
				// KeystoneReady — Barbican validates tokens against the
				// Keystone child — and on the AccountReady of the
				// KeystoneService child it projects, whose aggregate Ready
				// folds into BarbicanReady. It carries one gate the others
				// do not: on a dedicated secret store it holds the projection
				// until the OpenBao instance it provisions serves requests.
				{Name: "Barbican", Fn: func(ctx context.Context) (ctrl.Result, error) {
					return r.reconcileBarbican(ctx, &cp)
				}},
				// OVN carries no condition gate: it reads the OVNCentral named
				// by services.neutron.ovn.centralRef and mirrors its readiness
				// into OVNReady, which is what the Neutron projection that
				// follows consumes. The central is deployed outside the plane,
				// so nothing this chain produces can make it ready.
				{Name: "OVN", Fn: func(ctx context.Context) (ctrl.Result, error) {
					return r.reconcileOVN(ctx, &cp)
				}},
				// ServiceAccounts aggregates the readiness of the
				// KeystoneService children the Glance/Placement/Barbican legs
				// applied earlier in this same pass into
				// ServiceAccountsReady. It reads only, so it carries no
				// condition gate.
				{Name: "ServiceAccounts", Fn: func(ctx context.Context) (ctrl.Result, error) {
					return r.reconcileServiceAccounts(ctx, &cp)
				}},
				// RegistrationTenantStores provisions the per-tenant store in the
				// allowlisted namespaces standalone KeystoneService CRs register
				// from. Like KORC it carries no condition gate, and legitimately
				// so: it consumes no condition this chain produces, because the
				// trio it writes depends on cert-manager and OpenBao alone —
				// exactly like its blocking-prefix twin, which likewise runs
				// ungated. It sits in the GROUP rather than in that prefix so a
				// namespace the control plane does not own can never park
				// DBCredentials, AdminPassword and Keystone behind it.
				{Name: "RegistrationTenantStores", Fn: func(ctx context.Context) (ctrl.Result, error) {
					return r.reconcileRegistrationTenantStores(ctx, &cp)
				}},
			})
		}},
	}

	result, err := commonreconcile.RunPipeline(ctx, instrumenter.Instrument, pipeline)
	return r.updateStatus(ctx, &cp, statusBefore, result, err)
}

// updateStatus persists the current status conditions and returns the given
// result and error, delegating to commonreconcile.UpdateStatus: the write is
// skipped when the pass left status semantically unchanged from the
// statusBefore snapshot (no write → no watch event → no resourceVersion
// churn), and a failed write is joined with reconcileErr so the original
// reconcile failure stays visible. The mutate hook recomputes the aggregate
// Ready condition on EVERY status write — including the in-progress/
// early-return paths where a sub-reconciler requeued before the chain
// converged — projects status.services/status.updatePhase via
// setServicesStatus, and stamps status.observedGeneration so a stale status
// is distinguishable from a current one.
func (r *ControlPlaneReconciler) updateStatus(ctx context.Context, cp *c5c3v1alpha1.ControlPlane, statusBefore *c5c3v1alpha1.ControlPlaneStatus, result ctrl.Result, reconcileErr error) (ctrl.Result, error) {
	return controlPlaneSkeleton.UpdateStatus(ctx, r.Client, cp, statusBefore, &cp.Status, func() {
		setServicesStatus(cp)
		cp.Status.ObservedGeneration = cp.Generation
	}, result, reconcileErr)
}

// controlPlaneSkeleton bundles the shared controller-skeleton glue (Ready
// aggregation and the no-op-skipping status write) with the ControlPlane's
// sub-condition vocabulary and status accessor.
var controlPlaneSkeleton = commonreconcile.Skeleton[*c5c3v1alpha1.ControlPlane, c5c3v1alpha1.ControlPlaneStatus]{
	SubConditionTypes: subConditionTypes,
	Conditions:        func(cp *c5c3v1alpha1.ControlPlane) *[]metav1.Condition { return &cp.Status.Conditions },
}

// setReadyCondition sets the aggregate Ready condition based on all
// sub-conditions, delegating to the shared skeleton with the ControlPlane
// sub-condition vocabulary. conditions.AllTrue checks only the
// subConditionTypes, not the Ready condition itself, so this is not
// self-referential.
func setReadyCondition(cp *c5c3v1alpha1.ControlPlane) {
	controlPlaneSkeleton.SetReady(cp)
}

// duplicateControlPlaneIncumbent returns the name of the ControlPlane that owns
// cp's namespace when cp is NOT it — i.e. when cp must be parked. The owner is
// the oldest ControlPlane in the namespace by CreationTimestamp, with the
// lexically smallest Name breaking creation-time ties, so every evaluation
// deterministically picks the same incumbent. An empty string means cp itself
// is the incumbent (or the only ControlPlane) and reconciliation may proceed.
// The List goes through the informer cache: unlike the admission-time webhook
// check this guard runs on every reconcile, so eventual consistency is enough
// — a briefly stale cache only delays the parking by one requeue.
func (r *ControlPlaneReconciler) duplicateControlPlaneIncumbent(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (string, error) {
	var cps c5c3v1alpha1.ControlPlaneList
	if err := r.List(ctx, &cps, client.InNamespace(cp.Namespace)); err != nil {
		return "", fmt.Errorf("listing ControlPlanes in namespace %q for the duplicate guard: %w", cp.Namespace, err)
	}
	incumbent := cp
	for i := range cps.Items {
		other := &cps.Items[i]
		if other.UID == cp.UID {
			continue
		}
		if other.CreationTimestamp.Before(&incumbent.CreationTimestamp) ||
			(other.CreationTimestamp.Equal(&incumbent.CreationTimestamp) && other.Name < incumbent.Name) {
			incumbent = other
		}
	}
	if incumbent.UID == cp.UID {
		return "", nil
	}
	return incumbent.Name, nil
}

// parkDuplicateControlPlane sets Ready=False with reason DuplicateControlPlane
// naming the incumbent, persists the status, and requeues. It deliberately
// bypasses updateStatus: setReadyCondition would recompute Ready from the
// sub-conditions and overwrite the DuplicateControlPlane reason. The periodic
// requeue lets the parked CR take over automatically once the incumbent is
// fully deleted — no watch event fires on the duplicate's behalf when that
// happens.
func (r *ControlPlaneReconciler) parkDuplicateControlPlane(ctx context.Context, cp *c5c3v1alpha1.ControlPlane, incumbent string) (ctrl.Result, error) {
	// Route through the shared status writer so a steady parked state skips the
	// no-op write, but keep the deliberate no-re-aggregation semantics: the
	// mutate hook sets ONLY the DuplicateControlPlane Ready condition (updateStatus
	// would recompute Ready from the sub-conditions and overwrite this reason).
	statusBefore := cp.Status.DeepCopy()
	return commonreconcile.UpdateStatus(ctx, r.Client, cp, statusBefore, &cp.Status, func() {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "DuplicateControlPlane",
			Message: fmt.Sprintf(
				"parked: ControlPlane %q is older and owns namespace %q; only one ControlPlane is permitted per namespace",
				incumbent, cp.Namespace,
			),
		})
		cp.Status.ObservedGeneration = cp.Generation
	}, ctrl.Result{RequeueAfter: duplicateControlPlaneRequeueAfter}, nil)
}

// keystoneServiceKey is the key under which status.services reports the
// projected Keystone service readiness.
const keystoneServiceKey = "keystone"

// horizonServiceKey is the key under which status.services reports the
// Horizon dashboard.
const horizonServiceKey = "horizon"

// glanceServiceKey is the key under which status.services reports the Glance
// image service.
const glanceServiceKey = "glance"

// placementServiceKey is the key under which status.services reports the
// Placement service.
const placementServiceKey = "placement"

// barbicanServiceKey is the key under which status.services reports the Barbican
// key manager.
const barbicanServiceKey = "barbican"

// setServicesStatus records status.services and status.updatePhase on every
// status write (#476). Both fields were declared on ControlPlaneStatus but never
// written. status.updatePhase is fixed at Idle until the release-update state
// machine is implemented — the other UpdatePhase values are reserved (see the
// UpdatePhase DECISION comment), and "no update in progress" is the honest L1
// state. status.services maps each projected service to its observed readiness,
// derived from the corresponding sub-condition, with the release the service is
// being driven to.
func setServicesStatus(cp *c5c3v1alpha1.ControlPlane) {
	cp.Status.UpdatePhase = c5c3v1alpha1.UpdatePhaseIdle
	// Only report the Keystone service when it is actually managed by this
	// ControlPlane (spec.services.keystone set). When unset the ControlPlane
	// manages no Keystone, so status.services stays empty rather than reporting a
	// service that does not exist.
	// One entry per configured service (keystone, horizon, glance, placement,
	// barbican), in a stable order; unmanaged services are omitted rather than
	// reported as a service that does not exist. The entry NAMES carry beyond
	// status: the webhook's shared/dedicated transition freeze reads
	// status.services[].name to tell a service's CREATE from a service dropped and
	// re-added (serviceDeclaredBefore), so a service missing an arm here is a
	// service the freeze cannot see.
	var services []c5c3v1alpha1.ServiceStatus
	if cp.Spec.Services.Keystone != nil {
		services = append(services, c5c3v1alpha1.ServiceStatus{
			Name:    keystoneServiceKey,
			Ready:   conditions.AllTrue(cp.Status.Conditions, conditionTypeKeystoneReady),
			Release: cp.Spec.OpenStackRelease,
		})
	}
	if cp.Spec.Services.Horizon != nil {
		services = append(services, c5c3v1alpha1.ServiceStatus{
			Name:    horizonServiceKey,
			Ready:   conditions.AllTrue(cp.Status.Conditions, conditionTypeHorizonReady),
			Release: cp.Spec.OpenStackRelease,
		})
	}
	if cp.Spec.Services.Glance != nil {
		services = append(services, c5c3v1alpha1.ServiceStatus{
			Name:    glanceServiceKey,
			Ready:   conditions.AllTrue(cp.Status.Conditions, conditionTypeGlanceReady),
			Release: cp.Spec.OpenStackRelease,
		})
	}
	if cp.Spec.Services.Placement != nil {
		services = append(services, c5c3v1alpha1.ServiceStatus{
			Name:    placementServiceKey,
			Ready:   conditions.AllTrue(cp.Status.Conditions, conditionTypePlacementReady),
			Release: cp.Spec.OpenStackRelease,
		})
	}
	if cp.Spec.Services.Barbican != nil {
		services = append(services, c5c3v1alpha1.ServiceStatus{
			Name:    barbicanServiceKey,
			Ready:   conditions.AllTrue(cp.Status.Conditions, conditionTypeBarbicanReady),
			Release: cp.Spec.OpenStackRelease,
		})
	}
	cp.Status.Services = services
}

// controlPlaneSecretNameExtractor is the controller-runtime IndexerFunc registered
// under ControlPlaneSecretNameIndexKey. It returns the deduplicated, non-empty
// set of Secret names a ControlPlane CR references, so the field indexer can
// resolve a Secret event to the referencing CR(s) without listing every
// ControlPlane in the namespace:
//
//   - the EFFECTIVE admin-password Secret — the operator-owned per-ControlPlane
//     Secret in managed mode, the user-supplied
//     spec.korc.adminCredential.passwordSecretRef.name in brownfield and External
//     mode;
//   - the private-CA bundle Secret of whichever Keystone endpoint K-ORC dials
//     (keystoneCABundleRef: the External-mode installation's, or a placed
//     Keystone's), so rotating the CA wakes the ControlPlane immediately instead
//     of waiting for the cache resync.
//
// The two may name the same Secret, so the result is deduplicated: a duplicate
// index entry would enqueue the same ControlPlane twice per Secret event.
func controlPlaneSecretNameExtractor(obj client.Object) []string {
	cp, ok := obj.(*c5c3v1alpha1.ControlPlane)
	if !ok {
		// controller-runtime should never call us with the wrong type; a nil
		// return is safer than a panic if it ever does.
		return nil
	}
	names := []string{}
	if name := effectiveAdminPasswordSecretRef(cp).Name; name != "" {
		names = append(names, name)
	}
	if ref := keystoneCABundleRef(cp); ref != nil && ref.Name != "" && !slices.Contains(names, ref.Name) {
		names = append(names, ref.Name)
	}
	return names
}

// registerControlPlaneSecretNameIndex registers the ControlPlane field indexer
// under ControlPlaneSecretNameIndexKey with the given FieldIndexer.
// SetupWithManager calls this once against mgr.GetFieldIndexer() so
// secretToControlPlaneMapper can resolve a Secret event to the referencing
// ControlPlane CRs via an O(1) reverse lookup. The returned error is wrapped with
// the index key so the registration site is identifiable in manager-startup
// failure logs.
func registerControlPlaneSecretNameIndex(ctx context.Context, indexer client.FieldIndexer) error {
	return watch.RegisterSecretNameIndex(ctx, indexer, &c5c3v1alpha1.ControlPlane{}, ControlPlaneSecretNameIndexKey, controlPlaneSecretNameExtractor)
}

// secretToControlPlaneMapper returns a MapFunc that maps Secret events to
// reconcile requests for ControlPlane CRs that reference the Secret by name,
// resolved via the ControlPlaneSecretNameIndexKey field indexer. The admin
// password Secret is typically ESO-managed (owned by the ExternalSecret
// controller, not the ControlPlane), so an owner-ref watch would never match it;
// the index-backed namespace-scoped List is what wakes the ControlPlane when its
// admin password rotates. It binds the shared watch.SecretToOwnersMapper to the
// ControlPlane types in the index-only shape (no owner-ref leg).
func secretToControlPlaneMapper(c client.Reader) handler.MapFunc {
	// AllNamespaces widens the index-backed List to the whole cluster: a
	// ControlPlane that places Keystone in a namespace of its own materialises the
	// admin-password Secret THERE, so the namespace-scoped List would look for the
	// referencing ControlPlane in a namespace it does not live in and drop the
	// rotation event.
	//
	// The index keys on the Secret NAME alone, though, so a cluster-wide List also
	// matches a ControlPlane that references the same name in a namespace this
	// Secret has nothing to do with. Filter those out: a ControlPlane is only woken
	// by a Secret in a namespace it actually occupies — its own, or one it places a
	// service in.
	indexed := watch.SecretToOwnersMapper(c, watch.SecretMapperConfig{
		IndexKey:      ControlPlaneSecretNameIndexKey,
		NewList:       func() client.ObjectList { return &c5c3v1alpha1.ControlPlaneList{} },
		AllNamespaces: true,
	})
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var requests []reconcile.Request
		for _, req := range indexed(ctx, obj) {
			cp := &c5c3v1alpha1.ControlPlane{}
			if err := c.Get(ctx, req.NamespacedName, cp); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				// A transient cache error must not swallow a legitimate rotation
				// event; enqueue and let reconcile resolve authoritatively.
				log.FromContext(ctx).V(1).Info("ControlPlane Get failed while scoping a Secret event; enqueueing anyway",
					"controlPlane", req.NamespacedName, "error", err)
				requests = append(requests, req)
				continue
			}
			if slices.Contains(controlPlaneNamespaces(cp), obj.GetNamespace()) {
				requests = append(requests, req)
			}
		}
		return requests
	}
}

// namespacedStoreToControlPlaneMapper enqueues the ControlPlanes whose effective
// store reference resolves to the changed namespaced SecretStore. It replaces the
// shared watch.StoreRefFanOut on the namespaced leg, whose namespace-scoped List
// assumes a CR can only reference a store in its OWN namespace: the per-tenant
// store is provisioned in EVERY namespace the ControlPlane places a service in,
// so a store flipping unready in a service namespace must still wake the
// ControlPlane that lives elsewhere. The List is therefore cluster-wide, and a
// ControlPlane matches when the store's name is the one it selected AND the
// store's namespace is one it actually occupies — so an identically-named store
// in an unrelated namespace wakes nobody.
//
// The cluster-scoped leg keeps the shared fan-out: a ClusterSecretStore is
// namespace-less, so its List is already unscoped.
//
// A ControlPlane also matches on the store it provisions in an ALLOWLISTED
// registration namespace, which is not one it occupies (see
// reconcileRegistrationTenantStores). That store is not the plane's own delivery
// path, so it is matched separately rather than by widening the namespace test:
// only an operator-provisioned trio qualifies, so a plane that overrode its store
// reference never matches one.
func namespacedStoreToControlPlaneMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var list c5c3v1alpha1.ControlPlaneList
		if err := c.List(ctx, &list); err != nil {
			log.FromContext(ctx).Error(err, "listing ControlPlanes for secret-store watch",
				"store", client.ObjectKeyFromObject(obj))
			return nil
		}

		var requests []reconcile.Request
		for i := range list.Items {
			cp := &list.Items[i]
			if !storeWakesControlPlane(cp, obj) {
				continue
			}
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cp)})
		}
		return requests
	}
}

// storeWakesControlPlane reports whether a namespaced SecretStore event is one cp
// has to reconcile on: the store it routes its own secret traffic through, in a
// namespace it occupies, or a per-tenant store it provisions in an allowlisted
// registration namespace.
func storeWakesControlPlane(cp *c5c3v1alpha1.ControlPlane, store client.Object) bool {
	ref := effectiveControlPlaneStoreRef(cp)
	if ref.Kind == commonv1.SecretStoreKindNamespaced && ref.Name == store.GetName() &&
		slices.Contains(controlPlaneNamespaces(cp), store.GetNamespace()) {
		return true
	}
	// The registration stores are operator-provisioned, so an explicit
	// spec.secretStoreRef means there are none to wake for.
	if cp.Spec.SecretStoreRef != nil || store.GetName() != esoTenantStoreName {
		return false
	}
	sr := cp.Spec.KORC.ServiceRegistrations
	return sr != nil && slices.Contains(sr.AllowedNamespaces, store.GetNamespace())
}

// keystoneServiceToControlPlaneMapper enqueues the ControlPlane a KeystoneService
// registers against, so a registration appearing in or leaving an allowlisted
// namespace moves that namespace into or out of the tenant-store provisioning set
// at watch latency rather than at the next periodic resync.
//
// It needs no client: the reference resolves from the object alone, its namespace
// defaulting to the CR's own exactly as resolveControlPlane resolves it. An empty
// name (a CR that bypassed admission) maps to nothing rather than to a request for
// an unnamed ControlPlane.
func keystoneServiceToControlPlaneMapper(_ context.Context, obj client.Object) []reconcile.Request {
	ks, ok := obj.(*c5c3v1alpha1.KeystoneService)
	if !ok || ks.Spec.ControlPlaneRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{
		Namespace: cmp.Or(ks.Spec.ControlPlaneRef.Namespace, ks.Namespace),
		Name:      ks.Spec.ControlPlaneRef.Name,
	}}}
}

// controlPlaneNamespaces returns every namespace the ControlPlane occupies: its
// own, plus each namespace it places a service in. It is the deduplicated set the
// per-namespace concerns walk — the tenant stores are provisioned in each, and
// the store watch matches against it.
func controlPlaneNamespaces(cp *c5c3v1alpha1.ControlPlane) []string {
	namespaces := []string{cp.Namespace}
	for _, ns := range cp.DedicatedServiceNamespaces() {
		namespaces = append(namespaces, ns.Name)
	}
	return namespaces
}

// storeToControlPlaneMapper returns a MapFunc that enqueues the ControlPlanes
// whose effective secret store reference resolves to the changed store object.
// watchedKind selects which store scope this mapper is registered against — a
// cluster-scoped ClusterSecretStore (shared across namespaces) or a namespaced
// SecretStore (per tenant). A status transition (e.g. ESO losing the backend
// connection) must retrigger reconcile on the ControlPlanes that route
// credentials through the changed store; otherwise DBCredentialsReady /
// AdminPasswordReady / AdminCredentialReady would stay stale-True until the next
// periodic resync (#476). A ControlPlane that omits spec.secretStoreRef resolves
// to the operator-provisioned per-tenant namespaced store via
// effectiveControlPlaneStoreRef, so a status transition on that store also
// retriggers reconcile. It binds the shared watch.StoreRefFanOut to the
// ControlPlane list type.
func storeToControlPlaneMapper(c client.Reader, watchedKind commonv1.SecretStoreRefKind) handler.MapFunc {
	return watch.StoreRefFanOut(c, watchedKind,
		func() client.ObjectList { return &c5c3v1alpha1.ControlPlaneList{} },
		func(o client.Object) commonv1.SecretStoreRefSpec {
			cp, ok := o.(*c5c3v1alpha1.ControlPlane)
			if !ok {
				return commonv1.SecretStoreRefSpec{}
			}
			return effectiveControlPlaneStoreRef(cp)
		})
}

// controlPlaneTargetClusters reports the clusters the ControlPlane at key places
// services on, read from the management cluster where the CR lives. It is the
// gate every target-cluster watch leg carries (see
// commonmulticluster.RemoteRequestsAmong): a leg is engaged on every registered
// cluster rather than on the ones a CR names, so an event only reaches a
// ControlPlane that named the cluster it arrived from.
//
// It is the set-valued counterpart of commonmulticluster.TargetClusterOf, which
// the service operators bind instead. A ControlPlane places each service
// separately, so one CR can name several clusters at once; the error is returned
// as it comes back, and RemoteRequestsAmong is what decides that a NotFound is
// the ordinary answer and anything else is worth a log line.
//
// The read costs no API call and no second informer: the controller's own For
// leg already holds every ControlPlane in the local cache this reads through.
func controlPlaneTargetClusters(c client.Reader) commonmulticluster.TargetClustersFunc {
	return func(ctx context.Context, key types.NamespacedName) ([]string, error) {
		cp := &c5c3v1alpha1.ControlPlane{}
		if err := c.Get(ctx, key, cp); err != nil {
			return nil, err
		}
		return cp.TargetClusterNames(), nil
	}
}

// SetupWithManager registers the ControlPlaneReconciler with the controller
// manager. It Owns every child CR the sub-reconcilers project (MariaDB,
// Keystone, Horizon, Glance/GlanceBackend, the eight K-ORC resources, the
// Memcached CR, the mTLS Certificate, and the ESO ExternalSecret/PushSecret/
// VaultDynamicSecret) so an upstream child status transition retriggers
// reconcile, Watches Secrets so an admin-password rotation wakes the owning
// ControlPlane via the field indexer, and Watches the OpenBao-backed secret
// stores so an ESO/OpenBao outage reflects in the credential conditions
// promptly rather than after the next periodic resync (#476).
//
// The legs for kinds a sibling operator owns (Keystone, Horizon, Glance,
// GlanceBackend and KeystoneIdentityBackend) are registered only when a
// discovery probe reports the CRD served. The eight K-ORC kinds are NOT among
// them: like MariaDB, Memcached and the ESO kinds, K-ORC is a hard dependency
// of every reconcile pass, so its watches stay unconditional (see the Owns
// block below and the HARD CRD DEPENDENCY note in reconcile_korc.go).
// Registering a watch for an absent CRD dead-locks manager start: its informer
// never syncs, controller-runtime aborts on the CacheSyncTimeout, and the elected leader
// crash-loops while still reporting Ready. When any optional CRD is missing, a
// leader-gated runnable re-probes discovery and returns an error the moment one
// appears, restarting the process so the now-served watch is registered on the
// next pass — the only way to add a watch, since controller-runtime cannot
// mount one on a running manager (see crd_presence.go). The wiring lives in
// buildControlPlaneController so the production path and the integration test
// drive identical leg registration.
//
// Every leg is pinned to a cluster. The ones described above watch the
// management cluster, where the ControlPlane CRs and the children of a service
// that names no target cluster live. A second set watches the clusters a service
// can be placed on, where an owner reference cannot reach and the ownership
// labels are what maps a child back to the CR that placed it.
//
// The shared controller options it applies let independent CRs reconcile in
// parallel instead of serialising at the controller-runtime default of 1, and
// the tuned RateLimiter caps per-item failure backoff at 30s rather than the
// default 1000s (see bootstrap.TypedControllerOptions).
func (r *ControlPlaneReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	return r.setupWithOptions(mgr, bootstrap.TypedControllerOptions[mcreconcile.Request](r.MaxConcurrentReconciles))
}

// setupWithOptions carries the production wiring SetupWithManager applies. The
// controller options are a parameter so the integration suite can register this
// exact chain with SkipNameValidation set, rather than a hand-built copy of it
// that drifts the moment a leg is added.
func (r *ControlPlaneReconciler) setupWithOptions(mgr mcmanager.Manager, opts crcontroller.TypedOptions[mcreconcile.Request]) error {
	b, err := r.buildControlPlaneController(mgr, mcbuilder.ControllerManagedBy(mgr).WithOptions(opts))
	if err != nil {
		return err
	}
	// The default wrapper turns an error matching multicluster.ErrClusterNotFound
	// into a successful reconcile. This operator instead surfaces an unresolvable
	// cluster as a condition and requeues, so the wrapper stays off and the error
	// semantics remain byte-identical to the classic builder's.
	return b.
		WithClusterNotFoundWrapper(false).
		Complete(commonmulticluster.LocalReconciler(r))
}

// buildControlPlaneController applies every watch leg to b and returns it ready
// to Complete. SetupWithManager and the integration test both call it so the
// production path and the test drive IDENTICAL leg registration. The legs for
// optional sibling-operator kinds are gated on a discovery probe
// (probeOptionalWatches); when the probe reports a kind missing its legs are
// skipped and a leader-gated crdWatchGate is registered to restart the process
// once the CRD appears (see crd_presence.go).
//
// DECISION (Memcached Owns): memcached.c5c3.io ships no Go module (see
// memcachedGVK in reconcile_infrastructure.go), so the Memcached child is owned
// as an *unstructured.Unstructured carrying that shared GVK rather than a typed
// client object — exactly how ensureMemcached creates it. The same memcachedGVK
// constant is reused so the watch and the create-or-update agree on the kind.
//
// DECISION (ESO Owns): the ExternalSecret and PushSecret children are owned via
// SetControllerReference by the DB-credential, admin-password and K-ORC
// sub-reconcilers, so Owns() is the direct wiring (no name-based mapper needed).
// It wakes the ControlPlane on every ESO status tick, which is acceptable for
// the goal of reflecting ESO sync/outage transitions in the credential
// conditions promptly; a relevance predicate could be added later if the
// reconcile volume becomes a concern.
func (r *ControlPlaneReconciler) buildControlPlaneController(mgr mcmanager.Manager, b *mcbuilder.Builder) (*mcbuilder.Builder, error) {
	// The management cluster: where the ControlPlane CRs live, and the only
	// cluster the discovery guard below probes. A target cluster's own kinds are
	// probed per cluster, when it is engaged (commonmulticluster.ClusterServesKind).
	local := mgr.GetLocalManager()

	// The uncached reader for the never-watched RBAC kinds (see APIReader).
	if r.APIReader == nil {
		r.APIReader = local.GetAPIReader()
	}

	// Register the field indexer before Watches so secretToControlPlaneMapper
	// can rely on it for its MatchingFields lookup. It goes on the LOCAL field
	// indexer, not mgr's: with a provider configured, the multicluster manager's
	// indexer registers against the target clusters, which hold no ControlPlane
	// CR, and applying it while engaging one would fail that engagement. Every
	// request the legs below emit is pinned to the management cluster, so a
	// remote event resolves its CR through this index all the same.
	if err := registerControlPlaneSecretNameIndex(context.Background(), local.GetFieldIndexer()); err != nil {
		return nil, err
	}

	disco, err := discovery.NewDiscoveryClientForConfig(local.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("building discovery client for optional CRD probe: %w", err)
	}

	served, missing, err := probeOptionalWatches(disco, r.Scheme)
	if err != nil {
		// A non-NotFound discovery error (API server unreachable, RBAC
		// forbidden) aborts setup so the pod restarts rather than silently
		// dropping watches; NotFound is folded into the served map by the probe.
		return nil, err
	}

	// isServed reports whether the API server serves obj's kind, reusing the
	// discovery result the probe already computed. The GVKForObject error branch
	// is unreachable in practice — the probe resolved these exact types against
	// the same scheme — so treating a resolution failure as "not served" (skip
	// the leg) is a safe degraded answer rather than a fatal setup error.
	isServed := func(obj client.Object) bool {
		gvk, err := apiutil.GVKForObject(obj, r.Scheme)
		if err != nil {
			return false
		}
		return served[gvk]
	}

	memcached := &unstructured.Unstructured{}
	memcached.SetGroupVersionKind(memcachedGVK)

	// The per-ControlPlane mTLS client Certificate is Owned as an
	// *unstructured.Unstructured carrying certificateGVK (mirroring the Memcached
	// Owns) so the c5c3 operator takes no cert-manager Go dependency.
	certificate := &unstructured.Unstructured{}
	certificate.SetGroupVersionKind(certificateGVK)

	// The RabbitmqCluster kind is watched as an *unstructured.Unstructured
	// carrying messaging.RabbitmqClusterGVK (the c5c3 operator takes no
	// dependency on the RabbitMQ Cluster Operator's Go module), and its legs sit
	// under the discovery guard below because the CRD is opt-in infrastructure.
	rabbitmq := &unstructured.Unstructured{}
	rabbitmq.SetGroupVersionKind(messaging.RabbitmqClusterGVK)

	// Every leg watching the management cluster carries both engage options
	// below; see their definition for why an unpinned leg would quietly stop
	// watching it once a provider is configured.
	engageLocal := commonmulticluster.EngageLocalCluster
	engageNoProviders := commonmulticluster.EngageNoProviderClusters

	// Every leg watching a target cluster is engaged on all of them, not on the
	// ones some ControlPlane places a service on, so it has to drop the events
	// belonging to a CR that places its services elsewhere (see
	// commonmulticluster.RemoteRequestsAmong).
	targets := controlPlaneTargetClusters(local.GetClient())

	// Unconditional legs: the ControlPlane itself, the infrastructure children
	// the c5c3 operator ships or hard-depends on, and the secret-store watches.
	b = b.
		// Filter the CR's own status-only updates so Status().Update does not
		// re-wake the controller (see watch.CRUpdatePredicate). Together with
		// the no-op status-write skip in updateStatus this closes the
		// self-wake loop the bare For() previously allowed.
		For(&c5c3v1alpha1.ControlPlane{}, mcbuilder.WithPredicates(watch.CRUpdatePredicate()), engageLocal, engageNoProviders).
		Owns(&mariadbv1alpha1.MariaDB{}, engageLocal, engageNoProviders).
		// The eight K-ORC kinds are a HARD dependency of every reconcile pass:
		// reconcileKORC unconditionally mints the admin ApplicationCredential and
		// projects the catalog/identity resources, reading them through the cached
		// client with no spec or condition gate. A missing K-ORC CRD would surface
		// there as a no-match hard error, not a slimmable start-up state, so — like
		// MariaDB, Memcached and the ESO kinds — these Owns legs stay unconditional
		// rather than sitting behind the discovery guard below. The manager must
		// fail fast at start if K-ORC is absent.
		Owns(&orcv1alpha1.ApplicationCredential{}, engageLocal, engageNoProviders).
		Owns(&orcv1alpha1.Service{}, engageLocal, engageNoProviders).
		Owns(&orcv1alpha1.Endpoint{}, engageLocal, engageNoProviders).
		Owns(&orcv1alpha1.User{}, engageLocal, engageNoProviders).
		Owns(&orcv1alpha1.Domain{}, engageLocal, engageNoProviders).
		Owns(&orcv1alpha1.Project{}, engageLocal, engageNoProviders).
		Owns(&orcv1alpha1.Role{}, engageLocal, engageNoProviders).
		Owns(&orcv1alpha1.RoleAssignment{}, engageLocal, engageNoProviders).
		Owns(memcached, engageLocal, engageNoProviders).
		Owns(certificate, engageLocal, engageNoProviders).
		Owns(&esov1.ExternalSecret{}, engageLocal, engageNoProviders).
		Owns(&esov1alpha1.PushSecret{}, engageLocal, engageNoProviders).
		Owns(&esgenv1alpha1.VaultDynamicSecret{}, engageLocal, engageNoProviders).
		// The registration children of the built-in services: Glance, Placement and
		// Barbican each project one KeystoneService and hold their projection until
		// the child reports AccountReady, then their own readiness until it reports
		// the aggregate Ready. Both are status-only writes, and the
		// registration mapper leg further down drops exactly those through
		// watch.CRUpdatePredicate, so without a leg of its own a child going ready
		// would only reach the ControlPlane at the next periodic requeue. This Owns
		// leg carries a child co-located with the ControlPlane, which holds a
		// controller owner reference; a child placed in a namespace of its own
		// rides the labelled cross-namespace leg below. An event matching several
		// legs yields the same request several times, which the workqueue
		// deduplicates.
		Owns(&c5c3v1alpha1.KeystoneService{}, engageLocal, engageNoProviders).
		Watches(&corev1.Secret{}, commonmulticluster.LocalRequests(
			secretToControlPlaneMapper(local.GetClient()),
		), engageLocal, engageNoProviders).
		// Watch both the cluster-scoped ClusterSecretStore and the namespaced
		// SecretStore a ControlPlane can select via spec.secretStoreRef, so an
		// ESO/OpenBao outage reflects in the credential conditions as soon as ESO
		// flips the selected store's Ready condition. Each mapper enqueues only
		// the ControlPlanes whose effective store ref matches the changed store.
		Watches(&esov1.ClusterSecretStore{}, commonmulticluster.LocalRequests(
			storeToControlPlaneMapper(local.GetClient(), commonv1.SecretStoreKindCluster),
		), engageLocal, engageNoProviders).
		Watches(&esov1.SecretStore{}, commonmulticluster.LocalRequests(
			namespacedStoreToControlPlaneMapper(local.GetClient()),
		), engageLocal, engageNoProviders).
		// Cross-namespace children carry no owner reference (Kubernetes forbids one
		// across namespaces), so Owns() never fires for a service placed in a
		// namespace of its own. Watch the same kinds a second time through the
		// ownership labels the projections stamp on them, so a status transition on
		// such a child still wakes its ControlPlane. The label predicate gates each
		// leg so the shared informers only run the mapper for a labelled child; an
		// unlabelled object is filtered before the mapper, so same-namespace children
		// keep flowing through Owns() alone and neither leg double-enqueues the
		// other's objects. The Namespace leg installs a cluster-wide Namespace
		// informer, so the predicate is what keeps that informer from waking the
		// mapper on every namespace event in the cluster. (The Keystone, Horizon,
		// Glance, GlanceBackend, Placement, Barbican, BarbicanSecretStore,
		// OpenBaoCluster, OpenBaoTenant and RabbitmqCluster cross-namespace legs
		// belong to this group too but are registered under the discovery guard
		// below, co-located with their Owns.)
		Watches(&mariadbv1alpha1.MariaDB{}, crossNamespaceChildHandler(),
			mcbuilder.WithPredicates(crossNamespaceChildPredicate()), engageLocal, engageNoProviders).
		Watches(memcached, crossNamespaceChildHandler(),
			mcbuilder.WithPredicates(crossNamespaceChildPredicate()), engageLocal, engageNoProviders).
		Watches(&esov1.ExternalSecret{}, crossNamespaceChildHandler(),
			mcbuilder.WithPredicates(crossNamespaceChildPredicate()), engageLocal, engageNoProviders).
		// A registration child of a service placed in a namespace of its own: the
		// same status flips the Owns leg above carries for a co-located one, which
		// is where the built-in registration children and their gates are described.
		Watches(&c5c3v1alpha1.KeystoneService{}, crossNamespaceChildHandler(),
			mcbuilder.WithPredicates(crossNamespaceChildPredicate()), engageLocal, engageNoProviders).
		// The namespace itself: a Managed one being deleted out from under a live
		// ControlPlane must re-drive NamespacesReady rather than wait for the next
		// periodic resync.
		Watches(&corev1.Namespace{}, crossNamespaceChildHandler(),
			mcbuilder.WithPredicates(crossNamespaceChildPredicate()), engageLocal, engageNoProviders).
		// Standalone registrations: which allowlisted namespaces host one is what
		// decides where reconcileRegistrationTenantStores provisions a tenant store,
		// so a KeystoneService appearing or being deleted has to re-drive the plane.
		// Such a CR is no child of the ControlPlane — it is authored by whoever runs
		// the service — so it is mapped by its own reference, not by ownership; the
		// ControlPlane's own registration children are covered by the two ownership
		// legs above. The predicate drops the KeystoneService controller's
		// status-only writes, which cannot move the provisioning set.
		Watches(&c5c3v1alpha1.KeystoneService{}, commonmulticluster.LocalRequests(
			keystoneServiceToControlPlaneMapper,
		), mcbuilder.WithPredicates(watch.CRUpdatePredicate()), engageLocal, engageNoProviders)

	// The same children, once more, on the clusters a ControlPlane can place a
	// service on. Neither the Owns legs nor the cross-namespace ones above reach
	// them: an owner reference does not cross a cluster boundary and neither does
	// a local informer, so the ownership labels are all that maps a child on a
	// target cluster back to the CR that placed it.
	//
	// The two input legs at the end are not that watch over again. An input is
	// written by something other than this operator — ESO writes the
	// admin-password Secret, the tenant SecretStore — so it carries no ownership
	// labels and crossNamespaceChildMapper maps it to nothing. Only a leg of its
	// own makes a rotation or a store outage on the target reach the CR at watch
	// latency rather than at the next periodic resync, exactly as it does
	// management-side. A kind may carry several legs; the requests they produce
	// are deduplicated by the workqueue.
	//
	// Every mapper here reads through the LOCAL client whatever cluster delivered
	// the event: the ControlPlane CRs live on the management cluster alone.
	remoteLegs := []struct {
		obj client.Object
		fn  handler.MapFunc
	}{
		{&mariadbv1alpha1.MariaDB{}, crossNamespaceChildMapper},
		{memcached, crossNamespaceChildMapper},
		{certificate, crossNamespaceChildMapper},
		{&esov1.ExternalSecret{}, crossNamespaceChildMapper},
		{&esov1alpha1.PushSecret{}, crossNamespaceChildMapper},
		{&esgenv1alpha1.VaultDynamicSecret{}, crossNamespaceChildMapper},
		{&esov1.SecretStore{}, crossNamespaceChildMapper},
		{&corev1.Secret{}, crossNamespaceChildMapper},
		{&openbaov1alpha1.OpenBaoCluster{}, crossNamespaceChildMapper},
		{&openbaov1alpha1.OpenBaoTenant{}, crossNamespaceChildMapper},
		{&corev1.Namespace{}, crossNamespaceChildMapper},
		{&corev1.Secret{}, secretToControlPlaneMapper(local.GetClient())},
		{&esov1.SecretStore{}, namespacedStoreToControlPlaneMapper(local.GetClient())},
	}
	for _, leg := range remoteLegs {
		// The kind the leg filters clusters by is resolved from the object
		// itself, so the filter and the informer cannot describe different kinds:
		// a filter naming a kind the target serves while the leg watches one it
		// does not would engage the leg anyway, which is the whole-cluster
		// engagement failure ClusterServesKind exists to prevent.
		gvk, err := apiutil.GVKForObject(leg.obj, r.Scheme)
		if err != nil {
			return nil, fmt.Errorf("resolving the kind of a target-cluster watch leg: %w", err)
		}
		b = b.Watches(leg.obj, commonmulticluster.RemoteRequestsAmong(leg.fn, targets),
			commonmulticluster.RemoteWatchOptions(gvk)...)
	}

	// Guarded legs: kinds owned by a sibling operator whose CRD may be absent.
	// For each such kind both its Owns leg and its cross-namespace Watches leg (see
	// the cross-namespace block above) are co-located under one guard because both
	// informers target the same CRD and both would block manager start when it is
	// not served. The two openbao.org kinds join them: the openbao-operator is
	// installed only for a Barbican that takes a dedicated secret store, so a
	// ControlPlane without one runs on a cluster that never served them. The
	// RabbitmqCluster kind joins them because messaging is opt-in: a Keystone-only
	// install on a cluster without the RabbitMQ CRD starts clean, and crdWatchGate
	// restarts the operator once that CRD appears.
	//
	// The OVNCentral kind is guarded too, but not from this loop: it is referenced
	// rather than projected, so it carries neither an Owns leg nor a
	// cross-namespace child leg and takes a mapper-based Watches leg of its own
	// below.
	for _, obj := range []client.Object{
		&keystonev1alpha1.Keystone{},
		&horizonv1alpha1.Horizon{},
		&glancev1alpha1.Glance{},
		&glancev1alpha1.GlanceBackend{},
		&placementv1alpha1.Placement{},
		&barbicanv1alpha1.Barbican{},
		&barbicanv1alpha1.BarbicanSecretStore{},
		&openbaov1alpha1.OpenBaoCluster{},
		&openbaov1alpha1.OpenBaoTenant{},
		rabbitmq,
	} {
		if isServed(obj) {
			b = b.Owns(obj, engageLocal, engageNoProviders).
				Watches(obj, crossNamespaceChildHandler(),
					mcbuilder.WithPredicates(crossNamespaceChildPredicate()), engageLocal, engageNoProviders)
		}
	}
	// KeystoneIdentityBackend CRs are authored by the operator, not projected
	// by the ControlPlane, so they carry no owner reference an Owns() could
	// match. Watch them by keystoneRef so attaching (or detaching, or the
	// backend reaching Ready) re-projects the Horizon websso choices and the
	// Keystone trusted_dashboard without waiting for a periodic resync.
	if isServed(&keystonev1alpha1.KeystoneIdentityBackend{}) {
		b = b.Watches(&keystonev1alpha1.KeystoneIdentityBackend{}, commonmulticluster.LocalRequests(
			r.identityBackendToControlPlaneMapper,
		), engageLocal, engageNoProviders)
	}
	// OVNCentral CRs are deployed infra-style and only referenced by
	// services.neutron.ovn.centralRef, so they carry no owner reference an Owns()
	// could match. Watch them through the mapper so a change to the central's
	// status re-runs reconcileOVN instead of waiting for a periodic resync.
	if isServed(&ovnv1alpha1.OVNCentral{}) {
		b = b.Watches(&ovnv1alpha1.OVNCentral{}, commonmulticluster.LocalRequests(
			r.ovnCentralToControlPlaneMapper,
		), engageLocal, engageNoProviders)
	}

	if len(missing) > 0 {
		msgs := make([]string, 0, len(missing))
		for _, gvk := range missing {
			msgs = append(msgs, gvk.String())
		}
		ctrl.Log.WithName("setup").Info(
			"skipping watches for optional CRDs not served by the API server; a leader-gated re-check will restart the operator when one appears",
			"missingKinds", msgs,
		)
		// The gate rides the LOCAL manager: it is a plain leader-gated runnable
		// over the management cluster's discovery client, not a cluster-aware one
		// the multicluster manager could engage per target.
		if err := local.Add(&crdWatchGate{disco: disco, missing: missing, interval: crdRecheckInterval}); err != nil {
			return nil, fmt.Errorf("registering crdWatchGate for missing optional CRDs: %w", err)
		}
	}

	return b, nil
}
