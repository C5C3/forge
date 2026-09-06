// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import "time"

// Requeue intervals used by the ControlPlane sub-reconcilers while waiting on a
// projected child CR to converge. Centralised here
// so the wait cadence is consistent and tunable in one place.
const (
	// namespaceRequeueAfter is the backoff the Namespaces sub-reconciler uses
	// while a dedicated service namespace is not usable yet: an External one that
	// has not been provisioned, a Managed one still Terminating, or one whose name
	// is taken by a namespace the operator refuses to adopt. All three are
	// resolved out-of-band (or not at all), so the cadence is unhurried — the
	// condition, not the requeue rate, is what tells the operator what to do.
	namespaceRequeueAfter = 15 * time.Second

	// infraRequeueAfter is the backoff used while a managed MariaDB/Memcached
	// child is still converging to Ready.
	infraRequeueAfter = 15 * time.Second

	// esoTenantStoreRequeueAfter is the backoff the ESO-tenant-store sub-reconciler
	// uses while waiting for the per-ControlPlane SecretStore to reach Ready (the
	// mTLS client cert to be issued and ESO to validate the OpenBao backend). It
	// short-circuits the pipeline so the store-consuming sub-reconcilers do not
	// run before the store they default onto exists.
	esoTenantStoreRequeueAfter = 10 * time.Second

	// dbCredentialsRequeueAfter is the backoff the DB-credentials sub-reconciler
	// uses while waiting for the per-ControlPlane DB-credential ExternalSecret to
	// sync to Ready. DECISION: a dedicated named
	// constant (rather than reusing korcRequeueAfter) matches the
	// per-sub-reconciler naming convention already established here, so the wait
	// cadence of each sub-reconciler is independently documented and tunable.
	dbCredentialsRequeueAfter = 10 * time.Second

	// adminPasswordRequeueAfter is the backoff the AdminPassword sub-reconciler
	// uses while waiting for the per-ControlPlane admin-password ExternalSecret to
	// sync to Ready. DECISION: a dedicated named constant
	// (rather than reusing korcRequeueAfter) matches the per-sub-reconciler naming
	// convention already established here, so the wait cadence of each
	// sub-reconciler is independently documented and tunable.
	adminPasswordRequeueAfter = 10 * time.Second

	// keystoneInfraGateRequeueAfter is the short backoff used while the Keystone
	// sub-reconciler is gated on InfrastructureReady; it is small so the Keystone
	// CR is projected promptly once the infrastructure converges.
	keystoneInfraGateRequeueAfter = 5 * time.Second

	// korcRequeueAfter is the backoff used by the K-ORC / admin-credential /
	// catalog sub-reconcilers while waiting on a gate (KORCReady,
	// AdminCredentialReady, the K-ORC clouds.yaml ExternalSecret) or on a K-ORC
	// child CR (ApplicationCredential/Service/Endpoint) to converge, and while a
	// missing K-ORC CRD keeps the sub-reconciler from making progress
	korcRequeueAfter = 10 * time.Second

	// credentialRotationWaitInterval is the short backoff the CredentialRotation
	// reconciler uses while waiting for the ControlPlane reconciler to mint the
	// admin ApplicationCredential CR (Bootstrap) or for a ControlPlane / admin
	// password Secret to appear.
	credentialRotationWaitInterval = 10 * time.Second

	// remintStallTimeout bounds how long the admin ApplicationCredential may stay
	// Terminating during a re-mint before reconcileKORC escalates KORCReady from
	// the transient "ReMinting" reason to "ReMintStalled". A stuck finalizer (e.g.
	// K-ORC cannot reach Keystone to revoke the old credential) otherwise loops on
	// "ReMinting" indefinitely with no operator-visible signal. The window is
	// generous so a slow-but-progressing revoke is not flagged as stalled.
	remintStallTimeout = 5 * time.Minute

	// cloudsYamlSyncStuckTimeout bounds how long the materialised K-ORC clouds.yaml
	// Secret may disagree with the freshly assembled admin credential before
	// reconcileAdminCredential escalates AdminCredentialReady from the transient
	// "WaitingForCloudsYamlSync" reason to the alertable "CloudsYamlSyncStuck". A
	// never-converging ESO/OpenBao sync otherwise looks identical to a 2-second
	// transient miss, so on-call waits forever with no terminal signal. Measured
	// from the credential's LastRotation (when the current id was minted); the
	// window is generous so a healthy force-sync is never flagged as stuck.
	cloudsYamlSyncStuckTimeout = 15 * time.Minute

	// externalImportStallGrace bounds how long an External-mode K-ORC admin
	// import (Domain/User) may report Available=False on "Waiting for OpenStack
	// resource to be created externally" before reconcileKORC escalates KORCReady
	// from the transient "WaitingForApplicationCredential" reason to the alertable
	// "ImportStalled". In External mode every import target pre-exists by
	// definition, so a persistent wait is a misconfiguration (a wrong
	// external.endpointType or spec.region resolving to a different Keystone), not
	// a resource that is about to appear. Two minutes is deliberately shorter than
	// remintStallTimeout / orcTeardownStallTimeout (both 5m) — those wait on work
	// that is genuinely in flight, whereas a resolvable import has nothing to wait
	// for — while staying generous enough that a slow K-ORC resync is never
	// flagged.
	externalImportStallGrace = 2 * time.Minute

	// duplicateControlPlaneRequeueAfter is the backoff a parked duplicate
	// ControlPlane uses while an older ControlPlane owns its namespace
	// (defense-in-depth). Deleting the incumbent enqueues no
	// event for the parked CR, so this periodic requeue is what lets the
	// parked ControlPlane take over once the incumbent is fully gone.
	duplicateControlPlaneRequeueAfter = 30 * time.Second

	// orcTeardownStallTimeout bounds how long the ControlPlane finalizer waits for
	// the operator-owned K-ORC CRs (ApplicationCredential/Service/Endpoint/User/
	// Domain) to disappear during deletion before force-removing their K-ORC
	// finalizers and releasing the ControlPlane finalizer anyway. Those K-ORC
	// finalizers revoke/delete against the Keystone API; if Keystone (and in
	// managed mode its MariaDB) is already gone, K-ORC can never complete and the
	// ControlPlane/namespace would otherwise hang indefinitely on Terminating ORC
	// CRs. After this window reconcileDelete strips the openstack.k-orc.cloud/*
	// finalizers and emits a Warning event so the wedge is operator-visible. The
	// window mirrors remintStallTimeout: generous enough that a slow-but-
	// progressing revoke is not cut short.
	orcTeardownStallTimeout = 5 * time.Minute

	// registrationTeardownStallTimeout bounds how long the ControlPlane finalizer
	// waits for the projected KeystoneService registrations to disappear before
	// continuing the teardown without them. It is a budget of its OWN rather than a
	// share of orcTeardownStallTimeout because the two waits are sequential, not
	// concurrent: the registration wait blocks the K-ORC sweep entirely, so one
	// deadline measured from the deletion timestamp would let an unreachable
	// Keystone consume the whole window here and leave the admin
	// ApplicationCredential's revocation without a single attempt. Shorter than the
	// K-ORC window because a registration that cannot finish is stuck on the very
	// Keystone the sweep behind it still has to be given time to reach.
	registrationTeardownStallTimeout = 2 * time.Minute

	// orcTeardownDeadline is the point, measured from the deletion timestamp, at
	// which the K-ORC sweep, the owned-PushSecret wait and the cross-namespace
	// sweep give up (they run after each other, on whatever is left, so they share
	// one). It is the registration window plus the K-ORC one, so each phase gets
	// its own budget instead of racing the phase before it for a shared deadline.
	orcTeardownDeadline = registrationTeardownStallTimeout + orcTeardownStallTimeout

	// messagingTeardownDeadline bounds, measured from the deletion timestamp, the
	// wait for the managed message bus to leave etcd before the ControlPlane
	// finalizer is released (deleteManagedMessagingBeforeRelease). The RabbitMQ
	// Cluster Operator's own deletion path labels the broker pods to skip their
	// preStop drain, so a healthy teardown takes seconds; the bound is for a
	// cluster-operator that is absent or wedged, where holding on would trade an
	// orphaned broker for an undeletable ControlPlane.
	messagingTeardownDeadline = 3 * time.Minute

	// crdRecheckInterval is the cadence at which the crdWatchGate re-probes
	// discovery for the optional sibling-operator CRDs that were missing when
	// the operator started. When one of them appears the gate returns an error
	// so mgr.Start unwinds and the Deployment restarts the process into a clean
	// discovery pass; a restart is the only recovery path because
	// controller-runtime cannot add a watch to a running manager. The cadence is
	// deliberately unhurried: installing a sibling operator is a rare,
	// human-driven event, so one extra wait of up to this interval is invisible
	// next to the operator's own restart time.
	crdRecheckInterval = 30 * time.Second
)
