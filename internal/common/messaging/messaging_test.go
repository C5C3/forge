// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package messaging

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

const (
	msgInstance   = "neutron"
	msgNamespace  = "openstack"
	msgCluster    = "openstack-rabbitmq"
	msgUserSecret = "openstack-rabbitmq-default-user"
	msgDerived    = "neutron-transport-url"
	msgCondition  = "SecretsReady"
	msgRequeue    = 15 * time.Second
)

func msgScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return s
}

func msgOwner() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "neutron-owner", Namespace: msgNamespace, UID: "msg-uid"},
	}
}

// rabbitmqCluster builds the unstructured RabbitmqCluster the managed flow reads.
// A non-empty secretName is reported under status.defaultUser.secretReference; an
// empty one leaves the status absent, the shape before the operator has created
// the default user.
func rabbitmqCluster(secretName, secretNamespace string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(RabbitmqClusterGVK)
	u.SetName(msgCluster)
	u.SetNamespace(msgNamespace)
	if secretName == "" {
		return u
	}
	ref := map[string]interface{}{"name": secretName}
	if secretNamespace != "" {
		ref["namespace"] = secretNamespace
	}
	_ = unstructured.SetNestedMap(u.Object, ref, "status", "defaultUser", "secretReference")
	return u
}

func defaultUserSecret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: msgUserSecret, Namespace: msgNamespace},
		Data:       data,
	}
}

func fullDefaultUserData() map[string][]byte {
	return map[string][]byte{
		"username": []byte("default_user_abc"),
		"password": []byte("s3cr3t"),
		"host":     []byte("openstack-rabbitmq.openstack.svc"),
		"port":     []byte("5672"),
	}
}

func brownfieldSecret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "external-bus", Namespace: msgNamespace},
		Data:       data,
	}
}

func managedSpec() *commonv1.MessagingSpec {
	return &commonv1.MessagingSpec{ClusterRef: &corev1.LocalObjectReference{Name: msgCluster}}
}

func brownfieldSpec(key string) *commonv1.MessagingSpec {
	return &commonv1.MessagingSpec{
		SecretRef: &commonv1.SecretRefSpec{Name: "external-bus", Key: key},
	}
}

func msgParams(c client.Client, s *runtime.Scheme, owner client.Object,
	spec *commonv1.MessagingSpec, conds *[]metav1.Condition,
) TransportURLSecretFlowParams {
	return TransportURLSecretFlowParams{
		Client:        c,
		Scheme:        s,
		Owner:         owner,
		InstanceName:  msgInstance,
		Namespace:     msgNamespace,
		Messaging:     spec,
		Conditions:    conds,
		Generation:    3,
		ConditionType: msgCondition,
		RequeueAfter:  msgRequeue,
	}
}

func findMsgCond(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}

// expectNoDerivedSecret asserts that the flow materialised nothing.
func expectNoDerivedSecret(g *WithT, c client.Client) {
	g.THelper()
	err := c.Get(context.Background(), client.ObjectKey{Name: msgDerived, Namespace: msgNamespace}, &corev1.Secret{})
	g.Expect(err).To(HaveOccurred(), "no derived Secret may exist on a waiting or failing path")
}

// --- pure helpers ---

func TestTransportURLEnvVar(t *testing.T) {
	g := NewWithT(t)
	g.Expect(TransportURLSecretName(msgInstance)).To(Equal(msgDerived))

	e := TransportURLEnvVar(msgInstance)
	g.Expect(e.Name).To(Equal("OS_DEFAULT__TRANSPORT_URL"))
	g.Expect(e.ValueFrom.SecretKeyRef.Name).To(Equal(msgDerived))
	g.Expect(e.ValueFrom.SecretKeyRef.Key).To(Equal("transport_url"))
	g.Expect(e.Value).To(BeEmpty(), "the URL carries the broker password and must never be an inline value")
}

func TestBuildTransportURL(t *testing.T) {
	g := NewWithT(t)
	hexDigest := regexp.MustCompile(`^[0-9a-f]{64}$`)

	t.Run("escapes the userinfo", func(t *testing.T) {
		g := NewWithT(t)
		got, digest := BuildTransportURL("u", "p@ss:w/rd", "openstack-rabbitmq.openstack.svc", 5672)
		g.Expect(got).To(Equal("rabbit://u:p%40ss%3Aw%2Frd@openstack-rabbitmq.openstack.svc:5672/"))
		g.Expect(digest).To(MatchRegexp(hexDigest.String()))
	})

	base, baseDigest := BuildTransportURL("u", "p", "h", 5672)
	g.Expect(base).To(Equal("rabbit://u:p@h:5672/"))
	g.Expect(baseDigest).To(MatchRegexp(hexDigest.String()))

	// The digest is stable for identical inputs and moves for every changed one,
	// so a rotated credential or a moved broker rolls the pods.
	_, same := BuildTransportURL("u", "p", "h", 5672)
	g.Expect(same).To(Equal(baseDigest))

	for _, tc := range []struct {
		name                 string
		user, password, host string
		port                 int32
	}{
		{name: "username", user: "other", password: "p", host: "h", port: 5672},
		{name: "password", user: "u", password: "other", host: "h", port: 5672},
		{name: "host", user: "u", password: "p", host: "other", port: 5672},
		{name: "port", user: "u", password: "p", host: "h", port: 5671},
	} {
		t.Run("digest changes with the "+tc.name, func(t *testing.T) {
			g := NewWithT(t)
			_, digest := BuildTransportURL(tc.user, tc.password, tc.host, tc.port)
			g.Expect(digest).To(MatchRegexp(hexDigest.String()))
			g.Expect(digest).NotTo(Equal(baseDigest))
		})
	}
}

func TestRabbitSection(t *testing.T) {
	g := NewWithT(t)

	plain := RabbitSection(nil, "")
	g.Expect(plain).To(Equal(map[string]string{
		"rabbit_quorum_queue":           "true",
		"rabbit_transient_quorum_queue": "true",
		"use_queue_manager":             "true",
	}))

	withTLS := RabbitSection(&commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "rabbitmq-ca", Key: "ca.crt"},
	}, "/etc/rabbitmq-ca/ca.crt")
	g.Expect(withTLS).To(HaveKeyWithValue("ssl", "true"))
	g.Expect(withTLS).To(HaveKeyWithValue("ssl_ca_file", "/etc/rabbitmq-ca/ca.crt"))
	g.Expect(withTLS).To(HaveKeyWithValue("rabbit_quorum_queue", "true"))

	// The URL carries the broker password, so it is delivered through the env
	// override and never rendered into the ConfigMap section.
	g.Expect(plain).NotTo(HaveKey("transport_url"))
	g.Expect(withTLS).NotTo(HaveKey("transport_url"))
}

func TestEgressPort(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		want int32
	}{
		{name: "explicit port", url: "rabbit://u:p@h:5671/", want: 5671},
		{name: "no port", url: "rabbit://u:p@h/", want: 5672},
		{name: "unparseable", url: "rabbit://u:p@h\x7f/", want: 5672},
		{name: "non-numeric port", url: "rabbit://u:p@h:amqp/", want: 5672},
		{name: "out of range port", url: "rabbit://u:p@h:70000/", want: 5672},
		{name: "empty", url: "", want: 5672},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(EgressPort(tc.url)).To(Equal(tc.want))
		})
	}
}

// --- spec shape ---

func TestReconcileTransportURLSecret_specShape(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spec    *commonv1.MessagingSpec
		wantErr string
	}{
		{name: "NilSpec", spec: nil, wantErr: "messaging spec is nil"},
		{name: "NeitherMode", spec: &commonv1.MessagingSpec{}, wantErr: "messaging spec sets neither clusterRef nor secretRef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			s := msgScheme()
			owner := msgOwner()
			var conds []metav1.Condition
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()

			_, _, digest, err := ReconcileTransportURLSecret(context.Background(), msgParams(c, s, owner, tc.spec, &conds))
			g.Expect(err).To(MatchError(tc.wantErr))
			g.Expect(digest).To(BeEmpty())
			g.Expect(conds).To(BeEmpty(), "an unusable spec is an error, not a waiting condition")
			expectNoDerivedSecret(g, c)
		})
	}
}

// --- managed mode ---

func TestReconcileTransportURLSecret_managedCreatesClaimedSecret(t *testing.T) {
	g := NewWithT(t)
	s := msgScheme()
	owner := msgOwner()
	var conds []metav1.Condition
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, rabbitmqCluster(msgUserSecret, msgNamespace), defaultUserSecret(fullDefaultUserData())).
		Build()

	res, _, digest, err := ReconcileTransportURLSecret(context.Background(), msgParams(c, s, owner, managedSpec(), &conds))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	wantURL, wantDigest := BuildTransportURL("default_user_abc", "s3cr3t", "openstack-rabbitmq.openstack.svc", 5672)
	g.Expect(digest).To(Equal(wantDigest))

	derived := &corev1.Secret{}
	g.Expect(c.Get(context.Background(),
		client.ObjectKey{Name: msgDerived, Namespace: msgNamespace}, derived)).To(Succeed())
	g.Expect(derived.Data).To(HaveLen(1))
	g.Expect(string(derived.Data[commonv1.DefaultTransportURLSecretKey])).To(Equal(wantURL))
	// multicluster.Claim on a local client sets a controller owner reference, so
	// the derived Secret is reaped with the owning CR.
	g.Expect(derived.OwnerReferences).To(HaveLen(1))
	g.Expect(metav1.IsControlledBy(derived, owner)).To(BeTrue())
	// The flow reports readiness to nobody: the caller's Secrets step does that.
	g.Expect(conds).To(BeEmpty())
}

// TestReconcileTransportURLSecret_managedDefaultsSecretNamespace pins the
// fallback for a secretReference that names no namespace: the flow reads the
// Secret from its own namespace rather than from the empty one.
func TestReconcileTransportURLSecret_managedDefaultsSecretNamespace(t *testing.T) {
	g := NewWithT(t)
	s := msgScheme()
	owner := msgOwner()
	var conds []metav1.Condition
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, rabbitmqCluster(msgUserSecret, ""), defaultUserSecret(fullDefaultUserData())).
		Build()

	_, _, digest, err := ReconcileTransportURLSecret(context.Background(), msgParams(c, s, owner, managedSpec(), &conds))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(digest).NotTo(BeEmpty())
}

func TestReconcileTransportURLSecret_managedRepairsDrift(t *testing.T) {
	for _, tc := range []struct {
		name string
		data map[string][]byte
	}{
		{
			name: "extra key",
			data: map[string][]byte{commonv1.DefaultTransportURLSecretKey: []byte("placeholder"), "extra": []byte("x")},
		},
		{
			name: "changed value",
			data: map[string][]byte{commonv1.DefaultTransportURLSecretKey: []byte("rabbit://stale:stale@old:5672/")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			s := msgScheme()
			owner := msgOwner()
			var conds []metav1.Condition
			drifted := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: msgDerived, Namespace: msgNamespace},
				Data:       tc.data,
			}
			c := fake.NewClientBuilder().WithScheme(s).
				WithObjects(owner, drifted, rabbitmqCluster(msgUserSecret, msgNamespace),
					defaultUserSecret(fullDefaultUserData())).
				Build()

			_, _, digest, err := ReconcileTransportURLSecret(context.Background(),
				msgParams(c, s, owner, managedSpec(), &conds))
			g.Expect(err).NotTo(HaveOccurred())

			wantURL, wantDigest := BuildTransportURL("default_user_abc", "s3cr3t", "openstack-rabbitmq.openstack.svc", 5672)
			g.Expect(digest).To(Equal(wantDigest))

			derived := &corev1.Secret{}
			g.Expect(c.Get(context.Background(),
				client.ObjectKey{Name: msgDerived, Namespace: msgNamespace}, derived)).To(Succeed())
			// Data is replaced wholesale: the extra key is gone and the value refreshed.
			g.Expect(derived.Data).To(HaveLen(1))
			g.Expect(string(derived.Data[commonv1.DefaultTransportURLSecretKey])).To(Equal(wantURL))
		})
	}
}

// foreignOwnerReference is the controller reference a CR other than the flow's
// owner leaves on a Secret of the same derived name. Both kinds of this
// operator derive <name>-transport-url from their own bare name, so two CRs
// sharing a name in one namespace collide on it.
func foreignOwnerReference() []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{
		APIVersion:         "v1",
		Kind:               "ConfigMap",
		Name:               "other-owner",
		UID:                "other-uid",
		Controller:         &yes,
		BlockOwnerDeletion: &yes,
	}}
}

// TestReconcileTransportURLSecret_refusesForeignSecret pins the ownership gate on
// the update branch: a derived Secret another CR controls is refused rather than
// rewritten, so two owners resolving to different brokers cannot flip one Secret
// back and forth and roll both workloads on every pass.
func TestReconcileTransportURLSecret_refusesForeignSecret(t *testing.T) {
	g := NewWithT(t)
	s := msgScheme()
	owner := msgOwner()
	var conds []metav1.Condition
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            msgDerived,
			Namespace:       msgNamespace,
			OwnerReferences: foreignOwnerReference(),
		},
		Data: map[string][]byte{
			commonv1.DefaultTransportURLSecretKey: []byte("rabbit://other:other@other:5672/"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, foreign, rabbitmqCluster(msgUserSecret, msgNamespace),
			defaultUserSecret(fullDefaultUserData())).
		Build()

	_, url, digest, err := ReconcileTransportURLSecret(context.Background(),
		msgParams(c, s, owner, managedSpec(), &conds))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("refusing to overwrite derived Secret openstack/neutron-transport-url"))
	g.Expect(url).To(BeEmpty())
	g.Expect(digest).To(BeEmpty())

	kept := &corev1.Secret{}
	g.Expect(c.Get(context.Background(),
		client.ObjectKey{Name: msgDerived, Namespace: msgNamespace}, kept)).To(Succeed())
	g.Expect(string(kept.Data[commonv1.DefaultTransportURLSecretKey])).
		To(Equal("rabbit://other:other@other:5672/"), "the foreign Secret must be left untouched")
	g.Expect(kept.OwnerReferences).To(Equal(foreignOwnerReference()))
}

// TestReconcileTransportURLSecret_claimsUnownedSecret pins the other half of the
// gate: a Secret nobody owns is adopted and the ownership persisted even when
// the URL already matches, so the broker password is reaped with the CR rather
// than surviving it.
func TestReconcileTransportURLSecret_claimsUnownedSecret(t *testing.T) {
	g := NewWithT(t)
	s := msgScheme()
	owner := msgOwner()
	var conds []metav1.Condition
	wantURL, _ := BuildTransportURL("default_user_abc", "s3cr3t", "openstack-rabbitmq.openstack.svc", 5672)
	unowned := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: msgDerived, Namespace: msgNamespace},
		Data:       map[string][]byte{commonv1.DefaultTransportURLSecretKey: []byte(wantURL)},
	}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, unowned, rabbitmqCluster(msgUserSecret, msgNamespace),
			defaultUserSecret(fullDefaultUserData())).
		Build()

	_, _, _, err := ReconcileTransportURLSecret(context.Background(),
		msgParams(c, s, owner, managedSpec(), &conds))
	g.Expect(err).NotTo(HaveOccurred())

	adopted := &corev1.Secret{}
	g.Expect(c.Get(context.Background(),
		client.ObjectKey{Name: msgDerived, Namespace: msgNamespace}, adopted)).To(Succeed())
	g.Expect(metav1.IsControlledBy(adopted, owner)).To(BeTrue())
	g.Expect(string(adopted.Data[commonv1.DefaultTransportURLSecretKey])).To(Equal(wantURL))
}

func TestReconcileTransportURLSecret_managedWaits(t *testing.T) {
	for _, tc := range []struct {
		name        string
		objects     []client.Object
		wantMessage []string
	}{
		{
			name:        "cluster not found",
			objects:     nil,
			wantMessage: []string{"RabbitmqCluster openstack/openstack-rabbitmq", "not found"},
		},
		{
			name:        "secretReference absent",
			objects:     []client.Object{rabbitmqCluster("", "")},
			wantMessage: []string{"RabbitmqCluster openstack/openstack-rabbitmq", "status.defaultUser.secretReference.name"},
		},
		{
			name:        "default-user Secret not found",
			objects:     []client.Object{rabbitmqCluster(msgUserSecret, msgNamespace)},
			wantMessage: []string{"default-user Secret openstack/openstack-rabbitmq-default-user", "not found"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			s := msgScheme()
			owner := msgOwner()
			var conds []metav1.Condition
			objects := append([]client.Object{owner}, tc.objects...)
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).Build()

			res, _, digest, err := ReconcileTransportURLSecret(context.Background(),
				msgParams(c, s, owner, managedSpec(), &conds))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(digest).To(BeEmpty())
			g.Expect(res.RequeueAfter).To(Equal(msgRequeue))

			cond := findMsgCond(conds, msgCondition)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(ReasonWaitingForMessagingCredentials))
			g.Expect(cond.ObservedGeneration).To(Equal(int64(3)))
			for _, part := range tc.wantMessage {
				g.Expect(cond.Message).To(ContainSubstring(part))
			}
			expectNoDerivedSecret(g, c)
		})
	}
}

// The "secretReference absent" case above covers a cluster with no status at all;
// this one covers a status that carries the field with an empty string.
func TestReconcileTransportURLSecret_managedWaitsOnEmptySecretReferenceName(t *testing.T) {
	g := NewWithT(t)
	s := msgScheme()
	owner := msgOwner()
	var conds []metav1.Condition
	cluster := rabbitmqCluster(msgUserSecret, msgNamespace)
	g.Expect(unstructured.SetNestedField(cluster.Object, "",
		"status", "defaultUser", "secretReference", "name")).To(Succeed())
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner, cluster).Build()

	res, _, digest, err := ReconcileTransportURLSecret(context.Background(), msgParams(c, s, owner, managedSpec(), &conds))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(digest).To(BeEmpty())
	g.Expect(res.RequeueAfter).To(Equal(msgRequeue))
	g.Expect(findMsgCond(conds, msgCondition).Message).
		To(ContainSubstring("status.defaultUser.secretReference.name"))
	expectNoDerivedSecret(g, c)
}

func TestReconcileTransportURLSecret_managedWaitsOnIncompleteDefaultUser(t *testing.T) {
	for _, key := range []string{"username", "password", "host", "port"} {
		for _, variant := range []struct {
			name  string
			value []byte
		}{
			{name: "missing", value: nil},
			{name: "empty", value: []byte("")},
		} {
			t.Run(key+" "+variant.name, func(t *testing.T) {
				g := NewWithT(t)
				s := msgScheme()
				owner := msgOwner()
				var conds []metav1.Condition
				data := fullDefaultUserData()
				if variant.value == nil {
					delete(data, key)
				} else {
					data[key] = variant.value
				}
				c := fake.NewClientBuilder().WithScheme(s).
					WithObjects(owner, rabbitmqCluster(msgUserSecret, msgNamespace), defaultUserSecret(data)).
					Build()

				res, _, digest, err := ReconcileTransportURLSecret(context.Background(),
					msgParams(c, s, owner, managedSpec(), &conds))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(digest).To(BeEmpty())
				g.Expect(res.RequeueAfter).To(Equal(msgRequeue))

				cond := findMsgCond(conds, msgCondition)
				g.Expect(cond.Reason).To(Equal(ReasonWaitingForMessagingCredentials))
				g.Expect(cond.Message).To(ContainSubstring("default-user Secret openstack/openstack-rabbitmq-default-user"))
				g.Expect(cond.Message).To(ContainSubstring(`missing key "` + key + `"`))
				expectNoDerivedSecret(g, c)
			})
		}
	}
}

func TestReconcileTransportURLSecret_managedWrapsClusterReadError(t *testing.T) {
	g := NewWithT(t)
	s := msgScheme()
	owner := msgOwner()
	var conds []metav1.Condition
	boom := errors.New("etcd unavailable")
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, rabbitmqCluster(msgUserSecret, msgNamespace)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*unstructured.Unstructured); ok {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()

	_, _, digest, err := ReconcileTransportURLSecret(context.Background(), msgParams(c, s, owner, managedSpec(), &conds))
	g.Expect(err).To(MatchError(boom))
	g.Expect(err.Error()).To(Equal("reading RabbitmqCluster openstack/openstack-rabbitmq: etcd unavailable"))
	g.Expect(digest).To(BeEmpty())
	g.Expect(conds).To(BeEmpty())
	expectNoDerivedSecret(g, c)
}

func TestReconcileTransportURLSecret_managedWrapsSecretReadError(t *testing.T) {
	g := NewWithT(t)
	s := msgScheme()
	owner := msgOwner()
	var conds []metav1.Condition
	boom := errors.New("etcd unavailable")
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, rabbitmqCluster(msgUserSecret, msgNamespace), defaultUserSecret(fullDefaultUserData())).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if key.Name == msgUserSecret {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()

	_, _, digest, err := ReconcileTransportURLSecret(context.Background(), msgParams(c, s, owner, managedSpec(), &conds))
	g.Expect(err).To(MatchError(boom))
	g.Expect(err.Error()).
		To(Equal("reading default-user Secret openstack/openstack-rabbitmq-default-user: etcd unavailable"))
	g.Expect(digest).To(BeEmpty())
	expectNoDerivedSecret(g, c)
}

func TestReconcileTransportURLSecret_managedRejectsNonIntegerPort(t *testing.T) {
	g := NewWithT(t)
	s := msgScheme()
	owner := msgOwner()
	var conds []metav1.Condition
	data := fullDefaultUserData()
	data["port"] = []byte("amqp")
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, rabbitmqCluster(msgUserSecret, msgNamespace), defaultUserSecret(data)).
		Build()

	_, _, digest, err := ReconcileTransportURLSecret(context.Background(), msgParams(c, s, owner, managedSpec(), &conds))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(HavePrefix(`parsing default-user port "amqp": `))
	g.Expect(digest).To(BeEmpty())
	g.Expect(conds).To(BeEmpty())
	expectNoDerivedSecret(g, c)
}

// --- brownfield mode ---

func TestReconcileTransportURLSecret_brownfieldCopiesVerbatim(t *testing.T) {
	const external = "rabbit://svc:pw@bus.example.com:5671/neutron?ssl=1"

	for _, tc := range []struct {
		name    string
		specKey string
		dataKey string
	}{
		{name: "default key", specKey: "", dataKey: commonv1.DefaultTransportURLSecretKey},
		{name: "explicit key", specKey: "bus-url", dataKey: "bus-url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			s := msgScheme()
			owner := msgOwner()
			var conds []metav1.Condition
			c := fake.NewClientBuilder().WithScheme(s).
				WithObjects(owner, brownfieldSecret(map[string][]byte{tc.dataKey: []byte(external)})).
				Build()

			res, _, digest, err := ReconcileTransportURLSecret(context.Background(),
				msgParams(c, s, owner, brownfieldSpec(tc.specKey), &conds))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.IsZero()).To(BeTrue())
			g.Expect(digest).NotTo(BeEmpty())

			derived := &corev1.Secret{}
			g.Expect(c.Get(context.Background(),
				client.ObjectKey{Name: msgDerived, Namespace: msgNamespace}, derived)).To(Succeed())
			g.Expect(derived.Data).To(HaveLen(1))
			// The vhost, the port and the query survive untouched.
			g.Expect(string(derived.Data[commonv1.DefaultTransportURLSecretKey])).To(Equal(external))
			g.Expect(metav1.IsControlledBy(derived, owner)).To(BeTrue())
			g.Expect(EgressPort(external)).To(Equal(int32(5671)))
		})
	}
}

func TestReconcileTransportURLSecret_brownfieldWaits(t *testing.T) {
	for _, tc := range []struct {
		name        string
		objects     []client.Object
		wantMessage []string
	}{
		{
			name:        "Secret not found",
			objects:     nil,
			wantMessage: []string{"brownfield transport-URL Secret openstack/external-bus", "not found"},
		},
		{
			name:        "key missing",
			objects:     []client.Object{brownfieldSecret(map[string][]byte{"other": []byte("rabbit://u:p@h/")})},
			wantMessage: []string{"brownfield transport-URL Secret openstack/external-bus", `missing key "transport_url"`},
		},
		{
			name:        "value empty",
			objects:     []client.Object{brownfieldSecret(map[string][]byte{commonv1.DefaultTransportURLSecretKey: []byte("")})},
			wantMessage: []string{"brownfield transport-URL Secret openstack/external-bus", `missing key "transport_url"`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			s := msgScheme()
			owner := msgOwner()
			var conds []metav1.Condition
			objects := append([]client.Object{owner}, tc.objects...)
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).Build()

			res, _, digest, err := ReconcileTransportURLSecret(context.Background(),
				msgParams(c, s, owner, brownfieldSpec(""), &conds))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(digest).To(BeEmpty())
			g.Expect(res.RequeueAfter).To(Equal(msgRequeue))

			cond := findMsgCond(conds, msgCondition)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(ReasonWaitingForMessagingCredentials))
			for _, part := range tc.wantMessage {
				g.Expect(cond.Message).To(ContainSubstring(part))
			}
			expectNoDerivedSecret(g, c)
		})
	}
}

func TestReconcileTransportURLSecret_brownfieldRejectsForeignScheme(t *testing.T) {
	for _, tc := range []struct {
		name       string
		value      string
		wantScheme string
	}{
		{name: "amqp", value: "amqp://u:p@h:5672/", wantScheme: "amqp"},
		{name: "unparseable", value: "rabbit://u:p@h\x7f/", wantScheme: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			s := msgScheme()
			owner := msgOwner()
			var conds []metav1.Condition
			c := fake.NewClientBuilder().WithScheme(s).
				WithObjects(owner, brownfieldSecret(map[string][]byte{
					commonv1.DefaultTransportURLSecretKey: []byte(tc.value),
				})).Build()

			_, _, digest, err := ReconcileTransportURLSecret(context.Background(),
				msgParams(c, s, owner, brownfieldSpec(""), &conds))
			g.Expect(err).To(MatchError(
				`brownfield transport URL in Secret openstack/external-bus key "transport_url": ` +
					`scheme must be rabbit, got "` + tc.wantScheme + `"`))
			g.Expect(digest).To(BeEmpty())
			g.Expect(conds).To(BeEmpty())
			// The error never quotes the value itself: it carries the broker password.
			g.Expect(err.Error()).NotTo(ContainSubstring("p@h"))
			expectNoDerivedSecret(g, c)
		})
	}
}

// --- read-only resolve ---

// TestResolveTransportURL_ReadsWithoutWriting pins the projector entry point:
// both modes hand back the URL and its digest, the waiting path reports the
// missing upstream object, and no run leaves a derived Secret behind.
func TestResolveTransportURL_ReadsWithoutWriting(t *testing.T) {
	t.Run("managed", func(t *testing.T) {
		g := NewWithT(t)
		s := msgScheme()
		owner := msgOwner()
		var conds []metav1.Condition
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(owner, rabbitmqCluster(msgUserSecret, msgNamespace), defaultUserSecret(fullDefaultUserData())).
			Build()

		wantURL, wantDigest := BuildTransportURL(
			"default_user_abc", "s3cr3t", "openstack-rabbitmq.openstack.svc", 5672)
		transportURL, digest, waitMsg, err := ResolveTransportURL(context.Background(),
			msgParams(c, s, owner, managedSpec(), &conds))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(waitMsg).To(BeEmpty())
		g.Expect(transportURL).To(Equal(wantURL))
		g.Expect(digest).To(Equal(wantDigest))
		g.Expect(conds).To(BeEmpty(), "the resolve half reports on no condition")
		expectNoDerivedSecret(g, c)
	})

	t.Run("brownfield", func(t *testing.T) {
		const external = "rabbit://svc:pw@bus.example.com:5671/neutron?ssl=1"

		g := NewWithT(t)
		s := msgScheme()
		owner := msgOwner()
		var conds []metav1.Condition
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(owner, brownfieldSecret(map[string][]byte{
				commonv1.DefaultTransportURLSecretKey: []byte(external),
			})).Build()

		transportURL, digest, waitMsg, err := ResolveTransportURL(context.Background(),
			msgParams(c, s, owner, brownfieldSpec(""), &conds))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(waitMsg).To(BeEmpty())
		g.Expect(transportURL).To(Equal(external))
		g.Expect(digest).NotTo(BeEmpty())
		expectNoDerivedSecret(g, c)
	})

	t.Run("nil spec", func(t *testing.T) {
		g := NewWithT(t)
		s := msgScheme()
		owner := msgOwner()
		var conds []metav1.Condition
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()

		transportURL, digest, waitMsg, err := ResolveTransportURL(context.Background(),
			msgParams(c, s, owner, nil, &conds))
		g.Expect(err).To(MatchError("messaging spec is nil"))
		g.Expect(transportURL).To(BeEmpty())
		g.Expect(digest).To(BeEmpty())
		g.Expect(waitMsg).To(BeEmpty())
		expectNoDerivedSecret(g, c)
	})

	t.Run("neither mode", func(t *testing.T) {
		g := NewWithT(t)
		s := msgScheme()
		owner := msgOwner()
		var conds []metav1.Condition
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()

		transportURL, digest, waitMsg, err := ResolveTransportURL(context.Background(),
			msgParams(c, s, owner, &commonv1.MessagingSpec{}, &conds))
		g.Expect(err).To(MatchError("messaging spec sets neither clusterRef nor secretRef"))
		g.Expect(transportURL).To(BeEmpty())
		g.Expect(digest).To(BeEmpty())
		g.Expect(waitMsg).To(BeEmpty())
		expectNoDerivedSecret(g, c)
	})

	t.Run("managed waits for the default-user Secret", func(t *testing.T) {
		g := NewWithT(t)
		s := msgScheme()
		owner := msgOwner()
		var conds []metav1.Condition
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(owner, rabbitmqCluster(msgUserSecret, msgNamespace)).Build()

		transportURL, digest, waitMsg, err := ResolveTransportURL(context.Background(),
			msgParams(c, s, owner, managedSpec(), &conds))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(waitMsg).To(Equal(
			"default-user Secret openstack/openstack-rabbitmq-default-user not found"))
		g.Expect(transportURL).To(BeEmpty())
		g.Expect(digest).To(BeEmpty())
		g.Expect(conds).To(BeEmpty())
		expectNoDerivedSecret(g, c)
	})
}
