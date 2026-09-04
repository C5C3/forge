// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the optional-CRD presence probe and the restart gate:
// servedKindsForGroupVersion against discovery, probeOptionalWatches classification,
// and crdWatchGate lifecycle.
package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/c5c3/cobaltcore/internal/common/messaging"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
	horizonv1alpha1 "github.com/c5c3/cobaltcore/operators/horizon/api/v1alpha1"
	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
	placementv1alpha1 "github.com/c5c3/cobaltcore/operators/placement/api/v1alpha1"
	openbaov1alpha1 "github.com/dc-tec/openbao-operator/api/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	discoveryfake "k8s.io/client-go/discovery/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// The GroupVersions referenced by the presence-probe tests. keystone, horizon,
// glance, placement, barbican and neutron are the sibling-operator groups
// SetupWithManager probes for, ovn is the group of the OVNCentral the network
// service only references, and openbao is the third-party group a dedicated
// Barbican secret store is built from; orc is the hard-dependency group
// TestOptionalWatchObjects_ExcludesKORC asserts never appears in the optional set.
// rabbitmq is the RabbitMQ Cluster Operator's group, carrying the one broker kind
// the ControlPlane projects for a managed spec.infrastructure.messaging block.
// Verified against the api packages' GroupVersion / GroupName (and
// messaging.RabbitmqClusterGVK) before hardcoding here.
var (
	keystoneGV  = schema.GroupVersion{Group: "keystone.openstack.c5c3.io", Version: "v1alpha1"}
	horizonGV   = schema.GroupVersion{Group: "horizon.openstack.c5c3.io", Version: "v1alpha1"}
	glanceGV    = schema.GroupVersion{Group: "glance.openstack.c5c3.io", Version: "v1alpha1"}
	placementGV = schema.GroupVersion{Group: "placement.openstack.c5c3.io", Version: "v1alpha1"}
	barbicanGV  = schema.GroupVersion{Group: "barbican.openstack.c5c3.io", Version: "v1alpha1"}
	openbaoGV   = schema.GroupVersion{Group: "openbao.org", Version: "v1alpha1"}
	orcGV       = schema.GroupVersion{Group: "openstack.k-orc.cloud", Version: "v1alpha1"}
	rabbitmqGV  = schema.GroupVersion{Group: "rabbitmq.com", Version: "v1beta1"}
	neutronGV   = schema.GroupVersion{Group: "neutron.openstack.c5c3.io", Version: "v1alpha1"}
	ovnGV       = schema.GroupVersion{Group: "ovn.openstack.c5c3.io", Version: "v1alpha1"}
)

// optionalWatchTestScheme registers the API groups the presence probe resolves GVKs
// against (keystone, horizon, glance, placement, barbican, neutron, ovn, openbao)
// plus K-ORC, which
// TestOptionalWatchObjects_ExcludesKORC needs registered to prove no K-ORC kind is
// listed as an optional watch. controllerTestScheme registers only c5c3 + client-go,
// so it cannot resolve any of those kinds; this helper adds them.
func optionalWatchTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	adders := []struct {
		name string
		add  func(*runtime.Scheme) error
	}{
		{"keystone", keystonev1alpha1.AddToScheme},
		{"horizon", horizonv1alpha1.AddToScheme},
		{"glance", glancev1alpha1.AddToScheme},
		{"placement", placementv1alpha1.AddToScheme},
		{"barbican", barbicanv1alpha1.AddToScheme},
		{"neutron", neutronv1alpha1.AddToScheme},
		{"ovn", ovnv1alpha1.AddToScheme},
		{"openbao", openbaov1alpha1.AddToScheme},
		{"orc", orcv1alpha1.AddToScheme},
	}
	for _, a := range adders {
		if err := a.add(s); err != nil {
			t.Fatalf("adding %s scheme: %v", a.name, err)
		}
	}
	return s
}

// fakeServerResources is a mutex-guarded serverResourcesLister stub. It is safe to
// mutate from a test goroutine while a crdWatchGate.Start goroutine reads it, so the
// gate tests stay clean under go test -race. GroupVersions absent from byGV return the
// real client-side NotFound StatusError shape via apierrors.NewNotFound, matching how
// discovery reports an uninstalled CRD.
type fakeServerResources struct {
	mu   sync.Mutex
	byGV map[string]*metav1.APIResourceList
	// err, when set together with a non-zero errCalls, is returned instead of a
	// resource list. errCalls < 0 means "return err on every call"; errCalls > 0
	// returns err for that many calls and then decrements toward zero, after which
	// the stub serves normally again — this models a transient discovery outage.
	err      error
	errCalls int
	// callCount records every ServerResourcesForGroupVersion invocation so tests can
	// assert the probe fetches each GroupVersion once rather than once per kind.
	callCount int
}

func newFakeServerResources() *fakeServerResources {
	return &fakeServerResources{byGV: map[string]*metav1.APIResourceList{}}
}

func (f *fakeServerResources) ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callCount++
	if f.err != nil && f.errCalls != 0 {
		if f.errCalls > 0 {
			f.errCalls--
		}
		return nil, f.err
	}
	if list, ok := f.byGV[groupVersion]; ok {
		return list, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{}, groupVersion)
}

// serve records that the given GroupVersion lists the given Kinds, so subsequent
// lookups for it succeed. Callable mid-test to simulate a CRD being installed.
func (f *fakeServerResources) serve(gv schema.GroupVersion, kinds ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := gv.String()
	list := f.byGV[key]
	if list == nil {
		list = &metav1.APIResourceList{GroupVersion: key}
		f.byGV[key] = list
	}
	for _, k := range kinds {
		list.APIResources = append(list.APIResources, metav1.APIResource{Kind: k})
	}
}

// setError makes every subsequent call return err (a non-NotFound discovery failure).
func (f *fakeServerResources) setError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
	f.errCalls = -1
}

// setErrorForCalls makes the next n calls return err, then serve normally.
func (f *fakeServerResources) setErrorForCalls(err error, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
	f.errCalls = n
}

// calls returns how many times ServerResourcesForGroupVersion has been invoked.
func (f *fakeServerResources) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callCount
}

// zeroProbeBackoff removes the startup-probe retry sleep for the duration of a test so
// the retry path runs instantly, restoring the production value afterwards.
func zeroProbeBackoff(t *testing.T) {
	t.Helper()
	prev := discoveryProbeBackoff
	discoveryProbeBackoff = 0
	t.Cleanup(func() { discoveryProbeBackoff = prev })
}

// --- servedKindsForGroupVersion ---

func TestServedKindsForGroupVersion_ServedKind(t *testing.T) {
	g := NewGomegaWithT(t)

	disco := newFakeServerResources()
	disco.serve(keystoneGV, "Keystone")

	kinds, err := servedKindsForGroupVersion(disco, keystoneGV)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kinds).To(HaveKeyWithValue("Keystone", true), "a listed Kind must be in the served set")
}

func TestServedKindsForGroupVersion_AbsentGroupVersion(t *testing.T) {
	g := NewGomegaWithT(t)

	// The real fake discovery client with no Resources returns the genuine
	// client-side NotFound StatusError, proving apierrors.IsNotFound matches it.
	disco := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{}}

	kinds, err := servedKindsForGroupVersion(disco, keystoneGV)
	g.Expect(err).NotTo(HaveOccurred(), "an absent GroupVersion is not-found, not an error")
	g.Expect(kinds).To(BeEmpty())
}

func TestServedKindsForGroupVersion_GroupPresentKindAbsent(t *testing.T) {
	g := NewGomegaWithT(t)

	// The keystone group is served but only lists Keystone, not the separately
	// installed KeystoneIdentityBackend CRD.
	disco := newFakeServerResources()
	disco.serve(keystoneGV, "Keystone")

	kinds, err := servedKindsForGroupVersion(disco, keystoneGV)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kinds).NotTo(HaveKey("KeystoneIdentityBackend"), "an unlisted Kind must be absent from the served set")
}

func TestServedKindsForGroupVersion_UpstreamError(t *testing.T) {
	g := NewGomegaWithT(t)

	sentinel := errors.New("discovery boom")
	disco := newFakeServerResources()
	disco.setError(sentinel)

	kinds, err := servedKindsForGroupVersion(disco, keystoneGV)
	g.Expect(kinds).To(BeNil())
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, sentinel)).To(BeTrue(), "the upstream error must be wrapped with %%w")
	g.Expect(err.Error()).To(ContainSubstring("listing server resources for keystone.openstack.c5c3.io/v1alpha1: "))
}

// --- probeOptionalWatches ---

// serveAllOptionalKinds populates the stub with every optional kind across the nine
// GroupVersions, matching optionalWatchObjects.
func serveAllOptionalKinds(f *fakeServerResources) {
	f.serve(keystoneGV, "Keystone", "KeystoneIdentityBackend")
	f.serve(horizonGV, "Horizon")
	f.serve(glanceGV, "Glance", "GlanceBackend")
	f.serve(placementGV, "Placement")
	f.serve(barbicanGV, "Barbican", "BarbicanSecretStore")
	f.serve(openbaoGV, "OpenBaoCluster", "OpenBaoTenant")
	f.serve(rabbitmqGV, "RabbitmqCluster")
	f.serve(neutronGV, "Neutron")
	f.serve(ovnGV, "OVNCentral")
}

func TestProbeOptionalWatches_AllServed(t *testing.T) {
	g := NewGomegaWithT(t)

	disco := newFakeServerResources()
	serveAllOptionalKinds(disco)

	served, missing, err := probeOptionalWatches(disco, optionalWatchTestScheme(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(missing).To(BeEmpty(), "no CRD is missing when every group is served")
	g.Expect(served).To(HaveLen(13), "all 13 optional kinds must be recorded")
	for gvk, ok := range served {
		g.Expect(ok).To(BeTrue(), "expected %s to be served", gvk)
	}
}

func TestProbeOptionalWatches_SubsetServed(t *testing.T) {
	g := NewGomegaWithT(t)

	// keystone group served but without KeystoneIdentityBackend; horizon, glance,
	// placement, barbican, openbao, rabbitmq, neutron and ovn groups entirely absent.
	disco := newFakeServerResources()
	disco.serve(keystoneGV, "Keystone")

	served, missing, err := probeOptionalWatches(disco, optionalWatchTestScheme(t))
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(missing).To(ConsistOf(
		horizonGV.WithKind("Horizon"),
		glanceGV.WithKind("Glance"),
		glanceGV.WithKind("GlanceBackend"),
		keystoneGV.WithKind("KeystoneIdentityBackend"),
		placementGV.WithKind("Placement"),
		barbicanGV.WithKind("Barbican"),
		barbicanGV.WithKind("BarbicanSecretStore"),
		openbaoGV.WithKind("OpenBaoCluster"),
		openbaoGV.WithKind("OpenBaoTenant"),
		rabbitmqGV.WithKind("RabbitmqCluster"),
		neutronGV.WithKind("Neutron"),
		ovnGV.WithKind("OVNCentral"),
	), "exactly the uninstalled kinds must be reported missing")

	g.Expect(served).To(HaveLen(13))
	// The one served kind is marked true.
	g.Expect(served[keystoneGV.WithKind("Keystone")]).To(BeTrue())
	// The twelve missing kinds are marked false.
	g.Expect(served[horizonGV.WithKind("Horizon")]).To(BeFalse())
	g.Expect(served[glanceGV.WithKind("Glance")]).To(BeFalse())
	g.Expect(served[glanceGV.WithKind("GlanceBackend")]).To(BeFalse())
	g.Expect(served[keystoneGV.WithKind("KeystoneIdentityBackend")]).To(BeFalse())
	g.Expect(served[placementGV.WithKind("Placement")]).To(BeFalse())
	g.Expect(served[barbicanGV.WithKind("Barbican")]).To(BeFalse())
	g.Expect(served[barbicanGV.WithKind("BarbicanSecretStore")]).To(BeFalse())
	g.Expect(served[openbaoGV.WithKind("OpenBaoCluster")]).To(BeFalse())
	g.Expect(served[openbaoGV.WithKind("OpenBaoTenant")]).To(BeFalse())
	g.Expect(served[rabbitmqGV.WithKind("RabbitmqCluster")]).To(BeFalse())
	g.Expect(served[neutronGV.WithKind("Neutron")]).To(BeFalse())
	g.Expect(served[ovnGV.WithKind("OVNCentral")]).To(BeFalse())
}

// TestProbeOptionalWatches_BarbicanWithoutOpenBao covers the partial install a
// Barbican on an EXTERNAL secret store produces: the barbican CRDs are served but
// the openbao-operator was never installed, because no dedicated instance is
// provisioned. Each group must classify on its own, so the barbican legs register
// while the two openbao legs are skipped and reported for the restart gate. The
// rabbitmq group is served here so the openbao pair is the only missing one.
func TestProbeOptionalWatches_BarbicanWithoutOpenBao(t *testing.T) {
	g := NewGomegaWithT(t)

	disco := newFakeServerResources()
	disco.serve(keystoneGV, "Keystone", "KeystoneIdentityBackend")
	disco.serve(horizonGV, "Horizon")
	disco.serve(glanceGV, "Glance", "GlanceBackend")
	disco.serve(placementGV, "Placement")
	disco.serve(barbicanGV, "Barbican", "BarbicanSecretStore")
	disco.serve(rabbitmqGV, "RabbitmqCluster")
	disco.serve(neutronGV, "Neutron")
	disco.serve(ovnGV, "OVNCentral")

	served, missing, err := probeOptionalWatches(disco, optionalWatchTestScheme(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(missing).To(ConsistOf(
		openbaoGV.WithKind("OpenBaoCluster"),
		openbaoGV.WithKind("OpenBaoTenant"),
	), "only the openbao kinds may be reported missing")
	g.Expect(served[barbicanGV.WithKind("Barbican")]).To(BeTrue())
	g.Expect(served[barbicanGV.WithKind("BarbicanSecretStore")]).To(BeTrue())
}

// TestProbeOptionalWatches_RabbitmqClusterMissing covers the install messaging is
// opt-in for: every sibling-operator CRD is served, but the rabbitmq-cluster-operator
// was never installed because no ControlPlane declares a managed messaging block.
// The broker leg alone must be skipped, and the probe must report the kind for the
// restart gate instead of failing setup.
func TestProbeOptionalWatches_RabbitmqClusterMissing(t *testing.T) {
	g := NewGomegaWithT(t)

	disco := newFakeServerResources()
	disco.serve(keystoneGV, "Keystone", "KeystoneIdentityBackend")
	disco.serve(horizonGV, "Horizon")
	disco.serve(glanceGV, "Glance", "GlanceBackend")
	disco.serve(placementGV, "Placement")
	disco.serve(barbicanGV, "Barbican", "BarbicanSecretStore")
	disco.serve(openbaoGV, "OpenBaoCluster", "OpenBaoTenant")
	disco.serve(neutronGV, "Neutron")
	disco.serve(ovnGV, "OVNCentral")

	served, missing, err := probeOptionalWatches(disco, optionalWatchTestScheme(t))
	g.Expect(err).NotTo(HaveOccurred(),
		"an uninstalled rabbitmq-cluster-operator is a NotFound, not a probe failure")
	g.Expect(missing).To(ConsistOf(messaging.RabbitmqClusterGVK),
		"only the RabbitmqCluster kind may be reported missing")
	g.Expect(served[messaging.RabbitmqClusterGVK]).To(BeFalse())
	g.Expect(served[keystoneGV.WithKind("Keystone")]).To(BeTrue(),
		"the sibling-service legs must still register when only the broker CRD is absent")
}

func TestProbeOptionalWatches_DiscoveryError(t *testing.T) {
	g := NewGomegaWithT(t)
	zeroProbeBackoff(t)

	// Every call fails: a persistent (not merely transient) discovery outage must
	// still abort the probe once the bounded retry is exhausted, so the pod restarts
	// rather than starting with watches silently dropped.
	sentinel := errors.New("api server unreachable")
	disco := newFakeServerResources()
	disco.setError(sentinel)

	served, missing, err := probeOptionalWatches(disco, optionalWatchTestScheme(t))
	g.Expect(served).To(BeNil())
	g.Expect(missing).To(BeNil())
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, sentinel)).To(BeTrue(), "a non-NotFound discovery error must abort the probe")
}

func TestProbeOptionalWatches_TransientErrorRetries(t *testing.T) {
	g := NewGomegaWithT(t)
	zeroProbeBackoff(t)

	// A single transient (non-NotFound) discovery blip on the first GroupVersion
	// query — realistic during an apiserver rollout or etcd compaction — must be
	// absorbed by the bounded retry rather than aborting setup and crash-looping the
	// pod. After the blip clears every kind classifies as served.
	disco := newFakeServerResources()
	serveAllOptionalKinds(disco)
	disco.setErrorForCalls(errors.New("transient discovery blip"), 1)

	served, missing, err := probeOptionalWatches(disco, optionalWatchTestScheme(t))
	g.Expect(err).NotTo(HaveOccurred(), "a single transient blip must not abort the startup probe")
	g.Expect(missing).To(BeEmpty())
	g.Expect(served).To(HaveLen(13))
	for gvk, ok := range served {
		g.Expect(ok).To(BeTrue(), "expected %s to be served after the blip cleared", gvk)
	}
}

func TestProbeOptionalWatches_FetchesEachGroupVersionOnce(t *testing.T) {
	g := NewGomegaWithT(t)

	// The 13 optional kinds span only nine GroupVersions, so the probe must query
	// discovery nine times, not once per kind.
	disco := newFakeServerResources()
	serveAllOptionalKinds(disco)

	_, _, err := probeOptionalWatches(disco, optionalWatchTestScheme(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(disco.calls()).To(Equal(9),
		"each of the nine GroupVersions must be queried exactly once, not per kind")
}

// --- optionalWatchObjects ---

// TestOptionalWatchObjects_ExcludesKORC guards the invariant that the eight K-ORC
// kinds are hard dependencies Owned unconditionally by buildControlPlaneController, not
// slimmable watches guarded behind the discovery probe. Listing any K-ORC kind here
// would let the manager start on a cluster whose K-ORC CRDs are unserved, only for the
// very next reconcile pass to hard-error with a no-match when reconcileKORC reads the
// admin ApplicationCredential (or a service leg applies a KeystoneService child's
// K-ORC CR), wedging the ControlPlane instead of failing fast at startup. The probe
// never reports K-ORC missing, so nothing else in the suite catches a regression here.
func TestOptionalWatchObjects_ExcludesKORC(t *testing.T) {
	g := NewGomegaWithT(t)

	scheme := optionalWatchTestScheme(t)
	for _, obj := range optionalWatchObjects() {
		gvk, err := apiutil.GVKForObject(obj, scheme)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(gvk.Group).NotTo(Equal(orcGV.Group),
			"K-ORC kind %s must not be an optional watch: it is a hard dependency read unconditionally on every reconcile pass", gvk)
	}
}

// TestOptionalWatchObjects_IncludesRabbitmqCluster pins both halves of how the
// broker leg is guarded: the kind is in the optional set, and it resolves off the
// object itself. optionalWatchTestScheme registers no rabbitmq.com type, so a
// typed object would fail GVKForObject here; the *unstructured.Unstructured
// carrying messaging.RabbitmqClusterGVK resolves without a scheme entry, which
// is what lets the c5c3 operator watch the kind without importing its Go module.
func TestOptionalWatchObjects_IncludesRabbitmqCluster(t *testing.T) {
	g := NewGomegaWithT(t)

	scheme := optionalWatchTestScheme(t)
	matches := 0
	for _, obj := range optionalWatchObjects() {
		gvk, err := apiutil.GVKForObject(obj, scheme)
		g.Expect(err).NotTo(HaveOccurred(),
			"every optional watch object must resolve its GVK, %T did not", obj)
		if gvk == messaging.RabbitmqClusterGVK {
			matches++
		}
	}
	g.Expect(matches).To(Equal(1),
		"exactly one optional watch object must carry the RabbitmqCluster GVK")
}

// --- crdWatchGate ---

func TestCRDWatchGate_NeedLeaderElection(t *testing.T) {
	g := NewGomegaWithT(t)

	gate := &crdWatchGate{}
	g.Expect(gate.NeedLeaderElection()).To(BeTrue(),
		"only the elected leader should restart itself for a new watch")
}

func TestCRDWatchGate_StartEmptyMissing(t *testing.T) {
	g := NewGomegaWithT(t)

	// No missing CRDs: Start must return immediately even called synchronously. The
	// non-zero interval guards the test from hanging if the early return regressed.
	gate := &crdWatchGate{
		disco:    newFakeServerResources(),
		interval: time.Second,
	}
	g.Expect(gate.Start(context.Background())).To(Succeed(),
		"an empty missing set must return nil without waiting")
}

func TestCRDWatchGate_StartContextCancelled(t *testing.T) {
	g := NewGomegaWithT(t)

	// The CRD never appears; cancelling the context must stop the gate with nil,
	// because losing leadership or shutting down is not an error.
	disco := newFakeServerResources()
	gate := &crdWatchGate{
		disco:    disco,
		missing:  []schema.GroupVersionKind{keystoneGV.WithKind("Keystone")},
		interval: 10 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- gate.Start(ctx) }()
	cancel()

	g.Eventually(errCh, time.Second, 10*time.Millisecond).Should(Receive(BeNil()),
		"a cancelled context must return nil")
}

func TestCRDWatchGate_StartReturnsWhenCRDAppears(t *testing.T) {
	g := NewGomegaWithT(t)

	disco := newFakeServerResources()
	gvk := keystoneGV.WithKind("Keystone")
	gate := &crdWatchGate{
		disco:    disco,
		missing:  []schema.GroupVersionKind{gvk},
		interval: 10 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- gate.Start(ctx) }()

	// Let a few ticks observe the CRD still absent, then install it mid-run.
	time.Sleep(40 * time.Millisecond)
	disco.serve(keystoneGV, "Keystone")

	var got error
	g.Eventually(errCh, time.Second, 10*time.Millisecond).Should(Receive(&got))
	g.Expect(got).To(HaveOccurred())
	g.Expect(got.Error()).To(ContainSubstring(gvk.String()))
	g.Expect(got.Error()).To(ContainSubstring("became available after startup; restarting to register its watch"))
}

func TestCRDWatchGate_TransientErrorContinues(t *testing.T) {
	g := NewGomegaWithT(t)

	// The first few discovery calls fail with a non-NotFound error; the gate must
	// log and retry rather than return early or return nil. Once the stub serves the
	// kind, the gate returns the became-available error, proving the transient
	// failure did not abort it.
	disco := newFakeServerResources()
	gvk := keystoneGV.WithKind("Keystone")
	disco.setErrorForCalls(errors.New("transient discovery blip"), 3)
	disco.serve(keystoneGV, "Keystone")

	gate := &crdWatchGate{
		disco:    disco,
		missing:  []schema.GroupVersionKind{gvk},
		interval: 10 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- gate.Start(ctx) }()

	var got error
	g.Eventually(errCh, time.Second, 10*time.Millisecond).Should(Receive(&got))
	g.Expect(got).To(HaveOccurred())
	g.Expect(got.Error()).To(ContainSubstring("became available after startup; restarting to register its watch"))
}
