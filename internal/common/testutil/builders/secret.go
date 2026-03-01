package builders

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SecretBuilder provides a fluent builder pattern for constructing
// corev1.Secret objects, primarily intended for use in tests.
type SecretBuilder struct {
	secret corev1.Secret
}

// NewSecretBuilder creates a new SecretBuilder with default values.
func NewSecretBuilder() *SecretBuilder {
	return &SecretBuilder{}
}

// WithName sets the metadata name of the Secret.
func (b *SecretBuilder) WithName(name string) *SecretBuilder {
	b.secret.Name = name
	return b
}

// WithNamespace sets the metadata namespace of the Secret.
func (b *SecretBuilder) WithNamespace(namespace string) *SecretBuilder {
	b.secret.Namespace = namespace
	return b
}

// WithData sets the binary data of the Secret.
func (b *SecretBuilder) WithData(data map[string][]byte) *SecretBuilder {
	b.secret.Data = data
	return b
}

// WithStringData sets the string data of the Secret.
func (b *SecretBuilder) WithStringData(data map[string]string) *SecretBuilder {
	b.secret.StringData = data
	return b
}

// WithLabels merges the provided labels into the Secret's existing labels.
// If the labels map has not been initialized, it will be created.
func (b *SecretBuilder) WithLabels(labels map[string]string) *SecretBuilder {
	if b.secret.Labels == nil {
		b.secret.Labels = make(map[string]string)
	}
	for k, v := range labels {
		b.secret.Labels[k] = v
	}
	return b
}

// WithOwnerRef appends an OwnerReference to the Secret's metadata.
func (b *SecretBuilder) WithOwnerRef(ref metav1.OwnerReference) *SecretBuilder {
	b.secret.OwnerReferences = append(b.secret.OwnerReferences, ref)
	return b
}

// Build returns a deep copy of the constructed Secret. Calling Build multiple
// times returns independent objects that can be modified without affecting
// each other or the builder's internal state.
func (b *SecretBuilder) Build() *corev1.Secret {
	return b.secret.DeepCopy()
}

// Create builds the Secret and creates it in the cluster using the provided
// controller-runtime client.
func (b *SecretBuilder) Create(ctx context.Context, c client.Client) (*corev1.Secret, error) {
	secret := b.Build()
	if err := c.Create(ctx, secret); err != nil {
		return nil, fmt.Errorf("creating Secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}
	return secret, nil
}
