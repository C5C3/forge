package builders

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSecretBuilder_WithName(t *testing.T) {
	g := NewGomegaWithT(t)

	secret := NewSecretBuilder().
		WithName("my-secret").
		Build()

	g.Expect(secret.Name).To(Equal("my-secret"))
}

func TestSecretBuilder_WithNamespace(t *testing.T) {
	g := NewGomegaWithT(t)

	secret := NewSecretBuilder().
		WithNamespace("my-namespace").
		Build()

	g.Expect(secret.Namespace).To(Equal("my-namespace"))
}

func TestSecretBuilder_WithData(t *testing.T) {
	g := NewGomegaWithT(t)

	data := map[string][]byte{
		"username": []byte("admin"),
		"password": []byte("secret"),
	}

	secret := NewSecretBuilder().
		WithData(data).
		Build()

	g.Expect(secret.Data).To(Equal(data))
}

func TestSecretBuilder_WithStringData(t *testing.T) {
	g := NewGomegaWithT(t)

	stringData := map[string]string{
		"config.yaml": "key: value",
	}

	secret := NewSecretBuilder().
		WithStringData(stringData).
		Build()

	g.Expect(secret.StringData).To(Equal(stringData))
}

func TestSecretBuilder_WithLabels(t *testing.T) {
	g := NewGomegaWithT(t)

	secret := NewSecretBuilder().
		WithLabels(map[string]string{
			"app": "myapp",
		}).
		WithLabels(map[string]string{
			"env": "test",
		}).
		Build()

	g.Expect(secret.Labels).To(HaveLen(2))
	g.Expect(secret.Labels).To(HaveKeyWithValue("app", "myapp"))
	g.Expect(secret.Labels).To(HaveKeyWithValue("env", "test"))
}

func TestSecretBuilder_WithOwnerRef(t *testing.T) {
	g := NewGomegaWithT(t)

	ref1 := metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Name:       "owner1",
		UID:        "uid-1",
	}
	ref2 := metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "owner2",
		UID:        "uid-2",
	}

	secret := NewSecretBuilder().
		WithOwnerRef(ref1).
		WithOwnerRef(ref2).
		Build()

	g.Expect(secret.OwnerReferences).To(HaveLen(2))
	g.Expect(secret.OwnerReferences[0].Name).To(Equal("owner1"))
	g.Expect(secret.OwnerReferences[1].Name).To(Equal("owner2"))
}

func TestSecretBuilder_FullChain(t *testing.T) {
	g := NewGomegaWithT(t)

	data := map[string][]byte{
		"token": []byte("abc123"),
	}
	stringData := map[string]string{
		"extra": "value",
	}
	labels := map[string]string{
		"app":  "myapp",
		"tier": "backend",
	}
	ownerRef := metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "Service",
		Name:       "my-svc",
		UID:        "uid-svc",
	}

	secret := NewSecretBuilder().
		WithName("full-secret").
		WithNamespace("full-ns").
		WithData(data).
		WithStringData(stringData).
		WithLabels(labels).
		WithOwnerRef(ownerRef).
		Build()

	g.Expect(secret.Name).To(Equal("full-secret"))
	g.Expect(secret.Namespace).To(Equal("full-ns"))
	g.Expect(secret.Data).To(Equal(data))
	g.Expect(secret.StringData).To(Equal(stringData))
	g.Expect(secret.Labels).To(Equal(labels))
	g.Expect(secret.OwnerReferences).To(HaveLen(1))
	g.Expect(secret.OwnerReferences[0].Name).To(Equal("my-svc"))
}

func TestSecretBuilder_Build_IndependentCopies(t *testing.T) {
	g := NewGomegaWithT(t)

	builder := NewSecretBuilder().
		WithName("copy-test").
		WithNamespace("ns").
		WithData(map[string][]byte{"key": []byte("value")})

	secret1 := builder.Build()
	secret2 := builder.Build()

	// Different pointer values
	g.Expect(secret1).NotTo(BeIdenticalTo(secret2))

	// Modify one copy and verify the other is unaffected
	secret1.Name = "modified"
	secret1.Data["key"] = []byte("changed")

	g.Expect(secret2.Name).To(Equal("copy-test"))
	g.Expect(secret2.Data["key"]).To(Equal([]byte("value")))
}

func TestSecretBuilder_PartialChain(t *testing.T) {
	g := NewGomegaWithT(t)

	secret := NewSecretBuilder().
		WithName("partial").
		WithNamespace("partial-ns").
		Build()

	g.Expect(secret.Name).To(Equal("partial"))
	g.Expect(secret.Namespace).To(Equal("partial-ns"))
	g.Expect(secret.Data).To(BeNil())
	g.Expect(secret.StringData).To(BeNil())
	g.Expect(secret.Labels).To(BeNil())
	g.Expect(secret.OwnerReferences).To(BeEmpty())
}

func TestSecretBuilder_Create(t *testing.T) {
	g := NewGomegaWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())

	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	builder := NewSecretBuilder().
		WithName("created-secret").
		WithNamespace("default").
		WithStringData(map[string]string{"key": "value"})

	secret, err := builder.Create(context.Background(), c)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(secret.Name).To(Equal("created-secret"))
	g.Expect(secret.Namespace).To(Equal("default"))

	// Verify the secret was actually created in the fake cluster.
	fetched := &corev1.Secret{}
	err = c.Get(context.Background(), client.ObjectKey{Name: "created-secret", Namespace: "default"}, fetched)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(fetched.Name).To(Equal("created-secret"))
}

func TestSecretBuilder_Create_PropagatesError(t *testing.T) {
	g := NewGomegaWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())

	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	builder := NewSecretBuilder().
		WithName("dup-secret").
		WithNamespace("default")

	// First create succeeds.
	_, err := builder.Create(context.Background(), c)
	g.Expect(err).NotTo(HaveOccurred())

	// Second create with the same name should return an error.
	_, err = builder.Create(context.Background(), c)
	g.Expect(err).To(HaveOccurred())
}
