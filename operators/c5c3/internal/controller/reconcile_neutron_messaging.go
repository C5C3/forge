// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// neutronMessagingCAKey is the data key the CA mirror carries the broker's CA
// bundle under, and the key the projected Neutron's messaging TLS block names.
const neutronMessagingCAKey = "ca.crt"

// neutronMessagingSecretName returns the name of the brownfield transport-URL
// Secret the ControlPlane writes beside the Neutron child
// ("<cp>-neutron-messaging").
//
// The name is the ControlPlane's own on purpose. In that same namespace the
// neutron operator claims messaging.TransportURLSecretName(neutronName(cp))
// ("<cp>-neutron-transport-url") for the Secret it derives from
// spec.messaging.secretRef, so writing the bus under that name would leave two
// controllers rewriting one object on every pass.
func neutronMessagingSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return neutronName(cp) + "-messaging"
}

// neutronMessagingCASecretName returns the name of the CA mirror that carries
// the broker's CA bundle into the Neutron namespace
// ("<cp>-neutron-messaging-ca"), under the same
// name-of-our-own rule as neutronMessagingSecretName.
func neutronMessagingCASecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return neutronName(cp) + "-messaging-ca"
}

// reconcileNeutronMessaging delivers the ControlPlane-wide message bus
// (spec.infrastructure.messaging) into the namespace the network service runs in.
//
// The bus is declared in the ControlPlane's own namespace and read there, on the
// management cluster. Neutron may run somewhere else: in a namespace of its own,
// or on a target cluster. The neutron operator resolves spec.messaging in the
// Neutron's namespace on the Neutron's cluster and would find nothing there, so
// the ControlPlane resolves the bus itself and hands the child a brownfield
// secretRef pointing at neutronMessagingSecretName(cp), written on the client
// that namespace resolves to. A managed clusterRef and a brownfield secretRef
// both arrive as the same one Secret.
//
// A placed Neutron receives the URL the bus declares, whatever cluster it runs
// on. Reaching a broker across a cluster boundary is the bus operator's concern,
// and the neutron operator's own D11 rule keeps a pure-OVN Neutron from dialing
// the URL at all.
//
// halt=true means the caller returns res and err as they are and projects no
// child: either the material is not there yet (res requeues, err is nil) or the
// write failed (err is non-nil). halt=false means the URL was delivered. Every
// condition this pass writes is on NeutronReady.
func (r *ControlPlaneReconciler) reconcileNeutronMessaging(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) (res ctrl.Result, halt bool, err error) {
	failed := conditionFailer(cp, conditionTypeNeutronReady)

	// The caller only calls this for a ControlPlane that declares a bus, so a
	// missing block is a programming error rather than a state to wait out.
	if cp.Spec.Infrastructure == nil || cp.Spec.Infrastructure.Messaging == nil {
		err := errors.New("spec.infrastructure.messaging is nil")
		failed("NeutronMessagingError", err.Error())
		return ctrl.Result{}, true, err
	}

	transportURL, _, waitMessage, err := messaging.ResolveTransportURL(ctx, messaging.TransportURLSecretFlowParams{
		Client:    r.Client,
		Namespace: cp.Namespace,
		Messaging: cp.Spec.Infrastructure.Messaging,
	})
	if err != nil {
		failed("NeutronMessagingError", err.Error())
		return ctrl.Result{}, true, fmt.Errorf("resolving the shared bus transport URL: %w", err)
	}
	if waitMessage != "" {
		// The RabbitmqCluster, its default-user Secret or the brownfield Secret is
		// not there yet. Nothing is written, so the child never sees a partial URL.
		failed(messaging.ReasonWaitingForMessagingCredentials, waitMessage)
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, true, nil
	}

	namespace := cp.NeutronNamespace()
	children, err := r.childrenClientFor(ctx, cp, namespace)
	if err != nil {
		failed(commonmulticluster.TargetClusterUnavailable, err.Error())
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, true, nil
	}

	name := neutronMessagingSecretName(cp)
	if serr := r.ensureOwnedSecret(ctx, children, cp, name, namespace, func(secret *corev1.Secret) error {
		secret.Data[commonv1.DefaultTransportURLSecretKey] = []byte(transportURL)
		return nil
	}); serr != nil {
		failed("NeutronMessagingError", serr.Error())
		return ctrl.Result{}, true, fmt.Errorf("writing the neutron messaging Secret %s/%s: %w",
			namespace, name, serr)
	}

	tls := cp.Spec.Infrastructure.Messaging.TLS
	if tls == nil {
		// A plaintext bus leaves no mirror behind, but the mirror is NOT reaped
		// here: the live child still names it, and the projection that drops that
		// pointer is several halting gates further down the pass.
		// pruneNeutronMessagingCA runs on the far side of it.
		return ctrl.Result{}, false, nil
	}

	bundleName := tls.CABundleSecretRef.Name
	bundleKey := tls.CABundleSecretRef.Key
	if bundleKey == "" {
		bundleKey = c5c3v1alpha1.DefaultCABundleSecretKey
	}

	// The bundle is read beside the bus it belongs to, on the management cluster,
	// and mirrored to wherever Neutron runs.
	bundleSecret := &corev1.Secret{}
	switch gerr := r.Get(ctx, client.ObjectKey{Namespace: cp.Namespace, Name: bundleName}, bundleSecret); {
	case apierrors.IsNotFound(gerr):
		failed("WaitingForMessagingCABundle", fmt.Sprintf(
			"messaging CA bundle Secret %s/%s (key %q) named by spec.infrastructure.messaging.tls.caBundleSecretRef "+
				"does not exist", cp.Namespace, bundleName, bundleKey))
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, true, nil
	case gerr != nil:
		failed("NeutronMessagingError", gerr.Error())
		return ctrl.Result{}, true, fmt.Errorf("reading messaging CA bundle Secret %s/%s: %w",
			cp.Namespace, bundleName, gerr)
	}

	// An empty key is the ordinary transient of a two-step "create the Secret,
	// then populate it" flow, so it waits exactly as a missing Secret does rather
	// than mirroring an empty trust anchor.
	bundle := bundleSecret.Data[bundleKey]
	if len(bundle) == 0 {
		failed("WaitingForMessagingCABundle", fmt.Sprintf(
			"messaging CA bundle Secret %s/%s carries no data under key %q", cp.Namespace, bundleName, bundleKey))
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, true, nil
	}

	caName := neutronMessagingCASecretName(cp)
	if serr := r.ensureOwnedSecret(ctx, children, cp, caName, namespace, func(secret *corev1.Secret) error {
		secret.Data[neutronMessagingCAKey] = bundle
		return nil
	}); serr != nil {
		failed("NeutronMessagingError", serr.Error())
		return ctrl.Result{}, true, fmt.Errorf("writing the neutron messaging CA Secret %s/%s: %w",
			namespace, caName, serr)
	}

	return ctrl.Result{}, false, nil
}

// pruneNeutronMessagingCA removes the CA mirror once the shared bus no longer
// declares TLS. A plaintext bus must leave no trust anchor behind in the Neutron
// namespace, and dropping the tls block has to converge that namespace rather
// than pin the last mirrored bundle.
//
// It is deliberately NOT part of reconcileNeutronMessaging, which runs BEFORE the
// projection. The live child's spec.messaging.tls names this Secret as a volume
// source, and every gate between the two — the KeystoneService registration mid
// service-account rotation, a Dynamic DB credential mid rotation, a transient API
// error — halts the pass with the pointer still in place. Deleting the mirror
// there would leave the child naming a Secret that no longer exists, so every
// Neutron pod that restarts in that window wedges on CreateContainerConfigError,
// with nothing in the condition set naming the cause.
//
// Removing the pointer from the CR is not enough either: the workload that mounts
// the mirror is rendered by the neutron-operator, one pass behind the apply. The
// caller therefore runs this only once the child reports having converged on the
// pointer-free spec — see the gate in reconcileNeutron — so the referent never
// outlives its last reference.
//
// A same-named Secret this ControlPlane never wrote is left alone. halt has the
// same meaning as in reconcileNeutronMessaging, and every condition is on
// NeutronReady.
func (r *ControlPlaneReconciler) pruneNeutronMessagingCA(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) (res ctrl.Result, halt bool, err error) {
	failed := conditionFailer(cp, conditionTypeNeutronReady)

	namespace := cp.NeutronNamespace()
	children, err := r.childrenClientFor(ctx, cp, namespace)
	if err != nil {
		failed(commonmulticluster.TargetClusterUnavailable, err.Error())
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, true, nil
	}

	caName := neutronMessagingCASecretName(cp)
	if derr := commonreconcile.DeleteOrphanedChildFunc(ctx, children, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: caName, Namespace: namespace},
	}, func(live client.Object) bool { return isControlPlaneChild(live, cp) }); derr != nil {
		failed("NeutronMessagingError", derr.Error())
		return ctrl.Result{}, true, fmt.Errorf("deleting the stale neutron messaging CA Secret %s/%s: %w",
			namespace, caName, derr)
	}
	return ctrl.Result{}, false, nil
}
