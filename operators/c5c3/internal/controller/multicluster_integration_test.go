// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// All multicluster envtest coverage of the ControlPlane reconciler lives in the
// one test function below, on purpose. The kubeconfig provider registers its
// registration-Secret watch under the fixed controller name
// "kubeconfig-provider" and exposes no SkipNameValidation escape, while
// controller-runtime validates controller names against a process-global set. A
// second provider anywhere in this test binary would therefore fail to register.
// One manager, one provider, one function: the scenarios are ordered subtests
// over the shared setup, and each one builds on the state the previous left
// behind.
//
// The reconciler itself is registered through the production watch wiring
// (setupWithOptions, the chain SetupWithManager applies) with SkipNameValidation
// set, so it does not claim the real controller name either. That name belongs
// to TestSetupWithManager_BothControllersStart, which is the one test in this
// binary allowed to call the real SetupWithManager methods (see the header of
// setupwithmanager_integration_test.go).

package controller

import (
	"context"
	"testing"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonenvtest "github.com/c5c3/cobaltcore/internal/common/testutil/envtest"
	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/c5c3/internal/testutil"
	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// TestIntegration_Multicluster_ControlPlanePlacement runs the ControlPlane
// reconciler on a management cluster with a second envtest environment
// registered as target cluster, and walks the per-service placement split:
// registration, a ControlPlane that places its Keystone service on the target,
// the admin password it has to read back off that cluster, a placed built-in
// service whose registration stays home while its credentials are mirrored onto
// the target, a placed network service that takes the shared bus and its OVN gate
// with it, a ControlPlane naming an unregistered cluster, and the deletion that
// sweeps the placed namespaces off the target again.
func TestIntegration_Multicluster_ControlPlanePlacement(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	const (
		// mcClustersNamespace mirrors the --clusters-namespace default the
		// operator binary passes to the provider.
		mcClustersNamespace = "c5c3-clusters"
		mcTargetCluster     = "target-b"

		// The two ControlPlanes and their namespaces. Namespaces are fixed rather
		// than generated so the same name can be looked up on both clusters: a
		// placed namespace exists on the management cluster too, and the split
		// assertions read one key through both clients.
		mcNamespace         = "mc-cp"
		mcKeystoneNamespace = "mc-cp-identity"
		mcGlanceNamespace   = "mc-cp-image"
		mcNetworkNamespace  = "mc-cp-network"
		mcControlPlane      = "cp"

		// The OVN control plane the network service programs. It is deployed
		// outside the ControlPlane and only referenced, so this test creates it
		// and drives its status the way the ovn-operator would.
		mcOVNCentral = "mc-ovn"

		// The shared message bus, declared brownfield: this plane has no broker to
		// simulate, and a URL in a Secret is what a brownfield block resolves to.
		// mcBusSecret lives in the ControlPlane's own namespace, which is where the
		// bus is declared and read; mcBusTransportURL is what a placed service must
		// receive on the cluster it runs on.
		mcBusSecret       = "mc-bus-url"
		mcBusTransportURL = "rabbit://u:p@bus.mc-cp.svc:5672/"

		mcUnknownNamespace  = "mc-unknown"
		mcUnknownKeystoneNS = "mc-unknown-identity"
		mcUnknownCluster    = "does-not-exist"

		// The cleartext admin password reconcileKORC reads across the cluster
		// boundary, and the decoy of the same shape planted on the management
		// cluster to prove it is not the one being read.
		mcAdminPassword = "super-secret-admin-password"
		mcDecoyPassword = "decoy-on-the-management-cluster"

		// mcEngageTimeout bounds cluster engagement: the provider has to parse the
		// kubeconfig, build a cluster, and sync its cache before GetCluster
		// answers.
		mcEngageTimeout = 60 * time.Second
	)

	// One scheme for both environments, the manager, and the provider's
	// per-cluster clients. They all write the same kinds, and a client built on a
	// scheme that does not know a CRD kind fails its first write with "no kind is
	// registered" — which is what the ClusterOptions override below prevents on
	// the clusters the provider builds.
	mcScheme := testutil.BuildControllerScheme(c5c3v1alpha1.AddToScheme)

	// --- Environment B: the target cluster.
	//
	// It carries the shared fake CRDs of the external operators whose objects the
	// ControlPlane places (MariaDB, Memcached, ESO, cert-manager, openbao) and
	// deliberately NEITHER the c5c3 CRDs NOR the sibling service-operator ones: a
	// target cluster holds the workload, never the CRs. The K-ORC kinds ARE
	// served here, because they ship in those same shared dirs, and that is what
	// makes the "the K-ORC ensemble is not on B" assertions below a real absence
	// rather than a kind the cluster could never have answered for.
	targetClient, targetCfg := commonenvtest.StartEnvTestWithConfig(t, mcScheme, commonenvtest.CommonFakeCRDDirs())

	// --- Environment A: the management cluster, hosting the manager.
	provider := commonmulticluster.NewKubeconfigProvider(commonmulticluster.KubeconfigProviderOptions{
		Namespace: mcClustersNamespace,
		// Without this the provider builds every target cluster's client on
		// client-go's global scheme, which knows no CRD kind, and the first
		// MariaDB or ESO write fails with "no kind is registered".
		ClusterOptions: []cluster.Option{func(o *cluster.Options) { o.Scheme = mcScheme }},
	})

	var mcMgr mcmanager.Manager
	mgmtClient, ctx, _ := commonenvtest.StartManagedEnvTest(t, commonenvtest.ManagedEnvTestConfig{
		Name:              "c5c3-multicluster",
		Scheme:            mcScheme,
		CRDDirectoryPaths: testutil.CRDDirectoryPaths(),
		WebhookDir:        testutil.C5c3WebhookDir(),
		BuildManager: func(cfg *rest.Config, opts ctrl.Options) (ctrl.Manager, error) {
			m, err := mcmanager.New(cfg, provider, opts)
			if err != nil {
				return nil, err
			}
			mcMgr = m
			// The multicluster manager is not a ctrl.Manager (its Add takes the
			// multicluster Runnable), so the helper hosts and starts the local one.
			// That is the same thing: the multicluster manager's Start adds a
			// runnable provider to the local manager and then starts it, and the
			// kubeconfig provider is not a runnable one. Its Secret watch is an
			// ordinary controller on the local manager, registered by
			// SetupWithManager below.
			return m.GetLocalManager(), nil
		},
		RegisterWebhooks: func(mgr ctrl.Manager) error {
			// mgr.GetAPIReader() mirrors main.go: admission lookups read the API
			// server directly, never a stale cache.
			if err := (&c5c3v1alpha1.ControlPlaneWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
				return err
			}
			// The webhook manifests installed by envtest carry the KeystoneService
			// entries (failurePolicy=Fail), so the handler must be served here too.
			return (&c5c3v1alpha1.KeystoneServiceWebhook{}).SetupWebhookWithManager(mgr)
		},
		RegisterController: func(mgr ctrl.Manager) error {
			// The provider's engagement machinery has to be registered before the
			// controllers, exactly as internal/common/bootstrap does it, so
			// engagement precedes the first reconcile.
			if err := provider.SetupWithManager(context.Background(), mcMgr); err != nil {
				return err
			}

			r := &ControlPlaneReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("controlplane-controller"),
				// The Resolver is the whole point of this test: it turns a
				// service's targetClusterRef into the client that service's
				// children are written with.
				Resolver: mcMgr,
			}
			// The production watch wiring, shared with SetupWithManager: the legs
			// pinned to the management cluster, and the same kinds again on the
			// clusters a service can be placed on, keyed on the ownership labels.
			// A child written on the target therefore produces a watch event here,
			// and the field index and the discovery guard come with the same call.
			// SkipNameValidation keeps the one-real-controller-name-per-test-binary
			// constraint at the top of this file intact.
			opts := bootstrap.TypedControllerOptions[mcreconcile.Request](1)
			opts.SkipNameValidation = ptr.To(true)
			return r.setupWithOptions(mcMgr, opts)
		},
	})

	// The provider watches this namespace for registration Secrets.
	mcEnsureNamespace(t, ctx, mgmtClient, mcClustersNamespace)

	cp := integrationManagedControlPlane(mcControlPlane, mcNamespace)
	// The bus is declared from the start rather than added with the network
	// service: the validating webhook requires it beside services.neutron, and a
	// messaging block cannot be added to a live ControlPlane and then removed
	// again, so declaring it once keeps the later update to the service block
	// alone.
	cp.Spec.Infrastructure.Messaging = &commonv1.MessagingSpec{
		SecretRef: &commonv1.SecretRefSpec{Name: mcBusSecret},
	}
	cpKey := types.NamespacedName{Name: mcControlPlane, Namespace: mcNamespace}

	// Every object the placed Keystone service takes with it, keyed in the
	// service namespace. Each one is looked up through BOTH clients below: on the
	// target it must exist and carry the claim, on the management cluster it must
	// not exist at all. The names are derived from the CR rather than written out,
	// so a renamed child fails the lookup instead of silently changing what the
	// split is asserted over.
	placedKey := client.ObjectKey{Namespace: mcKeystoneNamespace, Name: keystoneName(cp)}
	mariadbKey := client.ObjectKey{
		Namespace: mcKeystoneNamespace,
		Name:      cp.Spec.Infrastructure.Database.ClusterRef.Name,
	}
	memcachedKey := client.ObjectKey{
		Namespace: mcKeystoneNamespace,
		Name:      cp.Spec.Infrastructure.Cache.ClusterRef.Name,
	}
	tenantStoreKey := client.ObjectKey{Namespace: mcKeystoneNamespace, Name: esoTenantStoreName}
	tenantSAKey := client.ObjectKey{Namespace: mcKeystoneNamespace, Name: esoTenantServiceAccountName}
	tenantCertKey := client.ObjectKey{Namespace: mcKeystoneNamespace, Name: esoTenantClientCertName}
	dbCredSAKey := client.ObjectKey{Namespace: mcKeystoneNamespace, Name: dbCredentialServiceAccountName}
	dbCredKey := client.ObjectKey{Namespace: mcKeystoneNamespace, Name: dbCredentialSecretName(cp)}
	dbCredCertKey := client.ObjectKey{Namespace: mcKeystoneNamespace, Name: dbCredentialClientCertName(cp)}
	adminPasswordKey := client.ObjectKey{Namespace: mcKeystoneNamespace, Name: adminPasswordSecretName(cp)}
	// The one object the placed NETWORK service takes with it that no other
	// service has: the shared bus, delivered as a Secret on the cluster Neutron
	// runs on. It is asserted in the network subtest and swept in the deletion one.
	neutronBusKey := client.ObjectKey{Namespace: mcNetworkNamespace, Name: neutronMessagingSecretName(cp)}

	t.Run("register the target cluster", func(t *testing.T) {
		g := NewGomegaWithT(t)

		kubeconfig, err := commonenvtest.KubeconfigBytes(targetCfg, mcTargetCluster)
		g.Expect(err).NotTo(HaveOccurred(), "build kubeconfig for the target environment")

		g.Expect(mgmtClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      mcTargetCluster,
				Namespace: mcClustersNamespace,
				Labels:    map[string]string{"sigs.k8s.io/multicluster-runtime-kubeconfig": "true"},
			},
			Data: map[string][]byte{"kubeconfig": kubeconfig},
		})).To(Succeed(), "create the registration Secret")

		g.Eventually(func() error {
			_, err := mcMgr.GetCluster(ctx, mcruntime.ClusterName(mcTargetCluster))
			return err
		}, mcEngageTimeout, itPollInterval).Should(Succeed(),
			"the provider should engage the target cluster from its registration Secret")
	})

	t.Run("placing keystone moves its ensemble onto the target cluster", func(t *testing.T) {
		g := NewGomegaWithT(t)

		mcEnsureNamespace(t, ctx, mgmtClient, mcNamespace)
		ensureReadyClusterSecretStore(t, ctx, mgmtClient)
		// The ControlPlane's own namespace stays on the management cluster, so its
		// tenant store is seeded here. The one in the placed namespace cannot be
		// seeded yet: that namespace does not exist on either cluster until the
		// reconciler creates it.
		ensureReadySecretStore(t, ctx, mgmtClient, esoTenantStoreName, mcNamespace)
		// The brownfield bus the ControlPlane declares. It is read in the
		// ControlPlane's own namespace on the management cluster, whatever cluster
		// the service consuming it runs on.
		g.Expect(mgmtClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: mcBusSecret, Namespace: mcNamespace},
			Data:       map[string][]byte{commonv1.DefaultTransportURLSecretKey: []byte(mcBusTransportURL)},
		})).To(Succeed(), "seed the brownfield transport-URL Secret")

		cp.Spec.Services.Keystone.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
			Name:      mcKeystoneNamespace,
			Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
		}
		// A placed catalog service must advertise an externally routable address:
		// the webhook rejects one whose catalog row would carry only an in-cluster
		// Service DNS name nothing outside the target cluster can resolve.
		cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.com/v3"
		cp.Spec.Services.Keystone.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: mcTargetCluster}
		g.Expect(mgmtClient.Create(ctx, cp)).To(Succeed(), "create the placing ControlPlane")

		// --- The namespace itself is ensured on BOTH clusters, and that is not a
		// leak: the Keystone CR is created and reconciled on the management
		// cluster, in the namespace its service is assigned to, while the
		// keystone-operator then projects the workload into the namespace of the
		// same name on the target. Both sides have to exist, so both are checked —
		// and the target-side one carries the claim.
		targetNS := &corev1.Namespace{}
		mcExpectRemoteClaim(t, ctx, targetClient, client.ObjectKey{Name: mcKeystoneNamespace},
			targetNS, "service namespace", cp)
		g.Expect(targetNS.Labels).To(HaveKeyWithValue(managedByLabel, managedByValue),
			"the operator records that it owns the namespace it created")
		mcEventuallyExists(t, ctx, mgmtClient, client.ObjectKey{Name: mcKeystoneNamespace},
			&corev1.Namespace{}, "service namespace")

		// --- The backing services land on the target and nowhere else. They are
		// simulated Ready there too: the pipeline short-circuits at Infrastructure
		// while either is converging, so nothing below runs until they report.
		mcEventuallyExists(t, ctx, targetClient, mariadbKey, &mariadbv1alpha1.MariaDB{}, "MariaDB")
		mcExpectAbsent(t, ctx, mgmtClient, mariadbKey, &mariadbv1alpha1.MariaDB{}, "MariaDB")
		mcEventuallyExists(t, ctx, targetClient, memcachedKey, mcMemcached(), "Memcached")
		mcExpectAbsent(t, ctx, mgmtClient, memcachedKey, mcMemcached(), "Memcached")

		simulateMariaDBReadyWhenPresent(t, ctx, targetClient, mariadbKey)
		simulateMemcachedReadyWhenPresent(t, ctx, targetClient, memcachedKey)
		waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeInfrastructureReady, metav1.ConditionTrue, itEventuallyTimeout)

		// --- The ESO tenant trio: the store has to authenticate from the cluster
		// whose ESO materialises the Secrets, and its client certificate has to be
		// issued by the cert-manager there. The KEYSTONE namespace gets no copy at
		// home, because every path delivering into it — the admin password, the DB
		// credentials, the service accounts — resolves its store on the cluster the
		// service runs on. Only a namespace hosting a projected registration also
		// needs one at home, which the image service below exercises.
		mcEventuallyExists(t, ctx, targetClient, tenantSAKey, &corev1.ServiceAccount{}, "tenant ServiceAccount")
		mcEventuallyExists(t, ctx, targetClient, tenantCertKey, mcCertificate(), "tenant Certificate")
		mcEventuallyExists(t, ctx, targetClient, tenantStoreKey, &esov1.SecretStore{}, "tenant SecretStore")
		for _, absent := range []struct {
			key  client.ObjectKey
			obj  client.Object
			what string
		}{
			{tenantSAKey, &corev1.ServiceAccount{}, "tenant ServiceAccount"},
			{tenantCertKey, mcCertificate(), "tenant Certificate"},
			{tenantStoreKey, &esov1.SecretStore{}, "tenant SecretStore"},
		} {
			mcExpectAbsent(t, ctx, mgmtClient, absent.key, absent.obj, absent.what)
		}
		ensureReadySecretStore(t, ctx, targetClient, esoTenantStoreName, mcKeystoneNamespace)
		waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeESOTenantStoreReady, metav1.ConditionTrue, itEventuallyTimeout)

		// --- The DB-credential quartet follows the database it issues against.
		mcEventuallyExists(t, ctx, targetClient, dbCredSAKey, &corev1.ServiceAccount{}, "DB-credential ServiceAccount")
		mcEventuallyExists(t, ctx, targetClient, dbCredCertKey, mcCertificate(), "DB-credential Certificate")
		mcEventuallyExists(t, ctx, targetClient, dbCredKey, &esgenv1alpha1.VaultDynamicSecret{}, "DB-credential generator")
		mcEventuallyExists(t, ctx, targetClient, dbCredKey, &esov1.ExternalSecret{}, "DB-credential ExternalSecret")
		for _, absent := range []struct {
			key  client.ObjectKey
			obj  client.Object
			what string
		}{
			{dbCredSAKey, &corev1.ServiceAccount{}, "DB-credential ServiceAccount"},
			{dbCredCertKey, mcCertificate(), "DB-credential Certificate"},
			{dbCredKey, &esgenv1alpha1.VaultDynamicSecret{}, "DB-credential generator"},
			{dbCredKey, &esov1.ExternalSecret{}, "DB-credential ExternalSecret"},
		} {
			mcExpectAbsent(t, ctx, mgmtClient, absent.key, absent.obj, absent.what)
		}
		g.Expect(simulators.SimulateExternalSecretSync(ctx, targetClient, dbCredKey)).
			To(Succeed(), "simulate the DB-credential ExternalSecret sync on the target cluster")
		waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeDBCredentialsReady, metav1.ConditionTrue, itEventuallyTimeout)

		// --- The admin-password ExternalSecret is materialised beside the Keystone
		// child, so it rides the target cluster's ESO. The helper resolves the
		// namespace from the CR and asserts the label claim on a placed one.
		simulateAdminPasswordExternalSecretSyncWhenPresent(t, ctx, targetClient, cp)
		mcExpectAbsent(t, ctx, mgmtClient, adminPasswordKey, &esov1.ExternalSecret{}, "admin-password ExternalSecret")
		waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeAdminPasswordReady, metav1.ConditionTrue, itEventuallyTimeout)

		// --- Every remote object is claimed by the five labels and by nothing
		// else. An owner reference would name a UID the target cluster cannot
		// resolve, and the labels are the only handle the teardown sweep has.
		for _, claimed := range []struct {
			key  client.ObjectKey
			obj  client.Object
			what string
		}{
			{mariadbKey, &mariadbv1alpha1.MariaDB{}, "MariaDB"},
			{memcachedKey, mcMemcached(), "Memcached"},
			{tenantSAKey, &corev1.ServiceAccount{}, "tenant ServiceAccount"},
			{tenantCertKey, mcCertificate(), "tenant Certificate"},
			{tenantStoreKey, &esov1.SecretStore{}, "tenant SecretStore"},
			{dbCredSAKey, &corev1.ServiceAccount{}, "DB-credential ServiceAccount"},
			{dbCredCertKey, mcCertificate(), "DB-credential Certificate"},
			{dbCredKey, &esgenv1alpha1.VaultDynamicSecret{}, "DB-credential generator"},
			{dbCredKey, &esov1.ExternalSecret{}, "DB-credential ExternalSecret"},
			{adminPasswordKey, &esov1.ExternalSecret{}, "admin-password ExternalSecret"},
		} {
			mcExpectRemoteClaim(t, ctx, targetClient, claimed.key, claimed.obj, claimed.what, cp)
		}

		// --- The Keystone CR stays on the management cluster, carrying the ref its
		// own operator projects by. The target cluster does not even serve the
		// Keystone kind, so it could not be there.
		keystone := &keystonev1alpha1.Keystone{}
		mcEventuallyExists(t, ctx, mgmtClient, placedKey, keystone, "Keystone child")
		g.Expect(keystone.Spec.TargetClusterRef).NotTo(BeNil(),
			"the Keystone child must carry the ref the ControlPlane placed its service with")
		g.Expect(keystone.Spec.TargetClusterRef.Name).To(Equal(mcTargetCluster))
		g.Expect(keystone.OwnerReferences).To(BeEmpty(),
			"the API server rejects a cross-namespace controller owner reference")
		mcExpectAbsent(t, ctx, targetClient, placedKey, &keystonev1alpha1.Keystone{}, "Keystone child")

		// --- Placing children on a target cluster is what installs the
		// remote-children finalizer: nothing else can sweep them.
		live := &c5c3v1alpha1.ControlPlane{}
		g.Expect(mgmtClient.Get(ctx, cpKey, live)).To(Succeed())
		g.Expect(controllerutil.ContainsFinalizer(live, commonmulticluster.RemoteChildrenFinalizer)).To(BeTrue(),
			"a ControlPlane whose children live on another cluster must carry the remote-children finalizer")
		g.Expect(controllerutil.ContainsFinalizer(live, controlPlaneORCFinalizer)).To(BeTrue(),
			"the ORC-teardown finalizer must be installed as well")
	})

	t.Run("the K-ORC ensemble stays home and reads the admin password off the target", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// The blocking prefix ends at Keystone, and the tail group (KORC among it)
		// only runs once the child reports Ready.
		simulateKeystoneReadyWhenPresent(t, ctx, mgmtClient, placedKey)
		waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeKeystoneReady, metav1.ConditionTrue, itEventuallyTimeout)

		// The admin password is the ONE thing a reconcile pass reads off another
		// cluster. No ESO runs on either environment, so the ExternalSecret synced
		// above materialised nothing and reconcileKORC has nothing to read yet.
		cond := waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeKORCReady, metav1.ConditionFalse, itEventuallyTimeout)
		g.Expect(cond.Reason).To(Equal("WaitingForAdminPassword"),
			"the mint must defer while the admin password Secret does not exist on the target cluster")

		// A decoy of exactly the right name and namespace, on the WRONG cluster. It
		// is what makes the seed below discriminating: a reconcileKORC that read
		// through the local client instead of the resolved one would find this and
		// advance, and every assertion after it would hold for the wrong reason.
		g.Expect(mgmtClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: adminPasswordKey.Name, Namespace: adminPasswordKey.Namespace},
			Data:       map[string][]byte{"password": []byte(mcDecoyPassword)},
		})).To(Succeed(), "plant the decoy admin password on the management cluster")

		g.Consistently(func() string {
			live := &c5c3v1alpha1.ControlPlane{}
			if err := mgmtClient.Get(ctx, cpKey, live); err != nil {
				return ""
			}
			c := meta.FindStatusCondition(live.Status.Conditions, conditionTypeKORCReady)
			if c == nil {
				return ""
			}
			return c.Reason
		}, korcRequeueAfter+5*time.Second, itPollInterval).Should(Equal("WaitingForAdminPassword"),
			"the decoy on the management cluster must not satisfy a read that belongs to the target cluster")

		// The real one, where the placed ExternalSecret would have materialised it.
		g.Expect(targetClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: adminPasswordKey.Name, Namespace: adminPasswordKey.Namespace},
			Data:       map[string][]byte{"password": []byte(mcAdminPassword)},
		})).To(Succeed(), "seed the materialised admin password on the target cluster")

		// --- Everything the mint writes stays on the management cluster: K-ORC
		// runs there, whatever cluster the workload was placed on.
		acKey := client.ObjectKey{Namespace: mcNamespace, Name: adminAppCredentialName(cp)}
		mcEventuallyExists(t, ctx, mgmtClient, acKey, &orcv1alpha1.ApplicationCredential{}, "admin ApplicationCredential")
		mcExpectAbsent(t, ctx, targetClient, acKey, &orcv1alpha1.ApplicationCredential{}, "admin ApplicationCredential")

		simulateApplicationCredentialAvailableWhenPresent(t, ctx, mgmtClient, acKey)
		waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeKORCReady, metav1.ConditionTrue, itEventuallyTimeout)

		// --- On to the catalog, which is where the K-ORC Service/Endpoint pair the
		// ControlPlane advertises the placed Keystone with materialises. Every step
		// of it runs against the management cluster.
		cloudsYamlKey := client.ObjectKey{Namespace: mcNamespace, Name: korcCloudsYamlSecretName}
		mcEventuallyExists(t, ctx, mgmtClient, cloudsYamlKey, &esov1.ExternalSecret{}, "clouds.yaml ExternalSecret")
		g.Expect(simulators.SimulateExternalSecretSync(ctx, mgmtClient, cloudsYamlKey)).
			To(Succeed(), "simulate the k-orc clouds.yaml ExternalSecret sync")
		simulatePushSecretSyncedWhenPresent(t, ctx, mgmtClient,
			client.ObjectKey{Namespace: mcNamespace, Name: adminAppCredentialPushSecretName(cp)})
		simulateCloudsYamlMaterializedWhenPresent(t, ctx, mgmtClient, cp)
		waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeAdminCredentialReady, metav1.ConditionTrue, itEventuallyTimeout)

		simulateCatalogServiceEndpointAvailableWhenPresent(t, ctx, mgmtClient, cp)
		waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeCatalogReady, metav1.ConditionTrue, itEventuallyTimeout)

		catalogServiceKey := client.ObjectKey{Namespace: mcNamespace, Name: keystoneServiceName(cp)}
		catalogEndpointKey := client.ObjectKey{Namespace: mcNamespace, Name: keystoneEndpointName(cp)}
		g.Expect(mgmtClient.Get(ctx, catalogServiceKey, &orcv1alpha1.Service{})).To(Succeed(),
			"the catalog Service must be registered on the management cluster")
		g.Expect(mgmtClient.Get(ctx, catalogEndpointKey, &orcv1alpha1.Endpoint{})).To(Succeed(),
			"the catalog Endpoint must be registered on the management cluster")
		mcExpectAbsent(t, ctx, targetClient, catalogServiceKey, &orcv1alpha1.Service{}, "catalog Service")
		mcExpectAbsent(t, ctx, targetClient, catalogEndpointKey, &orcv1alpha1.Endpoint{}, "catalog Endpoint")
	})

	t.Run("a placed built-in service registers at home and mirrors its credentials", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// The image service joins the plane on the same target cluster, in a
		// namespace of its own. A placed catalog service has to advertise an
		// externally routable address for the reason Keystone does: its catalog row
		// is read from every cluster, and an in-cluster Service DNS name resolves on
		// none of the others.
		g.Eventually(func() error {
			live := &c5c3v1alpha1.ControlPlane{}
			if err := mgmtClient.Get(ctx, cpKey, live); err != nil {
				return err
			}
			live.Spec.Services.Glance = integrationGlanceService()
			live.Spec.Services.Glance.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
				Name:      mcGlanceNamespace,
				Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
			}
			live.Spec.Services.Glance.PublicEndpoint = "https://glance.example.com"
			live.Spec.Services.Glance.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: mcTargetCluster}
			return mgmtClient.Update(ctx, live)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "place the image service on the target cluster")

		// --- The backing services follow the service, so the shared database and
		// cache materialise a second time in the image service's namespace, on the
		// cluster that namespace lives on. Infrastructure short-circuits the pipeline
		// while either is converging, so nothing below runs until they report.
		glanceMariaDBKey := client.ObjectKey{
			Namespace: mcGlanceNamespace,
			Name:      cp.Spec.Infrastructure.Database.ClusterRef.Name,
		}
		glanceMemcachedKey := client.ObjectKey{
			Namespace: mcGlanceNamespace,
			Name:      cp.Spec.Infrastructure.Cache.ClusterRef.Name,
		}
		mcEventuallyExists(t, ctx, targetClient, glanceMariaDBKey, &mariadbv1alpha1.MariaDB{}, "image-side MariaDB")
		mcExpectAbsent(t, ctx, mgmtClient, glanceMariaDBKey, &mariadbv1alpha1.MariaDB{}, "image-side MariaDB")
		simulateMariaDBReadyWhenPresent(t, ctx, targetClient, glanceMariaDBKey)
		simulateMemcachedReadyWhenPresent(t, ctx, targetClient, glanceMemcachedKey)

		// --- The tenant-store trio of the placed namespace exists on BOTH clusters.
		// The copy on the target is what the ESO there materialises the service's
		// Secrets through; the copy at home is what the registration resolves, since
		// a KeystoneService is reconciled on the cluster its CR lives on.
		glanceSAKey := client.ObjectKey{Namespace: mcGlanceNamespace, Name: esoTenantServiceAccountName}
		glanceCertKey := client.ObjectKey{Namespace: mcGlanceNamespace, Name: esoTenantClientCertName}
		glanceStoreKey := client.ObjectKey{Namespace: mcGlanceNamespace, Name: esoTenantStoreName}
		for _, cluster := range []struct {
			name string
			c    client.Client
		}{
			{"management", mgmtClient},
			{"target", targetClient},
		} {
			mcEventuallyExists(t, ctx, cluster.c, glanceSAKey, &corev1.ServiceAccount{},
				cluster.name+"-side tenant ServiceAccount")
			mcEventuallyExists(t, ctx, cluster.c, glanceCertKey, mcCertificate(),
				cluster.name+"-side tenant Certificate")
			mcEventuallyExists(t, ctx, cluster.c, glanceStoreKey, &esov1.SecretStore{},
				cluster.name+"-side tenant SecretStore")
		}

		// --- The home copy alone does not open the gate: the plane holds on the
		// target's, and its condition names the cluster it is waiting on.
		ensureReadySecretStore(t, ctx, mgmtClient, esoTenantStoreName, mcGlanceNamespace)
		g.Eventually(func(ig Gomega) {
			live := &c5c3v1alpha1.ControlPlane{}
			ig.Expect(mgmtClient.Get(ctx, cpKey, live)).To(Succeed())
			cond := meta.FindStatusCondition(live.Status.Conditions, conditionTypeESOTenantStoreReady)
			ig.Expect(cond).NotTo(BeNil())
			ig.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			ig.Expect(cond.Reason).To(Equal("SecretStoreNotReady"))
			ig.Expect(cond.Message).To(ContainSubstring(mcGlanceNamespace))
			ig.Expect(cond.Message).To(ContainSubstring("on the target cluster"))
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
			"a placed namespace is gated on both of its tenant stores, and the message says which one is missing")

		ensureReadySecretStore(t, ctx, targetClient, esoTenantStoreName, mcGlanceNamespace)
		waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeESOTenantStoreReady, metav1.ConditionTrue, itEventuallyTimeout)

		// --- The registration itself is reconciled at home whatever cluster the
		// service runs on: it authenticates through the admin credential, which is
		// materialised on the management cluster alone.
		registrationKey := client.ObjectKey{Namespace: mcGlanceNamespace, Name: mcControlPlane + "-glance"}
		mcEventuallyExists(t, ctx, mgmtClient, registrationKey, &c5c3v1alpha1.KeystoneService{},
			"Glance registration")
		mcExpectAbsent(t, ctx, targetClient, registrationKey, &c5c3v1alpha1.KeystoneService{},
			"Glance registration")

		// --- Its credentials, though, follow the service. The registration delivers
		// them at home only, so the ControlPlane materialises the same OpenBao path a
		// second time on the cluster the image service runs on, under the same name
		// its pods read.
		mirrorKey := client.ObjectKey{Namespace: mcGlanceNamespace, Name: mcControlPlane + "-glance-credentials"}
		mirror := &esov1.ExternalSecret{}
		mcEventuallyExists(t, ctx, targetClient, mirrorKey, mirror, "registration credentials mirror")
		g.Expect(mirror.Spec.Data).NotTo(BeEmpty())
		g.Expect(mirror.Spec.Data[0].RemoteRef.Key).To(Equal(
			"openstack/keystone/"+mcGlanceNamespace+"/"+mcControlPlane+"-glance/service-accounts/credentials"),
			"the mirror reads the registration's own per-CR OpenBao path")
		g.Expect(mirror.Spec.SecretStoreRef.Kind).To(Equal(string(commonv1.SecretStoreKindNamespaced)))
		g.Expect(mirror.Spec.SecretStoreRef.Name).To(Equal(esoTenantStoreName),
			"the mirror routes through the ControlPlane's effective store, which is the tenant store beside it")
		mcExpectRemoteClaim(t, ctx, targetClient, mirrorKey, &esov1.ExternalSecret{},
			"registration credentials mirror", cp)

		// --- And that is as far as this plane goes: no KeystoneService controller
		// runs here, so the registration never provisions the Keystone account and
		// GlanceReady parks on it rather than projecting a Glance that would
		// authenticate as a user nothing created.
		cond := waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeGlanceReady, metav1.ConditionFalse, itEventuallyTimeout)
		g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceRegistration))
	})

	t.Run("a placed network service takes its bus credentials and its OVN gate with it", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// The OVNCentral the network service programs, created beside the
		// ControlPlane rather than in the network namespace: the ref below spells no
		// namespace, and an empty one resolves to the ControlPlane's own namespace.
		// The defaulting webhook writes that value into the ref, and
		// NeutronOVNCentralNamespace() reads an empty one the same way for a CR that
		// bypassed admission. Its targetClusterRef names the cluster the service is
		// placed on, so reconcileOVN selects the in-cluster database addresses and
		// never demands externallyReachable.
		ovnKey := client.ObjectKey{Namespace: mcNamespace, Name: mcOVNCentral}
		g.Expect(mgmtClient.Create(ctx, &ovnv1alpha1.OVNCentral{
			ObjectMeta: metav1.ObjectMeta{Name: mcOVNCentral, Namespace: mcNamespace},
			Spec: ovnv1alpha1.OVNCentralSpec{
				TLS:              ovnv1alpha1.OVNTLSSpec{IssuerRef: ovnv1alpha1.OVNIssuerRef{Name: "test-issuer"}},
				TargetClusterRef: &commonv1.TargetClusterRefSpec{Name: mcTargetCluster},
			},
		})).To(Succeed(), "create the referenced OVNCentral on the management cluster")
		simulateOVNCentralReadyWhenPresent(t, ctx, mgmtClient, ovnKey)

		// The network service joins the plane on the same target cluster, in a
		// namespace of its own, advertising an externally routable address for the
		// reason its image sibling does.
		g.Eventually(func() error {
			live := &c5c3v1alpha1.ControlPlane{}
			if err := mgmtClient.Get(ctx, cpKey, live); err != nil {
				return err
			}
			live.Spec.Services.Neutron = &c5c3v1alpha1.ServiceNeutronSpec{
				WorkerReplicas: ptr.To(int32(1)),
				OVN: c5c3v1alpha1.NeutronOVNSpec{
					CentralRef: c5c3v1alpha1.NeutronOVNCentralRef{Name: mcOVNCentral},
				},
				Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
					Name:      mcNetworkNamespace,
					Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
				},
				PublicEndpoint:   "https://neutron.example.com",
				TargetClusterRef: &commonv1.TargetClusterRefSpec{Name: mcTargetCluster},
			}
			return mgmtClient.Update(ctx, live)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "place the network service on the target cluster")

		// --- The namespace is created on both clusters, and the backing services
		// follow the service onto the target. Infrastructure short-circuits the
		// pipeline while either is converging, so nothing below runs until they
		// report.
		networkNSKey := client.ObjectKey{Name: mcNetworkNamespace}
		mcEventuallyExists(t, ctx, targetClient, networkNSKey, &corev1.Namespace{}, "network service namespace")
		mcEventuallyExists(t, ctx, mgmtClient, networkNSKey, &corev1.Namespace{}, "network service namespace")

		neutronMariaDBKey := client.ObjectKey{
			Namespace: mcNetworkNamespace,
			Name:      cp.Spec.Infrastructure.Database.ClusterRef.Name,
		}
		neutronMemcachedKey := client.ObjectKey{
			Namespace: mcNetworkNamespace,
			Name:      cp.Spec.Infrastructure.Cache.ClusterRef.Name,
		}
		mcEventuallyExists(t, ctx, targetClient, neutronMariaDBKey, &mariadbv1alpha1.MariaDB{}, "network-side MariaDB")
		mcExpectAbsent(t, ctx, mgmtClient, neutronMariaDBKey, &mariadbv1alpha1.MariaDB{}, "network-side MariaDB")
		simulateMariaDBReadyWhenPresent(t, ctx, targetClient, neutronMariaDBKey)
		simulateMemcachedReadyWhenPresent(t, ctx, targetClient, neutronMemcachedKey)

		// --- The tenant store of the placed namespace exists on both clusters, for
		// the reason the image service's does, and the plane is gated on both.
		networkStoreKey := client.ObjectKey{Namespace: mcNetworkNamespace, Name: esoTenantStoreName}
		mcEventuallyExists(t, ctx, mgmtClient, networkStoreKey, &esov1.SecretStore{}, "management-side tenant SecretStore")
		mcEventuallyExists(t, ctx, targetClient, networkStoreKey, &esov1.SecretStore{}, "target-side tenant SecretStore")
		ensureReadySecretStore(t, ctx, mgmtClient, esoTenantStoreName, mcNetworkNamespace)
		ensureReadySecretStore(t, ctx, targetClient, esoTenantStoreName, mcNetworkNamespace)
		waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeESOTenantStoreReady, metav1.ConditionTrue, itEventuallyTimeout)

		// --- The OVN gate. The ControlPlane owns nothing of it: it reads the central
		// on the management cluster and mirrors the verdict, which is what the
		// projection behind it consumes.
		// The wait is on the reason as well as on the status: before the network
		// service was declared OVNReady was already True, with reason OVNNotManaged.
		g.Eventually(func(ig Gomega) {
			live := &c5c3v1alpha1.ControlPlane{}
			ig.Expect(mgmtClient.Get(ctx, cpKey, live)).To(Succeed())
			cond := meta.FindStatusCondition(live.Status.Conditions, conditionTypeOVNReady)
			ig.Expect(cond).NotTo(BeNil())
			ig.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			ig.Expect(cond.Reason).To(Equal("OVNCentralReady"))
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
			"the referenced central serves both databases on the cluster the service was placed on")

		// --- The bus follows the service. It is declared and read in the
		// ControlPlane's own namespace on the management cluster, and delivered as a
		// Secret in the network namespace on the cluster the service runs on, claimed
		// by the ownership labels because no owner reference crosses a cluster.
		busSecret := &corev1.Secret{}
		mcEventuallyExists(t, ctx, targetClient, neutronBusKey, busSecret, "neutron messaging Secret")
		g.Expect(string(busSecret.Data[commonv1.DefaultTransportURLSecretKey])).To(Equal(mcBusTransportURL),
			"the placed service receives the URL the ControlPlane's own bus block declares")
		mcExpectRemoteClaim(t, ctx, targetClient, neutronBusKey, &corev1.Secret{}, "neutron messaging Secret", cp)
		mcExpectAbsent(t, ctx, mgmtClient, neutronBusKey, &corev1.Secret{}, "neutron messaging Secret")

		// --- The registration is reconciled at home whatever cluster the service
		// runs on, exactly as the image service's is.
		registrationKey := client.ObjectKey{Namespace: mcNetworkNamespace, Name: mcControlPlane + "-neutron"}
		mcEventuallyExists(t, ctx, mgmtClient, registrationKey, &c5c3v1alpha1.KeystoneService{},
			"Neutron registration")
		mcExpectAbsent(t, ctx, targetClient, registrationKey, &c5c3v1alpha1.KeystoneService{},
			"Neutron registration")

		// --- Its credentials, though, follow the service: the ControlPlane
		// materialises the registration's own OpenBao path a second time on the
		// cluster the network service runs on, under the name its pods read.
		mirrorKey := client.ObjectKey{Namespace: mcNetworkNamespace, Name: mcControlPlane + "-neutron-credentials"}
		mirror := &esov1.ExternalSecret{}
		mcEventuallyExists(t, ctx, targetClient, mirrorKey, mirror, "registration credentials mirror")
		g.Expect(mirror.Spec.Data).NotTo(BeEmpty())
		g.Expect(mirror.Spec.Data[0].RemoteRef.Key).To(Equal(
			"openstack/keystone/"+mcNetworkNamespace+"/"+mcControlPlane+"-neutron/service-accounts/credentials"),
			"the mirror reads the registration's own per-CR OpenBao path")
		mcExpectRemoteClaim(t, ctx, targetClient, mirrorKey, &esov1.ExternalSecret{},
			"registration credentials mirror", cp)

		// --- And that is as far as this plane goes: no KeystoneService controller
		// runs here, so the registration never provisions the Keystone account and
		// NeutronReady parks on it rather than projecting a Neutron that would
		// authenticate as a user nothing created.
		g.Eventually(func(ig Gomega) {
			live := &c5c3v1alpha1.ControlPlane{}
			ig.Expect(mgmtClient.Get(ctx, cpKey, live)).To(Succeed())
			cond := meta.FindStatusCondition(live.Status.Conditions, conditionTypeNeutronReady)
			ig.Expect(cond).NotTo(BeNil())
			ig.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			ig.Expect(cond.Reason).To(Equal(reasonWaitingForServiceRegistration))
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
			"the network service parks on the account its registration has not provisioned")
	})

	t.Run("a ControlPlane naming an unregistered cluster creates nothing", func(t *testing.T) {
		g := NewGomegaWithT(t)

		mcEnsureNamespace(t, ctx, mgmtClient, mcUnknownNamespace)

		unknown := integrationManagedControlPlane("unknown-cp", mcUnknownNamespace)
		unknown.Spec.Services.Keystone.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
			Name:      mcUnknownKeystoneNS,
			Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
		}
		unknown.Spec.Services.Keystone.PublicEndpoint = "https://keystone.elsewhere.example.com/v3"
		unknown.Spec.Services.Keystone.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: mcUnknownCluster}
		g.Expect(mgmtClient.Create(ctx, unknown)).To(Succeed(),
			"a ref naming an unregistered cluster is admitted: registration is a runtime fact, not a schema one")

		unknownKey := types.NamespacedName{Name: unknown.Name, Namespace: mcUnknownNamespace}
		cond := waitForControlPlaneCondition(t, ctx, mgmtClient, unknownKey,
			conditionTypeNamespacesReady, metav1.ConditionFalse, itEventuallyTimeout)
		g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
		g.Expect(cond.Message).To(ContainSubstring("cluster not found"),
			"the resolver's message should reach the condition verbatim")

		// The client is resolved BEFORE anything is written, so the namespace is
		// created on neither cluster — not even on the management one, where it
		// would have been ensured for a resolvable ref.
		for _, c := range []client.Client{mgmtClient, targetClient} {
			mcExpectAbsent(t, ctx, c, client.ObjectKey{Name: mcUnknownKeystoneNS},
				&corev1.Namespace{}, "service namespace")
		}

		// Nothing was placed, so there is nothing for the remote-children finalizer
		// to hold the CR open for.
		live := &c5c3v1alpha1.ControlPlane{}
		g.Expect(mgmtClient.Get(ctx, unknownKey, live)).To(Succeed())
		g.Expect(controllerutil.ContainsFinalizer(live, commonmulticluster.RemoteChildrenFinalizer)).To(BeFalse(),
			"the remote-children finalizer must not go on a CR whose target cluster never resolved")
	})

	t.Run("deleting the ControlPlane sweeps the placed namespace off the target", func(t *testing.T) {
		g := NewGomegaWithT(t)

		live := &c5c3v1alpha1.ControlPlane{}
		g.Expect(mgmtClient.Get(ctx, cpKey, live)).To(Succeed())
		g.Expect(mgmtClient.Delete(ctx, live)).To(Succeed(), "delete the placing ControlPlane")

		// The Keystone child is deleted on the management cluster, where it lives,
		// and the sweep waits for it: its own operator's ESO cleanup authenticates
		// through the tenant store the sweep is about to remove.
		g.Eventually(func() bool {
			return apierrors.IsNotFound(mgmtClient.Get(ctx, placedKey, &keystonev1alpha1.Keystone{}))
		}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
			"the cross-namespace Keystone child must be deleted explicitly — no GC cascade reaches it")

		// Everything the ControlPlane placed is swept off the target cluster. The
		// sweep does not wait for what it deleted, and a delete is asynchronous, so
		// each object is polled until it is gone rather than read once.
		swept := []struct {
			key  client.ObjectKey
			obj  client.Object
			what string
		}{
			{mariadbKey, &mariadbv1alpha1.MariaDB{}, "MariaDB"},
			{memcachedKey, mcMemcached(), "Memcached"},
			{tenantSAKey, &corev1.ServiceAccount{}, "tenant ServiceAccount"},
			{tenantCertKey, mcCertificate(), "tenant Certificate"},
			{tenantStoreKey, &esov1.SecretStore{}, "tenant SecretStore"},
			{dbCredSAKey, &corev1.ServiceAccount{}, "DB-credential ServiceAccount"},
			{dbCredCertKey, mcCertificate(), "DB-credential Certificate"},
			{dbCredKey, &esgenv1alpha1.VaultDynamicSecret{}, "DB-credential generator"},
			{dbCredKey, &esov1.ExternalSecret{}, "DB-credential ExternalSecret"},
			{adminPasswordKey, &esov1.ExternalSecret{}, "admin-password ExternalSecret"},
			{neutronBusKey, &corev1.Secret{}, "neutron messaging Secret"},
		}
		g.Eventually(func(ig Gomega) {
			for _, child := range swept {
				ig.Expect(apierrors.IsNotFound(targetClient.Get(ctx, child.key, child.obj))).
					To(BeTrue(), "%s %s should be swept off the target cluster", child.what, child.key)
			}
			pushSecrets := &esov1alpha1.PushSecretList{}
			ig.Expect(targetClient.List(ctx, pushSecrets, client.InNamespace(mcKeystoneNamespace))).To(Succeed())
			ig.Expect(pushSecrets.Items).To(BeEmpty(), "no PushSecret should survive the sweep")
		}, itEventuallyTimeout, itPollInterval).Should(Succeed())

		// envtest runs no namespace controller, so a deleted namespace stays
		// Terminating forever. The DeletionTimestamp is what the operator is
		// responsible for, on both clusters — it created the namespace on both.
		for name, c := range map[string]client.Client{"target": targetClient, "management": mgmtClient} {
			for _, namespace := range []string{mcKeystoneNamespace, mcNetworkNamespace} {
				g.Eventually(func() bool {
					ns := &corev1.Namespace{}
					if err := c.Get(ctx, client.ObjectKey{Name: namespace}, ns); err != nil {
						return apierrors.IsNotFound(err)
					}
					return !ns.DeletionTimestamp.IsZero()
				}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
					"the Managed %s namespace must be deleted on the %s cluster", namespace, name)
			}
		}

		// Both finalizers released, so the CR leaves etcd.
		g.Eventually(func() bool {
			return apierrors.IsNotFound(mgmtClient.Get(ctx, cpKey, &c5c3v1alpha1.ControlPlane{}))
		}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
			"the ControlPlane should leave etcd once the ORC and remote-children finalizers are released")

		// What the ControlPlane only ever READ stays. The sweep selects on the
		// ownership labels, and nothing stamped them on the admin-password Secret
		// this test seeded, so a sweep that took it would be taking somebody else's
		// object.
		g.Expect(targetClient.Get(ctx, adminPasswordKey, &corev1.Secret{})).To(Succeed(),
			"the seeded admin-password Secret is an input, not a child, and must survive the sweep")
	})
}

// mcMemcached returns an empty Memcached carrier: the memcached.c5c3.io CRD
// ships no Go module, so the reconciler creates it — and this test reads it —
// as an *unstructured.Unstructured carrying memcachedGVK. A fresh object per
// call, because every Get populates the one it is handed.
func mcMemcached() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(memcachedGVK)
	return u
}

// mcCertificate returns an empty cert-manager Certificate carrier, for the same
// reason as mcMemcached: no Go type ships for it, so it is handled unstructured.
func mcCertificate() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(certificateGVK)
	return u
}

// mcEnsureNamespace creates the namespace on c, tolerating one that already
// exists so a subtest can seed the same name on both clusters.
func mcEnsureNamespace(t testing.TB, ctx context.Context, c client.Client, name string) {
	t.Helper()
	g := NewGomegaWithT(t)

	err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	if apierrors.IsAlreadyExists(err) {
		return
	}
	g.Expect(err).NotTo(HaveOccurred(), "create namespace %s", name)
}

// mcEventuallyExists polls c until the object at key exists.
func mcEventuallyExists(
	t testing.TB, ctx context.Context, c client.Client,
	key client.ObjectKey, obj client.Object, what string,
) {
	t.Helper()
	g := NewGomegaWithT(t)

	g.Eventually(func() error {
		return c.Get(ctx, key, obj)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "%s %s should exist", what, key)
}

// mcExpectAbsent asserts the object at key does not exist on c. A missing
// namespace answers NotFound too, and a kind the cluster does not serve at all
// answers no-match — both mean the object is not there, which is what the
// management-side half of every split assertion is claiming.
func mcExpectAbsent(
	t testing.TB, ctx context.Context, c client.Client,
	key client.ObjectKey, obj client.Object, what string,
) {
	t.Helper()
	g := NewGomegaWithT(t)

	err := c.Get(ctx, key, obj)
	g.Expect(apierrors.IsNotFound(err) || meta.IsNoMatchError(err)).To(BeTrue(),
		"%s %s should not exist on this cluster, got %v", what, key, err)
}

// mcExpectRemoteClaim polls c until the object at key is claimed the way a
// remote child has to be: by the five ownership labels — the owner triple the
// shared teardown selects on, plus the cross-namespace pair this operator's
// watch legs map a child back to its ControlPlane by — and by no owner
// reference at all. It polls rather than reads once because the projection is
// asynchronous.
func mcExpectRemoteClaim(
	t testing.TB, ctx context.Context, c client.Client,
	key client.ObjectKey, obj client.Object, what string, cp *c5c3v1alpha1.ControlPlane,
) {
	t.Helper()
	g := NewGomegaWithT(t)

	want := map[string]string{
		commonmulticluster.OwnerKindLabel:      "ControlPlane",
		commonmulticluster.OwnerNameLabel:      cp.Name,
		commonmulticluster.OwnerNamespaceLabel: cp.Namespace,
		controlPlaneNameLabel:                  cp.Name,
		controlPlaneNamespaceLabel:             cp.Namespace,
	}

	g.Eventually(func(ig Gomega) {
		ig.Expect(c.Get(ctx, key, obj)).To(Succeed())
		ig.Expect(obj.GetOwnerReferences()).To(BeEmpty(),
			"%s %s must carry no owner reference: it would name a UID this cluster cannot resolve", what, key)
		for label, value := range want {
			ig.Expect(obj.GetLabels()).To(HaveKeyWithValue(label, value),
				"%s %s should be labelled as owned by ControlPlane %s/%s", what, key, cp.Namespace, cp.Name)
		}
	}, itEventuallyTimeout, itPollInterval).Should(Succeed())
}
