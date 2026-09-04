// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strconv"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// The projected Neutron CR is named "{controlplane.Name}-neutron", the same
// deterministic, collision-free naming convention as the Keystone, Horizon,
// Glance, Placement, and Barbican children (see keystoneNameSuffix), and lives in
// cp.NeutronNamespace(): the ControlPlane's own namespace by default, or the one
// services.neutron.namespace assigns.

// neutronNameSuffix is appended to the ControlPlane name to derive the name of
// the projected Neutron CR (and, through it, its credential and registration
// objects).
const neutronNameSuffix = "-neutron"

// defaultNeutronRepository is the canonical Neutron image repository; the tag is
// derived from spec.openStackRelease unless spec.services.neutron.image overrides
// the whole image reference.
const defaultNeutronRepository = "ghcr.io/c5c3/neutron"

// neutronDeletionAllowedAnnotation, when set to a truthy value on a ControlPlane,
// opts that ControlPlane in to tearing down a previously-projected Neutron child
// (and its DB-credential ExternalSecret and messaging Secrets) when
// spec.services.neutron is unset. The preserve-by-default posture mirrors the
// Keystone/Horizon/Glance/Placement/Barbican annotations for a consistent operator
// UX: an accidental block drop never silently removes a running service.
const neutronDeletionAllowedAnnotation = "c5c3.io/allow-neutron-deletion"

// defaultNeutronDatabaseName is the logical database name the Neutron schema
// always lives in, regardless of whether Neutron shares the ControlPlane's
// database cluster or takes a dedicated one. It is also the one schema the
// pre-wired OpenBao engine role grants on, so it is not a per-ControlPlane knob.
const defaultNeutronDatabaseName = "neutron"

// neutronName returns the name of the Neutron CR the reconciler projects for the
// given ControlPlane (see neutronNameSuffix).
func neutronName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + neutronNameSuffix
}

// neutronDeletionAllowed reports whether cp opts in to deleting its projected
// Neutron child when spec.services.neutron is unset, via a truthy
// neutronDeletionAllowedAnnotation. A missing, malformed, or non-truthy value
// means "preserve".
func neutronDeletionAllowed(cp *c5c3v1alpha1.ControlPlane) bool {
	allowed, err := strconv.ParseBool(cp.Annotations[neutronDeletionAllowedAnnotation])
	return err == nil && allowed
}

// neutronKeystoneEndpoint returns the Keystone endpoint URL projected into the
// Neutron child's spec.keystoneEndpoint. Neutron validates every token
// server-side against this URL, so what the Neutron pods can reach decides it:
// the cluster Neutron is placed on against the one Keystone runs on, the rule
// keystoneEndpointFor holds for every service.
func neutronKeystoneEndpoint(cp *c5c3v1alpha1.ControlPlane) string {
	return keystoneEndpointFor(cp, cp.NeutronTargetClusterRef())
}

// neutronEndpointURL renders the in-cluster URL of the projected Neutron API
// Service by naming convention (the Neutron API listens on 9696), the
// cross-service endpoint contract the catalog registers against.
func neutronEndpointURL(cp *c5c3v1alpha1.ControlPlane) string {
	return managedServiceURL(neutronName(cp), cp.NeutronNamespace(), 9696, "")
}

// reconcileNeutron projects spec.services.neutron into an owned Neutron CR and
// drives the NeutronReady condition.
//
// The sub-reconciler is GATED on KeystoneReady (Neutron validates every token
// against the ControlPlane's Keystone child), on OVNReady (the ML2/OVN mechanism
// driver writes every network into the referenced central's Northbound database),
// and on the KeystoneService child it projects for Neutron (Neutron authenticates
// as the Keystone user that registration provisions). Once gated through, it
// delivers the shared message bus into the network service's namespace, ensures
// the DB-credential ExternalSecret, projects the Neutron CR (database/cache
// DeepCopied from the resolved backing services, the Keystone endpoint derived
// top-down through neutronKeystoneEndpoint), and folds both children's readiness
// into NeutronReady.
func (r *ControlPlaneReconciler) reconcileNeutron(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// spec.services.neutron is optional. When unset, this ControlPlane manages no
	// network service and reports NeutronReady as not-managed so the aggregate
	// Ready condition is not blocked (staged adoption). A previously-projected child
	// is preserved unless the ControlPlane opts in to deletion.
	if cp.Spec.Services.Neutron == nil {
		message := "spec.services.neutron is unset; no Neutron service is managed by this ControlPlane"
		if neutronDeletionAllowed(cp) {
			if err := r.deleteOrphanedNeutron(ctx, cp); err != nil {
				return ctrl.Result{}, err
			}
		} else {
			// Preserve the child, but NEVER the credential minter. A live
			// VaultDynamicSecret keeps issuing a fresh MySQL user with ALL PRIVILEGES
			// on the neutron schema at every refresh interval, indefinitely, for a
			// service this ControlPlane has been told it no longer manages: no
			// consumer, no revocation, and a NeutronReady=True/NeutronNotManaged
			// condition that surfaces none of it. Preserving a running service does
			// not imply preserving the generator behind its credentials, so the
			// dynamic objects come down either way.
			//
			// NeutronNamespace() still resolves correctly here: removing a
			// services.neutron.namespace assignment is rejected by
			// validateServiceNamespacesImmutable, so the only admissible way to reach
			// this branch with a live generator is the co-located one, where the
			// generator sits in the ControlPlane's own namespace.
			r.deleteDynamicDBCredentialObjects(ctx, cp, neutronDBCredentialTarget(cp))
			message += fmt.Sprintf("; any previously-projected Neutron child is preserved "+
				"(set annotation %s=true to allow deletion), but its dynamic DB-credential generator is torn down",
				neutronDeletionAllowedAnnotation)
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeNeutronReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cp.Generation,
			Reason:             "NeutronNotManaged",
			Message:            message,
		})
		return ctrl.Result{}, nil
	}

	// Resolve the backing services Neutron actually talks to: its own dedicated
	// instances when it opted into them, the ControlPlane-wide shared ones
	// otherwise.
	//
	// Nil-safety fail-safe. The projection DeepCopies these, so an unresolvable
	// instance has nothing to project and the deref below would panic; the shared
	// bus is dereferenced for its TLS block on the same path. The validating
	// webhook requires spec.infrastructure with a messaging block outside External
	// mode (and forbids services.neutron in External mode), so this only fires for
	// a webhook-bypassed CR.
	database := effectiveNeutronDatabase(cp)
	cache := effectiveNeutronCache(cp)
	if database == nil || cache == nil ||
		cp.Spec.Infrastructure == nil || cp.Spec.Infrastructure.Messaging == nil {
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	}

	// Gate on KeystoneReady.
	if !conditions.AllTrue(cp.Status.Conditions, conditionTypeKeystoneReady) {
		logger.Info("Keystone not ready, deferring Neutron projection")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeNeutronReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForKeystone",
			Message:            "KeystoneReady is not True; Neutron projection deferred",
		})
		return ctrl.Result{RequeueAfter: keystoneInfraGateRequeueAfter}, nil
	}

	// Gate on OVNReady. A Neutron pointed at a central whose databases do not serve
	// cannot program a single network, so the projection waits for the verdict
	// reconcileOVN mirrored earlier in this same pass.
	if !conditions.AllTrue(cp.Status.Conditions, conditionTypeOVNReady) {
		logger.Info("OVN not ready, deferring Neutron projection")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeNeutronReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForOVN",
			Message:            "OVNReady is not True; Neutron projection deferred",
		})
		return ctrl.Result{RequeueAfter: keystoneInfraGateRequeueAfter}, nil
	}

	// Deliver the ControlPlane-wide bus into the namespace (and onto the cluster)
	// the network service runs in. The transport URL's digest is not projected: the
	// neutron operator rolls its pods off the Secret it derives itself, so a second
	// digest on the child would only add a redundant rollout trigger.
	if msgRes, halt, err := r.reconcileNeutronMessaging(ctx, cp); halt {
		return msgRes, err
	}

	// Register Neutron against the identity plane: one KeystoneService child
	// carrying the network catalog entry and the service account Neutron
	// authenticates as, mirrored onto a placed Neutron's cluster and gated on the
	// account it provisions.
	child, regRes, halt, err := r.reconcileBuiltinRegistration(ctx, cp, desiredNeutronRegistration(cp),
		"Neutron", conditionTypeNeutronReady)
	if halt {
		return regRes, err
	}

	// The EFFECTIVE credentials mode of the database Neutron connects to, resolved
	// once so the credential projection below, its readiness gate, and the mode
	// stamped onto the child further down can never disagree.
	dynamic := database.ClusterRef != nil && neutronDBCredentialsDynamicEnabled(cp)

	// Ensure the DB-credential objects BEFORE the child so the Secret it references
	// exists when the neutron-operator resolves it. Managed only: a brownfield
	// database (ClusterRef nil) carries a user-supplied credential out-of-band, so
	// there is nothing for the operator to project. In Dynamic mode the shared
	// helper also holds the projection until an engine-issued credential has landed
	// (see ensureServiceDBCredential).
	if database.ClusterRef != nil {
		res, halt, err := r.ensureServiceDBCredential(ctx, cp, neutronDBCredentialTarget(cp),
			dynamic, "Neutron", conditionTypeNeutronReady)
		if halt {
			return res, err
		}
	}

	// Resolve the Neutron image. spec.services.neutron.image overrides the
	// release-derived default when set.
	image := commonv1.ImageSpec{
		Repository: defaultNeutronRepository,
		Tag:        cp.Spec.OpenStackRelease,
	}
	if override := cp.Spec.Services.Neutron.Image; override != nil {
		image = *override
	}

	// Place the child in the namespace assigned to the network service (the
	// ControlPlane's own unless services.neutron.namespace says otherwise). A child
	// outside the ControlPlane's namespace can carry no owner reference, so it is
	// stamped with the ownership labels and applied unowned.
	neutronNS := cp.NeutronNamespace()
	crossNamespace := neutronNS != cp.Namespace
	nn := &neutronv1alpha1.Neutron{
		ObjectMeta: metav1.ObjectMeta{
			Name:      neutronName(cp),
			Namespace: neutronNS,
		},
	}
	if crossNamespace {
		stampControlPlaneChildLabels(nn, cp)
	}

	nn.Spec.OpenStackRelease = cp.Spec.OpenStackRelease
	nn.Spec.Image = image

	// Thread the service's target cluster onto the child verbatim; a nil source
	// yields nil and leaves the child unplaced (see the Keystone projection).
	nn.Spec.TargetClusterRef = cp.Spec.Services.Neutron.TargetClusterRef.DeepCopy()

	// Project the merged extraConfig (globalExtraConfig unioned with the
	// per-service block, per-service winning key by key). Assigned
	// unconditionally, following the revert-on-clear convention: a nil merge keeps
	// the SSA-applied intent free of spec.extraConfig, so a direct edit on the
	// child stays unowned until a ControlPlane block is set, and clearing the
	// ControlPlane block reverts the child rather than pinning the last value.
	nn.Spec.ExtraConfig = c5c3v1alpha1.MergedExtraConfig(
		cp.Spec.GlobalExtraConfig, cp.Spec.Services.Neutron.ExtraConfig)

	// Point Neutron at the SAME backing services the ControlPlane provisioned. The
	// logical database is always "neutron": its own schema keeps it isolated from
	// Keystone's on a shared cluster, and it is the one schema the pre-wired
	// OpenBao engine role grants on. DeepCopy (over a plain struct copy) is
	// required because DatabaseSpec/CacheSpec carry pointer fields, so a shallow
	// copy would alias cp.Spec.
	nn.Spec.Database = *database.DeepCopy()
	nn.Spec.Database.Database = defaultNeutronDatabaseName
	// In managed mode the operator OWNS the neutron DB credential: reconcileNeutron
	// materialises it (above) into a per-ControlPlane Secret named
	// neutronDBCredentialSecretName(cp). Override the projected Neutron CR's
	// database.secretRef to that operator-owned Secret (key "password"), and
	// project the EFFECTIVE credentials mode: Dynamic (engine-issued) is the default
	// on the managed shared database, a per-service override or the shared Static
	// opt-out flips it to Static, and a dedicated neutron database stays Static (no
	// engine role can mint its credentials). Brownfield (ClusterRef nil) leaves the
	// user-supplied secretRef and credentialsMode in place.
	if database.ClusterRef != nil {
		nn.Spec.Database.SecretRef = commonv1.SecretRefSpec{Name: neutronDBCredentialSecretName(cp), Key: "password"}
		if dynamic {
			nn.Spec.Database.CredentialsMode = commonv1.CredentialsModeDynamic
		} else {
			nn.Spec.Database.CredentialsMode = commonv1.CredentialsModeStatic
		}
	}

	nn.Spec.Cache = *cache.DeepCopy()

	// The Keystone endpoint is derived top-down from the ControlPlane rather than
	// read from the Keystone child's status: no machine consumer reads status
	// endpoints per the settled convention. keystonePublicEndpoint is the
	// browser/client-facing URL Neutron advertises on a 401 (empty when Keystone is
	// not externally exposed, in which case the child falls back to the internal
	// endpoint).
	nn.Spec.KeystoneEndpoint = neutronKeystoneEndpoint(cp)
	nn.Spec.KeystonePublicEndpoint = keystonePublicEndpoint(cp.Spec.Services.Keystone)

	nn.Spec.Region = cp.Spec.Region

	// The Keystone service user Neutron authenticates as, the account the
	// registration child provisions: user and project as declared on that child,
	// both domains the ControlPlane's effective admin domain (which the
	// registration resolves the same way, its own domainName being unset), and the
	// password read from the consumer Secret the registration delivers.
	nn.Spec.ServiceUser = neutronv1alpha1.ServiceUserSpec{
		Username:          c5c3v1alpha1.NeutronServiceAccountName,
		ProjectName:       c5c3v1alpha1.NeutronServiceProjectName,
		UserDomainName:    adminDomainName(cp),
		ProjectDomainName: adminDomainName(cp),
		SecretRef: commonv1.SecretRefSpec{
			Name: keystoneServiceCredentialsSecretName(child),
			Key:  "password",
		},
	}

	// Project the ControlPlane's RESOLVED store selection onto the Neutron child so
	// it never falls back to its own shared-cluster-store default.
	nn.Spec.SecretStoreRef = effectiveControlPlaneStoreRefPtr(cp)

	// DeepCopy for the same aliasing reason as Database above; a nil source yields
	// nil, clearing any previously-projected gateway so removal tears the HTTPRoute
	// down. The Neutron CRD's GatewaySpec is an alias of the shared commonv1 type,
	// so the ControlPlane's block is projected as it stands.
	nn.Spec.Gateway = cp.Spec.Services.Neutron.Gateway.DeepCopy()

	// Resolve replicas to the shared operator default, then let an override win.
	// Assigning unconditionally means clearing services.neutron.replicas reverts
	// the child to the default instead of leaving the previously-projected value
	// pinned on the fetched child. The RPC workers carry their own count on the
	// same terms.
	nn.Spec.Deployment.Replicas = commonv1.DefaultReplicas
	if cp.Spec.Services.Neutron.Replicas != nil {
		nn.Spec.Deployment.Replicas = *cp.Spec.Services.Neutron.Replicas
	}
	nn.Spec.Workers.Deployment.Replicas = commonv1.DefaultReplicas
	if cp.Spec.Services.Neutron.WorkerReplicas != nil {
		nn.Spec.Workers.Deployment.Replicas = *cp.Spec.Services.Neutron.WorkerReplicas
	}

	// The shared bus reaches the child as a BROWNFIELD secretRef naming the Secret
	// reconcileNeutronMessaging wrote beside it: the neutron operator resolves
	// spec.messaging in the Neutron's own namespace on the Neutron's own cluster,
	// which is where that delivery lands. The CA mirror follows the same rule, and
	// only when the bus declares TLS at all.
	nn.Spec.Messaging = commonv1.MessagingSpec{
		SecretRef: &commonv1.SecretRefSpec{
			Name: neutronMessagingSecretName(cp),
			Key:  commonv1.DefaultTransportURLSecretKey,
		},
	}
	if cp.Spec.Infrastructure.Messaging.TLS != nil {
		nn.Spec.Messaging.TLS = &commonv1.MessagingTLSSpec{
			CABundleSecretRef: commonv1.SecretRefSpec{
				Name: neutronMessagingCASecretName(cp),
				Key:  neutronMessagingCAKey,
			},
		}
	}

	// The OVN control plane the ML2/OVN mechanism driver programs, with the
	// namespace RESOLVED here rather than passed through: an empty ref namespace
	// means the ControlPlane's own namespace, which is not the namespace the child
	// would default it to once it is placed elsewhere.
	nn.Spec.OVN = neutronv1alpha1.OVNSpec{
		CentralRef: neutronv1alpha1.OVNCentralRef{
			Name:      cp.Spec.Services.Neutron.OVN.CentralRef.Name,
			Namespace: cp.NeutronOVNCentralNamespace(),
		},
	}

	// spec.apiServer, spec.ovnDBSync, spec.networkPolicy, spec.autoscaling and
	// spec.logging are deliberately NOT set, the Placement posture: the child-side
	// defaults stay authoritative, and tuning them stays a standalone-CR concern.

	res, err := commonreconcile.ProjectChild(ctx, r.Client, r.Scheme, cp,
		commonreconcile.ChildProjectionParams[*neutronv1alpha1.Neutron]{
			Child:          nn,
			ConditionType:  conditionTypeNeutronReady,
			ReadyReason:    "NeutronReady",
			ReadyMessage:   "Projected Neutron CR is ready",
			WaitingReason:  "WaitingForNeutron",
			WaitingMessage: fmt.Sprintf("Neutron %q is not ready", nn.Name),
			// An Invalid (HTTP 422) rejection from the Neutron API server means the
			// projected spec violates a CRD/webhook rule: surface a distinct,
			// actionable reason so the wedge is diagnosable from the condition.
			RejectedReason: "NeutronProjectionRejected",
			RejectedMessage: func(err error) string {
				return fmt.Sprintf("Neutron API server rejected the projected spec; reconcile the ControlPlane spec "+
					"to a valid projection to recover: %v", err)
			},
			ErrorReason:     "NeutronError",
			ErrorMessage:    func(err error) string { return fmt.Sprintf("create-or-update Neutron: %v", err) },
			WaitRequeue:     infraRequeueAfter,
			Conditions:      &cp.Status.Conditions,
			Generation:      cp.Generation,
			ChildConditions: func(n *neutronv1alpha1.Neutron) []metav1.Condition { return n.Status.Conditions },
			Unowned:         crossNamespace,
		})
	if err != nil {
		return res, err
	}

	if !res.IsZero() {
		return res, nil
	}

	// The apply above is what removed spec.messaging.tls from the child when the
	// shared bus dropped its tls block — but only from the CR. The live Deployment
	// keeps mounting the mirror as a REQUIRED Secret volume source until the
	// neutron-operator has re-rendered it on a pass of its own, so a reap that fires
	// before then wedges every pod created in that window on FailedMount, and a
	// neutron-operator that is down or backing off turns the window into a permanent
	// one. The reap therefore waits for the child's own verdict on the pointer-free
	// spec: the server's view of the child names no CA bundle any more, and it has
	// reconciled the generation the apply produced. The readiness return above is
	// the other half of that verdict — a child whose pipeline short-circuited before
	// the Deployment step stamps observedGeneration anyway, but reports Ready=False
	// while it does. The status write that carries the verdict wakes this
	// ControlPlane again, so the reap is one watch away.
	//
	// It runs here and NOT on the messaging leg, which runs ahead of every gate that
	// can halt this pass with the pointer still live. See pruneNeutronMessagingCA.
	if cp.Spec.Infrastructure.Messaging.TLS == nil && nn.Spec.Messaging.TLS == nil &&
		nn.Status.ObservedGeneration >= nn.Generation {
		if pruneRes, halt, perr := r.pruneNeutronMessagingCA(ctx, cp); halt {
			return pruneRes, perr
		}
	}

	// The Neutron child is ready. NeutronReady still folds in the registration: a
	// running Neutron whose catalog entry never landed is reachable by nothing that
	// discovers it through the catalog, and the ControlPlane must not report the
	// network service as ready for it.
	if readyRes, pending := foldBuiltinRegistrationReady(cp, child, conditionTypeNeutronReady); pending {
		return readyRes, nil
	}
	return res, nil
}

// deleteOrphanedNeutron removes a previously-projected Neutron child, the
// DB-credential ExternalSecret, the two messaging Secrets, and the KeystoneService
// registration that follow it, when spec.services.neutron is unset AND the
// ControlPlane has opted in to deletion via neutronDeletionAllowedAnnotation (the
// caller gates this). Each object is only deleted when this ControlPlane still
// owns it (by owner reference in its own namespace, by the ownership labels in a
// service namespace); a foreign object colliding on a name is left alone.
//
// The referenced OVNCentral is never touched: it is deployed outside the plane
// and only read (see reconcileOVN).
//
// Deleting the registration is what removes Neutron from the Keystone catalog and
// from the identity plane: the KeystoneService controller's finalizer tears down
// the catalog rows, the service user and its project behind it.
func (r *ControlPlaneReconciler) deleteOrphanedNeutron(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) error {
	neutronNS := cp.NeutronNamespace()

	// The Dynamic-mode client Certificate has no Go type, so it is addressed
	// unstructured.
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	cert.SetName(neutronDBCredentialClientCertName(cp))
	cert.SetNamespace(neutronNS)

	children := []client.Object{
		// The Neutron child.
		&neutronv1alpha1.Neutron{
			ObjectMeta: metav1.ObjectMeta{Name: neutronName(cp), Namespace: neutronNS},
		},
		// The DB-credential ExternalSecret.
		&esov1.ExternalSecret{
			ObjectMeta: metav1.ObjectMeta{Name: neutronDBCredentialSecretName(cp), Namespace: neutronNS},
		},
		// The Dynamic-mode DB-credential objects: the VaultDynamicSecret generator,
		// its mTLS client Certificate, and the ServiceAccount whose token it
		// authenticates with.
		&esgenv1alpha1.VaultDynamicSecret{
			ObjectMeta: metav1.ObjectMeta{Name: neutronDBCredentialSecretName(cp), Namespace: neutronNS},
		},
		cert,
		&corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: neutronDBCredentialServiceAccountName, Namespace: neutronNS},
		},
		// The bus delivery: the brownfield transport-URL Secret and the CA mirror
		// beside it. Nothing else writes them, so an unmanaged service leaves no
		// broker credential behind in the namespace.
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: neutronMessagingSecretName(cp), Namespace: neutronNS},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: neutronMessagingCASecretName(cp), Namespace: neutronNS},
		},
	}
	for _, child := range children {
		if err := commonreconcile.DeleteOrphanedChildFunc(ctx, r.Client, child, func(live client.Object) bool {
			return isControlPlaneChild(live, cp)
		}); err != nil {
			return err
		}
	}

	// The KeystoneService registration. It lives beside the service, on the
	// management cluster whatever cluster Neutron runs on. The credential mirror a
	// PLACED service carries is not swept here: like every object this function
	// names it is resolved through NeutronNamespace(), which without a
	// services.neutron block is the ControlPlane's own namespace, so this sweep
	// reaches co-located objects only. The mirror is reaped by the ControlPlane
	// teardown, which sweeps a placed namespace's label-owned ExternalSecrets on the
	// target cluster.
	registration := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: neutronName(cp), Namespace: neutronNS},
	}
	return commonreconcile.DeleteOrphanedChildFunc(ctx, r.Client, registration, func(live client.Object) bool {
		return isControlPlaneChild(live, cp)
	})
}
