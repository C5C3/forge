// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// validControlPlane returns a ControlPlane with all required fields set to
// valid values. Tests modify this baseline to exercise specific rules.
func validControlPlane() *ControlPlane {
	return &ControlPlane{
		Spec: ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Region:           "RegionOne",
			Infrastructure: &InfrastructureSpec{
				Database: commonv1.DatabaseSpec{
					Host:      "db.example.com",
					Port:      3306,
					Database:  "openstack",
					SecretRef: commonv1.SecretRefSpec{Name: "db-creds"},
				},
				Cache: commonv1.CacheSpec{
					Backend: "dogpile.cache.pymemcache",
					Servers: []string{"mc:11211"},
				},
			},
			Services: ServicesSpec{
				Keystone: &ServiceKeystoneSpec{},
			},
			KORC: KORCSpec{
				AdminCredential: AdminCredentialSpec{
					CloudCredentialsRef: CloudCredentialsRef{CloudName: "admin"},
					PasswordSecretRef:   commonv1.SecretRefSpec{Name: "admin-pw"},
				},
			},
		},
	}
}

// --- Defaulting webhook tests ---

func TestDefault_SetsZeroValueDefaults(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := &ControlPlane{}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	g.Expect(cp.Spec.Region).To(Equal(DefaultRegion))
	g.Expect(cp.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName).To(Equal(DefaultCloudCredentialsSecretName))
	g.Expect(cp.Spec.KORC.AdminCredential.ApplicationCredential.Restricted).NotTo(BeNil())
	g.Expect(*cp.Spec.KORC.AdminCredential.ApplicationCredential.Restricted).To(BeTrue())
	g.Expect(cp.Spec.KORC.AdminCredential.ApplicationCredential.Rotation.Mode).To(Equal(RotationModePasswordDriven))

	// the eight well-known database/cache/admin-credential defaults on a
	// bare &ControlPlane{}.
	infra := cp.Spec.Infrastructure
	g.Expect(infra.Database.Database).To(Equal(DefaultDatabaseName))
	g.Expect(infra.Database.SecretRef.Name).To(Equal(DefaultDatabaseSecretName))
	g.Expect(infra.Database.ClusterRef).NotTo(BeNil())
	g.Expect(infra.Database.ClusterRef.Name).To(Equal(DefaultDatabaseClusterRefName))
	// database.secretRef.key is intentionally NOT defaulted.
	g.Expect(infra.Database.SecretRef.Key).To(BeEmpty())
	g.Expect(infra.Cache.Backend).To(Equal(DefaultCacheBackend))
	g.Expect(infra.Cache.ClusterRef).NotTo(BeNil())
	g.Expect(infra.Cache.ClusterRef.Name).To(Equal(DefaultCacheClusterRefName))
	cred := cp.Spec.KORC.AdminCredential
	g.Expect(cred.PasswordSecretRef.Name).To(Equal(DefaultAdminPasswordSecretName))
	g.Expect(cred.PasswordSecretRef.Key).To(Equal(DefaultAdminPasswordSecretKey))
	g.Expect(cred.CloudCredentialsRef.CloudName).To(Equal(DefaultCloudName))
	// admin identity (P1) defaults.
	g.Expect(cred.UserName).To(Equal(DefaultAdminUserName))
	g.Expect(cred.ProjectName).To(Equal(DefaultAdminProjectName))
	g.Expect(cred.DomainName).To(Equal(DefaultAdminDomainName))
}

// TestDefault_IsIdempotent verifies applying Default twice produces the same
// result.
func TestDefault_IsIdempotent(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := &ControlPlane{}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())
	first := cp.DeepCopy()
	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	g.Expect(cp.Spec.Region).To(Equal(first.Spec.Region))
	g.Expect(cp.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName).
		To(Equal(first.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName))
	g.Expect(*cp.Spec.KORC.AdminCredential.ApplicationCredential.Restricted).
		To(Equal(*first.Spec.KORC.AdminCredential.ApplicationCredential.Restricted))
	g.Expect(cp.Spec.KORC.AdminCredential.ApplicationCredential.Rotation.Mode).
		To(Equal(first.Spec.KORC.AdminCredential.ApplicationCredential.Rotation.Mode))

	// the eight new defaults are identical on a second pass.
	g.Expect(cp.Spec.Infrastructure.Database.Database).
		To(Equal(first.Spec.Infrastructure.Database.Database))
	g.Expect(cp.Spec.Infrastructure.Database.SecretRef.Name).
		To(Equal(first.Spec.Infrastructure.Database.SecretRef.Name))
	g.Expect(cp.Spec.Infrastructure.Database.ClusterRef).NotTo(BeNil())
	g.Expect(cp.Spec.Infrastructure.Database.ClusterRef.Name).
		To(Equal(first.Spec.Infrastructure.Database.ClusterRef.Name))
	g.Expect(cp.Spec.Infrastructure.Cache.Backend).
		To(Equal(first.Spec.Infrastructure.Cache.Backend))
	g.Expect(cp.Spec.Infrastructure.Cache.ClusterRef).NotTo(BeNil())
	g.Expect(cp.Spec.Infrastructure.Cache.ClusterRef.Name).
		To(Equal(first.Spec.Infrastructure.Cache.ClusterRef.Name))
	g.Expect(cp.Spec.KORC.AdminCredential.PasswordSecretRef.Name).
		To(Equal(first.Spec.KORC.AdminCredential.PasswordSecretRef.Name))
	g.Expect(cp.Spec.KORC.AdminCredential.PasswordSecretRef.Key).
		To(Equal(first.Spec.KORC.AdminCredential.PasswordSecretRef.Key))
	g.Expect(cp.Spec.KORC.AdminCredential.CloudCredentialsRef.CloudName).
		To(Equal(first.Spec.KORC.AdminCredential.CloudCredentialsRef.CloudName))
}

// TestDefault_PreservesExplicitValues verifies the defaulting webhook never
// overwrites operator-supplied values, including an explicit restricted:false
// The *bool lets us distinguish unset from explicit false.
func TestDefault_PreservesExplicitValues(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	restricted := false
	cp := &ControlPlane{
		Spec: ControlPlaneSpec{
			Region: "EU-West",
			Infrastructure: &InfrastructureSpec{
				Database: commonv1.DatabaseSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "my-db"},
					Database:   "mydb",
					SecretRef:  commonv1.SecretRefSpec{Name: "mydb-creds"},
				},
				Cache: commonv1.CacheSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "my-cache"},
					Backend:    "dogpile.cache.memcached",
				},
			},
			KORC: KORCSpec{
				AdminCredential: AdminCredentialSpec{
					CloudCredentialsRef: CloudCredentialsRef{
						CloudName:  "operator",
						SecretName: "custom-clouds-yaml",
					},
					PasswordSecretRef: commonv1.SecretRefSpec{Name: "my-admin", Key: "adminpw"},
					UserName:          "brownfield-admin",
					ProjectName:       "platform-admin",
					DomainName:        "heimdall",
					ApplicationCredential: ApplicationCredentialSpec{
						Restricted: &restricted,
						Rotation:   RotationSpec{Mode: RotationModeManual},
					},
				},
			},
		},
	}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	g.Expect(cp.Spec.Region).To(Equal("EU-West"))
	g.Expect(cp.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName).To(Equal("custom-clouds-yaml"))
	g.Expect(cp.Spec.KORC.AdminCredential.ApplicationCredential.Restricted).NotTo(BeNil())
	g.Expect(*cp.Spec.KORC.AdminCredential.ApplicationCredential.Restricted).To(BeFalse())
	g.Expect(cp.Spec.KORC.AdminCredential.ApplicationCredential.Rotation.Mode).To(Equal(RotationModeManual))

	// every explicitly-supplied well-known field is preserved.
	g.Expect(cp.Spec.Infrastructure.Database.ClusterRef).NotTo(BeNil())
	g.Expect(cp.Spec.Infrastructure.Database.ClusterRef.Name).To(Equal("my-db"))
	g.Expect(cp.Spec.Infrastructure.Database.Database).To(Equal("mydb"))
	g.Expect(cp.Spec.Infrastructure.Database.SecretRef.Name).To(Equal("mydb-creds"))
	g.Expect(cp.Spec.Infrastructure.Cache.ClusterRef).NotTo(BeNil())
	g.Expect(cp.Spec.Infrastructure.Cache.ClusterRef.Name).To(Equal("my-cache"))
	g.Expect(cp.Spec.Infrastructure.Cache.Backend).To(Equal("dogpile.cache.memcached"))
	g.Expect(cp.Spec.KORC.AdminCredential.PasswordSecretRef.Name).To(Equal("my-admin"))
	g.Expect(cp.Spec.KORC.AdminCredential.PasswordSecretRef.Key).To(Equal("adminpw"))
	g.Expect(cp.Spec.KORC.AdminCredential.CloudCredentialsRef.CloudName).To(Equal("operator"))
	// explicit non-default admin identity (P1) is preserved, not overwritten.
	g.Expect(cp.Spec.KORC.AdminCredential.UserName).To(Equal("brownfield-admin"))
	g.Expect(cp.Spec.KORC.AdminCredential.ProjectName).To(Equal("platform-admin"))
	g.Expect(cp.Spec.KORC.AdminCredential.DomainName).To(Equal("heimdall"))
}

// TestDefault_DoesNotInventModeForBrownfield verifies the defaulting webhook
// never coerces an explicit brownfield database/cache into managed mode: when a
// brownfield discriminator (database.host / cache.servers) is set, the matching
// clusterRef is left nil so the validating webhook's XOR check still passes,
// while the mode-neutral leaves are still defaulted.
func TestDefault_DoesNotInventModeForBrownfield(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	// Case A: brownfield database (host set) — database.clusterRef stays nil.
	cpDB := &ControlPlane{
		Spec: ControlPlaneSpec{
			Infrastructure: &InfrastructureSpec{
				Database: commonv1.DatabaseSpec{Host: "db.example.com"},
			},
		},
	}
	g.Expect(w.Default(context.Background(), cpDB)).To(Succeed())
	g.Expect(cpDB.Spec.Infrastructure.Database.ClusterRef).To(BeNil(),
		"brownfield host must not get an invented managed clusterRef")
	g.Expect(cpDB.Spec.Infrastructure.Database.Host).To(Equal("db.example.com"))
	// Mode-neutral leaves are still defaulted in brownfield mode.
	g.Expect(cpDB.Spec.Infrastructure.Database.Database).To(Equal(DefaultDatabaseName))
	g.Expect(cpDB.Spec.Infrastructure.Database.SecretRef.Name).To(Equal(DefaultDatabaseSecretName))
	g.Expect(cpDB.Spec.Infrastructure.Cache.Backend).To(Equal(DefaultCacheBackend))

	// Case B: brownfield cache (servers set) — cache.clusterRef stays nil.
	cpCache := &ControlPlane{
		Spec: ControlPlaneSpec{
			Infrastructure: &InfrastructureSpec{
				Cache: commonv1.CacheSpec{Servers: []string{"mc:11211"}},
			},
		},
	}
	g.Expect(w.Default(context.Background(), cpCache)).To(Succeed())
	g.Expect(cpCache.Spec.Infrastructure.Cache.ClusterRef).To(BeNil(),
		"brownfield servers must not get an invented managed clusterRef")
	g.Expect(cpCache.Spec.Infrastructure.Cache.Servers).To(ConsistOf("mc:11211"))
	// Mode-neutral leaves are still defaulted in brownfield mode.
	g.Expect(cpCache.Spec.Infrastructure.Database.Database).To(Equal(DefaultDatabaseName))
	g.Expect(cpCache.Spec.Infrastructure.Database.SecretRef.Name).To(Equal(DefaultDatabaseSecretName))
	g.Expect(cpCache.Spec.Infrastructure.Cache.Backend).To(Equal(DefaultCacheBackend))
}

// TestDefault_FillsEmptyNameOnPresentClusterRef covers the defaulting webhook's
// middle branch for both database and cache: a managed-mode clusterRef object
// that is present but carries an empty Name (the CRD schema permits a bare `{}`
// clusterRef). The webhook must fill the well-known managed name in place —
// preserving the existing clusterRef pointer — so the validating webhook's
// database/cache XOR check still passes after defaulting. Without this case the `else if clusterRef.Name == ""` arm of Default
// is unexercised.
func TestDefault_FillsEmptyNameOnPresentClusterRef(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	// clusterRef present but Name empty, with host/servers unset => managed mode.
	cp := &ControlPlane{
		Spec: ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Infrastructure: &InfrastructureSpec{
				Database: commonv1.DatabaseSpec{ClusterRef: &corev1.LocalObjectReference{}},
				Cache:    commonv1.CacheSpec{ClusterRef: &corev1.LocalObjectReference{}},
			},
		},
	}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	// The empty Name is filled in place; the original clusterRef pointer is kept.
	g.Expect(cp.Spec.Infrastructure.Database.ClusterRef).NotTo(BeNil())
	g.Expect(cp.Spec.Infrastructure.Database.ClusterRef.Name).To(Equal(DefaultDatabaseClusterRefName),
		"present-but-empty database clusterRef.name must be filled with the managed default")
	g.Expect(cp.Spec.Infrastructure.Database.Host).To(BeEmpty(),
		"filling the managed clusterRef name must not invent a brownfield host")
	g.Expect(cp.Spec.Infrastructure.Cache.ClusterRef).NotTo(BeNil())
	g.Expect(cp.Spec.Infrastructure.Cache.ClusterRef.Name).To(Equal(DefaultCacheClusterRefName),
		"present-but-empty cache clusterRef.name must be filled with the managed default")
	g.Expect(cp.Spec.Infrastructure.Cache.Servers).To(BeEmpty(),
		"filling the managed clusterRef name must not invent brownfield servers")

	// The defaulted spec must satisfy the database/cache XOR (exactly one side set).
	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred(),
		"a filled managed clusterRef must satisfy the database/cache XOR after defaulting")
}

// externalControlPlane returns a minimal, valid External-mode ControlPlane: the
// sketch CR from the issue (mode + external.authURL + the required
// korc.adminCredential.passwordSecretRef), with no infrastructure block. Tests
// modify this baseline to exercise the External-mode defaulting and validation.
func externalControlPlane() *ControlPlane {
	return &ControlPlane{
		Spec: ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Region:           "RegionOne",
			Services: ServicesSpec{
				Keystone: &ServiceKeystoneSpec{
					Mode: KeystoneModeExternal,
					External: &ExternalKeystoneSpec{
						AuthURL: "https://keystone.example.com/v3",
					},
				},
			},
			KORC: KORCSpec{
				AdminCredential: AdminCredentialSpec{
					CloudCredentialsRef: CloudCredentialsRef{CloudName: "admin"},
					PasswordSecretRef:   commonv1.SecretRefSpec{Name: "admin-pw"},
				},
			},
		},
	}
}

// TestDefault_ExternalModeDoesNotInventInfrastructure verifies the defaulting
// webhook never invents a managed database/cache block in External mode
// (spec.infrastructure stays nil) while it still materializes the external
// block's own defaults (endpointType -> public, caBundleSecretRef.key -> ca.crt).
func TestDefault_ExternalModeDoesNotInventInfrastructure(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := externalControlPlane()
	cp.Spec.Services.Keystone.External.CABundleSecretRef = &commonv1.SecretRefSpec{Name: "brownfield-keystone-ca"}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	g.Expect(cp.Spec.Infrastructure).To(BeNil(),
		"External mode must not invent a managed infrastructure block")
	ext := cp.Spec.Services.Keystone.External
	g.Expect(ext.EndpointType).To(Equal(DefaultExternalEndpointType),
		"external.endpointType must default to public")
	g.Expect(ext.CABundleSecretRef).NotTo(BeNil())
	g.Expect(ext.CABundleSecretRef.Key).To(Equal(DefaultCABundleSecretKey),
		"external.caBundleSecretRef.key must default to ca.crt")
	// The admin identity defaults still apply in External mode.
	g.Expect(cp.Spec.KORC.AdminCredential.UserName).To(Equal(DefaultAdminUserName))
	g.Expect(cp.Spec.KORC.AdminCredential.ProjectName).To(Equal(DefaultAdminProjectName))
	g.Expect(cp.Spec.KORC.AdminCredential.DomainName).To(Equal(DefaultAdminDomainName))
}

// TestDefault_ExternalModePreservesExplicitEndpointType verifies an explicit
// endpointType / caBundle key is preserved rather than overwritten in External
// mode (the error-path counterpart to the zero-value defaulting above).
func TestDefault_ExternalModePreservesExplicitEndpointType(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := externalControlPlane()
	cp.Spec.Services.Keystone.External.EndpointType = ExternalEndpointTypeInternal
	cp.Spec.Services.Keystone.External.CABundleSecretRef = &commonv1.SecretRefSpec{
		Name: "brownfield-keystone-ca", Key: "tls-ca.pem",
	}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	ext := cp.Spec.Services.Keystone.External
	g.Expect(ext.EndpointType).To(Equal(ExternalEndpointTypeInternal))
	g.Expect(ext.CABundleSecretRef.Key).To(Equal("tls-ca.pem"))
}

// TestDefault_ManagedModeAllocatesInfrastructureWhenNil locks today's
// omit-infrastructure contract through the pointer flip: an explicit Managed-mode
// (or unset-keystone) CR that omits spec.infrastructure still gets the block
// materialized and the managed clusterRefs invented, exactly as before.
func TestDefault_ManagedModeAllocatesInfrastructureWhenNil(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	for _, tc := range []struct {
		name string
		ks   *ServiceKeystoneSpec
	}{
		{"explicit managed mode", &ServiceKeystoneSpec{Mode: KeystoneModeManaged}},
		{"unset mode (defaults managed)", &ServiceKeystoneSpec{}},
		{"unset keystone service", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{Keystone: tc.ks}}}
			g.Expect(w.Default(context.Background(), cp)).To(Succeed())

			g.Expect(cp.Spec.Infrastructure).NotTo(BeNil(),
				"a non-External CR must get its infrastructure block materialized")
			g.Expect(cp.Spec.Infrastructure.Database.ClusterRef).NotTo(BeNil())
			g.Expect(cp.Spec.Infrastructure.Database.ClusterRef.Name).To(Equal(DefaultDatabaseClusterRefName))
			g.Expect(cp.Spec.Infrastructure.Cache.ClusterRef).NotTo(BeNil())
			g.Expect(cp.Spec.Infrastructure.Cache.ClusterRef.Name).To(Equal(DefaultCacheClusterRefName))
		})
	}
}

// TestDefault_ExternalModeIsIdempotent verifies applying Default twice to an
// External-mode CR produces the same result — in particular that the second pass
// does not invent an infrastructure block.
func TestDefault_ExternalModeIsIdempotent(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := externalControlPlane()

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())
	first := cp.DeepCopy()
	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	g.Expect(cp.Spec.Infrastructure).To(BeNil())
	g.Expect(cp.Spec.Services.Keystone.External.EndpointType).
		To(Equal(first.Spec.Services.Keystone.External.EndpointType))
	g.Expect(cp.Spec.Services.Keystone.Mode).To(Equal(first.Spec.Services.Keystone.Mode))
}

// --- Validation webhook tests ---

func TestValidateCreate_AcceptsValidControlPlane(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	_, err := w.ValidateCreate(context.Background(), validControlPlane())
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidateCreate_AcceptsUnsetKeystoneService(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	// Staged adoption / externally-managed Keystone: services.keystone unset.
	cp.Spec.Services.Keystone = nil

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred(),
		"a ControlPlane with services.keystone unset must be admitted")
}

func TestValidateCreate_RejectsBadOpenStackRelease(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.OpenStackRelease = "2025"

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("openStackRelease"))
}

func TestValidateCreate_AcceptsNamespacedSecretStoreRef(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store",
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred(),
		"a ControlPlane selecting a namespaced SecretStore must be admitted")
}

func TestValidateCreate_RejectsSecretStoreRefEmptyName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{Kind: commonv1.SecretStoreKindNamespaced}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("secretStoreRef"))
	g.Expect(err.Error()).To(ContainSubstring("name"))
}

func TestValidateCreate_RejectsSecretStoreRefUnknownKind(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreRefKind("Bogus"), Name: "some-store",
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("secretStoreRef"))
	g.Expect(err.Error()).To(ContainSubstring("kind"))
}

// TestValidateUpdate_AllowsSecretStoreRefSwitch verifies the store reference is
// mutable — switching stores is a supported operation (the operator moves the
// key material in place), so it must NOT be treated as an immutable field.
func TestValidateUpdate_AllowsSecretStoreRefSwitch(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := validControlPlane()
	newCP := validControlPlane()
	newCP.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store",
	}

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred(),
		"switching spec.secretStoreRef must be allowed on update")
}

func TestValidateCreate_RejectsKeystoneImageTagAndDigestBothSet(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	// Override the Keystone image with BOTH a tag and a digest — XOR violation.
	cp.Spec.Services.Keystone.Image = &commonv1.ImageSpec{
		Repository: "ghcr.io/c5c3/keystone",
		Tag:        "2025.2",
		Digest:     "sha256:1111111111111111111111111111111111111111111111111111111111111111",
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("exactly one of image.tag or image.digest"))
}

func TestValidateCreate_RejectsDatabaseBothSet(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	// Both clusterRef AND host set — XOR violation.
	cp.Spec.Infrastructure.Database.ClusterRef = &corev1.LocalObjectReference{Name: "mariadb"}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("database"))
}

func TestValidateCreate_RejectsDatabaseNeitherSet(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	// Neither clusterRef NOR host set — XOR violation.
	cp.Spec.Infrastructure.Database.Host = ""

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("database"))
}

// TestValidateCreate_RejectsDynamicCredentialsWithoutClusterRef verifies the
// defense-in-depth mirror of the shared DatabaseSpec CEL rule: engine-issued
// credentials (Dynamic) require managed mode (clusterRef set).
func TestValidateCreate_RejectsDynamicCredentialsWithoutClusterRef(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane() // brownfield (Host set, ClusterRef nil)
	cp.Spec.Infrastructure.Database.CredentialsMode = commonv1.CredentialsModeDynamic

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("credentialsMode"))
	g.Expect(err.Error()).To(ContainSubstring("requires clusterRef"))
}

// TestValidateCreate_AcceptsDynamicCredentialsWithClusterRef verifies Dynamic is
// accepted in managed mode.
func TestValidateCreate_AcceptsDynamicCredentialsWithClusterRef(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := managedControlPlane()
	cp.Spec.Infrastructure.Database.CredentialsMode = commonv1.CredentialsModeDynamic

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_RejectsDatabaseReplicasTwo verifies that a managed-mode
// ControlPlane requesting database.replicas: 2 is rejected. The managed MariaDB
// projection turns any replicas>1 into a Galera cluster, and a two-node Galera
// cluster cannot hold a quorum majority, so a single pod disruption takes the
// whole database offline. The CRD marker only enforces Minimum=1, making this
// webhook the enforcement point.
func TestValidateCreate_RejectsDatabaseReplicasTwo(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := managedControlPlane()
	cp.Spec.Infrastructure.Database.Replicas = 2

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("replicas"))
	g.Expect(err.Error()).To(ContainSubstring("quorum"))
}

// TestValidateCreate_AcceptsQuorumSafeDatabaseReplicas verifies that the
// quorum-safe replica counts — 1 (standalone) and 3 (Galera with a majority) —
// pass validation, so the replicas>1==2 guard does not over-restrict legitimate
// topologies.
func TestValidateCreate_AcceptsQuorumSafeDatabaseReplicas(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	for _, replicas := range []int32{1, 3, 5} {
		cp := managedControlPlane()
		cp.Spec.Infrastructure.Database.Replicas = replicas
		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred(), "replicas=%d should be accepted", replicas)
	}
}

func TestValidateCreate_RejectsCacheBothSet(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Infrastructure.Cache.ClusterRef = &corev1.LocalObjectReference{Name: "memcached"}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("cache"))
}

func TestValidateCreate_RejectsCacheNeitherSet(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Infrastructure.Cache.Servers = nil

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("cache"))
}

func TestValidateCreate_RejectsMissingPasswordSecretRef(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.KORC.AdminCredential.PasswordSecretRef.Name = ""

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("passwordSecretRef"))
}

// --- Policy rule name/value tests (#479) ---
//
// The c5c3 webhook previously validated policy rules not at all, so an invalid
// rule on spec.global or spec.services.keystone.policyOverrides wedged the
// control plane indirectly via the keystone webhook. The validate() method now
// delegates to the shared policy.ValidatePolicyRules on both fields.

func TestValidateCreate_RejectsEmptyGlobalPolicyRuleName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.GlobalPolicyOverrides = &commonv1.PolicySpec{Rules: map[string]string{"": "role:admin"}}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("global"))
	g.Expect(err.Error()).To(ContainSubstring("policy rule name must not be empty"))
}

func TestValidateCreate_RejectsEmptyGlobalPolicyRuleValue(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.GlobalPolicyOverrides = &commonv1.PolicySpec{Rules: map[string]string{"identity:get_user": ""}}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("global"))
	g.Expect(err.Error()).To(ContainSubstring("policy rule value must not be empty"))
}

func TestValidateCreate_RejectsEmptyServicePolicyRuleValue(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Keystone.PolicyOverrides = &commonv1.PolicySpec{
		Rules: map[string]string{"identity:get_user": ""},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("policyOverrides"))
	g.Expect(err.Error()).To(ContainSubstring("policy rule value must not be empty"))
}

func TestValidateCreate_AcceptsValidPolicyRules(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.GlobalPolicyOverrides = &commonv1.PolicySpec{Rules: map[string]string{"identity:get_user": "role:admin"}}
	cp.Spec.Services.Keystone.PolicyOverrides = &commonv1.PolicySpec{
		Rules: map[string]string{"identity:list_user": "role:reader"},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_AccumulatesAllErrors puts EVERY validation rule into a
// broken state simultaneously and asserts the returned error names every field,
// pinning the webhook's no-short-circuit (accumulate-all) contract. If a future change short-circuits on the first error, this test
// fails because the later field substrings go missing.
func TestValidateCreate_AccumulatesAllErrors(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()

	// Break every rule at once.
	cp.Spec.OpenStackRelease = "2025" // bad release pattern
	// Database: host is already set in the baseline; adding clusterRef makes BOTH
	// set => XOR violation.
	cp.Spec.Infrastructure.Database.ClusterRef = &corev1.LocalObjectReference{Name: "mariadb"}
	// Cache: servers already set in the baseline; adding clusterRef => XOR violation.
	cp.Spec.Infrastructure.Cache.ClusterRef = &corev1.LocalObjectReference{Name: "memcached"}
	// Required passwordSecretRef.name missing.
	cp.Spec.KORC.AdminCredential.PasswordSecretRef.Name = ""
	// Unsupported rotation interval (not a whole number of days).
	cp.Spec.Services.Keystone.RotationInterval = &metav1.Duration{Duration: 5 * time.Hour}
	// Policy rules: an empty name on the global policy and an empty value on the
	// per-service override (the empty-value path is the issue #479 addition). Both
	// must participate in the aggregated error.
	cp.Spec.GlobalPolicyOverrides = &commonv1.PolicySpec{Rules: map[string]string{"": "role:admin"}}
	cp.Spec.Services.Keystone.PolicyOverrides = &commonv1.PolicySpec{
		Rules: map[string]string{"identity:get_user": ""},
	}
	// Dedicated backing services: a Dynamic credentialsMode the dedicated database
	// cannot support, and a Horizon dedicated cache colliding with the Keystone one.
	cp.Spec.Services.Keystone.DedicatedBackingServices = &KeystoneDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{
			ClusterRef:      &corev1.LocalObjectReference{Name: "cp-keystone-db"},
			CredentialsMode: commonv1.CredentialsModeDynamic,
			Database:        "keystone",
			SecretRef:       commonv1.SecretRefSpec{Name: "keystone-db"},
		},
		Cache: &commonv1.CacheSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-cache"},
			Backend:    commonv1.DefaultCacheBackend,
		},
	}
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{
		DedicatedBackingServices: &HorizonDedicatedBackingServicesSpec{
			Cache: &commonv1.CacheSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-cache"},
				Backend:    commonv1.DefaultCacheBackend,
			},
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())

	msg := err.Error()
	g.Expect(msg).To(ContainSubstring("openStackRelease"), "release pattern error must be present")
	g.Expect(msg).To(ContainSubstring("database"), "database XOR error must be present")
	g.Expect(msg).To(ContainSubstring("cache"), "cache XOR error must be present")
	g.Expect(msg).To(ContainSubstring("passwordSecretRef"), "required passwordSecretRef error must be present")
	g.Expect(msg).To(ContainSubstring("rotationInterval"), "rotation interval error must be present")
	g.Expect(msg).To(ContainSubstring("global"), "global policy rule-name error must be present")
	g.Expect(msg).To(ContainSubstring("policyOverrides"), "per-service policy rule-value error must be present")
	g.Expect(msg).To(ContainSubstring("policy rule name must not be empty"))
	g.Expect(msg).To(ContainSubstring("policy rule value must not be empty"))
	g.Expect(msg).To(ContainSubstring("credentialsMode Dynamic is not supported on a dedicated database"),
		"dedicated Dynamic-credentials error must be present")
	g.Expect(msg).To(ContainSubstring("horizon.dedicatedBackingServices.cache.clusterRef.name"),
		"dedicated cache collision error must be present")
}

// TestValidateCreate_RejectsBadRotationInterval verifies a rotationInterval the
// reconciler's intervalToCron cannot represent is rejected at admission rather
// than surfacing as a steady-state KeystoneReady=False.
func TestValidateCreate_RejectsBadRotationInterval(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	for _, bad := range []time.Duration{5 * time.Hour, 25 * time.Hour, -24 * time.Hour, 0} {
		cp := validControlPlane()
		cp.Spec.Services.Keystone.RotationInterval = &metav1.Duration{Duration: bad}

		_, err := w.ValidateCreate(context.Background(), cp)
		// A zero Duration is the same as "unset" (nil pointer is the unset case; a
		// &Duration{0} is an explicit zero), which the rule treats as invalid.
		g.Expect(err).To(HaveOccurred(), "interval %v must be rejected", bad)
		g.Expect(err.Error()).To(ContainSubstring("rotationInterval"))
	}
}

// TestValidateCreate_AcceptsDailyAndWeeklyRotationIntervals verifies the
// rotationInterval values intervalToCron supports (any positive whole number of
// days, including the canonical 24h daily and 168h weekly) pass admission
func TestValidateCreate_AcceptsDailyAndWeeklyRotationIntervals(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	for _, ok := range []time.Duration{24 * time.Hour, 48 * time.Hour, 168 * time.Hour, 336 * time.Hour} {
		cp := validControlPlane()
		cp.Spec.Services.Keystone.RotationInterval = &metav1.Duration{Duration: ok}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred(), "interval %v must be accepted", ok)
	}
}

// TestValidateCreate_RejectsGatewayWithoutHostname verifies that configuring a
// gateway without a hostname is rejected at admission, so the reconciler never
// derives an empty "https:///v3" public endpoint (#476).
func TestValidateCreate_RejectsGatewayWithoutHostname(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Keystone.Gateway = &commonv1.GatewaySpec{
		ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		// Hostname intentionally empty.
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("hostname"))
}

// TestValidateCreate_AcceptsGatewayWithHostname verifies a gateway carrying a
// non-empty hostname passes admission (#476).
func TestValidateCreate_AcceptsGatewayWithHostname(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Keystone.Gateway = &commonv1.GatewaySpec{
		ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		Hostname:  "keystone.127-0-0-1.nip.io",
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_AcceptsNilGateway verifies the gateway hostname check does
// not fire when no gateway is configured (the field is optional) (#476).
func TestValidateCreate_AcceptsNilGateway(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Keystone.Gateway = nil

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidateUpdate_AcceptsValidChange(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := validControlPlane()
	newCP := validControlPlane()
	newCP.Spec.OpenStackRelease = "2026.1"

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())
}

// managedControlPlane returns a valid managed-mode ControlPlane: database and
// cache point at managed clusterRefs (not brownfield host/servers). The
// immutability tests start from this baseline so a clusterRef name or a mode
// flip is the only delta under test.
func managedControlPlane() *ControlPlane {
	cp := validControlPlane()
	cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: "openstack-db"},
		Database:   "openstack",
		SecretRef:  commonv1.SecretRefSpec{Name: "db-creds"},
	}
	cp.Spec.Infrastructure.Cache = commonv1.CacheSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: "openstack-memcached"},
		Backend:    "dogpile.cache.pymemcache",
	}
	cp.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName = "k-orc-clouds-yaml"
	return cp
}

// TestValidateUpdate_RejectsDatabaseModeFlip verifies that flipping the database
// between managed (clusterRef) and brownfield (host) mode is rejected on UPDATE,
// since the previously-projected MariaDB child would otherwise be orphaned (#476).
func TestValidateUpdate_RejectsDatabaseModeFlip(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	// managed -> brownfield.
	oldCP := managedControlPlane()
	newCP := managedControlPlane()
	newCP.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		Host: "db.example.com", Database: "openstack", SecretRef: commonv1.SecretRefSpec{Name: "db-creds"},
	}
	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("database mode"))

	// brownfield -> managed (the reverse direction).
	_, err = w.ValidateUpdate(context.Background(), newCP, oldCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("database mode"))
}

// TestValidateUpdate_RejectsDatabaseClusterRefRename verifies that renaming a
// managed database clusterRef is rejected on UPDATE, since the old MariaDB child
// would otherwise be orphaned while a new one is provisioned (#476).
func TestValidateUpdate_RejectsDatabaseClusterRefRename(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := managedControlPlane()
	newCP := managedControlPlane()
	newCP.Spec.Infrastructure.Database.ClusterRef = &corev1.LocalObjectReference{Name: "openstack-db-2"}

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("clusterRef.name"))
}

// TestValidateUpdate_RejectsCacheModeFlipAndRename verifies the cache mode flip
// and managed clusterRef rename are both rejected on UPDATE (#476).
func TestValidateUpdate_RejectsCacheModeFlipAndRename(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	// managed -> brownfield (servers) cache mode flip.
	oldCP := managedControlPlane()
	flipped := managedControlPlane()
	flipped.Spec.Infrastructure.Cache = commonv1.CacheSpec{
		Servers: []string{"mc:11211"}, Backend: "dogpile.cache.pymemcache",
	}
	_, err := w.ValidateUpdate(context.Background(), oldCP, flipped)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("cache mode"))

	// managed clusterRef rename.
	renamed := managedControlPlane()
	renamed.Spec.Infrastructure.Cache.ClusterRef = &corev1.LocalObjectReference{Name: "openstack-memcached-2"}
	_, err = w.ValidateUpdate(context.Background(), oldCP, renamed)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("clusterRef.name"))
}

// TestValidateUpdate_RejectsCloudSecretNameChange verifies that renaming
// cloudCredentialsRef.secretName is rejected on UPDATE, since the old K-ORC
// clouds.yaml ExternalSecret would otherwise be leaked (#476).
func TestValidateUpdate_RejectsCloudSecretNameChange(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := managedControlPlane()
	newCP := managedControlPlane()
	newCP.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName = "renamed-clouds-yaml"

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("secretName"))
}

// TestValidateUpdate_AllowsMutableFieldChanges verifies that updates which only
// touch mutable fields (replicas, an openStackRelease upgrade) are accepted on
// an otherwise-unchanged managed ControlPlane, so the immutability guard does
// not over-restrict legitimate edits (#476, #466). Region is now immutable
// (#466), so it is deliberately left unchanged here.
func TestValidateUpdate_AllowsMutableFieldChanges(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := managedControlPlane()

	newCP := managedControlPlane()
	newCP.Spec.OpenStackRelease = "2026.1"
	replicas := int32(3)
	newCP.Spec.Services.Keystone.Replicas = &replicas

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateUpdate_RejectsDatabaseNameChange verifies that renaming the shared
// database is rejected on UPDATE: the name is projected verbatim into the
// Keystone child's now-immutable spec.database.database, so a rename here would
// wedge the reconcile loop (#466). Only the database name changes, so the mode
// and clusterRef.name immutability checks stay satisfied.
func TestValidateUpdate_RejectsDatabaseNameChange(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := managedControlPlane()
	newCP := managedControlPlane()
	newCP.Spec.Infrastructure.Database.Database = "renamed"

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("database name is immutable"))
}

// TestValidateUpdate_RejectsDatabaseReplicasChange verifies that changing
// database.replicas is rejected on UPDATE: the count is projected into the owned
// MariaDB child's replica count and derived Galera topology, so editing it on a
// live control plane would drive a destructive scale-down or Galera toggle
// (3->1). Both directions are exercised so neither a scale-up nor a scale-down
// slips through.
func TestValidateUpdate_RejectsDatabaseReplicasChange(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	// 3 -> 1 (scale down / Galera toggle off).
	oldCP := managedControlPlane()
	oldCP.Spec.Infrastructure.Database.Replicas = 3
	newCP := managedControlPlane()
	newCP.Spec.Infrastructure.Database.Replicas = 1

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("database replicas is immutable"))

	// 1 -> 3 (the reverse direction).
	_, err = w.ValidateUpdate(context.Background(), newCP, oldCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("database replicas is immutable"))
}

// TestValidateUpdate_RejectsDatabaseStorageSizeChange verifies that changing
// database.storageSize is rejected on UPDATE: the size is projected into the owned
// MariaDB child's spec.storage.size, which the mariadb-operator refuses to resize
// on a live CR, so freezing it at admission surfaces the constraint with a clear
// message. Both grow and shrink are exercised so neither slips through.
func TestValidateUpdate_RejectsDatabaseStorageSizeChange(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	// 512Mi -> 100Gi (grow).
	oldCP := managedControlPlane()
	oldCP.Spec.Infrastructure.Database.StorageSize = "512Mi"
	newCP := managedControlPlane()
	newCP.Spec.Infrastructure.Database.StorageSize = "100Gi"

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("database storageSize is immutable"))

	// 100Gi -> 512Mi (shrink, the reverse direction).
	_, err = w.ValidateUpdate(context.Background(), newCP, oldCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("database storageSize is immutable"))
}

// TestValidateUpdate_AcceptsUnchangedDatabaseStorageSize guards against the
// immutability check over-firing: an UPDATE that leaves storageSize untouched (here
// while editing a mutable field) must still be accepted.
func TestValidateUpdate_AcceptsUnchangedDatabaseStorageSize(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	oldCP := managedControlPlane()
	oldCP.Spec.Infrastructure.Database.StorageSize = "512Mi"
	newCP := managedControlPlane()
	newCP.Spec.Infrastructure.Database.StorageSize = "512Mi"
	replicas := int32(3)
	newCP.Spec.Services.Keystone.Replicas = &replicas

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateUpdate_AcceptsStorageSizeMigrationFromEmpty covers a ControlPlane
// created before storageSize existed: "" is persisted, yet its live MariaDB was
// provisioned at DefaultDatabaseStorageSize. A first UPDATE that pins the field
// to that default (the size it already runs at) must be admitted as a one-time
// migration rather than rejected as a resize. Both the empty->default direction
// and the (defaulting-bypassed) default->empty direction are exercised.
func TestValidateUpdate_AcceptsStorageSizeMigrationFromEmpty(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	// "" (pre-existing) -> the default it already runs at.
	oldCP := managedControlPlane()
	oldCP.Spec.Infrastructure.Database.StorageSize = ""
	newCP := managedControlPlane()
	newCP.Spec.Infrastructure.Database.StorageSize = DefaultDatabaseStorageSize

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())

	// The reverse direction (field cleared back to the default) is equally a no-op.
	_, err = w.ValidateUpdate(context.Background(), newCP, oldCP)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateUpdate_RejectsStorageSizeResizeFromEmpty guards the other half of
// the migration normalization: pinning a pre-existing ("") ControlPlane to a
// size OTHER than the default it already runs at is a real resize the
// mariadb-operator would refuse, so it must still be rejected.
func TestValidateUpdate_RejectsStorageSizeResizeFromEmpty(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	oldCP := managedControlPlane()
	oldCP.Spec.Infrastructure.Database.StorageSize = ""
	newCP := managedControlPlane()
	newCP.Spec.Infrastructure.Database.StorageSize = "512Mi"

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("database storageSize is immutable"))
}

// TestValidateUpdate_RejectsRegionChange verifies that changing the region is
// rejected on UPDATE: the region is projected verbatim into the Keystone child's
// now-immutable spec.bootstrap.region (#466).
func TestValidateUpdate_RejectsRegionChange(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := managedControlPlane()
	newCP := managedControlPlane()
	newCP.Spec.Region = "EU-West"

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("region is immutable"))
}

// TestValidateUpdate_RejectsOpenStackReleaseDowngrade verifies that lowering the
// openStackRelease is rejected on UPDATE, because Keystone DB migrations are
// forward-only (#466). Both a year downgrade and a same-year minor downgrade are
// exercised.
func TestValidateUpdate_RejectsOpenStackReleaseDowngrade(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	// Year downgrade: 2025.2 -> 2024.1.
	oldCP := managedControlPlane()
	yearDown := managedControlPlane()
	yearDown.Spec.OpenStackRelease = "2024.1"
	_, err := w.ValidateUpdate(context.Background(), oldCP, yearDown)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("downgrade"))

	// Same-year minor downgrade: 2025.2 -> 2025.1.
	minorDown := managedControlPlane()
	minorDown.Spec.OpenStackRelease = "2025.1"
	_, err = w.ValidateUpdate(context.Background(), oldCP, minorDown)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("downgrade"))
}

// TestValidateUpdate_RejectsNonCadenceReleaseMinor guards the regression where a
// regex-valid but non-cadence minor was silently admitted on UPDATE. OpenStack
// ships only YYYY.1 and YYYY.2; before the release pattern was tightened to
// ^\d{4}\.[12]$, patching a live 2025.2 to 2025.9 passed validate() (whose regex
// accepted any single-digit minor) while validateReleaseNotDowngraded returned
// nil (release.ParseRelease rejects minor 9), admitting an edit that had been
// rejected before validateReleaseNotDowngraded delegated to ParseRelease.
func TestValidateUpdate_RejectsNonCadenceReleaseMinor(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := managedControlPlane()
	nonCadence := managedControlPlane()
	nonCadence.Spec.OpenStackRelease = "2025.9"

	_, err := w.ValidateUpdate(context.Background(), oldCP, nonCadence)
	g.Expect(err).To(HaveOccurred(),
		"a non-cadence openStackRelease minor must be rejected on UPDATE")
	g.Expect(err.Error()).To(ContainSubstring("openStackRelease"))
}

// TestValidateUpdate_AcceptsOpenStackReleaseUpgrade verifies that raising the
// openStackRelease is accepted (the monotonic-upgrade happy path) (#466).
func TestValidateUpdate_AcceptsOpenStackReleaseUpgrade(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := managedControlPlane()
	newCP := managedControlPlane()
	newCP.Spec.OpenStackRelease = "2026.1"

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateUpdate_AcceptsSameOpenStackRelease verifies that re-applying the
// same openStackRelease is accepted, so the downgrade guard does not fire on a
// no-op update (#466).
func TestValidateUpdate_AcceptsSameOpenStackRelease(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := managedControlPlane()
	newCP := managedControlPlane()

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidateDelete_AlwaysAllowed(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	_, err := w.ValidateDelete(context.Background(), &ControlPlane{})
	g.Expect(err).NotTo(HaveOccurred())
}

// --- One-ControlPlane-per-namespace tests ---

// webhookScheme builds a runtime.Scheme with the c5c3 API types registered, for
// the fake client backing the one-ControlPlane-per-namespace tests.
func webhookScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	g := NewGomegaWithT(t)
	s := runtime.NewScheme()
	g.Expect(AddToScheme(s)).To(Succeed())
	return s
}

// TestValidateCreate_RejectsSecondControlPlaneInNamespace verifies the
// one-ControlPlane-per-namespace contract: a CREATE is Forbidden when another
// ControlPlane already exists in the same namespace.
func TestValidateCreate_RejectsSecondControlPlaneInNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	existing := validControlPlane()
	existing.Name = "incumbent"
	existing.Namespace = "tenant-a"
	c := fake.NewClientBuilder().WithScheme(webhookScheme(t)).WithObjects(existing).Build()
	w := &ControlPlaneWebhook{Client: c}

	second := validControlPlane()
	second.Name = "newcomer"
	second.Namespace = "tenant-a"

	_, err := w.ValidateCreate(context.Background(), second)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("incumbent"))
	g.Expect(err.Error()).To(ContainSubstring("tenant-a"))
}

// TestValidateCreate_AllowsFirstControlPlane_AndUpdate verifies the first CREATE
// in an empty namespace is allowed, and that UPDATE never trips the
// one-per-namespace check even though the CR is present.
func TestValidateCreate_AllowsFirstControlPlane_AndUpdate(t *testing.T) {
	g := NewGomegaWithT(t)
	c := fake.NewClientBuilder().WithScheme(webhookScheme(t)).Build()
	w := &ControlPlaneWebhook{Client: c}

	first := validControlPlane()
	first.Name = "first"
	first.Namespace = "tenant-b"
	_, err := w.ValidateCreate(context.Background(), first)
	g.Expect(err).NotTo(HaveOccurred())

	cWith := fake.NewClientBuilder().WithScheme(webhookScheme(t)).WithObjects(first).Build()
	wWith := &ControlPlaneWebhook{Client: cWith}
	updated := first.DeepCopy()
	updated.Spec.OpenStackRelease = "2026.1"
	_, err = wWith.ValidateUpdate(context.Background(), first, updated)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- services.horizon validation ---

// TestValidateCreate_AcceptsHorizonBlock verifies a minimal (empty) horizon
// block passes validation — every ServiceHorizonSpec field is optional.
func TestValidateCreate_AcceptsHorizonBlock(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_RejectsHorizonGatewayWithoutHostname mirrors the keystone
// gateway hostname rule for the horizon service block.
func TestValidateCreate_RejectsHorizonGatewayWithoutHostname(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{
		Gateway: &commonv1.GatewaySpec{
			ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.horizon.gateway.hostname"))
}

// TestValidateCreate_RejectsHorizonImageTagAndDigestBothSet mirrors the
// ImageSpec tag/digest XOR defense-in-depth check for the horizon override.
func TestValidateCreate_RejectsHorizonImageTagAndDigestBothSet(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{
		Image: &commonv1.ImageSpec{
			Repository: "ghcr.io/c5c3/horizon",
			Tag:        "2025.2",
			Digest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("exactly one of image.tag or image.digest"))
}

// TestValidateCreate_RejectsHorizonEmptySecretKeyRefName covers the error path
// where secretKeyRef is present but carries no name.
func TestValidateCreate_RejectsHorizonEmptySecretKeyRefName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{
		SecretKeyRef: &commonv1.SecretRefSpec{Name: ""},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.horizon.secretKeyRef.name"))
}

// --- External-mode validation matrix ---

// TestValidateCreate_AcceptsMinimalExternalControlPlane is the acceptance proof
// for the issue's sketch CR: mode: External + external.authURL +
// korc.adminCredential.passwordSecretRef, no infrastructure block.
func TestValidateCreate_AcceptsMinimalExternalControlPlane(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	_, err := w.ValidateCreate(context.Background(), externalControlPlane())
	g.Expect(err).NotTo(HaveOccurred(),
		"the minimal External-mode sketch CR must be admitted")
}

// TestValidateCreate_RejectsExternalModeWithoutExternalBlock verifies the
// external block is required in External mode.
func TestValidateCreate_RejectsExternalModeWithoutExternalBlock(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := externalControlPlane()
	cp.Spec.Services.Keystone.External = nil

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("external is required when services.keystone.mode is External"))
}

// TestValidateCreate_RejectsExternalBlockInManagedMode verifies the external
// block may only be set in External mode.
func TestValidateCreate_RejectsExternalBlockInManagedMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane() // keystone mode unset (=> Managed)
	cp.Spec.Services.Keystone.External = &ExternalKeystoneSpec{AuthURL: "https://keystone.example.com/v3"}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.keystone.external"))
	g.Expect(err.Error()).To(ContainSubstring("may only be set when services.keystone.mode is External"))
}

// TestValidateCreate_RejectsManagedOnlyFieldsInExternalMode verifies each
// managed-only Keystone field is forbidden in External mode, each with a message
// naming the offending field.
func TestValidateCreate_RejectsManagedOnlyFieldsInExternalMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	replicas := int32(3)

	tests := []struct {
		name       string
		mutate     func(ks *ServiceKeystoneSpec)
		wantSubstr string
	}{
		{"replicas", func(ks *ServiceKeystoneSpec) { ks.Replicas = &replicas }, "services.keystone.replicas"},
		{"image", func(ks *ServiceKeystoneSpec) {
			ks.Image = &commonv1.ImageSpec{Repository: "r", Tag: "t"}
		}, "services.keystone.image"},
		{"policyOverrides", func(ks *ServiceKeystoneSpec) {
			ks.PolicyOverrides = &commonv1.PolicySpec{Rules: map[string]string{"a": "b"}}
		}, "services.keystone.policyOverrides"},
		{"rotationInterval", func(ks *ServiceKeystoneSpec) {
			ks.RotationInterval = &metav1.Duration{Duration: 24 * time.Hour}
		}, "services.keystone.rotationInterval"},
		{"gateway", func(ks *ServiceKeystoneSpec) {
			ks.Gateway = &commonv1.GatewaySpec{Hostname: "k.example.com"}
		}, "services.keystone.gateway"},
		{"publicEndpoint", func(ks *ServiceKeystoneSpec) {
			ks.PublicEndpoint = "https://k.example.com/v3"
		}, "services.keystone.publicEndpoint"},
		{"federationProxyImage", func(ks *ServiceKeystoneSpec) {
			ks.FederationProxyImage = &commonv1.ImageSpec{Repository: "r", Tag: "t"}
		}, "services.keystone.federationProxyImage"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cp := externalControlPlane()
			tc.mutate(cp.Spec.Services.Keystone)

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.wantSubstr))
			g.Expect(err.Error()).To(ContainSubstring("External"))
		})
	}
}

// TestValidateCreate_RejectsInfrastructureInExternalMode verifies
// spec.infrastructure is forbidden in External mode.
func TestValidateCreate_RejectsInfrastructureInExternalMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := externalControlPlane()
	cp.Spec.Infrastructure = &InfrastructureSpec{
		Database: commonv1.DatabaseSpec{Host: "db", Database: "d", SecretRef: commonv1.SecretRefSpec{Name: "s"}},
		Cache:    commonv1.CacheSpec{Backend: "b", Servers: []string{"mc:11211"}},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.infrastructure"))
	g.Expect(err.Error()).To(ContainSubstring("forbidden when services.keystone.mode is External"))
}

// TestValidateCreate_RejectsHorizonInExternalMode verifies services.horizon is
// forbidden in External mode (P2).
func TestValidateCreate_RejectsHorizonInExternalMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := externalControlPlane()
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.horizon"))
	g.Expect(err.Error()).To(ContainSubstring("External"))
}

// TestValidateCreate_RejectsMissingInfrastructureInManagedMode verifies
// spec.infrastructure is required for a non-External ControlPlane (preserving
// today's contract now that the Go field is optional). This is the webhook-only
// path — only reachable when Default() (which materializes the block) is
// bypassed, exactly what a direct validate() call exercises.
func TestValidateCreate_RejectsMissingInfrastructureInManagedMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Infrastructure = nil

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.infrastructure"))
	g.Expect(err.Error()).To(ContainSubstring("is required unless services.keystone.mode is External"))
}

// TestValidateCreate_RejectsMissingInfrastructureWithUnsetKeystone verifies the
// same requirement when services.keystone is unset (staged adoption is still a
// Managed control plane at the infrastructure layer).
func TestValidateCreate_RejectsMissingInfrastructureWithUnsetKeystone(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Keystone = nil
	cp.Spec.Infrastructure = nil

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.infrastructure"))
}

// TestValidateCreate_RejectsBadExternalAuthURL verifies a missing or malformed
// external.authURL is rejected. The hostless cases (https://, http:///v3) guard
// the SSRF-hardening: the coarse ^https?:// prefix accepted them, but the
// net/url-based gate requires a real host before the reconciler dials it.
func TestValidateCreate_RejectsBadExternalAuthURL(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	// missing.
	cpMissing := externalControlPlane()
	cpMissing.Spec.Services.Keystone.External.AuthURL = ""
	_, err := w.ValidateCreate(context.Background(), cpMissing)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("authURL is required"))

	for _, bad := range []string{
		"keystone.example.com",          // no scheme
		"ftp://keystone.example.com/v3", // wrong scheme
		"https://",                      // scheme only, no host
		"http:///v3",                    // path but empty host
	} {
		cpBad := externalControlPlane()
		cpBad.Spec.Services.Keystone.External.AuthURL = bad
		_, err = w.ValidateCreate(context.Background(), cpBad)
		g.Expect(err).To(HaveOccurred(), "expected %q to be rejected", bad)
		g.Expect(err.Error()).To(ContainSubstring("authURL"), "for input %q", bad)
	}
}

// TestValidateCreate_RejectsOverLongExternalAuthURL mirrors the MaxLength=2048
// marker. The CRD Pattern is end-unanchored, so a multi-kilobyte path is otherwise
// admissible — and the reconciler interpolates authURL into
// status.conditions[].message, whose 32768-byte cap is a WHOLE-OBJECT constraint:
// one over-long message makes every condition unpersistable and the reconciler
// spins in a backoff loop with no condition to diagnose it by.
func TestValidateCreate_RejectsOverLongExternalAuthURL(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	prefix := "https://keystone.example.com/"

	atCap := externalControlPlane()
	atCap.Spec.Services.Keystone.External.AuthURL = prefix + strings.Repeat("a", maxExternalAuthURLBytes-len(prefix))
	_, err := w.ValidateCreate(context.Background(), atCap)
	g.Expect(err).NotTo(HaveOccurred(), "an authURL exactly at the cap is admissible")

	overCap := externalControlPlane()
	overCap.Spec.Services.Keystone.External.AuthURL = prefix + strings.Repeat("a", maxExternalAuthURLBytes-len(prefix)+1)
	_, err = w.ValidateCreate(context.Background(), overCap)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.keystone.external.authURL"))
	g.Expect(err.Error()).To(ContainSubstring("at most 2048 bytes"))
}

// TestValidateCreate_RejectsEmptyCABundleSecretRefName verifies a present-but-
// nameless caBundleSecretRef is rejected in External mode.
func TestValidateCreate_RejectsEmptyCABundleSecretRefName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := externalControlPlane()
	cp.Spec.Services.Keystone.External.CABundleSecretRef = &commonv1.SecretRefSpec{Name: ""}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.keystone.external.caBundleSecretRef.name"))
}

// TestValidateCreate_RejectsPlaintextAuthURLWithCABundleSecretRef pins the coupling
// of the scheme to the CA bundle. An http:// endpoint never performs a TLS
// handshake, so the bundle is never consulted — yet every operator-visible signal
// says trust is enforced: the mint blocks on WaitingForCABundle until the Secret
// exists and `cacert` is projected into both credentials Secrets. Meanwhile K-ORC
// POSTs the admin password over the unencrypted connection on every mint and
// re-mint. Admission must reject the pair rather than silently void the bundle.
//
// Plain http:// WITHOUT a caBundleSecretRef stays admissible: it claims no
// transport security, so it misleads nobody.
func TestValidateCreate_RejectsPlaintextAuthURLWithCABundleSecretRef(t *testing.T) {
	cases := []struct {
		name      string
		authURL   string
		caBundle  *commonv1.SecretRefSpec
		wantError bool
	}{
		{
			name:      "http with a CA bundle is rejected",
			authURL:   "http://keystone.example.com/v3",
			caBundle:  &commonv1.SecretRefSpec{Name: "keystone-ca"},
			wantError: true,
		},
		{
			name:     "https with a CA bundle is accepted",
			authURL:  "https://keystone.example.com/v3",
			caBundle: &commonv1.SecretRefSpec{Name: "keystone-ca"},
		},
		{
			name:    "http without a CA bundle is accepted",
			authURL: "http://keystone.example.com/v3",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &ControlPlaneWebhook{}
			cp := externalControlPlane()
			cp.Spec.Services.Keystone.External.AuthURL = tc.authURL
			cp.Spec.Services.Keystone.External.CABundleSecretRef = tc.caBundle

			_, err := w.ValidateCreate(context.Background(), cp)
			if !tc.wantError {
				g.Expect(err).NotTo(HaveOccurred())
				return
			}
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("services.keystone.external.authURL"))
			g.Expect(err.Error()).To(ContainSubstring("must use scheme https when caBundleSecretRef is set"))
		})
	}
}

// TestValidateCreate_AccumulatesAllExternalModeErrors puts every External-mode
// rule into a broken state at once (external missing, infrastructure present,
// horizon present, all six managed-only fields set) and asserts the returned
// error names every field, pinning the no-short-circuit contract for the matrix.
// --- External-mode catalog stewardship (spec.services.keystone.external.catalog) ---

// TestValidateCreate_AcceptsExternalCatalogSpec proves the catalog surface
// admits with a non-default value: an explicit disambiguation filter.
func TestValidateCreate_AcceptsExternalCatalogSpec(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	cp := externalControlPlane()
	cp.Spec.Services.Keystone.External.Catalog = &ExternalCatalogSpec{
		IdentityServiceName: "keystone-legacy",
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_RejectsOverlongIdentityImportChildName pins the child-name
// guard External mode needs: it composes "{cp}-identity-endpoint-internal"
// unconditionally, so a ControlPlane name that overflows the apiserver's
// 253-byte metadata.name cap wedges ensureExternalCatalogImports in ImportError
// backoff.
func TestValidateCreate_RejectsOverlongIdentityImportChildName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	// One byte past the bound, and no catalog block at all: the mode-level guard
	// is the only thing standing between this CR and the wedge.
	cp := externalControlPlane()
	cp.Name = strings.Repeat("a", maxObjectNameBytes-identityImportChildNameOverhead+1)

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("metadata.name"))
	g.Expect(err.Error()).To(ContainSubstring("identity Endpoint import CR name would be 254 bytes"))
}

// TestValidateCreate_ManagedModeAcceptsLongName proves the identity-import guard
// is scoped to the mode that creates those imports: Managed mode composes only
// "{cp}-identity-endpoint", so a name the External guard rejects stays admissible.
func TestValidateCreate_ManagedModeAcceptsLongName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	cp := managedControlPlane()
	cp.Name = strings.Repeat("a", maxObjectNameBytes-identityImportChildNameOverhead+1)

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_RejectsIdentityServiceNameWithComma pins the one rule
// validateExternalCatalog carries: identityServiceName is cast to K-ORC's
// OpenStackName on the Service import filter, whose own CRD Pattern is `^[^,]+$`.
// A comma admitted here is rejected by the K-ORC CRD when the import CR is
// submitted, wedging the reconcile in ImportError backoff instead of failing at
// admission with a field the operator can act on.
func TestValidateCreate_RejectsIdentityServiceNameWithComma(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	cp := externalControlPlane()
	cp.Spec.Services.Keystone.External.Catalog = &ExternalCatalogSpec{IdentityServiceName: "keystone,v3"}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("catalog.identityServiceName"))
	g.Expect(err.Error()).To(ContainSubstring("must not contain a comma"))
}

// TestValidateCreate_ExternalCatalogIgnoredOutsideExternalMode proves the catalog
// block needs no dedicated Managed-mode rule: it lives under `external`, which is
// already forbidden outside External mode, so the existing rule catches it.
func TestValidateCreate_ExternalCatalogIgnoredOutsideExternalMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	cp := managedControlPlane()
	cp.Spec.Services.Keystone.External = &ExternalKeystoneSpec{
		AuthURL: "https://keystone.example.com/v3",
		Catalog: &ExternalCatalogSpec{IdentityServiceName: "keystone"},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("may only be set when services.keystone.mode is External"))
}

func TestValidateCreate_AccumulatesAllExternalModeErrors(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	replicas := int32(3)

	cp := externalControlPlane()
	cp.Name = strings.Repeat("a", 240)       // the identity Endpoint import name overflows 253 bytes
	cp.Spec.Services.Keystone.External = nil // external missing
	cp.Spec.Services.Keystone.Replicas = &replicas
	cp.Spec.Services.Keystone.Image = &commonv1.ImageSpec{Repository: "r", Tag: "t"}
	cp.Spec.Services.Keystone.PolicyOverrides = &commonv1.PolicySpec{Rules: map[string]string{"a": "b"}}
	cp.Spec.Services.Keystone.RotationInterval = &metav1.Duration{Duration: 24 * time.Hour}
	cp.Spec.Services.Keystone.Gateway = &commonv1.GatewaySpec{Hostname: "k.example.com"}
	cp.Spec.Services.Keystone.PublicEndpoint = "https://k.example.com/v3"
	cp.Spec.Services.Keystone.FederationProxyImage = &commonv1.ImageSpec{Repository: "r", Tag: "t"}
	cp.Spec.Infrastructure = &InfrastructureSpec{
		Database: commonv1.DatabaseSpec{Host: "db", Database: "d", SecretRef: commonv1.SecretRefSpec{Name: "s"}},
		Cache:    commonv1.CacheSpec{Backend: "b", Servers: []string{"mc:11211"}},
	}
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	msg := err.Error()
	g.Expect(msg).To(ContainSubstring("identity Endpoint import CR name"), "import-child-name error must be present")
	g.Expect(msg).To(ContainSubstring("external is required"), "external-required error must be present")
	g.Expect(msg).To(ContainSubstring("services.keystone.replicas"), "replicas-forbidden error must be present")
	g.Expect(msg).To(ContainSubstring("services.keystone.image"), "image-forbidden error must be present")
	g.Expect(msg).To(ContainSubstring("services.keystone.policyOverrides"), "policyOverrides-forbidden error must be present")
	g.Expect(msg).To(ContainSubstring("services.keystone.rotationInterval"), "rotationInterval-forbidden error must be present")
	g.Expect(msg).To(ContainSubstring("services.keystone.gateway"), "gateway-forbidden error must be present")
	g.Expect(msg).To(ContainSubstring("services.keystone.publicEndpoint"), "publicEndpoint-forbidden error must be present")
	g.Expect(msg).To(ContainSubstring("services.keystone.federationProxyImage"), "federationProxyImage-forbidden error must be present")
	g.Expect(msg).To(ContainSubstring("spec.infrastructure"), "infrastructure-forbidden error must be present")
	g.Expect(msg).To(ContainSubstring("services.horizon"), "horizon-forbidden error must be present")
}

// TestValidateCreate_FederationProxyImageDefenseInDepth covers the Managed-mode
// image checks that mirror the commonv1.ImageSpec markers: they surface on the
// ControlPlane the operator edits rather than as an opaque projection rejection
// on the Keystone child.
func TestValidateCreate_FederationProxyImageDefenseInDepth(t *testing.T) {
	w := &ControlPlaneWebhook{}

	tests := []struct {
		name       string
		image      *commonv1.ImageSpec
		wantSubstr string
	}{
		{"empty repository", &commonv1.ImageSpec{Tag: "dev"}, "federationProxyImage.repository must be set"},
		{"neither tag nor digest", &commonv1.ImageSpec{Repository: "r"}, "exactly one of federationProxyImage.tag or federationProxyImage.digest"},
		{
			"both tag and digest",
			&commonv1.ImageSpec{Repository: "r", Tag: "dev", Digest: "sha256:1111111111111111111111111111111111111111111111111111111111111111"},
			"exactly one of federationProxyImage.tag or federationProxyImage.digest",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := validControlPlane()
			cp.Spec.Services.Keystone.FederationProxyImage = tc.image

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.wantSubstr))
		})
	}
}

// TestValidateCreate_AcceptsDigestPinnedFederationProxyImage pins the happy
// path the override exists for: an immutable digest pin, and a locally built
// tag for the e2e suite.
func TestValidateCreate_AcceptsDigestPinnedFederationProxyImage(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for name, image := range map[string]*commonv1.ImageSpec{
		"digest pin": {
			Repository: "ghcr.io/c5c3/keystone-federation-proxy",
			Digest:     "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		},
		"local tag": {Repository: "ghcr.io/c5c3/keystone-federation-proxy", Tag: "dev"},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := validControlPlane()
			cp.Spec.Services.Keystone.FederationProxyImage = image

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).NotTo(HaveOccurred())
		})
	}
}

// TestValidateCreate_HorizonPublicEndpointMustBeURL covers the defense-in-depth
// URL parse: Keystone matches the derived WebSSO origin verbatim, so a value
// with no host could never match any dashboard.
func TestValidateCreate_HorizonPublicEndpointMustBeURL(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for name, endpoint := range map[string]string{
		"missing host": "https://",
		"wrong scheme": "ftp://horizon.example.com",
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := validControlPlane()
			cp.Spec.Services.Horizon = &ServiceHorizonSpec{PublicEndpoint: endpoint}

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("services.horizon.publicEndpoint"))
		})
	}
}

// TestValidateCreate_HorizonPublicEndpointMustBeABareOrigin covers the shapes the
// ^https?:// Pattern marker lets through and validateHTTPURL happily parses: the
// derived origin (publicEndpoint + "/auth/websso/") is projected onto the
// Keystone child's trusted_dashboard, which Keystone compares byte-for-byte
// against what the dashboard sends — so a path, query or fragment produces an
// origin that matches nothing, and every federated login fails only AFTER the
// user has authenticated at the identity provider.
//
// The gateway is deliberately left unset in each case: the rule holds on the
// gateway-less path too, which is where the scheme/host rules stop applying.
func TestValidateCreate_HorizonPublicEndpointMustBeABareOrigin(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for name, endpoint := range map[string]string{
		"query":    "https://horizon.example.com?utm=1",
		"fragment": "https://horizon.example.com#top",
		"path":     "https://horizon.example.com/dashboard",
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := validControlPlane()
			cp.Spec.Services.Horizon = &ServiceHorizonSpec{PublicEndpoint: endpoint}

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("services.horizon.publicEndpoint"))
			g.Expect(err.Error()).To(ContainSubstring("must be a bare origin"))
		})
	}
}

// TestValidateCreate_AcceptsHorizonPublicEndpointWithPort pins the case the
// override exists for: a dashboard published off the default HTTPS port. The
// trailing-slash form is accepted too — DerivedPublicEndpoint trims it before
// appending the WebSSO path.
func TestValidateCreate_AcceptsHorizonPublicEndpointWithPort(t *testing.T) {
	for _, endpoint := range []string{
		"https://horizon.example.com:8443",
		"https://horizon.example.com:8443/",
	} {
		t.Run(endpoint, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &ControlPlaneWebhook{}
			cp := validControlPlane()
			cp.Spec.Services.Horizon = &ServiceHorizonSpec{PublicEndpoint: endpoint}

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).NotTo(HaveOccurred())
		})
	}
}

// TestValidateCreate_RejectsUnusableGatewayHostname guards the shapes the
// reconciler cannot derive a browser-facing origin from. The check lives on
// BOTH service blocks because both hostnames feed a projection: horizon's the
// Keystone child's trusted_dashboard, keystone's the Horizon child's
// websso.keystoneURL. Without it a control character in a horizon field is
// caught only by the KEYSTONE child's webhook, taking a healthy Keystone
// projection down with an error naming neither the field nor the ControlPlane.
func TestValidateCreate_RejectsUnusableGatewayHostname(t *testing.T) {
	w := &ControlPlaneWebhook{}

	hostnames := map[string]string{
		"control character": "horizon.example.com\nx",
		"wildcard":          "*.example.com",
		"embedded port":     "horizon.example.com:8443",
		"carries a path":    "horizon.example.com/dashboard",
		"carries a scheme":  "https://horizon.example.com",
	}
	for name, hostname := range hostnames {
		t.Run("horizon/"+name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := validControlPlane()
			cp.Spec.Services.Horizon = &ServiceHorizonSpec{
				Gateway: &commonv1.GatewaySpec{
					Hostname:  hostname,
					ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
				},
			}

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("services.horizon.gateway.hostname"))
		})

		t.Run("keystone/"+name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := validControlPlane()
			cp.Spec.Services.Keystone.Gateway = &commonv1.GatewaySpec{
				Hostname:  hostname,
				ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
			}

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("services.keystone.gateway.hostname"))
		})
	}
}

// TestValidateCreate_AcceptsBareGatewayHostname pins the happy path: a concrete
// DNS name, the only shape the derived origins can be built from.
func TestValidateCreate_AcceptsBareGatewayHostname(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Keystone.Gateway = &commonv1.GatewaySpec{
		Hostname:  "keystone.127-0-0-1.nip.io",
		ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
	}
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{
		Gateway: &commonv1.GatewaySpec{
			Hostname:  "horizon.127-0-0-1.nip.io",
			ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_RejectsOverlongGatewayHostname guards the derived origins
// against a hostname long enough to overrun the children's own MaxLength
// markers: the API server would reject a projected child the operator never
// wrote, wedging the ControlPlane behind a field name that appears nowhere in
// its spec.
func TestValidateCreate_RejectsOverlongGatewayHostname(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	hostname := strings.Repeat("a", maxGatewayHostnameLen-len(".example.com")+1) + ".example.com"
	g.Expect(hostname).To(HaveLen(maxGatewayHostnameLen + 1))

	cp := validControlPlane()
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{
		Gateway: &commonv1.GatewaySpec{
			Hostname:  hostname,
			ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.horizon.gateway.hostname"))
	g.Expect(err.Error()).To(ContainSubstring("maximum DNS name length"))
}

// TestValidateCreate_HorizonPublicEndpointMustAgreeWithGateway pins the rule the
// field's own godoc documents: Django derives the WebSSO origin it sends from
// the request Host header — i.e. from gateway.hostname — and Keystone compares
// it verbatim. A divergent host is rejected only after the user has already
// typed their corporate password into the IdP, with nothing on the ControlPlane
// recording why.
func TestValidateCreate_HorizonPublicEndpointMustAgreeWithGateway(t *testing.T) {
	w := &ControlPlaneWebhook{}
	gateway := func() *commonv1.GatewaySpec {
		return &commonv1.GatewaySpec{
			Hostname:  "horizon.example.com",
			ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		}
	}

	t.Run("divergent host", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := validControlPlane()
		cp.Spec.Services.Horizon = &ServiceHorizonSpec{
			Gateway:        gateway(),
			PublicEndpoint: "https://dashboard.example.com",
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("services.horizon.publicEndpoint"))
		g.Expect(err.Error()).To(ContainSubstring(`must equal services.horizon.gateway.hostname "horizon.example.com"`))
	})

	t.Run("http scheme behind a TLS-terminating gateway", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := validControlPlane()
		cp.Spec.Services.Horizon = &ServiceHorizonSpec{
			Gateway:        gateway(),
			PublicEndpoint: "http://horizon.example.com",
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("scheme must be https"))
	})

	t.Run("matching host with a non-default port", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := validControlPlane()
		cp.Spec.Services.Horizon = &ServiceHorizonSpec{
			Gateway:        gateway(),
			PublicEndpoint: "https://horizon.example.com:8443",
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred(), "Gateway API hostnames carry no port, so the port may only differ")
	})
}

// TestValidateCreate_WarnsOnCleartextHorizonPublicEndpoint covers the gateway-less
// dashboard, where an http origin is a legal (if unwise) development setup that
// the CRD Pattern deliberately allows. Keystone POSTs the unscoped WebSSO token
// to that origin, so the downgrade must at least be surfaced.
func TestValidateCreate_WarnsOnCleartextHorizonPublicEndpoint(t *testing.T) {
	w := &ControlPlaneWebhook{}

	t.Run("http warns", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := validControlPlane()
		cp.Spec.Services.Horizon = &ServiceHorizonSpec{PublicEndpoint: "http://horizon.example.com"}

		warnings, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).To(HaveLen(1))
		g.Expect(warnings[0]).To(ContainSubstring("bearer token in cleartext"))
	})

	t.Run("https is silent", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := validControlPlane()
		cp.Spec.Services.Horizon = &ServiceHorizonSpec{PublicEndpoint: "https://horizon.example.com"}

		warnings, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).To(BeEmpty())
	})
}

// --- Mode transition gating ---

// TestValidateUpdate_RejectsManagedToExternal verifies flipping a live managed
// ControlPlane to External mode is rejected outright.
func TestValidateUpdate_RejectsManagedToExternal(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := managedControlPlane()
	newCP := externalControlPlane()

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("cannot be changed to External"))
}

// TestValidateUpdate_RejectsExternalToManaged verifies switching a live External
// ControlPlane back to Managed is rejected with the phase-3 takeover message.
func TestValidateUpdate_RejectsExternalToManaged(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := externalControlPlane()
	newCP := managedControlPlane()

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("phase-3"))
}

// TestValidateUpdate_RejectsExternalToNilKeystone verifies removing the keystone
// service from a live External ControlPlane (also a move away from External) is
// rejected.
func TestValidateUpdate_RejectsExternalToNilKeystone(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := externalControlPlane()
	newCP := externalControlPlane()
	newCP.Spec.Services.Keystone = nil

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
}

// TestValidateUpdate_AllowsNilKeystoneToManaged verifies staged adoption is
// preserved: adding a Managed keystone service to a control plane that had none
// is accepted (neither revision is External).
func TestValidateUpdate_AllowsNilKeystoneToManaged(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := managedControlPlane()
	oldCP.Spec.Services.Keystone = nil
	newCP := managedControlPlane() // keystone present, mode unset (=> Managed)

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateUpdate_AllowsExternalUnchanged verifies a no-op update of an
// External ControlPlane (both revisions External, same spec) is accepted, so the
// gating does not over-fire on a same-mode update.
func TestValidateUpdate_AllowsExternalUnchanged(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := externalControlPlane()
	newCP := externalControlPlane()

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateUpdate_RejectsInfrastructurePresenceFlip verifies removing the
// infrastructure block on a mode-unchanged managed ControlPlane is rejected by
// the presence-flip guard (defense-in-depth for webhook-bypassed states).
func TestValidateUpdate_RejectsInfrastructurePresenceFlip(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := managedControlPlane()
	newCP := managedControlPlane()
	newCP.Spec.Infrastructure = nil

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("infrastructure presence is immutable"))
}

// --- Service-registration allowlist tests (spec.korc.serviceRegistrations) ---

// registrationControlPlane returns a managed ControlPlane in namespace "openstack"
// whose allowlist admits the entries passed in, so each test mutates one aspect of
// a CR that is otherwise valid.
func registrationControlPlane(allowed ...string) *ControlPlane {
	cp := validControlPlane()
	cp.Namespace = "openstack"
	cp.Spec.KORC.ServiceRegistrations = &ServiceRegistrationsSpec{AllowedNamespaces: allowed}
	return cp
}

func TestValidateCreate_AcceptsServiceRegistrationAllowlist(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	_, err := w.ValidateCreate(context.Background(), registrationControlPlane("tenant-a", "tenant-b"))
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_AcceptsAbsentServiceRegistrations pins the default posture:
// no block at all is the own-plus-dedicated default, not an omission to reject.
func TestValidateCreate_AcceptsAbsentServiceRegistrations(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.KORC.ServiceRegistrations = nil

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_AcceptsEmptyServiceRegistrationAllowlist pins that a declared
// block with no entries is legal: it admits exactly what the absent block admits.
func TestValidateCreate_AcceptsEmptyServiceRegistrationAllowlist(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	_, err := w.ValidateCreate(context.Background(), registrationControlPlane())
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_AcceptsRedundantServiceRegistrationNamespaces pins the
// deliberate no-op: the ControlPlane's own namespace and a dedicated service
// namespace are admitted implicitly, so listing either is redundant rather than
// wrong. Rejecting them would make removing the keystone namespace block fail on
// an allowlist entry that never changed.
func TestValidateCreate_AcceptsRedundantServiceRegistrationNamespaces(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := registrationControlPlane("openstack", "identity")
	cp.Spec.Services.Keystone.Namespace = &ServiceNamespaceSpec{
		Name: "identity", Lifecycle: ServiceNamespaceLifecycleManaged,
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_RejectsServiceRegistrationNamespaceBadPattern mirrors the
// per-item RFC-1123 Pattern marker for webhook-bypassed callers: the value names a
// Kubernetes namespace.
func TestValidateCreate_RejectsServiceRegistrationNamespaceBadPattern(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	_, err := w.ValidateCreate(context.Background(), registrationControlPlane("Tenant_A"))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("serviceRegistrations.allowedNamespaces[0]"))
	g.Expect(err.Error()).To(ContainSubstring("RFC-1123 label"))
}

// TestValidateCreate_RejectsDuplicateServiceRegistrationNamespace mirrors the
// listType=set duplicate rejection the apiserver applies, for the same
// webhook-bypassed callers.
func TestValidateCreate_RejectsDuplicateServiceRegistrationNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	_, err := w.ValidateCreate(context.Background(), registrationControlPlane("tenant-a", "tenant-a"))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("serviceRegistrations.allowedNamespaces[1]"))
	g.Expect(err.Error()).To(ContainSubstring("Duplicate value"))
}

// TestValidateCreate_RejectsTooManyServiceRegistrationNamespaces mirrors the
// MaxItems cap: every admitted namespace is one the registration gate consults on
// every pass.
func TestValidateCreate_RejectsTooManyServiceRegistrationNamespaces(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	allowed := make([]string, 0, maxServiceRegistrationNamespaces+1)
	for i := range maxServiceRegistrationNamespaces + 1 {
		allowed = append(allowed, fmt.Sprintf("tenant-%d", i))
	}

	_, err := w.ValidateCreate(context.Background(), registrationControlPlane(allowed...))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("serviceRegistrations.allowedNamespaces"))
	g.Expect(err.Error()).To(ContainSubstring("must have at most 32 items"))
}

// --- Per-service dedicated backing services ---

// dedicatedControlPlane returns a ControlPlane whose Keystone service opts into a
// dedicated database AND cache, and whose Horizon dashboard opts into a dedicated
// cache. The SHARED block stays brownfield (the validControlPlane baseline), so
// the managed clusterRef names below cannot collide with a shared instance unless
// a test makes them.
func dedicatedControlPlane() *ControlPlane {
	cp := validControlPlane()
	cp.Name = "cp"
	cp.Spec.Services.Keystone.DedicatedBackingServices = &KeystoneDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-db"},
			Database:   "keystone",
			SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db"},
		},
		Cache: &commonv1.CacheSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-cache"},
			Backend:    commonv1.DefaultCacheBackend,
		},
	}
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{
		DedicatedBackingServices: &HorizonDedicatedBackingServicesSpec{
			Cache: &commonv1.CacheSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "cp-horizon-cache"},
				Backend:    commonv1.DefaultCacheBackend,
			},
		},
	}
	return cp
}

// TestDefault_DedicatedBackingServicesLeaves verifies a declared dedicated block
// takes the same leaf defaults as the shared one, with a managed clusterRef name
// DERIVED from the ControlPlane so it cannot collide with the shared instance,
// and with credentialsMode materialized to Static (a dedicated managed database
// cannot draw engine-issued credentials).
func TestDefault_DedicatedBackingServicesLeaves(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Name = "prod"
	cp.Spec.Services.Keystone.DedicatedBackingServices = &KeystoneDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{},
		Cache:    &commonv1.CacheSpec{},
	}
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{
		DedicatedBackingServices: &HorizonDedicatedBackingServicesSpec{Cache: &commonv1.CacheSpec{}},
	}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	db := cp.Spec.Services.Keystone.DedicatedBackingServices.Database
	g.Expect(db.ClusterRef).NotTo(BeNil())
	g.Expect(db.ClusterRef.Name).To(Equal("prod" + DedicatedKeystoneDatabaseClusterRefSuffix))
	g.Expect(db.Database).To(Equal(DefaultDatabaseName))
	g.Expect(db.SecretRef.Name).To(Equal(DefaultDatabaseSecretName))
	g.Expect(db.CredentialsMode).To(Equal(commonv1.CredentialsModeStatic),
		"a dedicated managed database is Static-only: no per-instance OpenBao engine role exists")

	ksCache := cp.Spec.Services.Keystone.DedicatedBackingServices.Cache
	g.Expect(ksCache.ClusterRef).NotTo(BeNil())
	g.Expect(ksCache.ClusterRef.Name).To(Equal("prod" + DedicatedKeystoneCacheClusterRefSuffix))
	g.Expect(ksCache.Backend).To(Equal(DefaultCacheBackend))

	hzCache := cp.Spec.Services.Horizon.DedicatedBackingServices.Cache
	g.Expect(hzCache.ClusterRef).NotTo(BeNil())
	g.Expect(hzCache.ClusterRef.Name).To(Equal("prod" + DedicatedHorizonCacheClusterRefSuffix))

	// The derived names must differ from each other AND from the shared defaults,
	// otherwise two instances would resolve to one child CR.
	g.Expect(db.ClusterRef.Name).NotTo(Equal(DefaultDatabaseClusterRefName))
	g.Expect(ksCache.ClusterRef.Name).NotTo(Equal(hzCache.ClusterRef.Name))
	g.Expect(ksCache.ClusterRef.Name).NotTo(Equal(DefaultCacheClusterRefName))

	// Defaulting must be idempotent on the dedicated leaves too.
	before := cp.DeepCopy()
	g.Expect(w.Default(context.Background(), cp)).To(Succeed())
	g.Expect(cp.Spec.Services).To(Equal(before.Spec.Services))
}

// TestDefault_DoesNotInventDedicatedBackingServices pins the shared-by-default
// contract: a service that does not opt in must come out of the defaulting
// webhook with NO dedicated block, so its projection keeps using the
// ControlPlane-wide instances.
func TestDefault_DoesNotInventDedicatedBackingServices(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	g.Expect(cp.Spec.Services.Keystone.DedicatedBackingServices).To(BeNil())
	g.Expect(cp.Spec.Services.Horizon.DedicatedBackingServices).To(BeNil())
	g.Expect(cp.DedicatedKeystoneDatabase()).To(BeNil())
	g.Expect(cp.DedicatedKeystoneCache()).To(BeNil())
	g.Expect(cp.DedicatedHorizonCache()).To(BeNil())
}

// TestDefault_DedicatedBrownfieldNotCoercedIntoManaged mirrors
// TestDefault_DoesNotInventModeForBrownfield for the dedicated blocks: an
// explicit external endpoint must never grow a managed clusterRef, which would
// make the block fail its own XOR check.
func TestDefault_DedicatedBrownfieldNotCoercedIntoManaged(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Name = "cp"
	cp.Spec.Services.Keystone.DedicatedBackingServices = &KeystoneDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{Host: "keystone-db.example.com", Port: 3306},
		Cache:    &commonv1.CacheSpec{Servers: []string{"keystone-mc:11211"}},
	}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	db := cp.Spec.Services.Keystone.DedicatedBackingServices.Database
	g.Expect(db.ClusterRef).To(BeNil(), "a brownfield dedicated database must not grow a managed clusterRef")
	g.Expect(db.CredentialsMode).To(BeEmpty(), "Static is only materialized for a MANAGED dedicated database")
	g.Expect(cp.Spec.Services.Keystone.DedicatedBackingServices.Cache.ClusterRef).To(BeNil())

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred(), "a brownfield dedicated instance must survive its own XOR check")
}

func TestValidateCreate_AcceptsDedicatedBackingServices(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	_, err := w.ValidateCreate(context.Background(), dedicatedControlPlane())
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_RejectsEmptyDedicatedBlock rejects an opt-in that requests
// nothing — the webhook mirror of the at-least-one-class CEL rule.
func TestValidateCreate_RejectsEmptyDedicatedBlock(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Keystone.DedicatedBackingServices = &KeystoneDedicatedBackingServicesSpec{}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("at least one backing-service class"))
}

// TestValidateCreate_RejectsDedicatedDatabaseXOR verifies the dedicated database
// inherits the managed-vs-brownfield XOR of the shared block: both modes set, or
// neither, is rejected.
func TestValidateCreate_RejectsDedicatedDatabaseXOR(t *testing.T) {
	tests := []struct {
		name string
		db   commonv1.DatabaseSpec
	}{
		{name: "both clusterRef and host", db: commonv1.DatabaseSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-db"},
			Host:       "db.example.com",
			Database:   "keystone",
			SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db"},
		}},
		{name: "neither clusterRef nor host", db: commonv1.DatabaseSpec{
			Database:  "keystone",
			SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &ControlPlaneWebhook{}
			cp := validControlPlane()
			db := tc.db
			cp.Spec.Services.Keystone.DedicatedBackingServices = &KeystoneDedicatedBackingServicesSpec{Database: &db}

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("exactly one of clusterRef or host must be set"))
			g.Expect(err.Error()).To(ContainSubstring("dedicatedBackingServices.database"))
		})
	}
}

// TestValidateCreate_RejectsDedicatedCacheXOR is the cache twin of
// TestValidateCreate_RejectsDedicatedDatabaseXOR, on the Horizon block so both
// services' dedicated caches are covered.
func TestValidateCreate_RejectsDedicatedCacheXOR(t *testing.T) {
	tests := []struct {
		name  string
		cache commonv1.CacheSpec
	}{
		{name: "both clusterRef and servers", cache: commonv1.CacheSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "cp-horizon-cache"},
			Servers:    []string{"mc:11211"},
			Backend:    commonv1.DefaultCacheBackend,
		}},
		{name: "neither clusterRef nor servers", cache: commonv1.CacheSpec{
			Backend: commonv1.DefaultCacheBackend,
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &ControlPlaneWebhook{}
			cp := validControlPlane()
			cache := tc.cache
			cp.Spec.Services.Horizon = &ServiceHorizonSpec{
				DedicatedBackingServices: &HorizonDedicatedBackingServicesSpec{Cache: &cache},
			}

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("exactly one of clusterRef or servers must be set"))
			g.Expect(err.Error()).To(ContainSubstring("horizon.dedicatedBackingServices.cache"))
		})
	}
}

// TestValidateCreate_RejectsCacheControlChars covers both cache blocks the
// ControlPlane owns: the shared spec.infrastructure.cache and a per-service
// dedicated cache. Each is projected onto a service CR's spec.cache, where
// cache.ResolveServers feeds the verbatim INI renderer — so a newline there
// injects an additional config line into the projected service config. Failing
// here keeps the ControlPlane from admitting a spec whose projected child CR its
// own webhook would then reject mid-reconcile.
func TestValidateCreate_RejectsCacheControlChars(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(cp *ControlPlane)
		wantPath string
	}{
		{
			name: "shared cache server with a newline",
			mutate: func(cp *ControlPlane) {
				cp.Spec.Infrastructure.Cache.Servers = []string{"mc:11211\nauth_url = http://attacker.example/v3"}
			},
			wantPath: "infrastructure.cache.servers[0]",
		},
		{
			name: "dedicated cache clusterRef name with a carriage return",
			mutate: func(cp *ControlPlane) {
				cp.Spec.Services.Horizon = &ServiceHorizonSpec{
					DedicatedBackingServices: &HorizonDedicatedBackingServicesSpec{
						Cache: &commonv1.CacheSpec{
							ClusterRef: &corev1.LocalObjectReference{
								Name: "cp-horizon-cache\rauth_url = http://attacker.example/v3",
							},
							Backend: commonv1.DefaultCacheBackend,
						},
					},
				}
			},
			wantPath: "horizon.dedicatedBackingServices.cache.clusterRef.name",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &ControlPlaneWebhook{}
			cp := validControlPlane()
			tc.mutate(cp)

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("must not contain a newline or carriage return"))
			g.Expect(err.Error()).To(ContainSubstring(tc.wantPath))
		})
	}
}

// TestValidateCreate_RejectsDynamicCredentialsOnDedicatedDatabase pins the one
// constraint a dedicated database carries that the shared block does not: the
// OpenBao database engine is bootstrapped once per NAMESPACE against the shared
// cluster, so no engine role can issue credentials for a dedicated instance and
// an admitted Dynamic dedicated database would wedge on an ExternalSecret that
// can never sync.
func TestValidateCreate_RejectsDynamicCredentialsOnDedicatedDatabase(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := dedicatedControlPlane()
	cp.Spec.Services.Keystone.DedicatedBackingServices.Database.CredentialsMode = commonv1.CredentialsModeDynamic

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("credentialsMode Dynamic is not supported on a dedicated database"))
}

// TestValidateCreate_RejectsDedicatedDatabaseReplicasTwo verifies the
// Galera-quorum rule applies to a dedicated database exactly as it does to the
// shared one — the projection that makes 2 unsafe is the same.
func TestValidateCreate_RejectsDedicatedDatabaseReplicasTwo(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := dedicatedControlPlane()
	cp.Spec.Services.Keystone.DedicatedBackingServices.Database.Replicas = 2

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("2 cannot hold a majority"))
	g.Expect(err.Error()).To(ContainSubstring("dedicatedBackingServices.database.replicas"))
}

// TestValidateCreate_RejectsDedicatedClusterRefCollision covers both collision
// axes: a dedicated instance named like the SHARED one, and two services'
// dedicated instances of the same class named alike. Either would make two
// projections resolve to one child CR and silently void the isolation the opt-in
// exists for.
func TestValidateCreate_RejectsDedicatedClusterRefCollision(t *testing.T) {
	t.Run("dedicated database collides with the shared database", func(t *testing.T) {
		g := NewGomegaWithT(t)
		w := &ControlPlaneWebhook{}
		cp := dedicatedControlPlane()
		// Make the shared block managed, then point the dedicated database at it.
		cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "openstack-db"},
			Database:   "openstack",
			SecretRef:  commonv1.SecretRefSpec{Name: "db-creds"},
		}
		cp.Spec.Services.Keystone.DedicatedBackingServices.Database.ClusterRef = &corev1.LocalObjectReference{Name: "openstack-db"}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("Duplicate value"))
		g.Expect(err.Error()).To(ContainSubstring("openstack-db"))
	})

	t.Run("two dedicated caches collide with each other", func(t *testing.T) {
		g := NewGomegaWithT(t)
		w := &ControlPlaneWebhook{}
		cp := dedicatedControlPlane()
		cp.Spec.Services.Horizon.DedicatedBackingServices.Cache.ClusterRef = &corev1.LocalObjectReference{Name: "cp-keystone-cache"}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("Duplicate value"))
		g.Expect(err.Error()).To(ContainSubstring("cp-keystone-cache"))
	})
}

// TestValidateCreate_RejectsDedicatedInExternalMode verifies the webhook mirror of
// the External-mode CEL forbid rule: an External ControlPlane provisions no
// backing services at all, so there is nothing to make dedicated.
func TestValidateCreate_RejectsDedicatedInExternalMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Infrastructure = nil
	cp.Spec.Services.Keystone = &ServiceKeystoneSpec{
		Mode:     KeystoneModeExternal,
		External: &ExternalKeystoneSpec{AuthURL: "https://keystone.example.com/v3"},
		DedicatedBackingServices: &KeystoneDedicatedBackingServicesSpec{
			Cache: &commonv1.CacheSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-cache"},
				Backend:    commonv1.DefaultCacheBackend,
			},
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.keystone.dedicatedBackingServices"))
	g.Expect(err.Error()).To(ContainSubstring("no backing services are provisioned at all"))
}

// TestValidateUpdate_RejectsDedicatedPresenceFlip pins the transition freeze: a
// live service cannot be moved between shared and dedicated backing services, in
// either direction, at either granularity (the whole block, or one class within
// it). The freeze is webhook-only precisely so a later transition feature can
// relax it to a gated migration.
func TestValidateUpdate_RejectsDedicatedPresenceFlip(t *testing.T) {
	tests := []struct {
		name           string
		oldCP, newCP   func() *ControlPlane
		wantSubstrings []string
	}{
		{
			name:  "shared -> dedicated block",
			oldCP: validControlPlane,
			newCP: dedicatedControlPlane,
		},
		{
			name:  "dedicated -> shared block",
			oldCP: dedicatedControlPlane,
			newCP: validControlPlane,
		},
		{
			name: "adding a dedicated class to an existing block",
			oldCP: func() *ControlPlane {
				cp := dedicatedControlPlane()
				cp.Spec.Services.Keystone.DedicatedBackingServices.Database = nil
				return cp
			},
			newCP:          dedicatedControlPlane,
			wantSubstrings: []string{"dedicatedBackingServices.database"},
		},
		{
			name:  "removing a dedicated class from an existing block",
			oldCP: dedicatedControlPlane,
			newCP: func() *ControlPlane {
				cp := dedicatedControlPlane()
				cp.Spec.Services.Keystone.DedicatedBackingServices.Cache = nil
				return cp
			},
			wantSubstrings: []string{"dedicatedBackingServices.cache"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &ControlPlaneWebhook{}

			_, err := w.ValidateUpdate(context.Background(), tc.oldCP(), tc.newCP())
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(
				"switching a service between shared and dedicated backing services",
			))
			for _, want := range tc.wantSubstrings {
				g.Expect(err.Error()).To(ContainSubstring(want))
			}
		})
	}
}

// TestValidateUpdate_RejectsDedicatedLeafChanges verifies a dedicated instance
// that stays declared is frozen on the same create-only leaves as the shared
// block: renaming its child CR, its logical database, or reshaping its topology
// would orphan the live instance or drive a destructive update on it.
func TestValidateUpdate_RejectsDedicatedLeafChanges(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(cp *ControlPlane)
		wantMsg string
	}{
		{
			name: "managed clusterRef rename",
			mutate: func(cp *ControlPlane) {
				cp.Spec.Services.Keystone.DedicatedBackingServices.Database.ClusterRef = &corev1.LocalObjectReference{Name: "renamed-db"}
			},
			wantMsg: "managed database clusterRef.name is immutable",
		},
		{
			name: "database name change",
			mutate: func(cp *ControlPlane) {
				cp.Spec.Services.Keystone.DedicatedBackingServices.Database.Database = "keystone2"
			},
			wantMsg: "database name is immutable",
		},
		{
			name: "replicas change",
			mutate: func(cp *ControlPlane) {
				cp.Spec.Services.Keystone.DedicatedBackingServices.Database.Replicas = 3
			},
			wantMsg: "database replicas is immutable after creation",
		},
		{
			name: "storageSize change",
			mutate: func(cp *ControlPlane) {
				cp.Spec.Services.Keystone.DedicatedBackingServices.Database.StorageSize = "512Mi"
			},
			wantMsg: "database storageSize is immutable after creation",
		},
		{
			name: "cache mode flip",
			mutate: func(cp *ControlPlane) {
				cp.Spec.Services.Horizon.DedicatedBackingServices.Cache.ClusterRef = nil
				cp.Spec.Services.Horizon.DedicatedBackingServices.Cache.Servers = []string{"mc:11211"}
			},
			wantMsg: "cache mode (managed clusterRef vs brownfield servers) is immutable",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &ControlPlaneWebhook{}
			oldCP := dedicatedControlPlane()
			// Pin storageSize on the old object so the "" -> default normalization
			// does not mask the resize test.
			oldCP.Spec.Services.Keystone.DedicatedBackingServices.Database.StorageSize = "100Gi"
			newCP := dedicatedControlPlane()
			newCP.Spec.Services.Keystone.DedicatedBackingServices.Database.StorageSize = "100Gi"
			tc.mutate(newCP)

			_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.wantMsg))
		})
	}
}

// TestValidateUpdate_AcceptsDedicatedCacheReplicasChange pins the other half of
// the immutability contract — only what genuinely cannot change is frozen. A
// cache replica count is reconciled in place on the owned Memcached (scaling a
// cache loses no data), so it stays mutable on a dedicated instance exactly as it
// is on the shared one.
func TestValidateUpdate_AcceptsDedicatedCacheReplicasChange(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := dedicatedControlPlane()
	oldCP.Spec.Services.Keystone.DedicatedBackingServices.Cache.Replicas = 1
	newCP := dedicatedControlPlane()
	newCP.Spec.Services.Keystone.DedicatedBackingServices.Cache.Replicas = 3

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- per-service namespace validation (issue #646) ---

// namespacedControlPlane is the baseline for the per-service namespace tests: a
// ControlPlane in "openstack" that places Keystone in an operator-owned
// namespace and the dashboard in a pre-existing one.
func namespacedControlPlane() *ControlPlane {
	cp := validControlPlane()
	cp.Name = "cp"
	cp.Namespace = "openstack"
	cp.Spec.Services.Keystone.Namespace = &ServiceNamespaceSpec{
		Name:      "identity",
		Lifecycle: ServiceNamespaceLifecycleManaged,
	}
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{
		Namespace: &ServiceNamespaceSpec{
			Name:      "dashboard",
			Lifecycle: ServiceNamespaceLifecycleExternal,
		},
	}
	return cp
}

// TestDefault_ServiceNamespaceLifecycle verifies the defaulting webhook
// materializes the Managed lifecycle on a DECLARED assignment — and never
// invents an assignment for a service that declared none.
func TestDefault_ServiceNamespaceLifecycle(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Keystone.Namespace = &ServiceNamespaceSpec{Name: "identity"}
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())
	g.Expect(cp.Spec.Services.Keystone.Namespace.Lifecycle).To(Equal(ServiceNamespaceLifecycleManaged))
	g.Expect(cp.Spec.Services.Horizon.Namespace).To(BeNil())

	// Idempotent, and an explicit lifecycle is preserved.
	cp.Spec.Services.Keystone.Namespace.Lifecycle = ServiceNamespaceLifecycleExternal
	g.Expect(w.Default(context.Background(), cp)).To(Succeed())
	g.Expect(cp.Spec.Services.Keystone.Namespace.Lifecycle).To(Equal(ServiceNamespaceLifecycleExternal))
}

// TestValidateCreate_AcceptsServiceNamespaces verifies the baseline shape — one
// Managed and one External assignment — is admissible.
func TestValidateCreate_AcceptsServiceNamespaces(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	_, err := w.ValidateCreate(context.Background(), namespacedControlPlane())
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_RejectsNamespaceEqualToControlPlane pins the no-op guard:
// naming the ControlPlane's own namespace is not "place it here", it is the
// shape that would make the operator claim ownership of — and at teardown delete
// — the namespace the ControlPlane itself lives in.
func TestValidateCreate_RejectsNamespaceEqualToControlPlane(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Namespace = "openstack"
	cp.Spec.Services.Keystone.Namespace = &ServiceNamespaceSpec{Name: "openstack"}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.keystone.namespace.name"))
	g.Expect(err.Error()).To(ContainSubstring("must differ from the ControlPlane's own namespace"))
}

// TestValidateCreate_RejectsInvalidNamespaceName mirrors the RFC-1123 Pattern
// marker for webhook-bypassed callers: the value names a Kubernetes namespace.
func TestValidateCreate_RejectsInvalidNamespaceName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Namespace = "openstack"
	cp.Spec.Services.Keystone.Namespace = &ServiceNamespaceSpec{Name: "Identity_NS"}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("RFC-1123 label"))
}

// TestValidateCreate_RejectsNamespaceLifecycleConflict pins the co-location rule:
// services sharing a namespace share its backing services and its tenant store,
// so they cannot disagree on who owns it — one declaration would have teardown
// delete the namespace the other declared untouchable.
func TestValidateCreate_RejectsNamespaceLifecycleConflict(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Namespace = "openstack"
	cp.Spec.Services.Keystone.Namespace = &ServiceNamespaceSpec{
		Name: "services", Lifecycle: ServiceNamespaceLifecycleManaged,
	}
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{
		Namespace: &ServiceNamespaceSpec{Name: "services", Lifecycle: ServiceNamespaceLifecycleExternal},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.horizon.namespace.lifecycle"))
	g.Expect(err.Error()).To(ContainSubstring("must declare the same lifecycle"))
}

// TestValidateCreate_RejectsNamespaceInExternalMode verifies the webhook mirror
// of the External-mode CEL forbid rule: no Keystone workload is deployed, so
// there is nothing to place.
func TestValidateCreate_RejectsNamespaceInExternalMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Infrastructure = nil
	cp.Spec.Services.Keystone = &ServiceKeystoneSpec{
		Mode:      KeystoneModeExternal,
		External:  &ExternalKeystoneSpec{AuthURL: "https://keystone.example.com/v3"},
		Namespace: &ServiceNamespaceSpec{Name: "identity"},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.keystone.namespace"))
	g.Expect(err.Error()).To(ContainSubstring("there is nothing to place"))
}

// TestValidateCreate_RejectsNamespaceClaimedByOtherControlPlane pins the
// tenant-key invariant in BOTH directions: a namespace already occupied by
// another ControlPlane cannot be claimed as a service namespace, and a
// ControlPlane cannot be created into a namespace another one already claims as
// its service namespace. The claim is cluster-wide — the incumbent lives in a
// different namespace than the newcomer, so a namespace-scoped List would miss it.
func TestValidateCreate_RejectsNamespaceClaimedByOtherControlPlane(t *testing.T) {
	t.Run("service namespace collides with another ControlPlane's own namespace", func(t *testing.T) {
		g := NewGomegaWithT(t)
		incumbent := validControlPlane()
		incumbent.Name = "other"
		incumbent.Namespace = "identity"
		c := fake.NewClientBuilder().WithScheme(webhookScheme(t)).WithObjects(incumbent).Build()
		w := &ControlPlaneWebhook{Client: c}

		_, err := w.ValidateCreate(context.Background(), namespacedControlPlane())
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("services.keystone.namespace.name"))
		g.Expect(err.Error()).To(ContainSubstring("already occupied by ControlPlane \"other\""))
	})

	t.Run("service namespace collides with another ControlPlane's service namespace", func(t *testing.T) {
		g := NewGomegaWithT(t)
		incumbent := namespacedControlPlane()
		incumbent.Name = "other"
		incumbent.Namespace = "other-ns"
		c := fake.NewClientBuilder().WithScheme(webhookScheme(t)).WithObjects(incumbent).Build()
		w := &ControlPlaneWebhook{Client: c}

		newcomer := namespacedControlPlane()
		newcomer.Name = "newcomer"
		newcomer.Namespace = "newcomer-ns"

		_, err := w.ValidateCreate(context.Background(), newcomer)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("identity"))
		g.Expect(err.Error()).To(ContainSubstring("belongs to at most one ControlPlane"))
	})

	t.Run("own namespace collides with another ControlPlane's service namespace", func(t *testing.T) {
		g := NewGomegaWithT(t)
		incumbent := namespacedControlPlane()
		incumbent.Name = "other"
		incumbent.Namespace = "other-ns"
		c := fake.NewClientBuilder().WithScheme(webhookScheme(t)).WithObjects(incumbent).Build()
		w := &ControlPlaneWebhook{Client: c}

		// A plain ControlPlane created INTO the namespace the incumbent already
		// claims for its Keystone service.
		newcomer := validControlPlane()
		newcomer.Name = "newcomer"
		newcomer.Namespace = "identity"

		_, err := w.ValidateCreate(context.Background(), newcomer)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("metadata.namespace"))
		g.Expect(err.Error()).To(ContainSubstring("already claimed as a service namespace"))
	})

	t.Run("a ControlPlane does not collide with itself", func(t *testing.T) {
		g := NewGomegaWithT(t)
		self := namespacedControlPlane()
		c := fake.NewClientBuilder().WithScheme(webhookScheme(t)).WithObjects(self).Build()
		w := &ControlPlaneWebhook{Client: c}

		g.Expect(w.validateNamespaceClaims(context.Background(), self)).NotTo(HaveOccurred())
	})
}

// TestValidateUpdate_RejectsServiceNamespaceChanges pins the create-only freeze:
// presence, name, and lifecycle are all immutable, because moving a live service
// across namespaces would strand its backing services, its tenant store, and
// every OpenBao path scoped by the old namespace.
func TestValidateUpdate_RejectsServiceNamespaceChanges(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ControlPlane)
		wantSub string
	}{
		{
			name:    "removing the assignment",
			mutate:  func(cp *ControlPlane) { cp.Spec.Services.Keystone.Namespace = nil },
			wantSub: "spec.services.keystone.namespace",
		},
		{
			name:    "renaming the namespace",
			mutate:  func(cp *ControlPlane) { cp.Spec.Services.Keystone.Namespace.Name = "identity-2" },
			wantSub: "spec.services.keystone.namespace.name",
		},
		{
			name: "flipping the lifecycle",
			mutate: func(cp *ControlPlane) {
				cp.Spec.Services.Horizon.Namespace.Lifecycle = ServiceNamespaceLifecycleManaged
			},
			wantSub: "spec.services.horizon.namespace.lifecycle",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &ControlPlaneWebhook{}
			oldCP := namespacedControlPlane()
			newCP := oldCP.DeepCopy()
			tc.mutate(newCP)

			_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.wantSub))
			g.Expect(err.Error()).To(ContainSubstring("the namespace a service is placed in is immutable"))
		})
	}

	t.Run("adding an assignment to a live ControlPlane", func(t *testing.T) {
		g := NewGomegaWithT(t)
		w := &ControlPlaneWebhook{}
		oldCP := validControlPlane()
		oldCP.Namespace = "openstack"
		newCP := oldCP.DeepCopy()
		newCP.Spec.Services.Keystone.Namespace = &ServiceNamespaceSpec{
			Name: "identity", Lifecycle: ServiceNamespaceLifecycleManaged,
		}

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("the namespace a service is placed in is immutable"))
	})

	t.Run("an unchanged assignment is not rejected", func(t *testing.T) {
		g := NewGomegaWithT(t)
		w := &ControlPlaneWebhook{}
		oldCP := namespacedControlPlane()
		newCP := oldCP.DeepCopy()
		newCP.Spec.OpenStackRelease = "2026.1"

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).NotTo(HaveOccurred())
	})
}

// --- services.glance validation (issue #672) ---

// validGlanceSpec is the minimal admissible glance block: a single S3 backend
// promoted to the default store. Tests mutate one aspect of it to exercise one
// rule.
func validGlanceSpec() *ServiceGlanceSpec {
	return &ServiceGlanceSpec{
		Backends: []GlanceBackendEntry{{
			Name:      "primary",
			Type:      "S3",
			IsDefault: true,
			S3: &GlanceBackendS3Spec{
				Endpoint:             "https://s3.example.com",
				Bucket:               "images",
				CredentialsSecretRef: SecretNameRef{Name: "glance-s3-creds"},
			},
		}},
	}
}

// The inline service-account names the three per-service fixture helpers below
// carry. See glanceControlPlane for why none of them is the service's own.
// glanceControlPlane returns a managed ControlPlane with a minimal valid glance
// block. The shared infrastructure stays brownfield (the validControlPlane
// baseline).
func glanceControlPlane() *ControlPlane {
	cp := validControlPlane()
	cp.Name = "cp"
	cp.Spec.Services.Glance = validGlanceSpec()
	return cp
}

// TestDefault_GlanceServiceNamespaceLifecycle verifies a declared glance
// namespace assignment takes the Managed lifecycle default, exactly as the
// keystone/horizon ones do.
func TestDefault_GlanceServiceNamespaceLifecycle(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Glance = validGlanceSpec()
	cp.Spec.Services.Glance.Namespace = &ServiceNamespaceSpec{Name: "images"}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())
	g.Expect(cp.Spec.Services.Glance.Namespace.Lifecycle).To(Equal(ServiceNamespaceLifecycleManaged))
}

// TestDefault_GlanceDedicatedBackingServicesLeaves verifies a declared glance
// dedicated block takes the same leaf defaults as the shared one, with a managed
// clusterRef name DERIVED from the ControlPlane and credentialsMode materialized
// to Static (a dedicated managed database cannot draw engine-issued credentials).
func TestDefault_GlanceDedicatedBackingServicesLeaves(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Name = "prod"
	cp.Spec.Services.Glance = validGlanceSpec()
	cp.Spec.Services.Glance.DedicatedBackingServices = &GlanceDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{},
		Cache:    &commonv1.CacheSpec{},
	}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	db := cp.Spec.Services.Glance.DedicatedBackingServices.Database
	g.Expect(db.ClusterRef).NotTo(BeNil())
	g.Expect(db.ClusterRef.Name).To(Equal("prod" + DedicatedGlanceDatabaseClusterRefSuffix))
	g.Expect(db.Database).To(Equal(DefaultDatabaseName))
	g.Expect(db.SecretRef.Name).To(Equal(DefaultDatabaseSecretName))
	g.Expect(db.CredentialsMode).To(Equal(commonv1.CredentialsModeStatic),
		"a dedicated managed database is Static-only: no per-instance OpenBao engine role exists")

	cache := cp.Spec.Services.Glance.DedicatedBackingServices.Cache
	g.Expect(cache.ClusterRef).NotTo(BeNil())
	g.Expect(cache.ClusterRef.Name).To(Equal("prod" + DedicatedGlanceCacheClusterRefSuffix))
	g.Expect(cache.Backend).To(Equal(DefaultCacheBackend))

	// Idempotent on the dedicated leaves too.
	before := cp.DeepCopy()
	g.Expect(w.Default(context.Background(), cp)).To(Succeed())
	g.Expect(cp.Spec.Services).To(Equal(before.Spec.Services))
}

// TestDefault_GlanceBrownfieldDedicatedNotCoercedIntoManaged mirrors the keystone
// case: an explicit brownfield endpoint must never grow a managed clusterRef (nor
// the Static mode, which is only materialized for a MANAGED dedicated database).
func TestDefault_GlanceBrownfieldDedicatedNotCoercedIntoManaged(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Name = "cp"
	cp.Spec.Services.Glance = validGlanceSpec()
	cp.Spec.Services.Glance.DedicatedBackingServices = &GlanceDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{Host: "glance-db.example.com", Port: 3306},
	}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	db := cp.Spec.Services.Glance.DedicatedBackingServices.Database
	g.Expect(db.ClusterRef).To(BeNil(), "a brownfield dedicated database must not grow a managed clusterRef")
	g.Expect(db.CredentialsMode).To(BeEmpty(), "Static is only materialized for a MANAGED dedicated database")
}

// TestValidateCreate_AcceptsGlanceControlPlane pins the admissible baseline.
func TestValidateCreate_AcceptsGlanceControlPlane(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	_, err := w.ValidateCreate(context.Background(), glanceControlPlane())
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_RejectsGlanceBackendsEmpty mirrors the MinItems floor: a
// glance block with no backend projects no image store.
func TestValidateCreate_RejectsGlanceBackendsEmpty(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.Backends = nil

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.glance.backends"))
	g.Expect(err.Error()).To(ContainSubstring("at least one backend"))
}

// TestValidateCreate_RejectsGlanceTwoDefaults mirrors the single-default CEL rule:
// two default stores leave the Glance default_backend ambiguous.
func TestValidateCreate_RejectsGlanceTwoDefaults(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.Backends = append(cp.Spec.Services.Glance.Backends, GlanceBackendEntry{
		Name: "secondary", Type: "S3", IsDefault: true,
		S3: &GlanceBackendS3Spec{
			Endpoint: "https://s3-2.example.com", Bucket: "images2",
			CredentialsSecretRef: SecretNameRef{Name: "glance-s3-creds-2"},
		},
	})

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("exactly one backends entry must set isDefault"))
}

// TestValidateCreate_RejectsGlanceZeroDefaults is the other half of the
// single-default rule: no default store at all is rejected too.
func TestValidateCreate_RejectsGlanceZeroDefaults(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.Backends[0].IsDefault = false

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("exactly one backends entry must set isDefault"))
}

// TestValidateCreate_RejectsGlanceBackendUnionViolation mirrors the type/s3 union
// CEL rule: a backend of type S3 must carry the s3 block.
func TestValidateCreate_RejectsGlanceBackendUnionViolation(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.Backends[0].S3 = nil

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("the s3 block must be set exactly when type is S3"))
}

// TestValidateCreate_RejectsGlanceBadEndpointURL covers the defense-in-depth
// http(s) shape gate on the S3 endpoint (beyond the coarse ^https?:// pattern).
func TestValidateCreate_RejectsGlanceBadEndpointURL(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.Backends[0].S3.Endpoint = "not-a-url"

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.glance.backends[0].s3.endpoint"))
}

// TestValidateCreate_RejectsGlanceMissingCredentialsSecretRefName covers the
// non-empty credentialsSecretRef.name requirement.
func TestValidateCreate_RejectsGlanceMissingCredentialsSecretRefName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.Backends[0].S3.CredentialsSecretRef.Name = ""

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.glance.backends[0].s3.credentialsSecretRef.name"))
}

// TestValidateCreate_RejectsGlanceImportFilteringConflicts mirrors the three
// mutual-exclusivity CEL rules on services.glance.importFiltering: glance
// evaluates a deny-list only while the matching allow-list is empty, so a CR
// setting both would silently lose the deny-list half of the policy.
func TestValidateCreate_RejectsGlanceImportFilteringConflicts(t *testing.T) {
	w := &ControlPlaneWebhook{}
	tests := []struct {
		name       string
		filtering  glancev1alpha1.ImportFilteringSpec
		wantSubstr string
	}{
		{"schemes", glancev1alpha1.ImportFilteringSpec{
			AllowedSchemes:    []string{"https"},
			DisallowedSchemes: []string{"http"},
		}, "allowedSchemes and disallowedSchemes are mutually exclusive"},
		{"hosts", glancev1alpha1.ImportFilteringSpec{
			AllowedHosts:    []string{"mirror.example.com"},
			DisallowedHosts: []string{"169.254.169.254"},
		}, "allowedHosts and disallowedHosts are mutually exclusive"},
		{"ports", glancev1alpha1.ImportFilteringSpec{
			AllowedPorts:    []int32{443},
			DisallowedPorts: []int32{80},
		}, "allowedPorts and disallowedPorts are mutually exclusive"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := glanceControlPlane()
			cp.Spec.Services.Glance.ImportFiltering = &tc.filtering

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.wantSubstr))
			g.Expect(err.Error()).To(ContainSubstring("glance ignores the deny-list when the allow-list is non-empty"))
			g.Expect(err.Error()).To(ContainSubstring("services.glance.importFiltering"))
		})
	}
}

// TestValidateCreate_RejectsGlanceImportFilteringBounds mirrors the per-item
// markers as defense in depth: a scheme outside the http/https enum, and a port
// on either side of the TCP range.
func TestValidateCreate_RejectsGlanceImportFilteringBounds(t *testing.T) {
	w := &ControlPlaneWebhook{}
	tests := []struct {
		name       string
		filtering  glancev1alpha1.ImportFilteringSpec
		wantSubstr string
	}{
		{"scheme not in enum", glancev1alpha1.ImportFilteringSpec{
			AllowedSchemes: []string{"ftp"},
		}, "services.glance.importFiltering.allowedSchemes[0]"},
		{"port zero", glancev1alpha1.ImportFilteringSpec{
			AllowedPorts: []int32{0},
		}, "services.glance.importFiltering.allowedPorts[0]"},
		{"port above range", glancev1alpha1.ImportFilteringSpec{
			AllowedPorts: []int32{70000},
		}, "services.glance.importFiltering.allowedPorts[0]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := glanceControlPlane()
			cp.Spec.Services.Glance.ImportFiltering = &tc.filtering

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.wantSubstr))
		})
	}
}

// TestValidateCreate_AcceptsGlanceImportFilteringLoosening pins the accepting
// side: widening the operator's HTTPS-on-443 default to an http mirror is a
// legitimate deployment choice, so allow-lists alone must be admitted.
func TestValidateCreate_AcceptsGlanceImportFilteringLoosening(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.ImportFiltering = &glancev1alpha1.ImportFilteringSpec{
		AllowedSchemes: []string{"http", "https"},
		AllowedHosts:   []string{"mirror.example.com"},
		AllowedPorts:   []int32{80, 443},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_WarnsGlanceImportFilteringPosture pins that the two
// admissible-but-misleading filter shapes reach the ControlPlane author, not
// only the projected Glance child: widening the scheme/port pin removes the
// primary web-download control, and a deny-list whose sibling allow-list still
// resolves to the operator default is never evaluated by glance.
func TestValidateCreate_WarnsGlanceImportFilteringPosture(t *testing.T) {
	tests := []struct {
		name      string
		filtering *glancev1alpha1.ImportFilteringSpec
		wantSubs  []string
		wantNone  bool
	}{
		{name: "unset block is silent", wantNone: true},
		{
			name: "http mirror loosening warns",
			filtering: &glancev1alpha1.ImportFilteringSpec{
				AllowedSchemes: []string{"http", "https"},
				AllowedPorts:   []int32{80, 443},
			},
			wantSubs: []string{
				"spec.services.glance.importFiltering.allowedSchemes is set to [http https]",
				"spec.services.glance.importFiltering.allowedPorts is set to [80 443]",
				"spec.networkPolicy.additionalEgress",
			},
		},
		{
			name:      "deny-only ports warn as inert",
			filtering: &glancev1alpha1.ImportFilteringSpec{DisallowedPorts: []int32{80}},
			wantSubs: []string{
				"spec.services.glance.importFiltering.disallowedPorts is set while " +
					"spec.services.glance.importFiltering.allowedPorts is unset",
				"this list is inert",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &ControlPlaneWebhook{}
			cp := glanceControlPlane()
			cp.Spec.Services.Glance.ImportFiltering = tc.filtering

			warnings, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).NotTo(HaveOccurred())

			joined := strings.Join(warnings, "\n")
			if tc.wantNone {
				g.Expect(joined).NotTo(ContainSubstring("importFiltering"))
				return
			}
			for _, sub := range tc.wantSubs {
				g.Expect(joined).To(ContainSubstring(sub))
			}
		})
	}
}

// TestValidateCreate_RejectsGlanceStagingSizeLimit mirrors the 1Mi floor on
// services.glance.staging.sizeLimit. The value is a resource.Quantity, which
// renders as x-kubernetes-int-or-string and carries no Minimum marker, so the
// glance module's exported validator is the only gate an unusable scratch bound
// ever meets on this CR — including the `100m`-for-`100Mi` suffix typo, which is
// positive and schema-legal.
func TestValidateCreate_RejectsGlanceStagingSizeLimit(t *testing.T) {
	w := &ControlPlaneWebhook{}
	tests := []struct {
		name      string
		sizeLimit string
	}{
		{"zero", "0"},
		{"negative", "-1Gi"},
		{"milli suffix typo", "100m"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := glanceControlPlane()
			cp.Spec.Services.Glance.Staging = &glancev1alpha1.StagingSpec{
				SizeLimit: ptr.To(resource.MustParse(tc.sizeLimit)),
			}

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("services.glance.staging.sizeLimit"))
			g.Expect(err.Error()).To(ContainSubstring("must be at least 1Mi"))
		})
	}
}

// TestValidateCreate_GlanceStagingUnbounded covers the other half of the shared
// validator on this CR: the deliberate opt-out back to unbounded scratch volumes
// is admitted on its own, and rejected when paired with a sizeLimit that the
// opt-out leaves nothing to bound.
func TestValidateCreate_GlanceStagingUnbounded(t *testing.T) {
	w := &ControlPlaneWebhook{}

	t.Run("alone accepted", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := glanceControlPlane()
		cp.Spec.Services.Glance.Staging = &glancev1alpha1.StagingSpec{Unbounded: true}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
	})

	t.Run("with sizeLimit rejected", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := glanceControlPlane()
		cp.Spec.Services.Glance.Staging = &glancev1alpha1.StagingSpec{
			Unbounded: true,
			SizeLimit: ptr.To(resource.MustParse("40Gi")),
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("services.glance.staging.unbounded"))
		g.Expect(err.Error()).To(ContainSubstring("mutually exclusive"))
	})
}

// TestValidateCreate_RejectsGlanceImageCacheSizeLimit mirrors the 1Mi floor on
// services.glance.imageCache.sizeLimit, the staging floor's twin. The value is a
// resource.Quantity, which renders as x-kubernetes-int-or-string and carries no
// Minimum marker, so the glance module's exported validator is the only gate an
// unusable cache budget ever meets on this CR — including the `100m`-for-`100Mi`
// suffix typo, which is positive and schema-legal.
func TestValidateCreate_RejectsGlanceImageCacheSizeLimit(t *testing.T) {
	w := &ControlPlaneWebhook{}
	tests := []struct {
		name      string
		sizeLimit string
	}{
		{"zero", "0"},
		{"negative", "-1Gi"},
		{"milli suffix typo", "100m"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := glanceControlPlane()
			cp.Spec.Services.Glance.ImageCache = &glancev1alpha1.ImageCacheSpec{
				SizeLimit: ptr.To(resource.MustParse(tc.sizeLimit)),
			}

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("services.glance.imageCache.sizeLimit"))
			g.Expect(err.Error()).To(ContainSubstring("must be at least 1Mi"))
		})
	}
}

// TestValidateCreate_RejectsGlanceImageCacheMaintenanceInterval covers the other
// floor the shared validator carries: a metav1.Duration renders as a plain
// string, so nothing in the schema stops a sub-minute maintenance loop that would
// spend the pod's local-disk bandwidth walking the cache instead of serving the
// downloads it exists to accelerate.
func TestValidateCreate_RejectsGlanceImageCacheMaintenanceInterval(t *testing.T) {
	w := &ControlPlaneWebhook{}
	tests := []struct {
		name     string
		interval time.Duration
	}{
		{"just under the floor", 59 * time.Second},
		{"zero", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := glanceControlPlane()
			cp.Spec.Services.Glance.ImageCache = &glancev1alpha1.ImageCacheSpec{
				MaintenanceInterval: &metav1.Duration{Duration: tc.interval},
			}

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("services.glance.imageCache.maintenanceInterval"))
			// The rendered detail is "must be at least 1m0s"; match its stable prefix.
			g.Expect(err.Error()).To(ContainSubstring("must be at least 1m"))
		})
	}
}

// TestValidateCreate_AcceptsGlanceImageCache pins the positive half: a block
// clearing both floors is admitted, and so is an empty one, which is how a
// ControlPlane asks for the cache with the glance operator's own defaults.
func TestValidateCreate_AcceptsGlanceImageCache(t *testing.T) {
	w := &ControlPlaneWebhook{}
	tests := []struct {
		name  string
		cache *glancev1alpha1.ImageCacheSpec
	}{
		{
			name:  "empty block takes the operator defaults",
			cache: &glancev1alpha1.ImageCacheSpec{},
		},
		{
			name: "both floors cleared",
			cache: &glancev1alpha1.ImageCacheSpec{
				SizeLimit:           ptr.To(resource.MustParse("20Gi")),
				MaintenanceInterval: &metav1.Duration{Duration: time.Minute},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := glanceControlPlane()
			cp.Spec.Services.Glance.ImageCache = tc.cache

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).NotTo(HaveOccurred())
		})
	}
}

// TestValidateCreate_RejectsGlanceImportPluginsInjectProperty mirrors the rules
// on the injected property names, the importFiltering bounds' counterpart one
// field over. A map key has no CRD marker at all, so the glance module's exported
// validator is the only gate here: a colon in the name is split off by the oslo
// Dict parser, which injects a property nobody wrote.
func TestValidateCreate_RejectsGlanceImportPluginsInjectProperty(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.ImportPlugins = &glancev1alpha1.ImportPluginsSpec{
		InjectMetadata: &glancev1alpha1.ImportInjectMetadataSpec{
			Properties: map[string]string{"hw:disk_bus": "scsi"},
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.glance.importPlugins.injectMetadata.properties"))
	g.Expect(err.Error()).To(ContainSubstring("splits each pair on the first colon"))
}

// TestValidateCreate_AcceptsGlanceImportPlugins pins the positive half: all three
// plugins selected at once, with a format inside the enum and an explicitly empty
// ignoreUserRoles, is a legitimate platform choice and must be admitted. The
// staging bound is set alongside because the decompression plugin requires it —
// see TestValidateCreate_RejectsGlanceDecompressionWithoutStagingBound.
func TestValidateCreate_AcceptsGlanceImportPlugins(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.Staging = &glancev1alpha1.StagingSpec{
		SizeLimit: ptr.To(resource.MustParse("40Gi")),
	}
	cp.Spec.Services.Glance.ImportPlugins = &glancev1alpha1.ImportPluginsSpec{
		Decompression: &glancev1alpha1.ImportDecompressionSpec{},
		Conversion:    &glancev1alpha1.ImportConversionSpec{OutputFormat: "raw"},
		InjectMetadata: &glancev1alpha1.ImportInjectMetadataSpec{
			Properties:      map[string]string{"hw_disk_bus": "scsi"},
			IgnoreUserRoles: []string{},
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_RejectsGlanceDecompressionWithoutStagingBound covers the
// cross-field rule on the ControlPlane copy of the two blocks. Both are projected
// onto the child Glance untouched, so admitting the pair here would only move the
// rejection to a reconcile the ControlPlane cannot report on.
func TestValidateCreate_RejectsGlanceDecompressionWithoutStagingBound(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.ImportPlugins = &glancev1alpha1.ImportPluginsSpec{
		Decompression: &glancev1alpha1.ImportDecompressionSpec{},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.glance.staging.sizeLimit"))
	g.Expect(err.Error()).To(ContainSubstring("importPlugins.decompression is enabled"))
}

// TestValidateCreate_GlancePublicEndpointMustBeURL covers the defense-in-depth
// URL parse behind the coarse ^https?:// pattern. The value is advertised
// verbatim as the public image catalog Endpoint and is projected into no child
// CR, so nothing downstream re-checks it: "https://" is schema-legal and would
// register a hostless URL no client can resolve.
func TestValidateCreate_GlancePublicEndpointMustBeURL(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for name, endpoint := range map[string]string{
		"missing host": "https://",
		"wrong scheme": "ftp://glance.example.com",
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := glanceControlPlane()
			cp.Spec.Services.Glance.PublicEndpoint = endpoint

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("services.glance.publicEndpoint"))
		})
	}
}

// TestValidateCreate_GlancePublicEndpointMustBeABareOrigin covers the shapes the
// ^https?:// Pattern marker lets through and validateHTTPURL happily parses. The
// Glance API is served at the root and clients append the API path to the catalog
// endpoint, so "https://glance.example.com?utm=1" yields
// "https://glance.example.com?utm=1/v2/images" and 404s every image call.
//
// The gateway is deliberately left unset in each case: the rule holds on the
// gateway-less path too, which is where the scheme/host rules stop applying.
func TestValidateCreate_GlancePublicEndpointMustBeABareOrigin(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for name, endpoint := range map[string]string{
		"query":    "https://glance.example.com?utm=1",
		"fragment": "https://glance.example.com#top",
		"path":     "https://glance.example.com/image",
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := glanceControlPlane()
			cp.Spec.Services.Glance.PublicEndpoint = endpoint

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("services.glance.publicEndpoint"))
			g.Expect(err.Error()).To(ContainSubstring("must be a bare origin"))
		})
	}
}

// TestValidateCreate_GlancePublicEndpointMustAgreeWithGateway pins the two
// cross-field rules. An http endpoint behind a TLS-terminating listener ships the
// caller's scoped Keystone token and the image payload in cleartext on EVERY
// image call; a divergent host advertises a catalog URL the Gateway listener
// never routes, which fails client-side with nothing on the ControlPlane
// recording why.
func TestValidateCreate_GlancePublicEndpointMustAgreeWithGateway(t *testing.T) {
	w := &ControlPlaneWebhook{}
	gateway := func() *commonv1.GatewaySpec {
		return &commonv1.GatewaySpec{
			Hostname:  "glance.example.com",
			ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		}
	}

	t.Run("divergent host", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := glanceControlPlane()
		cp.Spec.Services.Glance.Gateway = gateway()
		cp.Spec.Services.Glance.PublicEndpoint = "https://images.example.com"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("services.glance.publicEndpoint"))
		g.Expect(err.Error()).To(ContainSubstring(`must equal services.glance.gateway.hostname "glance.example.com"`))
	})

	t.Run("http scheme behind a TLS-terminating gateway", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := glanceControlPlane()
		cp.Spec.Services.Glance.Gateway = gateway()
		cp.Spec.Services.Glance.PublicEndpoint = "http://glance.example.com"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("scheme must be https"))
	})

	t.Run("matching host with a non-default port", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := glanceControlPlane()
		cp.Spec.Services.Glance.Gateway = gateway()
		cp.Spec.Services.Glance.PublicEndpoint = "https://glance.example.com:8443"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred(),
			"Gateway API hostnames carry no port, so the port is the reason the override exists")
	})

	t.Run("trailing slash", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := glanceControlPlane()
		cp.Spec.Services.Glance.Gateway = gateway()
		cp.Spec.Services.Glance.PublicEndpoint = "https://glance.example.com/"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred(), "clients normalize the catalog endpoint before appending the API path")
	})
}

// TestValidateCreate_WarnsOnCleartextGlancePublicEndpoint covers the gateway-less
// image service, where an http endpoint is a legal (if unwise) development setup
// the CRD Pattern deliberately allows. Every authenticated image call sends a
// scoped Keystone token to that URL, so the downgrade must at least be surfaced.
func TestValidateCreate_WarnsOnCleartextGlancePublicEndpoint(t *testing.T) {
	w := &ControlPlaneWebhook{}

	t.Run("http warns", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := glanceControlPlane()
		cp.Spec.Services.Glance.PublicEndpoint = "http://glance.example.com"

		warnings, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).To(HaveLen(1))
		g.Expect(warnings[0]).To(ContainSubstring("scoped Keystone token"))
	})

	t.Run("https is silent", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := glanceControlPlane()
		cp.Spec.Services.Glance.PublicEndpoint = "https://glance.example.com"

		warnings, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).To(BeEmpty())
	})
}

// TestValidateCreate_RejectsOverlongGlanceBackendChildName pins the composed
// GlanceBackend child-name length guard the CRD schema cannot express.
func TestValidateCreate_RejectsOverlongGlanceBackendChildName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := glanceControlPlane()
	cp.Name = "cp"
	// len("cp") + len("-glance-") + len(name) must exceed the 253-byte cap.
	cp.Spec.Services.Glance.Backends[0].Name = strings.Repeat("a", 253)

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("child GlanceBackend CR name would be"))
}

// The projected Glance child is bounded far below the 253-byte object-name cap:
// the Glance CRD's own webhook caps metadata.name because its db-purge CronJob
// appends a suffix Kubernetes counts against the 52-character CronJob limit.
// Without this guard the ControlPlane admits, reconcileGlance then fails to apply
// the child on every pass, and metadata.name is immutable — recovery means
// recreating the whole control plane.
func TestValidateCreate_RejectsOverlongProjectedGlanceChildName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	maxCPName := glancev1alpha1.MaxGlanceNameLength - glanceChildNameOverhead

	atLimit := glanceControlPlane()
	atLimit.Name = strings.Repeat("c", maxCPName)
	_, err := w.ValidateCreate(context.Background(), atLimit)
	g.Expect(err).NotTo(HaveOccurred(),
		"a name whose projected Glance child still fits must be accepted")

	tooLong := glanceControlPlane()
	tooLong.Name = strings.Repeat("c", maxCPName+1)
	_, err = w.ValidateCreate(context.Background(), tooLong)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("projected Glance child CR name would be"))

	// Without services.glance no Glance child is projected, so the bound does not
	// apply — the ControlPlane keeps the full 253-byte budget.
	noGlance := validControlPlane()
	noGlance.Name = strings.Repeat("c", maxCPName+1)
	_, err = w.ValidateCreate(context.Background(), noGlance)
	g.Expect(err).NotTo(HaveOccurred())
}

// Enabling Glance on an existing over-long ControlPlane is the one update that
// can newly violate the bound, so it is rejected; every other update on a CR that
// already carried Glance — including the finalizer removal that completes its
// deletion — must still pass, because metadata.name is immutable and a rejection
// would wedge it in Terminating.
func TestValidateUpdate_ProjectedGlanceChildNameBoundIsNewlyEnabledOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	overlong := strings.Repeat("c", glancev1alpha1.MaxGlanceNameLength-glanceChildNameOverhead+1)

	withoutGlance := validControlPlane()
	withoutGlance.Name = overlong
	enabling := glanceControlPlane()
	enabling.Name = overlong

	_, err := w.ValidateUpdate(context.Background(), withoutGlance, enabling)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("projected Glance child CR name would be"))

	grandfathered := glanceControlPlane()
	grandfathered.Name = overlong
	grandfathered.Finalizers = []string{"c5c3.io/finalizer"}
	deleting := grandfathered.DeepCopy()
	deleting.Finalizers = nil

	_, err = w.ValidateUpdate(context.Background(), grandfathered, deleting)
	g.Expect(err).NotTo(HaveOccurred(),
		"an over-long grandfathered ControlPlane must stay updatable, or its deletion never completes")
}

// TestValidateCreate_RejectsGlanceInExternalMode verifies the webhook cross-field
// forbid, mirroring services.horizon: no Keystone workload is deployed.
func TestValidateCreate_RejectsGlanceInExternalMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := externalControlPlane()
	cp.Spec.Services.Glance = validGlanceSpec()
	// Mirror the admission sequence: the mutating webhook runs before the
	// validating one.
	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.glance"))
	g.Expect(err.Error()).To(ContainSubstring("forbidden when services.keystone.mode is External"))
}

// TestValidateCreate_AcceptsGlanceInDedicatedNamespace pins the accepting side:
// a namespace no other ControlPlane claims admits the Glance placement.
func TestValidateCreate_AcceptsGlanceInDedicatedNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := glanceControlPlane()
	cp.Namespace = "openstack"
	cp.Spec.Services.Glance.Namespace = &ServiceNamespaceSpec{Name: "images", Lifecycle: ServiceNamespaceLifecycleManaged}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_RejectsGlanceNamespaceClaimedByOtherControlPlane mirrors the
// tenant-key claim rule for the glance service namespace.
func TestValidateCreate_RejectsGlanceNamespaceClaimedByOtherControlPlane(t *testing.T) {
	g := NewGomegaWithT(t)
	incumbent := validControlPlane()
	incumbent.Name = "other"
	incumbent.Namespace = "images"
	c := fake.NewClientBuilder().WithScheme(webhookScheme(t)).WithObjects(incumbent).Build()
	w := &ControlPlaneWebhook{Client: c}

	cp := glanceControlPlane()
	cp.Namespace = "openstack"
	cp.Spec.Services.Glance.Namespace = &ServiceNamespaceSpec{Name: "images", Lifecycle: ServiceNamespaceLifecycleManaged}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.glance.namespace.name"))
	g.Expect(err.Error()).To(ContainSubstring(`already occupied by ControlPlane "other"`))
}

// TestValidateUpdate_RejectsGlanceNamespaceChange pins the create-only freeze on
// the glance namespace assignment.
func TestValidateUpdate_RejectsGlanceNamespaceChange(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := glanceControlPlane()
	oldCP.Namespace = "openstack"
	oldCP.Spec.Services.Glance.Namespace = &ServiceNamespaceSpec{Name: "images", Lifecycle: ServiceNamespaceLifecycleManaged}
	newCP := oldCP.DeepCopy()
	newCP.Spec.Services.Glance.Namespace.Name = "images-2"

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.services.glance.namespace.name"))
	g.Expect(err.Error()).To(ContainSubstring("the namespace a service is placed in is immutable"))
}

// TestValidateCreate_RejectsGlanceDedicatedDatabaseDynamic confirms glance's
// dedicated database goes through the shared dedicated-backing validation: the
// Dynamic-credentials rule (no per-instance OpenBao engine role) applies to it
// exactly as to keystone's.
func TestValidateCreate_RejectsGlanceDedicatedDatabaseDynamic(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.DedicatedBackingServices = &GlanceDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{
			ClusterRef:      &corev1.LocalObjectReference{Name: "cp-glance-db"},
			CredentialsMode: commonv1.CredentialsModeDynamic,
			Database:        "glance",
			SecretRef:       commonv1.SecretRefSpec{Name: "glance-db"},
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("credentialsMode Dynamic is not supported on a dedicated database"))
	g.Expect(err.Error()).To(ContainSubstring("glance.dedicatedBackingServices.database"))
}

// TestValidateCreate_RejectsGlanceDedicatedClusterRefCollision pins that a glance
// dedicated cache colliding with keystone's is rejected: the two projections
// would resolve to one Memcached child CR.
func TestValidateCreate_RejectsGlanceDedicatedClusterRefCollision(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := glanceControlPlane()
	cp.Spec.Services.Keystone.DedicatedBackingServices = &KeystoneDedicatedBackingServicesSpec{
		Cache: &commonv1.CacheSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "shared-cache"},
			Backend:    commonv1.DefaultCacheBackend,
		},
	}
	cp.Spec.Services.Glance.DedicatedBackingServices = &GlanceDedicatedBackingServicesSpec{
		Cache: &commonv1.CacheSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "shared-cache"},
			Backend:    commonv1.DefaultCacheBackend,
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("Duplicate value"))
	g.Expect(err.Error()).To(ContainSubstring("shared-cache"))
}

// TestValidateUpdate_RejectsGlanceDedicatedPresenceFlip pins the transition freeze
// on the glance dedicated block: a live service cannot be moved between shared and
// dedicated backing services.
func TestValidateUpdate_RejectsGlanceDedicatedPresenceFlip(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := glanceControlPlane()
	newCP := glanceControlPlane()
	newCP.Spec.Services.Glance.DedicatedBackingServices = &GlanceDedicatedBackingServicesSpec{
		Cache: &commonv1.CacheSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "cp-glance-cache"},
			Backend:    commonv1.DefaultCacheBackend,
		},
	}

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("switching a service between shared and dedicated backing services"))
	g.Expect(err.Error()).To(ContainSubstring("glance.dedicatedBackingServices"))
}

// --- services.placement ---

// placementControlPlane returns a managed ControlPlane with a minimal placement
// block, so the tests below start from an admissible baseline and vary only the
// field (or the INI content) under test. The shared infrastructure stays
// brownfield (the validControlPlane baseline).
func placementControlPlane() *ControlPlane {
	cp := validControlPlane()
	cp.Name = "cp"
	cp.Spec.Services.Placement = &ServicePlacementSpec{}
	return cp
}

// TestDefault_PlacementServiceNamespaceLifecycle verifies a declared placement
// namespace assignment takes the Managed lifecycle default, exactly as the
// keystone/horizon/glance ones do.
func TestDefault_PlacementServiceNamespaceLifecycle(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Placement = &ServicePlacementSpec{Namespace: &ServiceNamespaceSpec{Name: "placement"}}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())
	g.Expect(cp.Spec.Services.Placement.Namespace.Lifecycle).To(Equal(ServiceNamespaceLifecycleManaged))
}

// TestDefault_PlacementDedicatedBackingServicesLeaves verifies a declared
// placement dedicated block takes the same leaf defaults as the shared one, with
// a managed clusterRef name DERIVED from the ControlPlane and credentialsMode
// materialized to Static (a dedicated managed database cannot draw engine-issued
// credentials).
func TestDefault_PlacementDedicatedBackingServicesLeaves(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Name = "prod"
	cp.Spec.Services.Placement = &ServicePlacementSpec{
		DedicatedBackingServices: &PlacementDedicatedBackingServicesSpec{
			Database: &commonv1.DatabaseSpec{},
			Cache:    &commonv1.CacheSpec{},
		},
	}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	db := cp.Spec.Services.Placement.DedicatedBackingServices.Database
	g.Expect(db.ClusterRef).NotTo(BeNil())
	g.Expect(db.ClusterRef.Name).To(Equal("prod" + DedicatedPlacementDatabaseClusterRefSuffix))
	g.Expect(db.Database).To(Equal(DefaultDatabaseName))
	g.Expect(db.SecretRef.Name).To(Equal(DefaultDatabaseSecretName))
	g.Expect(db.CredentialsMode).To(Equal(commonv1.CredentialsModeStatic),
		"a dedicated managed database is Static-only: no per-instance OpenBao engine role exists")

	cache := cp.Spec.Services.Placement.DedicatedBackingServices.Cache
	g.Expect(cache.ClusterRef).NotTo(BeNil())
	g.Expect(cache.ClusterRef.Name).To(Equal("prod" + DedicatedPlacementCacheClusterRefSuffix))
	g.Expect(cache.Backend).To(Equal(DefaultCacheBackend))

	// Idempotent on the dedicated leaves too.
	before := cp.DeepCopy()
	g.Expect(w.Default(context.Background(), cp)).To(Succeed())
	g.Expect(cp.Spec.Services).To(Equal(before.Spec.Services))
}

// TestValidateCreate_AcceptsPlacementControlPlane pins the admissible baseline.
func TestValidateCreate_AcceptsPlacementControlPlane(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	_, err := w.ValidateCreate(context.Background(), placementControlPlane())
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_AcceptsPlacementInDedicatedNamespace pins the accepting
// side: a namespace no other ControlPlane claims admits the Placement placement.
func TestValidateCreate_AcceptsPlacementInDedicatedNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := placementControlPlane()
	cp.Namespace = "openstack"
	cp.Spec.Services.Placement.Namespace = &ServiceNamespaceSpec{
		Name: "placement", Lifecycle: ServiceNamespaceLifecycleManaged,
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_PlacementPublicEndpointMustBeURL covers the
// defense-in-depth URL parse behind the coarse ^https?:// pattern. The value is
// advertised verbatim as the public placement catalog Endpoint and is projected
// into no child CR, so nothing downstream re-checks it: "https://" is
// schema-legal and would register a hostless URL no client can resolve.
func TestValidateCreate_PlacementPublicEndpointMustBeURL(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for name, endpoint := range map[string]string{
		"missing host": "https://",
		"wrong scheme": "ftp://placement.example.com",
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := placementControlPlane()
			cp.Spec.Services.Placement.PublicEndpoint = endpoint

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("services.placement.publicEndpoint"))
		})
	}
}

// TestValidateCreate_PlacementPublicEndpointMustBeABareOrigin covers the shapes
// the ^https?:// Pattern marker lets through and validateHTTPURL happily parses.
// The Placement API is served at the root and clients append the API path to the
// catalog endpoint, so "https://placement.example.com?utm=1" yields
// "https://placement.example.com?utm=1/resource_providers" and 404s every
// allocation call.
//
// The gateway is deliberately left unset in each case: the rule holds on the
// gateway-less path too, which is where the scheme/host rules stop applying.
func TestValidateCreate_PlacementPublicEndpointMustBeABareOrigin(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for name, endpoint := range map[string]string{
		"query":    "https://placement.example.com?utm=1",
		"fragment": "https://placement.example.com#top",
		"path":     "https://placement.example.com/placement",
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := placementControlPlane()
			cp.Spec.Services.Placement.PublicEndpoint = endpoint

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("services.placement.publicEndpoint"))
			g.Expect(err.Error()).To(ContainSubstring("must be a bare origin"))
		})
	}
}

// TestValidateCreate_PlacementPublicEndpointMustAgreeWithGateway pins the two
// cross-field rules. An http endpoint behind a TLS-terminating listener ships the
// caller's scoped Keystone token in cleartext on every allocation call; a
// divergent host advertises a catalog URL the Gateway listener never routes,
// which fails client-side with nothing on the ControlPlane recording why.
func TestValidateCreate_PlacementPublicEndpointMustAgreeWithGateway(t *testing.T) {
	w := &ControlPlaneWebhook{}
	gateway := func() *commonv1.GatewaySpec {
		return &commonv1.GatewaySpec{
			Hostname:  "placement.example.com",
			ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		}
	}

	t.Run("divergent host", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := placementControlPlane()
		cp.Spec.Services.Placement.Gateway = gateway()
		cp.Spec.Services.Placement.PublicEndpoint = "https://allocations.example.com"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("services.placement.publicEndpoint"))
		g.Expect(err.Error()).To(ContainSubstring(
			`must equal services.placement.gateway.hostname "placement.example.com"`,
		))
	})

	t.Run("http scheme behind a TLS-terminating gateway", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := placementControlPlane()
		cp.Spec.Services.Placement.Gateway = gateway()
		cp.Spec.Services.Placement.PublicEndpoint = "http://placement.example.com"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("scheme must be https"))
	})

	t.Run("matching host with a non-default port", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := placementControlPlane()
		cp.Spec.Services.Placement.Gateway = gateway()
		cp.Spec.Services.Placement.PublicEndpoint = "https://placement.example.com:8443"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred(),
			"Gateway API hostnames carry no port, so the port is the reason the override exists")
	})

	t.Run("trailing slash", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := placementControlPlane()
		cp.Spec.Services.Placement.Gateway = gateway()
		cp.Spec.Services.Placement.PublicEndpoint = "https://placement.example.com/"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred(), "clients normalize the catalog endpoint before appending the API path")
	})

	t.Run("wildcard gateway hostname", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := placementControlPlane()
		cp.Spec.Services.Placement.Gateway = gateway()
		cp.Spec.Services.Placement.Gateway.Hostname = "*.example.com"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("services.placement.gateway.hostname"))
	})
}

// TestValidateCreate_WarnsOnCleartextPlacementPublicEndpoint covers the
// gateway-less placement service, where an http endpoint is a legal (if unwise)
// development setup the CRD Pattern deliberately allows. Every allocation call
// sends a scoped Keystone token to that URL, so the downgrade must at least be
// surfaced.
func TestValidateCreate_WarnsOnCleartextPlacementPublicEndpoint(t *testing.T) {
	w := &ControlPlaneWebhook{}

	t.Run("http warns", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := placementControlPlane()
		cp.Spec.Services.Placement.PublicEndpoint = "http://placement.example.com"

		warnings, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).To(HaveLen(1))
		g.Expect(warnings[0]).To(ContainSubstring("scoped Keystone token"))
	})

	t.Run("https is silent", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := placementControlPlane()
		cp.Spec.Services.Placement.PublicEndpoint = "https://placement.example.com"

		warnings, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).To(BeEmpty())
	})
}

// TestValidateCreate_RejectsPlacementImageTagDigestXOR pins the defense-in-depth
// mirror of the commonv1.ImageSpec XValidation rule for callers that bypass CRD
// schema admission.
func TestValidateCreate_RejectsPlacementImageTagDigestXOR(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for name, img := range map[string]*commonv1.ImageSpec{
		"neither tag nor digest": {Repository: "ghcr.io/c5c3/placement"},
		"both tag and digest": {
			Repository: "ghcr.io/c5c3/placement",
			Tag:        "2025.2",
			Digest:     "sha256:" + strings.Repeat("a", 64),
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := placementControlPlane()
			cp.Spec.Services.Placement.Image = img

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("services.placement.image"))
			g.Expect(err.Error()).To(ContainSubstring("exactly one of image.tag or image.digest must be set"))
		})
	}
}

// TestValidateCreate_RejectsPlacementInExternalMode verifies the webhook
// cross-field forbid, mirroring services.glance: no Keystone workload is
// deployed, so Placement has no identity to validate its tokens against.
func TestValidateCreate_RejectsPlacementInExternalMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := externalControlPlane()
	cp.Spec.Services.Placement = &ServicePlacementSpec{}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.placement"))
	g.Expect(err.Error()).To(ContainSubstring("forbidden when services.keystone.mode is External"))
}

// TestValidateCreate_RejectsPlacementNamespaceClaimedByOtherControlPlane mirrors
// the tenant-key claim rule for the placement service namespace.
func TestValidateCreate_RejectsPlacementNamespaceClaimedByOtherControlPlane(t *testing.T) {
	g := NewGomegaWithT(t)
	incumbent := validControlPlane()
	incumbent.Name = "other"
	incumbent.Namespace = "placement"
	c := fake.NewClientBuilder().WithScheme(webhookScheme(t)).WithObjects(incumbent).Build()
	w := &ControlPlaneWebhook{Client: c}

	cp := placementControlPlane()
	cp.Namespace = "openstack"
	cp.Spec.Services.Placement.Namespace = &ServiceNamespaceSpec{
		Name: "placement", Lifecycle: ServiceNamespaceLifecycleManaged,
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.placement.namespace.name"))
	g.Expect(err.Error()).To(ContainSubstring(`already occupied by ControlPlane "other"`))
}

// TestValidateUpdate_RejectsPlacementNamespaceChange pins the create-only freeze
// on the placement namespace assignment.
func TestValidateUpdate_RejectsPlacementNamespaceChange(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := placementControlPlane()
	oldCP.Namespace = "openstack"
	oldCP.Spec.Services.Placement.Namespace = &ServiceNamespaceSpec{
		Name: "placement", Lifecycle: ServiceNamespaceLifecycleManaged,
	}
	newCP := oldCP.DeepCopy()
	newCP.Spec.Services.Placement.Namespace.Name = "placement-2"

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.services.placement.namespace.name"))
	g.Expect(err.Error()).To(ContainSubstring("the namespace a service is placed in is immutable"))
}

// TestValidateCreate_RejectsPlacementDedicatedDatabaseDynamic confirms
// placement's dedicated database goes through the shared dedicated-backing
// validation: the Dynamic-credentials rule (no per-instance OpenBao engine role)
// applies to it exactly as to keystone's.
func TestValidateCreate_RejectsPlacementDedicatedDatabaseDynamic(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := placementControlPlane()
	cp.Spec.Services.Placement.DedicatedBackingServices = &PlacementDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{
			ClusterRef:      &corev1.LocalObjectReference{Name: "cp-placement-db"},
			CredentialsMode: commonv1.CredentialsModeDynamic,
			Database:        "placement",
			SecretRef:       commonv1.SecretRefSpec{Name: "placement-db"},
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("credentialsMode Dynamic is not supported on a dedicated database"))
	g.Expect(err.Error()).To(ContainSubstring("placement.dedicatedBackingServices.database"))
}

// TestValidateUpdate_RejectsPlacementDedicatedPresenceFlip pins the transition
// freeze on the placement dedicated block: a live service cannot be moved
// between shared and dedicated backing services.
func TestValidateUpdate_RejectsPlacementDedicatedPresenceFlip(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := placementControlPlane()
	newCP := placementControlPlane()
	newCP.Spec.Services.Placement.DedicatedBackingServices = &PlacementDedicatedBackingServicesSpec{
		Cache: &commonv1.CacheSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "cp-placement-cache"},
			Backend:    commonv1.DefaultCacheBackend,
		},
	}

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("switching a service between shared and dedicated backing services"))
	g.Expect(err.Error()).To(ContainSubstring("placement.dedicatedBackingServices"))
}

// TestValidateUpdate_AcceptsAddingAServiceWithDedicatedBackingServices pins the
// other side of the freeze: ADDING a service that was not declared before is that
// service's create, not a shared->dedicated switch. The accessors return nil for
// both states, so without the declared-before gate the update is rejected with an
// offer to remove and recreate the whole ControlPlane — destroying the databases
// of the services already running on it to onboard one more.
func TestValidateUpdate_AcceptsAddingAServiceWithDedicatedBackingServices(t *testing.T) {
	tests := []struct {
		name string
		add  func(cp *ControlPlane)
	}{
		{
			name: "placement",
			add: func(cp *ControlPlane) {
				pl := placementControlPlane()
				cp.Spec.Services.Placement = pl.Spec.Services.Placement
				cp.Spec.Services.Placement.DedicatedBackingServices = &PlacementDedicatedBackingServicesSpec{
					Database: &commonv1.DatabaseSpec{
						ClusterRef: &corev1.LocalObjectReference{Name: "cp-placement-db"},
						Database:   "placement",
						SecretRef:  commonv1.SecretRefSpec{Name: "placement-db"},
					},
				}
			},
		},
		{
			name: "glance",
			add: func(cp *ControlPlane) {
				gl := glanceControlPlane()
				cp.Spec.Services.Glance = gl.Spec.Services.Glance
				cp.Spec.Services.Glance.DedicatedBackingServices = &GlanceDedicatedBackingServicesSpec{
					Cache: &commonv1.CacheSpec{
						ClusterRef: &corev1.LocalObjectReference{Name: "cp-glance-cache"},
						Backend:    commonv1.DefaultCacheBackend,
					},
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &ControlPlaneWebhook{}
			oldCP := validControlPlane()
			oldCP.Name = "cp"
			newCP := oldCP.DeepCopy()
			tc.add(newCP)

			_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
			g.Expect(err).NotTo(HaveOccurred())
		})
	}
}

// TestValidateUpdate_AcceptsAddingPlacementInADedicatedNamespace is the namespace
// twin of the dedicated-backing carve-out: assigning a namespace to a service the
// ControlPlane did not declare before is that service's create, so there is no
// live service, no backing service, and no credential material stranded in an old
// namespace for the move freeze to protect.
func TestValidateUpdate_AcceptsAddingPlacementInADedicatedNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := validControlPlane()
	oldCP.Name = "cp"
	oldCP.Namespace = "openstack"

	pl := placementControlPlane()
	newCP := oldCP.DeepCopy()
	newCP.Spec.Services.Placement = pl.Spec.Services.Placement
	newCP.Spec.Services.Placement.Namespace = &ServiceNamespaceSpec{
		Name: "placement", Lifecycle: ServiceNamespaceLifecycleManaged,
	}

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateUpdate_RejectsClaimingANamespaceAnotherControlPlaneOccupies closes
// the UPDATE path into the tenant-isolation check. The declared-before carve-out
// admits an update that ADDS a service with a namespace assignment, so the
// create-only claim check no longer sees every claim: without the re-check two
// ControlPlanes could end up owning one namespace — the tenant key the OpenBao
// paths, the database-engine role, and the templated eso-tenant policy are all
// scoped by — and each would then park on the fixed-name tenant objects the other
// already owns and may not adopt.
func TestValidateUpdate_RejectsClaimingANamespaceAnotherControlPlaneOccupies(t *testing.T) {
	// The ControlPlane doing the update: no placement, no claim of its own.
	newcomer := func() *ControlPlane {
		cp := validControlPlane()
		cp.Name = "cp-b"
		cp.Namespace = "openstack-b"
		return cp
	}
	// The update under test: onboard placement into namespace ns.
	addPlacementIn := func(cp *ControlPlane, ns string) {
		pl := placementControlPlane()
		cp.Spec.Services.Placement = pl.Spec.Services.Placement
		cp.Spec.Services.Placement.Namespace = &ServiceNamespaceSpec{
			Name: ns, Lifecycle: ServiceNamespaceLifecycleExternal,
		}
	}

	t.Run("a namespace an incumbent already claims", func(t *testing.T) {
		g := NewGomegaWithT(t)
		incumbent := validControlPlane()
		incumbent.Name = "cp-a"
		incumbent.Namespace = "openstack-a"
		incumbent.Spec.Services.Keystone.Namespace = &ServiceNamespaceSpec{
			Name: "images", Lifecycle: ServiceNamespaceLifecycleManaged,
		}
		oldCP := newcomer()
		c := fake.NewClientBuilder().WithScheme(webhookScheme(t)).
			WithObjects(incumbent, oldCP.DeepCopy()).Build()
		w := &ControlPlaneWebhook{Client: c}

		newCP := oldCP.DeepCopy()
		addPlacementIn(newCP, "images")

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("services.placement.namespace.name"))
		g.Expect(err.Error()).To(ContainSubstring(`already occupied by ControlPlane "cp-a"`))
	})

	t.Run("an unclaimed namespace stays admitted", func(t *testing.T) {
		g := NewGomegaWithT(t)
		incumbent := validControlPlane()
		incumbent.Name = "cp-a"
		incumbent.Namespace = "openstack-a"
		oldCP := newcomer()
		c := fake.NewClientBuilder().WithScheme(webhookScheme(t)).
			WithObjects(incumbent, oldCP.DeepCopy()).Build()
		w := &ControlPlaneWebhook{Client: c}

		newCP := oldCP.DeepCopy()
		addPlacementIn(newCP, "placement")

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).NotTo(HaveOccurred())
	})
}

// TestValidateUpdate_RejectsReAddingADroppedService pins that the declared-before
// carve-out is a carve-out for a service's CREATE, not a two-update path around
// the transition freezes. A shared-backed service drops from spec cleanly and its
// projected child is preserved by default, so keying the gate on the old SPEC
// alone would admit exactly what the freeze forbids on the next update: the
// running child re-pointed at a freshly projected, empty database — or at another
// namespace — with the schema it was running on stranded and no way back, since
// every route out then hits the freeze. status.services still reports the dropped
// service, so the gate engages.
func TestValidateUpdate_RejectsReAddingADroppedService(t *testing.T) {
	// The revision left behind by dropping spec.services.placement from a
	// ControlPlane that ran Placement on the shared backing services.
	dropped := func() *ControlPlane {
		cp := validControlPlane()
		cp.Name = "cp"
		cp.Namespace = "openstack"
		cp.Status.Services = []ServiceStatus{
			{Name: "keystone", Ready: true},
			{Name: "placement", Ready: true},
		}
		return cp
	}
	readd := func(cp *ControlPlane) *ServicePlacementSpec {
		pl := placementControlPlane()
		cp.Spec.Services.Placement = pl.Spec.Services.Placement
		return cp.Spec.Services.Placement
	}

	t.Run("with dedicated backing services", func(t *testing.T) {
		g := NewGomegaWithT(t)
		w := &ControlPlaneWebhook{}
		oldCP := dropped()
		newCP := oldCP.DeepCopy()
		readd(newCP).DedicatedBackingServices = &PlacementDedicatedBackingServicesSpec{
			Database: &commonv1.DatabaseSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "cp-placement-db"},
				Database:   "placement",
				SecretRef:  commonv1.SecretRefSpec{Name: "placement-db"},
			},
		}

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("switching a service between shared and dedicated backing services"))
		g.Expect(err.Error()).To(ContainSubstring("placement.dedicatedBackingServices"))
	})

	t.Run("in another namespace", func(t *testing.T) {
		g := NewGomegaWithT(t)
		w := &ControlPlaneWebhook{}
		oldCP := dropped()
		newCP := oldCP.DeepCopy()
		readd(newCP).Namespace = &ServiceNamespaceSpec{
			Name: "placement", Lifecycle: ServiceNamespaceLifecycleManaged,
		}

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("the namespace a service is placed in is immutable"))
	})

	t.Run("on the shared backing services it stays admitted", func(t *testing.T) {
		g := NewGomegaWithT(t)
		w := &ControlPlaneWebhook{}
		oldCP := dropped()
		newCP := oldCP.DeepCopy()
		readd(newCP)

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).NotTo(HaveOccurred())
	})
}

// TestValidateUpdate_RejectsDroppingAPlacementNamespaceAssignment pins that the
// declared-before carve-out does not weaken the move freeze: a live Placement
// still cannot shed its namespace assignment. reconcilePlacement's preserve path
// relies on PlacementNamespace() still resolving to the namespace the generator
// it tears down actually lives in.
func TestValidateUpdate_RejectsDroppingAPlacementNamespaceAssignment(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := placementControlPlane()
	oldCP.Namespace = "openstack"
	oldCP.Spec.Services.Placement.Namespace = &ServiceNamespaceSpec{
		Name: "placement", Lifecycle: ServiceNamespaceLifecycleManaged,
	}
	newCP := oldCP.DeepCopy()
	newCP.Spec.Services.Placement.Namespace = nil

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("the namespace a service is placed in is immutable"))
}

// The projected Placement child is bounded far below the 253-byte object-name
// cap: the placement operator names its API Service after the CR, and a Service
// name is a DNS-1035 label Kubernetes caps at 63 characters. Without this guard
// the ControlPlane admits, the Placement CR is created, and the placement
// operator then fails to apply the Service on every pass — with metadata.name
// immutable, recovery means recreating the whole control plane.
func TestValidateCreate_RejectsOverlongProjectedPlacementChildName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	maxCPName := maxServiceNameBytes - placementChildNameOverhead

	atLimit := placementControlPlane()
	atLimit.Name = strings.Repeat("c", maxCPName)
	_, err := w.ValidateCreate(context.Background(), atLimit)
	g.Expect(err).NotTo(HaveOccurred(),
		"a name whose projected Placement child still fits must be accepted")

	tooLong := placementControlPlane()
	tooLong.Name = strings.Repeat("c", maxCPName+1)
	_, err = w.ValidateCreate(context.Background(), tooLong)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("projected Placement child CR name would be"))

	// Without services.placement no Placement child is projected, so the bound
	// does not apply — the ControlPlane keeps the full 253-byte budget.
	noPlacement := validControlPlane()
	noPlacement.Name = strings.Repeat("c", maxCPName+1)
	_, err = w.ValidateCreate(context.Background(), noPlacement)
	g.Expect(err).NotTo(HaveOccurred())
}

// Enabling Placement on an existing over-long ControlPlane is the one update that
// can newly violate the bound, so it is rejected; every other update on a CR that
// already carried Placement — including the finalizer removal that completes its
// deletion — must still pass, because metadata.name is immutable and a rejection
// would wedge it in Terminating.
func TestValidateUpdate_ProjectedPlacementChildNameBoundIsNewlyEnabledOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	overlong := strings.Repeat("c", maxServiceNameBytes-placementChildNameOverhead+1)

	withoutPlacement := validControlPlane()
	withoutPlacement.Name = overlong
	enabling := placementControlPlane()
	enabling.Name = overlong

	_, err := w.ValidateUpdate(context.Background(), withoutPlacement, enabling)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("projected Placement child CR name would be"))

	grandfathered := placementControlPlane()
	grandfathered.Name = overlong
	grandfathered.Finalizers = []string{"c5c3.io/finalizer"}
	deleting := grandfathered.DeepCopy()
	deleting.Finalizers = nil

	_, err = w.ValidateUpdate(context.Background(), grandfathered, deleting)
	g.Expect(err).NotTo(HaveOccurred(),
		"an over-long grandfathered ControlPlane must stay updatable, or its deletion never completes")
}

// --- per-service databaseCredentialsMode override (issue #683) ---

// TestValidateCreate_RejectsKeystoneCredentialsModeOverrideDynamicOnDedicated pins
// that a Dynamic override on the Keystone service is rejected when Keystone
// declares a dedicated database: the override retargets the shared database the
// service does not use, and a dedicated database is Static-only. CEL cannot catch
// it — the value passes the Enum, and no CEL rule spans the dedicated block.
func TestValidateCreate_RejectsKeystoneCredentialsModeOverrideDynamicOnDedicated(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Name = "cp"
	cp.Spec.Services.Keystone.DatabaseCredentialsMode = commonv1.CredentialsModeDynamic
	cp.Spec.Services.Keystone.DedicatedBackingServices = &KeystoneDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-db"},
			Database:   "keystone",
			SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db"},
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.keystone.databaseCredentialsMode"))
	g.Expect(err.Error()).To(ContainSubstring(
		"Dynamic is not supported as an override on a service with a dedicated database",
	))
}

// TestValidateCreate_RejectsGlanceCredentialsModeOverrideDynamicOnDedicated is the
// glance mirror of the keystone dedicated-database rejection above.
func TestValidateCreate_RejectsGlanceCredentialsModeOverrideDynamicOnDedicated(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.DatabaseCredentialsMode = commonv1.CredentialsModeDynamic
	cp.Spec.Services.Glance.DedicatedBackingServices = &GlanceDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "cp-glance-db"},
			Database:   "glance",
			SecretRef:  commonv1.SecretRefSpec{Name: "glance-db"},
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.glance.databaseCredentialsMode"))
	g.Expect(err.Error()).To(ContainSubstring(
		"Dynamic is not supported as an override on a service with a dedicated database",
	))
}

// TestValidateCreate_RejectsPlacementCredentialsModeOverrideDynamicOnDedicated is
// the placement mirror of the keystone dedicated-database rejection above.
func TestValidateCreate_RejectsPlacementCredentialsModeOverrideDynamicOnDedicated(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := placementControlPlane()
	cp.Spec.Services.Placement.DatabaseCredentialsMode = commonv1.CredentialsModeDynamic
	cp.Spec.Services.Placement.DedicatedBackingServices = &PlacementDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "cp-placement-db"},
			Database:   "placement",
			SecretRef:  commonv1.SecretRefSpec{Name: "placement-db"},
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.placement.databaseCredentialsMode"))
	g.Expect(err.Error()).To(ContainSubstring(
		"Dynamic is not supported as an override on a service with a dedicated database",
	))
}

// TestValidateCreate_RejectsCredentialsModeOverrideDynamicOnBrownfieldShared pins
// that a Dynamic override on a service using the SHARED database is rejected when
// that shared database is brownfield (host set, no clusterRef), mirroring the
// commonv1.DatabaseSpec Dynamic-requires-clusterRef contract one level up.
func TestValidateCreate_RejectsCredentialsModeOverrideDynamicOnBrownfieldShared(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane() // shared database is brownfield (host set, no clusterRef)
	cp.Spec.Services.Keystone.DatabaseCredentialsMode = commonv1.CredentialsModeDynamic

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.keystone.databaseCredentialsMode"))
	g.Expect(err.Error()).To(ContainSubstring(
		"Dynamic requires the shared database to be managed (clusterRef)",
	))
}

// TestValidateCreate_RejectsKeystoneCredentialsModeOverrideInExternalMode pins the
// webhook mirror of the CEL forbid rule: databaseCredentialsMode is forbidden on
// the Keystone service in External mode (even a Static value), because no managed
// database is provisioned. The webhook is the enforcement point a direct validate()
// call exercises (CEL runs only against a live apiserver).
func TestValidateCreate_RejectsKeystoneCredentialsModeOverrideInExternalMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := externalControlPlane()
	cp.Spec.Services.Keystone.DatabaseCredentialsMode = commonv1.CredentialsModeStatic

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.keystone.databaseCredentialsMode"))
	g.Expect(err.Error()).To(ContainSubstring("forbidden when services.keystone.mode is External"))
}

// TestValidateCreate_AcceptsStaticCredentialsModeOverrideOnDedicated pins that a
// Static override is always admitted, even on a service that declares a dedicated
// database (only Dynamic is rejected there).
func TestValidateCreate_AcceptsStaticCredentialsModeOverrideOnDedicated(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Name = "cp"
	cp.Spec.Services.Keystone.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
	cp.Spec.Services.Keystone.DedicatedBackingServices = &KeystoneDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-db"},
			Database:   "keystone",
			SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db"},
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred(),
		"a Static override is always admitted, even on a service with a dedicated database")
}

// TestValidateCreate_AcceptsCredentialsModeOverridesOnManagedShared pins that a
// Static or Dynamic override on a MANAGED shared database is admitted for both
// services, in every combination — the staged-migration shape the field exists for.
func TestValidateCreate_AcceptsCredentialsModeOverridesOnManagedShared(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	tests := []struct {
		name         string
		keystoneMode string
		glanceMode   string
	}{
		{"keystone-dynamic-glance-static", commonv1.CredentialsModeDynamic, commonv1.CredentialsModeStatic},
		{"keystone-static-glance-dynamic", commonv1.CredentialsModeStatic, commonv1.CredentialsModeDynamic},
		{"both-dynamic", commonv1.CredentialsModeDynamic, commonv1.CredentialsModeDynamic},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cp := managedControlPlane() // shared database is managed (clusterRef)
			cp.Name = "cp"
			cp.Spec.Services.Keystone.DatabaseCredentialsMode = tc.keystoneMode
			cp.Spec.Services.Glance = validGlanceSpec()
			cp.Spec.Services.Glance.DatabaseCredentialsMode = tc.glanceMode

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).NotTo(HaveOccurred(),
				"a Static/Dynamic override on a managed shared database must be admitted for both services")
		})
	}
}

// --- extraConfig admission checks (Family A ownership/shape, Family B catalog) ---

// jsonSetting is a small helper for a Horizon extraConfig JSON value.
func jsonSetting(raw string) apiextensionsv1.JSON {
	return apiextensionsv1.JSON{Raw: []byte(raw)}
}

// TestValidateCreate_RejectsUnknownGlobalExtraConfigOption pins that a global INI
// option the Keystone catalog does not accept is rejected at the real
// globalExtraConfig leaf, naming the keystone catalog and its release.
func TestValidateCreate_RejectsUnknownGlobalExtraConfigOption(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.GlobalExtraConfig = map[string]map[string]string{"token": {"providr": "fernet"}}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.globalExtraConfig[token][providr]"))
	g.Expect(err.Error()).To(ContainSubstring("no such option in the keystone 2025.2 option catalog"))
}

// TestValidateCreate_RejectsGlobalKeystoneOptionUnknownToGlance pins the
// cross-service reach of globalExtraConfig: [token] expiration is a valid
// Keystone option but Glance has no [token] section, so declaring Glance rejects
// the global block against the GLANCE catalog.
func TestValidateCreate_RejectsGlobalKeystoneOptionUnknownToGlance(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := glanceControlPlane()
	cp.Spec.GlobalExtraConfig = map[string]map[string]string{"token": {"expiration": "3600"}}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.globalExtraConfig[token][expiration]"))
	g.Expect(err.Error()).To(ContainSubstring("no such section in the glance 2025.2 option catalog"))
}

// TestValidateCreate_RejectsUnknownGlanceExtraConfigOption pins a per-service
// Glance override the catalog does not accept.
func TestValidateCreate_RejectsUnknownGlanceExtraConfigOption(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.ExtraConfig = map[string]map[string]string{"DEFAULT": {"workerz": "4"}}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.services.glance.extraConfig[DEFAULT][workerz]"))
	g.Expect(err.Error()).To(ContainSubstring("no such option in the glance 2025.2 option catalog"))
}

// TestValidateCreate_RejectsUnknownPlacementExtraConfigOption pins the placement
// catalog leg from both sides: a per-service override the catalog does not
// accept, and the cross-service reach of globalExtraConfig. [token] expiration
// is a valid Keystone option, but placement has no [token] section, so declaring
// placement rejects the global block against the PLACEMENT catalog.
func TestValidateCreate_RejectsUnknownPlacementExtraConfigOption(t *testing.T) {
	w := &ControlPlaneWebhook{}

	t.Run("in placement extraConfig", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := placementControlPlane()
		cp.Spec.Services.Placement.ExtraConfig = map[string]map[string]string{
			"placement": {"randomize_allocation_candidatez": "true"},
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring(
			"spec.services.placement.extraConfig[placement][randomize_allocation_candidatez]",
		))
		g.Expect(err.Error()).To(ContainSubstring("no such option in the placement 2025.2 option catalog"))
	})

	t.Run("in globalExtraConfig", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := placementControlPlane()
		cp.Spec.GlobalExtraConfig = map[string]map[string]string{"token": {"expiration": "3600"}}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.globalExtraConfig[token][expiration]"))
		g.Expect(err.Error()).To(ContainSubstring("no such section in the placement 2025.2 option catalog"))
	})
}

// TestValidateCreate_RejectsUnknownOptionInBothBlocks pins the by-membership
// attribution: an unknown key present in both the global and the per-service
// block yields exactly two errors, one per contributing path.
func TestValidateCreate_RejectsUnknownOptionInBothBlocks(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.GlobalExtraConfig = map[string]map[string]string{"token": {"providr": "x"}}
	cp.Spec.Services.Keystone.ExtraConfig = map[string]map[string]string{"token": {"providr": "y"}}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.globalExtraConfig[token][providr]"))
	g.Expect(err.Error()).To(ContainSubstring("spec.services.keystone.extraConfig[token][providr]"))
	g.Expect(strings.Count(err.Error(), "no such option in the keystone 2025.2 option catalog")).
		To(Equal(2), "one error per contributing block")
}

// TestValidateCreate_RejectsUnknownSection pins the unknown-section message: it
// names the ControlPlane framing and never leaks the child webhook's
// "declare via spec.plugins" hint.
func TestValidateCreate_RejectsUnknownSection(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.GlobalExtraConfig = map[string]map[string]string{"notasection": {"foo": "bar"}}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.globalExtraConfig[notasection][foo]"))
	g.Expect(err.Error()).To(ContainSubstring("plugin-registered sections are not configurable through the ControlPlane"))
	g.Expect(err.Error()).NotTo(ContainSubstring("spec.plugins"))
}

// TestValidateCreate_ForbidsGlanceRejectedOwnedKey pins that a Rejected glance
// owned key is Forbidden regardless of which block carries it: [keystone_authtoken]
// password, because rendering it would leak the service password into the
// namespace-readable ConfigMap, and the [import_filtering_opts] keys, because
// rendering one would loosen the web-download URI filter behind the back of
// services.glance.importFiltering and its admission warnings.
func TestValidateCreate_ForbidsGlanceRejectedOwnedKey(t *testing.T) {
	w := &ControlPlaneWebhook{}

	t.Run("in globalExtraConfig", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := glanceControlPlane()
		cp.Spec.GlobalExtraConfig = map[string]map[string]string{"keystone_authtoken": {"password": "s3cr3t"}}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.globalExtraConfig[keystone_authtoken][password]"))
		g.Expect(err.Error()).To(ContainSubstring("password is managed via spec.serviceUser.secretRef and must not be set in extraConfig"))
	})

	t.Run("in glance extraConfig", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := glanceControlPlane()
		cp.Spec.Services.Glance.ExtraConfig = map[string]map[string]string{"keystone_authtoken": {"password": "s3cr3t"}}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.glance.extraConfig[keystone_authtoken][password]"))
		g.Expect(err.Error()).To(ContainSubstring("must not be set in extraConfig"))
	})

	t.Run("import filter in glance extraConfig", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := glanceControlPlane()
		cp.Spec.Services.Glance.ExtraConfig = map[string]map[string]string{
			"import_filtering_opts": {"disallowed_hosts": ""},
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.glance.extraConfig[import_filtering_opts][disallowed_hosts]"))
		g.Expect(err.Error()).To(ContainSubstring("disallowed_hosts is managed via spec.importFiltering"))
	})
}

// TestValidateCreate_ForbidsPlacementRejectedOwnedKey pins that a Rejected
// placement owned key is Forbidden regardless of which block carries it. [api]
// auth_strategy is the one to guard: the merged block has the last word, so
// honoring noauth2 would put the API on the no-auth middleware, unauthenticated
// from the moment the pods load the rendered file.
func TestValidateCreate_ForbidsPlacementRejectedOwnedKey(t *testing.T) {
	w := &ControlPlaneWebhook{}

	t.Run("in globalExtraConfig", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := placementControlPlane()
		cp.Spec.GlobalExtraConfig = map[string]map[string]string{"api": {"auth_strategy": "noauth2"}}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.globalExtraConfig[api][auth_strategy]"))
		g.Expect(err.Error()).To(ContainSubstring(
			"auth_strategy is managed via operator-computed and must not be set in extraConfig",
		))
		g.Expect(err.Error()).To(ContainSubstring("noauth2 disables token validation entirely"))
	})

	t.Run("in placement extraConfig", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := placementControlPlane()
		cp.Spec.Services.Placement.ExtraConfig = map[string]map[string]string{"api": {"auth_strategy": "noauth2"}}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.placement.extraConfig[api][auth_strategy]"))
		g.Expect(err.Error()).To(ContainSubstring("must not be set in extraConfig"))
	})

	t.Run("deprecated DEFAULT alias in placement extraConfig", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := placementControlPlane()
		cp.Spec.Services.Placement.ExtraConfig = map[string]map[string]string{
			"DEFAULT": {"auth_strategy": "noauth2"},
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.placement.extraConfig[DEFAULT][auth_strategy]"))
		g.Expect(err.Error()).To(ContainSubstring("the deprecated alias of [api] auth_strategy"))
	})
}

// TestValidateCreate_TrustedDashboardOwnership pins the conditional gate on the
// merged Keystone [federation] trusted_dashboard: Forbidden when the ControlPlane
// derives a dashboard endpoint from services.horizon (publicEndpoint or a bare
// gateway hostname), but admitted with an owned-key warning when no dashboard is
// derived (an externally-run dashboard doing WebSSO is legitimate).
func TestValidateCreate_TrustedDashboardOwnership(t *testing.T) {
	w := &ControlPlaneWebhook{}
	withTrustedDashboard := func() *ControlPlane {
		cp := validControlPlane()
		cp.Spec.GlobalExtraConfig = map[string]map[string]string{
			"federation": {"trusted_dashboard": "https://dash.example.com/auth/websso/"},
		}
		return cp
	}

	t.Run("rejected with horizon publicEndpoint", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := withTrustedDashboard()
		cp.Spec.Services.Horizon = &ServiceHorizonSpec{PublicEndpoint: "https://dash.example.com"}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.globalExtraConfig[federation][trusted_dashboard]"))
		g.Expect(err.Error()).To(ContainSubstring("Horizon-derived trusted-dashboards projection"))
	})

	t.Run("rejected with horizon gateway hostname only", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := withTrustedDashboard()
		cp.Spec.Services.Horizon = &ServiceHorizonSpec{Gateway: &commonv1.GatewaySpec{Hostname: "dash.example.com"}}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.globalExtraConfig[federation][trusted_dashboard]"))
	})

	t.Run("admitted with warning when horizon absent", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := withTrustedDashboard()

		warnings, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).To(ContainElement(ContainSubstring("[federation] trusted_dashboard")))
	})
}

// TestValidateCreate_ForbidsHorizonRejectedSettings pins that SECRET_KEY and one
// WebSSO and one multi-domain setting are Forbidden unconditionally at their real
// leaves — the ControlPlane projects websso/multiDomain dynamically, so a collision
// is not decidable at admission and the key would plant a runtime wedge.
func TestValidateCreate_ForbidsHorizonRejectedSettings(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{
		ExtraConfig: map[string]apiextensionsv1.JSON{
			"SECRET_KEY":                             jsonSetting(`"x"`),
			"WEBSSO_ENABLED":                         jsonSetting("true"),
			"OPENSTACK_KEYSTONE_MULTIDOMAIN_SUPPORT": jsonSetting("true"),
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.services.horizon.extraConfig[SECRET_KEY]"))
	g.Expect(err.Error()).To(ContainSubstring("spec.services.horizon.extraConfig[WEBSSO_ENABLED]"))
	g.Expect(err.Error()).To(ContainSubstring("spec.services.horizon.extraConfig[OPENSTACK_KEYSTONE_MULTIDOMAIN_SUPPORT]"))
}

// TestValidateCreate_RejectsMalformedExtraConfigShape pins the shape checks on
// every block: empty section, empty key, empty Horizon setting name, and a
// Horizon setting that is not a Python identifier.
func TestValidateCreate_RejectsMalformedExtraConfigShape(t *testing.T) {
	w := &ControlPlaneWebhook{}

	t.Run("empty global section name", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := validControlPlane()
		cp.Spec.GlobalExtraConfig = map[string]map[string]string{"": {"k": "v"}}
		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.globalExtraConfig"))
		g.Expect(err.Error()).To(ContainSubstring("extraConfig section name must not be empty"))
	})

	t.Run("empty keystone option key", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := validControlPlane()
		cp.Spec.Services.Keystone.ExtraConfig = map[string]map[string]string{"token": {"": "v"}}
		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.keystone.extraConfig[token]"))
		g.Expect(err.Error()).To(ContainSubstring("extraConfig key must not be empty"))
	})

	t.Run("empty glance section name", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := glanceControlPlane()
		cp.Spec.Services.Glance.ExtraConfig = map[string]map[string]string{"": {"k": "v"}}
		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.glance.extraConfig"))
		g.Expect(err.Error()).To(ContainSubstring("extraConfig section name must not be empty"))
	})

	t.Run("empty placement section name", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := placementControlPlane()
		cp.Spec.Services.Placement.ExtraConfig = map[string]map[string]string{"": {"k": "v"}}
		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.placement.extraConfig"))
		g.Expect(err.Error()).To(ContainSubstring("extraConfig section name must not be empty"))
	})

	t.Run("empty horizon setting name", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := validControlPlane()
		cp.Spec.Services.Horizon = &ServiceHorizonSpec{
			ExtraConfig: map[string]apiextensionsv1.JSON{"": jsonSetting(`"x"`)},
		}
		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.horizon.extraConfig"))
		g.Expect(err.Error()).To(ContainSubstring("extraConfig setting name must not be empty"))
	})

	t.Run("non-identifier horizon setting name", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := validControlPlane()
		cp.Spec.Services.Horizon = &ServiceHorizonSpec{
			ExtraConfig: map[string]apiextensionsv1.JSON{"not-an-identifier": jsonSetting(`"x"`)},
		}
		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.horizon.extraConfig[not-an-identifier]"))
		g.Expect(err.Error()).To(ContainSubstring("must be a valid Python identifier"))
	})
}

// TestValidateCreate_RejectsExtraConfigINIInjection pins the value-side INI
// injection guard: the ownership and catalog gates match by (section, key) name
// only, so a newline smuggled into a value (or a section name / key) renders a
// whole extra [section]/key the gates never inspected. The classic PoC smuggles
// the operator-owned [federation] trusted_dashboard through a legitimate
// [DEFAULT] debug value; without the shape guard it slips past every gate and
// Keystone POSTs WebSSO tokens to the attacker origin.
func TestValidateCreate_RejectsExtraConfigINIInjection(t *testing.T) {
	w := &ControlPlaneWebhook{}

	t.Run("newline in global value smuggles a section", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := validControlPlane()
		cp.Spec.GlobalExtraConfig = map[string]map[string]string{
			"DEFAULT": {"debug": "false\n[federation]\ntrusted_dashboard = https://attacker.example"},
		}
		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.globalExtraConfig[DEFAULT][debug]"))
		g.Expect(err.Error()).To(ContainSubstring("must not contain a newline or carriage return"))
	})

	t.Run("carriage return in keystone value", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := validControlPlane()
		cp.Spec.Services.Keystone.ExtraConfig = map[string]map[string]string{
			"DEFAULT": {"debug": "false\r[database]\rconnection = mysql://attacker/x"},
		}
		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.keystone.extraConfig[DEFAULT][debug]"))
		g.Expect(err.Error()).To(ContainSubstring("must not contain a newline or carriage return"))
	})

	t.Run("newline in glance key", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := glanceControlPlane()
		cp.Spec.Services.Glance.ExtraConfig = map[string]map[string]string{
			"DEFAULT": {"debug\n[keystone_authtoken]\npassword": "s3cr3t"},
		}
		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.glance.extraConfig[DEFAULT]"))
		g.Expect(err.Error()).To(ContainSubstring("must not contain a newline or carriage return"))
	})

	t.Run("newline in placement value smuggles the auth strategy", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := placementControlPlane()
		cp.Spec.Services.Placement.ExtraConfig = map[string]map[string]string{
			"DEFAULT": {"debug": "false\n[api]\nauth_strategy = noauth2"},
		}
		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.placement.extraConfig[DEFAULT][debug]"))
		g.Expect(err.Error()).To(ContainSubstring("must not contain a newline or carriage return"))
	})

	t.Run("newline in global section name", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := validControlPlane()
		cp.Spec.GlobalExtraConfig = map[string]map[string]string{
			"DEFAULT\n[federation]": {"k": "v"},
		}
		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.globalExtraConfig"))
		g.Expect(err.Error()).To(ContainSubstring("section name must not contain a newline or carriage return"))
	})
}

// TestValidateCreate_ForbidsKeystoneExtraConfigInExternalMode pins the webhook
// mirror of the CEL rule: services.keystone.extraConfig is Forbidden in External
// mode (no Keystone workload is deployed).
func TestValidateCreate_ForbidsKeystoneExtraConfigInExternalMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Infrastructure = nil
	cp.Spec.Services.Keystone = &ServiceKeystoneSpec{
		Mode:        KeystoneModeExternal,
		External:    &ExternalKeystoneSpec{AuthURL: "https://keystone.example.com/v3"},
		ExtraConfig: map[string]map[string]string{"token": {"expiration": "3600"}},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.services.keystone.extraConfig"))
	g.Expect(err.Error()).To(ContainSubstring("forbidden when services.keystone.mode is External"))
}

// TestValidateCreate_AcceptsValidOptionInBothBlocks pins that a catalog-valid,
// non-owned option present in both the global and the per-service block is
// admitted (no double-count, no spurious rejection).
func TestValidateCreate_AcceptsValidOptionInBothBlocks(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.GlobalExtraConfig = map[string]map[string]string{"token": {"expiration": "3600"}}
	cp.Spec.Services.Keystone.ExtraConfig = map[string]map[string]string{"token": {"expiration": "7200"}}

	warnings, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(warnings).To(BeEmpty())
}

// TestValidateCreate_AcceptsAllFourExtraConfigBlocks is the acceptance base case:
// a ControlPlane with all four blocks populated with catalog-valid, non-owned
// content is admitted with no warnings.
func TestValidateCreate_AcceptsAllFourExtraConfigBlocks(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := glanceControlPlane()
	cp.Spec.GlobalExtraConfig = map[string]map[string]string{"DEFAULT": {"log_date_format": "%Y-%m-%d %H:%M:%S"}}
	cp.Spec.Services.Keystone.ExtraConfig = map[string]map[string]string{"token": {"expiration": "3600"}}
	cp.Spec.Services.Glance.ExtraConfig = map[string]map[string]string{"cors": {"allowed_origin": "https://example.com"}}
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{
		ExtraConfig: map[string]apiextensionsv1.JSON{"CUSTOM_SETTING": jsonSetting("true")},
	}

	warnings, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(warnings).To(BeEmpty())
}

// TestValidateCreate_AcceptsAbsentExtraConfigBlocks pins that a CR with no
// extraConfig at all raises no extraConfig warning and no extraConfig error.
func TestValidateCreate_AcceptsAbsentExtraConfigBlocks(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	warnings, err := w.ValidateCreate(context.Background(), validControlPlane())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(warnings).To(BeEmpty())
}

// TestValidateCreate_WarnsOnReportedOwnedKey pins that a Reported owned key is
// honored but surfaced: the warning names the key, its OwnedBy source, and the
// contributing block path, and the CR is admitted.
func TestValidateCreate_WarnsOnReportedOwnedKey(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.GlobalExtraConfig = map[string]map[string]string{"DEFAULT": {"debug": "true"}}

	warnings, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(warnings).To(ContainElement(And(
		ContainSubstring("[DEFAULT] debug"),
		ContainSubstring("operator-computed"),
		ContainSubstring("spec.globalExtraConfig[DEFAULT][debug]"),
	)))
}

// TestValidateCreate_WarnsOnDeprecatedOption pins that a deprecated-but-accepted
// option is admitted with a warning naming its replacement.
func TestValidateCreate_WarnsOnDeprecatedOption(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.GlobalExtraConfig = map[string]map[string]string{"DEFAULT": {"logfile": "/var/log/keystone.log"}}

	warnings, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(warnings).To(ContainElement(And(
		ContainSubstring("logfile"),
		ContainSubstring("replaced by [DEFAULT] log_file"),
	)))
}

// TestValidateCreate_FailsOpenOnDigestPinnedKeystoneImage pins the fail-open: a
// digest-pinned Keystone image names no release, so the merged config is not
// validated — exactly one warning, zero errors.
func TestValidateCreate_FailsOpenOnDigestPinnedKeystoneImage(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Keystone.Image = &commonv1.ImageSpec{
		Repository: "ghcr.io/c5c3/keystone",
		Digest:     "sha256:" + strings.Repeat("a", 64),
	}
	cp.Spec.Services.Keystone.ExtraConfig = map[string]map[string]string{"token": {"expiration": "3600"}}

	warnings, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(warnings).To(HaveLen(1))
	g.Expect(warnings[0]).To(ContainSubstring("does not name an OpenStack release"))
}

// TestValidateCreate_FailsOpenOnCatalogLessRelease pins the fail-open for a
// syntactically valid release the build ships no catalog for: exactly one warning
// per declared INI service, zero errors.
func TestValidateCreate_FailsOpenOnCatalogLessRelease(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := glanceControlPlane()
	cp.Spec.OpenStackRelease = "2027.1"
	cp.Spec.Services.Keystone.ExtraConfig = map[string]map[string]string{"token": {"expiration": "3600"}}
	cp.Spec.Services.Glance.ExtraConfig = map[string]map[string]string{"cors": {"allowed_origin": "https://example.com"}}
	cp.Spec.Services.Placement = &ServicePlacementSpec{
		ExtraConfig: map[string]map[string]string{"placement": {"randomize_allocation_candidates": "true"}},
	}
	warnings, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(warnings).To(HaveLen(3))
	g.Expect(warnings).To(ContainElement(And(
		ContainSubstring("keystone"),
		ContainSubstring(`no catalog for release "2027.1"`),
	)))
	g.Expect(warnings).To(ContainElement(And(
		ContainSubstring("glance"),
		ContainSubstring(`no catalog for release "2027.1"`),
	)))
	g.Expect(warnings).To(ContainElement(And(
		ContainSubstring("placement"),
		ContainSubstring(`no catalog for release "2027.1"`),
	)))
}

// TestValidateUpdate_ExtraConfigCatalogGating pins the Family B update gate: a
// stored CR whose extraConfig is no longer catalog-valid stays mutable by an
// unrelated edit, but any edit that changes a catalog input re-validates and
// rejects. The stored (old) object is built directly, bypassing admission.
func TestValidateUpdate_ExtraConfigCatalogGating(t *testing.T) {
	w := &ControlPlaneWebhook{}
	staleInvalid := func() *ControlPlane {
		cp := validControlPlane()
		cp.Name = "cp"
		cp.Spec.GlobalExtraConfig = map[string]map[string]string{"token": {"providr": "fernet"}}
		return cp
	}

	t.Run("unrelated replicas bump is admitted", func(t *testing.T) {
		g := NewGomegaWithT(t)
		oldCP := staleInvalid()
		newCP := staleInvalid()
		replicas := int32(2)
		newCP.Spec.Services.Keystone.Replicas = &replicas

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).NotTo(HaveOccurred())
	})

	t.Run("editing the block re-validates and rejects", func(t *testing.T) {
		g := NewGomegaWithT(t)
		oldCP := staleInvalid()
		newCP := staleInvalid()
		newCP.Spec.GlobalExtraConfig = map[string]map[string]string{"token": {"providr2": "fernet"}}

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("no such option in the keystone 2025.2 option catalog"))
	})

	t.Run("changing openStackRelease re-validates and rejects", func(t *testing.T) {
		g := NewGomegaWithT(t)
		oldCP := staleInvalid()
		newCP := staleInvalid()
		newCP.Spec.OpenStackRelease = "2026.1"

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("no such option in the keystone 2026.1 option catalog"))
	})

	t.Run("changing keystone image re-validates and rejects", func(t *testing.T) {
		g := NewGomegaWithT(t)
		oldCP := staleInvalid()
		newCP := staleInvalid()
		newCP.Spec.Services.Keystone.Image = &commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"}

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("no such option in the keystone 2025.2 option catalog"))
	})

	t.Run("newly declaring glance re-validates and rejects", func(t *testing.T) {
		g := NewGomegaWithT(t)
		oldCP := staleInvalid()
		newCP := staleInvalid()
		newCP.Spec.Services.Glance = validGlanceSpec()

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.globalExtraConfig[token][providr]"))
	})

	t.Run("newly declaring placement re-validates and rejects", func(t *testing.T) {
		g := NewGomegaWithT(t)
		oldCP := staleInvalid()
		newCP := staleInvalid()
		newCP.Spec.Services.Placement = &ServicePlacementSpec{}

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.globalExtraConfig[token][providr]"))
		g.Expect(err.Error()).To(ContainSubstring("no such section in the placement 2025.2 option catalog"))
	})
}

// TestValidateUpdate_TrustedDashboardRejectedWhenHorizonAppears pins that Family A
// is un-gated on update: newly declaring a Horizon dashboard endpoint turns a
// stored, previously-admitted [federation] trusted_dashboard override into a
// rejection.
func TestValidateUpdate_TrustedDashboardRejectedWhenHorizonAppears(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := validControlPlane()
	oldCP.Name = "cp"
	oldCP.Spec.GlobalExtraConfig = map[string]map[string]string{
		"federation": {"trusted_dashboard": "https://dash.example.com/auth/websso/"},
	}
	newCP := oldCP.DeepCopy()
	newCP.Spec.Services.Horizon = &ServiceHorizonSpec{PublicEndpoint: "https://dash.example.com"}

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.globalExtraConfig[federation][trusted_dashboard]"))
	g.Expect(err.Error()).To(ContainSubstring("Horizon-derived trusted-dashboards projection"))
}

// --- services.barbican ---

// barbicanControlPlane returns a managed ControlPlane with a minimal barbican
// block, so the tests below start from an admissible baseline and vary only the
// field (or the INI content) under test. The store is the dedicated one, the
// mode that needs no references of its own. The shared infrastructure stays
// brownfield (the validControlPlane baseline).
func barbicanControlPlane() *ControlPlane {
	cp := validControlPlane()
	cp.Name = "cp"
	cp.Spec.Services.Barbican = &ServiceBarbicanSpec{
		SecretStore: ServiceBarbicanSecretStoreSpec{Dedicated: &BarbicanDedicatedSecretStoreSpec{}},
	}
	return cp
}

// externalBarbicanStore returns a complete external secret-store block, the
// shape the tests below break one field at a time.
func externalBarbicanStore() *BarbicanExternalSecretStoreSpec {
	return &BarbicanExternalSecretStoreSpec{
		URL:                  "https://openbao.example.com:8200",
		CredentialsSecretRef: barbicanv1alpha1.SecretNameRefSpec{Name: "barbican-approle"},
	}
}

// TestValidateCreate_AcceptsBarbicanControlPlane pins the admissible baseline.
func TestValidateCreate_AcceptsBarbicanControlPlane(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	_, err := w.ValidateCreate(context.Background(), barbicanControlPlane())
	g.Expect(err).NotTo(HaveOccurred())
}

// The projected Barbican child is bounded far below the 253-byte object-name
// cap: the barbican operator appends "-db-clean" to name its clean-up CronJob,
// and Kubernetes caps CronJob names at 52 characters, which leaves the Barbican
// CRD's own 43-character metadata.name bound. Without this guard the
// ControlPlane admits and the projection is then rejected on every pass — with
// metadata.name immutable, recovery means recreating the whole control plane.
func TestValidateCreate_RejectsOverlongProjectedBarbicanChildName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	maxCPName := barbicanv1alpha1.MaxBarbicanNameLength - barbicanChildNameOverhead
	g.Expect(maxCPName).To(Equal(34), "the 43-character child bound leaves 34 for the ControlPlane name")

	atLimit := barbicanControlPlane()
	atLimit.Name = strings.Repeat("c", maxCPName)
	_, err := w.ValidateCreate(context.Background(), atLimit)
	g.Expect(err).NotTo(HaveOccurred(),
		"a name whose projected Barbican child still fits must be accepted")

	tooLong := barbicanControlPlane()
	tooLong.Name = strings.Repeat("c", maxCPName+1)
	_, err = w.ValidateCreate(context.Background(), tooLong)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("projected Barbican child CR name would be"))
	g.Expect(err.Error()).To(ContainSubstring("caps metadata.name at 43"))

	// Without services.barbican no Barbican child is projected, so the bound does
	// not apply — the ControlPlane keeps the full 253-byte budget.
	noBarbican := validControlPlane()
	noBarbican.Name = strings.Repeat("c", maxCPName+1)
	_, err = w.ValidateCreate(context.Background(), noBarbican)
	g.Expect(err).NotTo(HaveOccurred())
}

// Enabling Barbican on an existing over-long ControlPlane is the one update that
// can newly violate the bound, so it is rejected; every other update on a CR that
// already carried Barbican — including the finalizer removal that completes its
// deletion — must still pass, because metadata.name is immutable and a rejection
// would wedge it in Terminating.
func TestValidateUpdate_ProjectedBarbicanChildNameBoundIsNewlyEnabledOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	overlong := strings.Repeat("c", barbicanv1alpha1.MaxBarbicanNameLength-barbicanChildNameOverhead+1)

	withoutBarbican := validControlPlane()
	withoutBarbican.Name = overlong
	enabling := barbicanControlPlane()
	enabling.Name = overlong

	_, err := w.ValidateUpdate(context.Background(), withoutBarbican, enabling)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("projected Barbican child CR name would be"))

	grandfathered := barbicanControlPlane()
	grandfathered.Name = overlong
	grandfathered.Finalizers = []string{"c5c3.io/finalizer"}
	deleting := grandfathered.DeepCopy()
	deleting.Finalizers = nil

	_, err = w.ValidateUpdate(context.Background(), grandfathered, deleting)
	g.Expect(err).NotTo(HaveOccurred(),
		"an over-long grandfathered ControlPlane must stay updatable, or its deletion never completes")
}

// TestDefault_BarbicanExternalStoreKVMountpoint verifies the webhook mirror of
// the +kubebuilder:default=barbican marker: an external store with no mount
// named takes the delivered one, an explicit mount survives, and the dedicated
// mode — which names no mount at all — is left alone.
func TestDefault_BarbicanExternalStoreKVMountpoint(t *testing.T) {
	w := &ControlPlaneWebhook{}

	t.Run("an absent mountpoint is materialized", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()
		cp.Spec.Services.Barbican.SecretStore = ServiceBarbicanSecretStoreSpec{External: externalBarbicanStore()}

		g.Expect(w.Default(context.Background(), cp)).To(Succeed())
		g.Expect(cp.Spec.Services.Barbican.SecretStore.External.KVMountpoint).To(Equal(DefaultBarbicanKVMountpoint))
	})

	t.Run("an explicit mountpoint is preserved", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()
		ext := externalBarbicanStore()
		ext.KVMountpoint = "kv-secrets"
		cp.Spec.Services.Barbican.SecretStore = ServiceBarbicanSecretStoreSpec{External: ext}

		g.Expect(w.Default(context.Background(), cp)).To(Succeed())
		g.Expect(cp.Spec.Services.Barbican.SecretStore.External.KVMountpoint).To(Equal("kv-secrets"))
	})

	t.Run("the dedicated mode names no mount", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()

		g.Expect(w.Default(context.Background(), cp)).To(Succeed())
		g.Expect(cp.Spec.Services.Barbican.SecretStore.External).To(BeNil())
		g.Expect(cp.Spec.Services.Barbican.SecretStore.Dedicated).NotTo(BeNil())
	})
}

// TestDefault_BarbicanDedicatedBackingServicesLeaves verifies a declared
// barbican dedicated block takes the same leaf defaults as the shared one, with
// a managed clusterRef name DERIVED from the ControlPlane and credentialsMode
// materialized to Static (a dedicated managed database cannot draw engine-issued
// credentials).
func TestDefault_BarbicanDedicatedBackingServicesLeaves(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := barbicanControlPlane()
	cp.Name = "prod"
	cp.Spec.Services.Barbican.DedicatedBackingServices = &BarbicanDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{},
		Cache:    &commonv1.CacheSpec{},
	}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	db := cp.Spec.Services.Barbican.DedicatedBackingServices.Database
	g.Expect(db.ClusterRef).NotTo(BeNil())
	g.Expect(db.ClusterRef.Name).To(Equal("prod-barbican-db"))
	g.Expect(db.Database).To(Equal(DefaultDatabaseName))
	g.Expect(db.SecretRef.Name).To(Equal(DefaultDatabaseSecretName))
	g.Expect(db.CredentialsMode).To(Equal(commonv1.CredentialsModeStatic),
		"a dedicated managed database is Static-only: no per-instance OpenBao engine role exists")

	cache := cp.Spec.Services.Barbican.DedicatedBackingServices.Cache
	g.Expect(cache.ClusterRef).NotTo(BeNil())
	g.Expect(cache.ClusterRef.Name).To(Equal("prod-barbican-cache"))
	g.Expect(cache.Backend).To(Equal(DefaultCacheBackend))

	// Idempotent on the dedicated leaves too.
	before := cp.DeepCopy()
	g.Expect(w.Default(context.Background(), cp)).To(Succeed())
	g.Expect(cp.Spec.Services).To(Equal(before.Spec.Services))
}

// TestDefault_BarbicanServiceNamespaceLifecycle verifies a declared barbican
// namespace assignment takes the Managed lifecycle default, exactly as the
// keystone/horizon/glance/placement ones do.
func TestDefault_BarbicanServiceNamespaceLifecycle(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := barbicanControlPlane()
	cp.Spec.Services.Barbican.Namespace = &ServiceNamespaceSpec{Name: "barbican"}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())
	g.Expect(cp.Spec.Services.Barbican.Namespace.Lifecycle).To(Equal(ServiceNamespaceLifecycleManaged))
}

// TestValidateCreate_RejectsBarbicanSecretStoreUnion pins the webhook mirror of
// the type-level CEL rule, which an API server that skips
// x-kubernetes-validations would otherwise let through: a store naming both
// modes addresses two different servers, and one naming neither leaves the
// projection with no server to point barbican at.
func TestValidateCreate_RejectsBarbicanSecretStoreUnion(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for name, store := range map[string]ServiceBarbicanSecretStoreSpec{
		"neither mode": {},
		"both modes": {
			Dedicated: &BarbicanDedicatedSecretStoreSpec{},
			External:  externalBarbicanStore(),
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := barbicanControlPlane()
			cp.Spec.Services.Barbican.SecretStore = store

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("spec.services.barbican.secretStore"))
			g.Expect(err.Error()).To(ContainSubstring("exactly one of dedicated or external must be set"))
		})
	}
}

// TestValidateCreate_RejectsBarbicanExternalStoreWithoutCredentials pins that an
// external store must name the Secret its AppRole credentials live in: without
// it the operator has nothing to authenticate the vault plugin with, and the
// child would park unready with no admission error naming the cause.
func TestValidateCreate_RejectsBarbicanExternalStoreWithoutCredentials(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := barbicanControlPlane()
	ext := externalBarbicanStore()
	ext.CredentialsSecretRef.Name = ""
	cp.Spec.Services.Barbican.SecretStore = ServiceBarbicanSecretStoreSpec{External: ext}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(
		"spec.services.barbican.secretStore.external.credentialsSecretRef.name",
	))
}

// TestValidateCreate_RejectsBarbicanExternalStoreWithEmptyCABundleName is the
// rejecting side of the optional CA reference: configuring the block and leaving
// its name empty names no Secret at all, which would leave the store trusting the
// pods' system roots while the operator believed a bundle was pinned.
func TestValidateCreate_RejectsBarbicanExternalStoreWithEmptyCABundleName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := barbicanControlPlane()
	ext := externalBarbicanStore()
	ext.CABundleSecretRef = &barbicanv1alpha1.SecretNameRefSpec{}
	cp.Spec.Services.Barbican.SecretStore = ServiceBarbicanSecretStoreSpec{External: ext}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(
		"spec.services.barbican.secretStore.external.caBundleSecretRef.name",
	))
}

// TestValidateCreate_RejectsBarbicanExternalStoreInsecureURL covers the shapes
// the ^https:// Pattern marker guards and the ones only a URL parse catches: the
// AppRole credentials and every secret barbican stores travel this URL, so a
// plaintext or hostless server address must never be admitted.
func TestValidateCreate_RejectsBarbicanExternalStoreInsecureURL(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for name, rawURL := range map[string]string{
		"plaintext http": "http://openbao.example.com:8200",
		"missing host":   "https://",
		"not a URL":      "openbao.example.com",
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := barbicanControlPlane()
			ext := externalBarbicanStore()
			ext.URL = rawURL
			cp.Spec.Services.Barbican.SecretStore = ServiceBarbicanSecretStoreSpec{External: ext}

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("spec.services.barbican.secretStore.external.url"))
		})
	}
}

// TestValidateCreate_AcceptsBarbicanExternalStore pins the accepting side of the
// external mode, CA bundle and OpenBao namespace included.
func TestValidateCreate_AcceptsBarbicanExternalStore(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := barbicanControlPlane()
	ext := externalBarbicanStore()
	ext.CABundleSecretRef = &barbicanv1alpha1.SecretNameRefSpec{Name: "openbao-ca"}
	ext.Namespace = "tenants/openstack"
	cp.Spec.Services.Barbican.SecretStore = ServiceBarbicanSecretStoreSpec{External: ext}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_RejectsBarbicanImageTagDigestXOR pins the defense-in-depth
// mirror of the commonv1.ImageSpec XValidation rule for callers that bypass CRD
// schema admission.
func TestValidateCreate_RejectsBarbicanImageTagDigestXOR(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for name, img := range map[string]*commonv1.ImageSpec{
		"neither tag nor digest": {Repository: "ghcr.io/c5c3/barbican"},
		"both tag and digest": {
			Repository: "ghcr.io/c5c3/barbican",
			Tag:        "2025.2",
			Digest:     "sha256:" + strings.Repeat("a", 64),
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := barbicanControlPlane()
			cp.Spec.Services.Barbican.Image = img

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("services.barbican.image"))
			g.Expect(err.Error()).To(ContainSubstring("exactly one of image.tag or image.digest must be set"))
		})
	}
}

// TestValidateCreate_BarbicanPublicEndpointMustAgreeWithGateway pins the rules
// the CRD markers cannot express. The value is advertised verbatim as the public
// key-manager catalog endpoint and is projected into no child CR, so nothing
// downstream re-checks it.
func TestValidateCreate_BarbicanPublicEndpointMustAgreeWithGateway(t *testing.T) {
	w := &ControlPlaneWebhook{}
	gateway := func() *commonv1.GatewaySpec {
		return &commonv1.GatewaySpec{
			Hostname:  "barbican.example.com",
			ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		}
	}

	t.Run("a path is not a bare origin", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()
		cp.Spec.Services.Barbican.PublicEndpoint = "https://barbican.example.com/keymanager"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("services.barbican.publicEndpoint"))
		g.Expect(err.Error()).To(ContainSubstring("must be a bare origin"))
	})

	t.Run("divergent host", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()
		cp.Spec.Services.Barbican.Gateway = gateway()
		cp.Spec.Services.Barbican.PublicEndpoint = "https://secrets.example.com"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring(
			`must equal services.barbican.gateway.hostname "barbican.example.com"`,
		))
	})

	t.Run("http scheme behind a TLS-terminating gateway", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()
		cp.Spec.Services.Barbican.Gateway = gateway()
		cp.Spec.Services.Barbican.PublicEndpoint = "http://barbican.example.com"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("scheme must be https"))
	})

	t.Run("matching host with a non-default port", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()
		cp.Spec.Services.Barbican.Gateway = gateway()
		cp.Spec.Services.Barbican.PublicEndpoint = "https://barbican.example.com:8443"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred(),
			"Gateway API hostnames carry no port, so the port is the reason the override exists")
	})

	// The documented tolerance: OpenStack clients normalize the catalog endpoint
	// before appending the API path, so one trailing slash is not a path.
	t.Run("a single trailing slash is tolerated", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()
		cp.Spec.Services.Barbican.Gateway = gateway()
		cp.Spec.Services.Barbican.PublicEndpoint = "https://barbican.example.com/"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
	})

	// The two arms of the gateway-hostname check itself, which every other subtest
	// reaches only through the well-formed helper.
	t.Run("wildcard gateway hostname", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()
		cp.Spec.Services.Barbican.Gateway = gateway()
		cp.Spec.Services.Barbican.Gateway.Hostname = "*.example.com"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("services.barbican.gateway.hostname"))
	})

	t.Run("gateway without a hostname", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()
		cp.Spec.Services.Barbican.Gateway = &commonv1.GatewaySpec{
			ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("services.barbican.gateway.hostname"))
		g.Expect(err.Error()).To(ContainSubstring("must be set when a gateway is configured"))
	})
}

// TestValidateCreate_WarnsOnCleartextBarbicanPublicEndpoint covers the
// gateway-less barbican service, where an http endpoint is a legal (if unwise)
// development setup the CRD Pattern deliberately allows. Every call sends a
// scoped Keystone token — and the secret material — to that URL, so the
// downgrade must at least be surfaced.
func TestValidateCreate_WarnsOnCleartextBarbicanPublicEndpoint(t *testing.T) {
	w := &ControlPlaneWebhook{}

	// The fixture takes a dedicated secret store, which carries its own standing
	// warning (TestValidateCreate_WarnsOnDedicatedBarbicanSecretStore), so the
	// assertions here are about the endpoint warning specifically.
	t.Run("http warns", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()
		cp.Spec.Services.Barbican.PublicEndpoint = "http://barbican.example.com"

		warnings, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).To(ContainElement(ContainSubstring("scoped Keystone token")))
	})

	t.Run("https is silent", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()
		cp.Spec.Services.Barbican.PublicEndpoint = "https://barbican.example.com"

		warnings, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).NotTo(ContainElement(ContainSubstring("scoped Keystone token")))
	})
}

// TestValidateCreate_WarnsOnDedicatedBarbicanSecretStore pins the admission
// signal the fieldless dedicated block otherwise has no way of carrying: the
// instance it provisions runs a single replica on the Development profile and
// keeps its static seal key in a Secret beside the volume that key seals. The
// external store, which addresses a server somebody else hardened, must stay
// silent.
func TestValidateCreate_WarnsOnDedicatedBarbicanSecretStore(t *testing.T) {
	w := &ControlPlaneWebhook{}

	t.Run("dedicated warns", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()

		warnings, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).To(ContainElement(And(
			ContainSubstring("single-replica"),
			ContainSubstring("static key"),
			ContainSubstring("secretStore.external"),
		)))
	})

	t.Run("dedicated warns on update too", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()

		warnings, err := w.ValidateUpdate(context.Background(), cp.DeepCopy(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).To(ContainElement(ContainSubstring("single-replica")))
	})

	t.Run("external is silent", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()
		cp.Spec.Services.Barbican.SecretStore = ServiceBarbicanSecretStoreSpec{
			External: &BarbicanExternalSecretStoreSpec{
				URL:                  "https://openbao.example.com:8200",
				CredentialsSecretRef: barbicanv1alpha1.SecretNameRefSpec{Name: "barbican-approle"},
			},
		}

		warnings, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).NotTo(ContainElement(ContainSubstring("single-replica")))
	})
}

// TestValidateCreate_RejectsBarbicanInExternalMode verifies the webhook
// cross-field forbid, mirroring services.glance and services.placement: no
// Keystone workload is deployed, so Barbican has no identity to validate its
// tokens against.
func TestValidateCreate_RejectsBarbicanInExternalMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := externalControlPlane()
	cp.Spec.Services.Barbican = &ServiceBarbicanSpec{
		SecretStore: ServiceBarbicanSecretStoreSpec{Dedicated: &BarbicanDedicatedSecretStoreSpec{}},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.barbican"))
	g.Expect(err.Error()).To(ContainSubstring("forbidden when services.keystone.mode is External"))
}

// TestValidateCreate_RejectsBarbicanNamespaceClaimedByOtherControlPlane mirrors
// the tenant-key claim rule for the barbican service namespace: it proves the
// assignment reaches declaredServiceNamespaces, which is what the cluster-wide
// claim check walks.
func TestValidateCreate_RejectsBarbicanNamespaceClaimedByOtherControlPlane(t *testing.T) {
	g := NewGomegaWithT(t)
	incumbent := validControlPlane()
	incumbent.Name = "other"
	incumbent.Namespace = "barbican"
	c := fake.NewClientBuilder().WithScheme(webhookScheme(t)).WithObjects(incumbent).Build()
	w := &ControlPlaneWebhook{Client: c}

	cp := barbicanControlPlane()
	cp.Namespace = "openstack"
	cp.Spec.Services.Barbican.Namespace = &ServiceNamespaceSpec{
		Name: "barbican", Lifecycle: ServiceNamespaceLifecycleManaged,
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.barbican.namespace.name"))
	g.Expect(err.Error()).To(ContainSubstring(`already occupied by ControlPlane "other"`))
}

// TestValidateUpdate_RejectsBarbicanNamespaceChange pins the create-only freeze
// on the barbican namespace assignment.
func TestValidateUpdate_RejectsBarbicanNamespaceChange(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := barbicanControlPlane()
	oldCP.Namespace = "openstack"
	oldCP.Spec.Services.Barbican.Namespace = &ServiceNamespaceSpec{
		Name: "barbican", Lifecycle: ServiceNamespaceLifecycleManaged,
	}
	newCP := oldCP.DeepCopy()
	newCP.Spec.Services.Barbican.Namespace.Name = "barbican-2"

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.services.barbican.namespace.name"))
	g.Expect(err.Error()).To(ContainSubstring("the namespace a service is placed in is immutable"))
}

// TestValidateCreate_RejectsBarbicanDedicatedDatabaseDynamic confirms barbican's
// dedicated block reaches the shared dedicated-backing validation: the
// Dynamic-credentials rule (no per-instance OpenBao engine role) applies to it
// exactly as to keystone's.
func TestValidateCreate_RejectsBarbicanDedicatedDatabaseDynamic(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := barbicanControlPlane()
	cp.Spec.Services.Barbican.DedicatedBackingServices = &BarbicanDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{
			ClusterRef:      &corev1.LocalObjectReference{Name: "cp-barbican-db"},
			CredentialsMode: commonv1.CredentialsModeDynamic,
			Database:        "barbican",
			SecretRef:       commonv1.SecretRefSpec{Name: "barbican-db"},
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("credentialsMode Dynamic is not supported on a dedicated database"))
	g.Expect(err.Error()).To(ContainSubstring("barbican.dedicatedBackingServices.database"))
}

// TestValidateCreate_RejectsEmptyBarbicanDedicatedBackingServices pins the
// webhook mirror of the at-least-one-class CEL rule on the barbican dedicated
// block.
func TestValidateCreate_RejectsEmptyBarbicanDedicatedBackingServices(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := barbicanControlPlane()
	cp.Spec.Services.Barbican.DedicatedBackingServices = &BarbicanDedicatedBackingServicesSpec{}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.services.barbican.dedicatedBackingServices"))
	g.Expect(err.Error()).To(ContainSubstring("at least one backing-service class must be declared"))
}

// TestValidateUpdate_RejectsBarbicanDedicatedPresenceFlip pins the transition
// freeze on the barbican dedicated block: a live service cannot be moved between
// shared and dedicated backing services.
func TestValidateUpdate_RejectsBarbicanDedicatedPresenceFlip(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := barbicanControlPlane()
	newCP := barbicanControlPlane()
	newCP.Spec.Services.Barbican.DedicatedBackingServices = &BarbicanDedicatedBackingServicesSpec{
		Cache: &commonv1.CacheSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "cp-barbican-cache"},
			Backend:    commonv1.DefaultCacheBackend,
		},
	}

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("switching a service between shared and dedicated backing services"))
	g.Expect(err.Error()).To(ContainSubstring("barbican.dedicatedBackingServices"))
}

// TestValidateCreate_RejectsBarbicanCredentialsModeOverrideDynamicOnDedicated is
// the barbican mirror of the keystone dedicated-database rejection: the override
// retargets the shared database the service does not use.
func TestValidateCreate_RejectsBarbicanCredentialsModeOverrideDynamicOnDedicated(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := barbicanControlPlane()
	cp.Spec.Services.Barbican.DatabaseCredentialsMode = commonv1.CredentialsModeDynamic
	cp.Spec.Services.Barbican.DedicatedBackingServices = &BarbicanDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "cp-barbican-db"},
			Database:   "barbican",
			SecretRef:  commonv1.SecretRefSpec{Name: "barbican-db"},
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.barbican.databaseCredentialsMode"))
	g.Expect(err.Error()).To(ContainSubstring(
		"Dynamic is not supported as an override on a service with a dedicated database",
	))
}

// TestValidateUpdate_FreezesBarbicanSecretStoreAddressing is the regression guard
// for a store change that STRANDS the secret material behind it. The
// BarbicanSecretStore CRD freezes its own kvMountpoint and its instanceRef/server
// discriminator, and its message ("delete and recreate the store instead") is
// addressed to a human who accepted that consequence. Without this rule the
// reconciler acts on it instead: reconcileBarbicanSecretStore sees the frozen-field
// drift, deletes the live store, and recreates it against the new mount or the new
// server — and, leaving dedicated, leaves the whole OpenBao ensemble (instance,
// raft PVC, seal key, cluster-scoped auth-delegator binding) behind with nothing
// converging on it.
func TestValidateUpdate_FreezesBarbicanSecretStoreAddressing(t *testing.T) {
	w := &ControlPlaneWebhook{}

	t.Run("dedicated to external", func(t *testing.T) {
		g := NewGomegaWithT(t)
		oldCP := barbicanControlPlane()
		newCP := oldCP.DeepCopy()
		newCP.Spec.Services.Barbican.SecretStore = ServiceBarbicanSecretStoreSpec{
			External: externalBarbicanStore(),
		}

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.barbican.secretStore"))
		g.Expect(err.Error()).To(ContainSubstring("secret-store mode (dedicated vs external) is immutable"))
	})

	t.Run("external to dedicated", func(t *testing.T) {
		g := NewGomegaWithT(t)
		oldCP := barbicanControlPlane()
		oldCP.Spec.Services.Barbican.SecretStore = ServiceBarbicanSecretStoreSpec{
			External: externalBarbicanStore(),
		}
		newCP := oldCP.DeepCopy()
		newCP.Spec.Services.Barbican.SecretStore = ServiceBarbicanSecretStoreSpec{
			Dedicated: &BarbicanDedicatedSecretStoreSpec{},
		}

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("secret-store mode (dedicated vs external) is immutable"))
	})

	t.Run("kvMountpoint change", func(t *testing.T) {
		g := NewGomegaWithT(t)
		oldCP := barbicanControlPlane()
		ext := externalBarbicanStore()
		ext.KVMountpoint = "barbican"
		oldCP.Spec.Services.Barbican.SecretStore = ServiceBarbicanSecretStoreSpec{External: ext}
		newCP := oldCP.DeepCopy()
		newCP.Spec.Services.Barbican.SecretStore.External.KVMountpoint = "tenant-barbican"

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring(
			"spec.services.barbican.secretStore.external.kvMountpoint",
		))
		g.Expect(err.Error()).To(ContainSubstring("immutable"))
	})

	// The default only fills an absent field, so naming it explicitly is not a
	// change and must not be rejected.
	t.Run("materialising the default mount is not a change", func(t *testing.T) {
		g := NewGomegaWithT(t)
		oldCP := barbicanControlPlane()
		oldCP.Spec.Services.Barbican.SecretStore = ServiceBarbicanSecretStoreSpec{
			External: externalBarbicanStore(),
		}
		newCP := oldCP.DeepCopy()
		newCP.Spec.Services.Barbican.SecretStore.External.KVMountpoint = DefaultBarbicanKVMountpoint

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).NotTo(HaveOccurred())
	})

	// Rotating the credentials or the CA bundle re-authenticates the same store
	// against the same server, which the CRD admits too.
	t.Run("credential and CA rotation stay mutable", func(t *testing.T) {
		g := NewGomegaWithT(t)
		oldCP := barbicanControlPlane()
		oldCP.Spec.Services.Barbican.SecretStore = ServiceBarbicanSecretStoreSpec{
			External: externalBarbicanStore(),
		}
		newCP := oldCP.DeepCopy()
		newCP.Spec.Services.Barbican.SecretStore.External.CredentialsSecretRef.Name = "barbican-approle-v2"
		newCP.Spec.Services.Barbican.SecretStore.External.CABundleSecretRef = &barbicanv1alpha1.SecretNameRefSpec{Name: "openbao-ca-v2"}

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).NotTo(HaveOccurred())
	})

	// The server ADDRESS is not a rotation: nothing downstream freezes
	// spec.openBao.server.url, so the new address is SSA-updated into the live
	// store in place and every store and retrieve goes to it from the next pass —
	// which is why `patch controlplanes` alone must not re-point a live key
	// manager at a server of the actor's choosing.
	t.Run("server URL is frozen", func(t *testing.T) {
		g := NewGomegaWithT(t)
		oldCP := barbicanControlPlane()
		oldCP.Spec.Services.Barbican.SecretStore = ServiceBarbicanSecretStoreSpec{
			External: externalBarbicanStore(),
		}
		newCP := oldCP.DeepCopy()
		newCP.Spec.Services.Barbican.SecretStore.External.URL = "https://openbao.attacker.example:8200"
		newCP.Spec.Services.Barbican.SecretStore.External.CredentialsSecretRef.Name = "attacker-approle"

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.barbican.secretStore.external.url"))
		g.Expect(err.Error()).To(ContainSubstring("url is immutable"))
	})

	// The OpenBao namespace scopes every request, so moving it strands the
	// material the same way a new server does.
	t.Run("OpenBao namespace is frozen", func(t *testing.T) {
		g := NewGomegaWithT(t)
		oldCP := barbicanControlPlane()
		ext := externalBarbicanStore()
		ext.Namespace = "tenant-a"
		oldCP.Spec.Services.Barbican.SecretStore = ServiceBarbicanSecretStoreSpec{External: ext}
		newCP := oldCP.DeepCopy()
		newCP.Spec.Services.Barbican.SecretStore.External.Namespace = "tenant-b"

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.barbican.secretStore.external.namespace"))
		g.Expect(err.Error()).To(ContainSubstring("namespace is immutable"))
	})

	// Dropping services.barbican preserves the child, the store and the whole
	// OpenBao ensemble, so a re-add while status still reports the service must
	// not be able to carry a store the two-revision comparison would have caught.
	t.Run("dropped and re-added barbican cannot re-declare its store", func(t *testing.T) {
		g := NewGomegaWithT(t)
		oldCP := barbicanControlPlane()
		oldCP.Spec.Services.Barbican = nil
		oldCP.Status.Services = []ServiceStatus{{Name: "barbican", Ready: true}}
		newCP := barbicanControlPlane()
		newCP.Spec.Services.Barbican.SecretStore = ServiceBarbicanSecretStoreSpec{
			External: externalBarbicanStore(),
		}

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.barbican.secretStore"))
		g.Expect(err.Error()).To(ContainSubstring("dropped from spec while the ControlPlane still reports it"))

		// The way out the message names has to be one admission can actually
		// accept. "Restore the previous services.barbican block" was not: restoring
		// it byte-for-byte is itself a re-add while status still reports the
		// service, so the only revision that could have satisfied the instruction
		// was the one it was attached to rejecting. The escape is the window
		// closing — the controller prunes status.services one pass after it
		// observes the drop — and past that point the re-add is admitted.
		g.Expect(err.Error()).To(ContainSubstring("status.services"))
		oldCP.Status.Services = nil
		_, err = w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).NotTo(HaveOccurred(), "the escape the refusal names must actually be admitted")
	})

	// The update that FIRST declares the service — on a ControlPlane that carries
	// no Barbican in spec and never reported one in status — names its mode freely.
	t.Run("newly declared barbican is free", func(t *testing.T) {
		g := NewGomegaWithT(t)
		oldCP := barbicanControlPlane()
		oldCP.Spec.Services.Barbican = nil
		newCP := barbicanControlPlane()

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).NotTo(HaveOccurred())
	})
}

// --- per-service target-cluster placement (issue #840) ---

// placedGateway is the minimal gateway block a placed service publishes itself
// through, the alternative the publicEndpoint requirement accepts.
func placedGateway(hostname string) *commonv1.GatewaySpec {
	return &commonv1.GatewaySpec{
		ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		Hostname:  hostname,
	}
}

// publishKeystone gives a ControlPlane the Keystone publicEndpoint every service
// placed away from Keystone requires — that service validates its tokens against
// Keystone over the public URL. The placement fixtures below set it for every
// service but Keystone itself, so each subtest fails on the rule it is about
// rather than on this one.
func publishKeystone(cp *ControlPlane) {
	cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.com/v3"
}

// placedService drives the placement tests over all five services at once. place
// returns a ControlPlane that declares the service on the "edge" cluster in a
// namespace of its own and advertises NOTHING of its own (Keystone excepted, see
// publishKeystone), so each test varies exactly the field its rule reads:
// dropNamespace takes the namespace away, publish adds a publicEndpoint, route
// adds a gateway.
type placedService struct {
	name string
	// catalog marks the services the ControlPlane advertises in the Keystone
	// service catalog, the ones that carry the publicEndpoint-or-gateway
	// requirement. Horizon is the exemption.
	catalog       bool
	place         func() *ControlPlane
	dropNamespace func(cp *ControlPlane)
	publish       func(cp *ControlPlane)
	route         func(cp *ControlPlane)
}

func placedServices() []placedService {
	return []placedService{
		{
			name: "keystone", catalog: true,
			place: func() *ControlPlane {
				cp := validControlPlane()
				cp.Name = "cp"
				cp.Namespace = "openstack"
				cp.Spec.Services.Keystone.Namespace = &ServiceNamespaceSpec{
					Name: "identity", Lifecycle: ServiceNamespaceLifecycleManaged,
				}
				cp.Spec.Services.Keystone.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge"}
				return cp
			},
			dropNamespace: func(cp *ControlPlane) { cp.Spec.Services.Keystone.Namespace = nil },
			publish: func(cp *ControlPlane) {
				cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.com/v3"
			},
			route: func(cp *ControlPlane) {
				cp.Spec.Services.Keystone.Gateway = placedGateway("keystone.example.com")
			},
		},
		{
			name: "horizon", catalog: false,
			place: func() *ControlPlane {
				cp := validControlPlane()
				cp.Name = "cp"
				cp.Namespace = "openstack"
				cp.Spec.Services.Horizon = &ServiceHorizonSpec{
					Namespace: &ServiceNamespaceSpec{
						Name: "dashboard", Lifecycle: ServiceNamespaceLifecycleManaged,
					},
					TargetClusterRef: &commonv1.TargetClusterRefSpec{Name: "edge"},
				}
				publishKeystone(cp)
				return cp
			},
			dropNamespace: func(cp *ControlPlane) { cp.Spec.Services.Horizon.Namespace = nil },
			publish: func(cp *ControlPlane) {
				cp.Spec.Services.Horizon.PublicEndpoint = "https://horizon.example.com"
			},
			route: func(cp *ControlPlane) {
				cp.Spec.Services.Horizon.Gateway = placedGateway("horizon.example.com")
			},
		},
		{
			name: "glance", catalog: true,
			place: func() *ControlPlane {
				cp := glanceControlPlane()
				cp.Namespace = "openstack"
				cp.Spec.Services.Glance.Namespace = &ServiceNamespaceSpec{
					Name: "images", Lifecycle: ServiceNamespaceLifecycleManaged,
				}
				cp.Spec.Services.Glance.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge"}
				publishKeystone(cp)
				return cp
			},
			dropNamespace: func(cp *ControlPlane) {
				cp.Spec.Services.Glance.Namespace = nil
			},
			publish: func(cp *ControlPlane) {
				cp.Spec.Services.Glance.PublicEndpoint = "https://glance.example.com"
			},
			route: func(cp *ControlPlane) {
				cp.Spec.Services.Glance.Gateway = placedGateway("glance.example.com")
			},
		},
		{
			name: "placement", catalog: true,
			place: func() *ControlPlane {
				cp := placementControlPlane()
				cp.Namespace = "openstack"
				cp.Spec.Services.Placement.Namespace = &ServiceNamespaceSpec{
					Name: "placement", Lifecycle: ServiceNamespaceLifecycleManaged,
				}
				cp.Spec.Services.Placement.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge"}
				publishKeystone(cp)
				return cp
			},
			dropNamespace: func(cp *ControlPlane) {
				cp.Spec.Services.Placement.Namespace = nil
			},
			publish: func(cp *ControlPlane) {
				cp.Spec.Services.Placement.PublicEndpoint = "https://placement.example.com"
			},
			route: func(cp *ControlPlane) {
				cp.Spec.Services.Placement.Gateway = placedGateway("placement.example.com")
			},
		},
		{
			name: "barbican", catalog: true,
			place: func() *ControlPlane {
				cp := barbicanControlPlane()
				cp.Namespace = "openstack"
				cp.Spec.Services.Barbican.Namespace = &ServiceNamespaceSpec{
					Name: "keymanager", Lifecycle: ServiceNamespaceLifecycleManaged,
				}
				cp.Spec.Services.Barbican.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge"}
				publishKeystone(cp)
				return cp
			},
			dropNamespace: func(cp *ControlPlane) {
				cp.Spec.Services.Barbican.Namespace = nil
			},
			publish: func(cp *ControlPlane) {
				cp.Spec.Services.Barbican.PublicEndpoint = "https://barbican.example.com"
			},
			route: func(cp *ControlPlane) {
				cp.Spec.Services.Barbican.Gateway = placedGateway("barbican.example.com")
			},
		},
	}
}

// TestValidateCreate_RejectsEmptyTargetClusterName pins the webhook layer of the
// name-only shape. The MinLength marker on the CRD catches this in a real
// cluster; the shared validator is what a caller bypassing schema admission
// meets, and without it an empty name would resolve to no cluster at all.
func TestValidateCreate_RejectsEmptyTargetClusterName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Name = "cp"
	cp.Namespace = "openstack"
	cp.Spec.Services.Keystone.Namespace = &ServiceNamespaceSpec{
		Name: "identity", Lifecycle: ServiceNamespaceLifecycleManaged,
	}
	cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.com/v3"
	cp.Spec.Services.Keystone.TargetClusterRef = &commonv1.TargetClusterRefSpec{}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.services.keystone.targetClusterRef.name"))
	g.Expect(err.Error()).To(ContainSubstring("target cluster name must be set"))
}

// TestValidateCreate_RequiresANamespaceForAPlacedService pins the dedicated-namespace
// rule for every service: a namespace maps to exactly one cluster, and the
// ControlPlane's own stays on the local one, so a service placed elsewhere
// without a namespace of its own would have its database, its tenant store, and
// its credential material provisioned on a cluster its workload does not run on.
func TestValidateCreate_RequiresANamespaceForAPlacedService(t *testing.T) {
	w := &ControlPlaneWebhook{}
	for _, tc := range placedServices() {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := tc.place()
			tc.publish(cp)
			tc.dropNamespace(cp)

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("spec.services." + tc.name + ".namespace"))
			g.Expect(err.Error()).To(ContainSubstring("a placed service needs a namespace of its own"))
		})
	}
}

// TestValidateCreate_RequiresAPublicAddressForAPlacedCatalogService pins the
// reachability rule and its one exemption. What the ControlPlane registers in
// the catalog for an unpublished service is its in-cluster Service DNS name,
// which resolves nowhere outside the cluster the service runs on; horizon is not
// in the catalog, so it is placed without publishing anything.
func TestValidateCreate_RequiresAPublicAddressForAPlacedCatalogService(t *testing.T) {
	w := &ControlPlaneWebhook{}
	for _, tc := range placedServices() {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("neither publicEndpoint nor gateway", func(t *testing.T) {
				g := NewGomegaWithT(t)
				_, err := w.ValidateCreate(context.Background(), tc.place())
				if !tc.catalog {
					g.Expect(err).NotTo(HaveOccurred(),
						"the dashboard is not in the service catalog, so nothing looks it up there")
					return
				}
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring("spec.services." + tc.name + ".publicEndpoint"))
				g.Expect(err.Error()).To(ContainSubstring("resolves nowhere from another cluster"))
			})

			t.Run("a publicEndpoint satisfies it", func(t *testing.T) {
				g := NewGomegaWithT(t)
				cp := tc.place()
				tc.publish(cp)

				_, err := w.ValidateCreate(context.Background(), cp)
				g.Expect(err).NotTo(HaveOccurred())
			})

			t.Run("a gateway satisfies it", func(t *testing.T) {
				g := NewGomegaWithT(t)
				cp := tc.place()
				tc.route(cp)

				_, err := w.ValidateCreate(context.Background(), cp)
				g.Expect(err).NotTo(HaveOccurred())
			})
		})
	}
}

// TestValidateCreate_RejectsDisagreeingTargetClustersInOneNamespace pins the
// co-location rule one level out from the lifecycle agreement: a namespace
// exists on exactly one cluster, together with the backing services and the
// tenant store scoped to it, so the services sharing it cannot disagree on which
// cluster that is. "One placed, one not" is a disagreement too.
func TestValidateCreate_RejectsDisagreeingTargetClustersInOneNamespace(t *testing.T) {
	w := &ControlPlaneWebhook{}
	// Keystone and the dashboard co-located in one namespace, each placed as the
	// case under test says.
	colocated := func(keystone, horizon *commonv1.TargetClusterRefSpec) *ControlPlane {
		cp := validControlPlane()
		cp.Name = "cp"
		cp.Namespace = "openstack"
		cp.Spec.Services.Keystone.Namespace = &ServiceNamespaceSpec{
			Name: "shared", Lifecycle: ServiceNamespaceLifecycleManaged,
		}
		cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.com/v3"
		cp.Spec.Services.Keystone.TargetClusterRef = keystone
		cp.Spec.Services.Horizon = &ServiceHorizonSpec{
			Namespace: &ServiceNamespaceSpec{
				Name: "shared", Lifecycle: ServiceNamespaceLifecycleManaged,
			},
			TargetClusterRef: horizon,
		}
		return cp
	}

	t.Run("two clusters", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := colocated(
			&commonv1.TargetClusterRefSpec{Name: "edge"},
			&commonv1.TargetClusterRefSpec{Name: "core"},
		)

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.horizon.targetClusterRef"))
		g.Expect(err.Error()).To(ContainSubstring(
			`services co-located in namespace "shared" must be placed on the same target cluster`,
		))
	})

	t.Run("placed next to unplaced", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := colocated(&commonv1.TargetClusterRefSpec{Name: "edge"}, nil)

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("must be placed on the same target cluster"))
	})

	t.Run("one cluster", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := colocated(
			&commonv1.TargetClusterRefSpec{Name: "edge"},
			&commonv1.TargetClusterRefSpec{Name: "edge"},
		)

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
	})
}

// TestValidateCreate_RequiresAPublishedKeystoneWhenAServiceIsPlacedAwayFromIt
// closes the gap the per-service catalog rule leaves: it demands publication of a
// service carrying a ref of its OWN, so an UNPLACED Keystone was never asked for
// one. A service on another cluster still validates its tokens against Keystone,
// cannot resolve its in-cluster Service DNS name, and would be projected with an
// EMPTY spec.keystoneEndpoint — which the child's CRD refuses (MinLength=1,
// ^https?://) on every pass, with nothing on the ControlPlane naming the field.
func TestValidateCreate_RequiresAPublishedKeystoneWhenAServiceIsPlacedAwayFromIt(t *testing.T) {
	w := &ControlPlaneWebhook{}
	// Glance placed on "edge", Keystone left at home. place() publishes Keystone,
	// so each case takes that back out and puts its own answer in.
	glancePlaced := func() *ControlPlane {
		for _, tc := range placedServices() {
			if tc.name == "glance" {
				cp := tc.place()
				cp.Spec.Services.Glance.PublicEndpoint = "https://glance.example.com"
				cp.Spec.Services.Keystone.PublicEndpoint = ""
				return cp
			}
		}
		t.Fatal("no glance case in placedServices()")
		return nil
	}

	t.Run("an unpublished local Keystone is rejected", func(t *testing.T) {
		g := NewGomegaWithT(t)

		_, err := w.ValidateCreate(context.Background(), glancePlaced())
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.keystone.publicEndpoint"))
		g.Expect(err.Error()).To(ContainSubstring("cannot resolve Keystone's in-cluster Service DNS name"))
	})

	t.Run("a publicEndpoint satisfies it", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := glancePlaced()
		cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.com/v3"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
	})

	t.Run("a gateway satisfies it", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := glancePlaced()
		cp.Spec.Services.Keystone.Gateway = placedGateway("keystone.example.com")

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
	})

	// The dashboard is exempt from the CATALOG rule, not from this one: Horizon
	// validates tokens against Keystone like every other service.
	t.Run("the dashboard triggers it too", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := validControlPlane()
		cp.Name = "cp"
		cp.Namespace = "openstack"
		cp.Spec.Services.Horizon = &ServiceHorizonSpec{
			Namespace: &ServiceNamespaceSpec{
				Name: "dashboard", Lifecycle: ServiceNamespaceLifecycleManaged,
			},
			TargetClusterRef: &commonv1.TargetClusterRefSpec{Name: "edge"},
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.keystone.publicEndpoint"))
	})

	// Nothing crosses a cluster boundary when everything stays at home, so an
	// unpublished Keystone remains the perfectly ordinary default.
	t.Run("an all-local ControlPlane is unaffected", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := validControlPlane()
		cp.Name = "cp"
		cp.Namespace = "openstack"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
	})
}

// TestValidateCreate_RejectsAPlaintextPlacedKeystoneEndpoint pins the scheme rule
// placement adds to services.keystone.publicEndpoint. Co-located, that URL only
// feeds the bootstrap and the catalog's public identity row; the moment a cluster
// boundary separates Keystone from anything, it becomes the auth_url every
// clouds.yaml renders the admin password and each service-account password NEXT
// TO, and it is dialled across that boundary on every mint, re-mint and delivery.
// http:// there hands an on-path observer a credential with the admin role on the
// whole control plane.
//
// The boundary is crossed from EITHER side, so the rule cannot key on Keystone's
// own ref: a service placed away from an unplaced Keystone reaches the very same
// URL, and reads it back out of its projected spec.keystoneEndpoint into
// [keystone_authtoken] on every token validation.
func TestValidateCreate_RejectsAPlaintextPlacedKeystoneEndpoint(t *testing.T) {
	w := &ControlPlaneWebhook{}
	placedKeystone := func(endpoint string) *ControlPlane {
		cp := validControlPlane()
		cp.Name = "cp"
		cp.Namespace = "openstack"
		cp.Spec.Services.Keystone.Namespace = &ServiceNamespaceSpec{
			Name: "identity", Lifecycle: ServiceNamespaceLifecycleManaged,
		}
		cp.Spec.Services.Keystone.PublicEndpoint = endpoint
		cp.Spec.Services.Keystone.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge"}
		return cp
	}
	// The mirror image: Keystone stays on the local cluster and Glance moves to
	// "edge". Everything a placed service needs is present, so the caller varies
	// nothing but Keystone's endpoint.
	glancePlacedAwayFromKeystone := func() *ControlPlane {
		cp := glanceControlPlane()
		cp.Namespace = "openstack"
		cp.Spec.Services.Glance.Namespace = &ServiceNamespaceSpec{
			Name: "images", Lifecycle: ServiceNamespaceLifecycleManaged,
		}
		cp.Spec.Services.Glance.PublicEndpoint = "https://glance.example.com"
		cp.Spec.Services.Glance.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge"}
		return cp
	}

	t.Run("http is rejected", func(t *testing.T) {
		g := NewGomegaWithT(t)

		_, err := w.ValidateCreate(context.Background(), placedKeystone("http://keystone.example.com:5000/v3"))
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.keystone.publicEndpoint"))
		g.Expect(err.Error()).To(ContainSubstring("must use scheme https when any service is placed away from Keystone"))
	})

	t.Run("https is admitted", func(t *testing.T) {
		g := NewGomegaWithT(t)

		_, err := w.ValidateCreate(context.Background(), placedKeystone("https://keystone.example.com/v3"))
		g.Expect(err).NotTo(HaveOccurred())
	})

	// The symmetric case, and the one a rule keyed on Keystone's own ref admits:
	// Keystone stays at home while Glance moves to "edge". The operator hands
	// Glance exactly this URL as its spec.keystoneEndpoint and delivers its
	// service-account password beside it, so the same credentials cross the same
	// boundary — the direction of the placement changes nothing.
	t.Run("a service placed away from an http Keystone is rejected", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := glancePlacedAwayFromKeystone()
		cp.Spec.Services.Keystone.PublicEndpoint = "http://keystone.example.com:5000/v3"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.keystone.publicEndpoint"))
		g.Expect(err.Error()).To(ContainSubstring("must use scheme https when any service is placed away from Keystone"))
	})

	t.Run("https satisfies it from that side too", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := glancePlacedAwayFromKeystone()
		cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.com/v3"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
	})

	// The rule is placement's, not the endpoint's: with nothing placed at all, the
	// publicEndpoint feeds the catalog and the bootstrap, never a credential
	// document, so http:// there stays admissible exactly as before.
	t.Run("an all-local ControlPlane keeps its http endpoint", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := placedKeystone("http://keystone.example.com:5000/v3")
		cp.Spec.Services.Keystone.TargetClusterRef = nil

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
	})
}

// TestKeystoneCABundleSecretRef covers the trust anchor a PLACED Keystone needs:
// K-ORC dials its https publicEndpoint from the management cluster with nothing
// but the container's system trust store, so without this field a target
// published with a private CA — the default posture of this stack — could never
// be verified and the only working configuration would be the plaintext one.
func TestKeystoneCABundleSecretRef(t *testing.T) {
	w := &ControlPlaneWebhook{}
	placed := func() *ControlPlane {
		cp := validControlPlane()
		cp.Name = "cp"
		cp.Namespace = "openstack"
		cp.Spec.Services.Keystone.Namespace = &ServiceNamespaceSpec{
			Name: "identity", Lifecycle: ServiceNamespaceLifecycleManaged,
		}
		cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.com/v3"
		cp.Spec.Services.Keystone.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge"}
		cp.Spec.Services.Keystone.CABundleSecretRef = &commonv1.SecretRefSpec{Name: "edge-keystone-ca"}
		return cp
	}

	t.Run("accepted on a placed Keystone", func(t *testing.T) {
		g := NewGomegaWithT(t)

		_, err := w.ValidateCreate(context.Background(), placed())
		g.Expect(err).NotTo(HaveOccurred())
	})

	t.Run("the key defaults to ca.crt", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := placed()

		g.Expect(w.Default(context.Background(), cp)).To(Succeed())
		g.Expect(cp.Spec.Services.Keystone.CABundleSecretRef.Key).To(Equal(DefaultCABundleSecretKey))
	})

	t.Run("a nameless ref is rejected", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := placed()
		cp.Spec.Services.Keystone.CABundleSecretRef = &commonv1.SecretRefSpec{}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.keystone.caBundleSecretRef.name"))
	})

	// A co-located Keystone is dialled over its in-cluster Service URL, which
	// performs no handshake: accepting a bundle there would report trust nothing
	// verifies, the hazard the External-mode plaintext rule rejects.
	t.Run("forbidden without a targetClusterRef", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := placed()
		cp.Spec.Services.Keystone.TargetClusterRef = nil

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.keystone.caBundleSecretRef"))
		g.Expect(err.Error()).To(ContainSubstring("forbidden without services.keystone.targetClusterRef"))
	})

	t.Run("forbidden in External mode", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := externalControlPlane()
		cp.Spec.Services.Keystone.CABundleSecretRef = &commonv1.SecretRefSpec{Name: "edge-keystone-ca"}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.keystone.caBundleSecretRef"))
		g.Expect(err.Error()).To(ContainSubstring("use services.keystone.external.caBundleSecretRef"))
	})

	// The bundle reaches K-ORC and nothing else. A service that does not share
	// Keystone's cluster gets the same https URL as its projected
	// spec.keystoneEndpoint and renders it into [keystone_authtoken], which has no
	// cafile option — so it would fail every token validation with no field on any
	// CR in the tree to supply the anchor with, while the ControlPlane reports
	// KORCReady=True. Admission refuses the combination instead.
	t.Run("forbidden while a service does not share Keystone's cluster", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			ref  *commonv1.TargetClusterRefSpec
		}{
			{"the dashboard stays at home", nil},
			{"the dashboard is on a third cluster", &commonv1.TargetClusterRefSpec{Name: "core"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				g := NewGomegaWithT(t)
				cp := placed()
				cp.Spec.Services.Horizon = &ServiceHorizonSpec{
					Namespace: &ServiceNamespaceSpec{
						Name: "dashboard", Lifecycle: ServiceNamespaceLifecycleManaged,
					},
					TargetClusterRef: tc.ref,
				}

				_, err := w.ValidateCreate(context.Background(), cp)
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring("spec.services.keystone.caBundleSecretRef"))
				g.Expect(err.Error()).To(ContainSubstring(
					"forbidden while a service does not share Keystone's target cluster",
				))
			})
		}
	})

	// Co-located on the target, every service reaches Keystone over its in-cluster
	// Service URL, which performs no handshake at all: only K-ORC, back on the
	// management cluster, dials the https endpoint, and the bundle reaches it.
	t.Run("accepted when every service shares Keystone's cluster", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := placed()
		cp.Spec.Services.Horizon = &ServiceHorizonSpec{
			Namespace: &ServiceNamespaceSpec{
				Name: "dashboard", Lifecycle: ServiceNamespaceLifecycleManaged,
			},
			TargetClusterRef: &commonv1.TargetClusterRefSpec{Name: "edge"},
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
	})
}

// TestValidateCreate_RejectsTargetClusterRefInExternalMode verifies the webhook
// mirror of the External-mode CEL forbid rule: no Keystone workload is deployed,
// so there is nothing to place.
func TestValidateCreate_RejectsTargetClusterRefInExternalMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := externalControlPlane()
	cp.Spec.Services.Keystone.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge"}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.keystone.targetClusterRef"))
	g.Expect(err.Error()).To(ContainSubstring("there is nothing to place"))
	// The placement prerequisites stay out of the External matrix: demanding the
	// namespace and the publicEndpoint that the same response forbids would leave
	// the operator no shape to write.
	g.Expect(err.Error()).NotTo(ContainSubstring("a placed service needs a namespace of its own"))
	g.Expect(err.Error()).NotTo(ContainSubstring("resolves nowhere from another cluster"))
}

// TestValidateUpdate_FreezesServiceTargetClusterRefs pins the create-only freeze:
// adding, removing, and renaming the ref are all rejected, because re-pointing a
// live service strands its workload, its database, and the material in its
// tenant store on the cluster it came from.
func TestValidateUpdate_FreezesServiceTargetClusterRefs(t *testing.T) {
	w := &ControlPlaneWebhook{}
	// A live ControlPlane whose Keystone is placed on "edge".
	placed := func() *ControlPlane {
		cp := validControlPlane()
		cp.Name = "cp"
		cp.Namespace = "openstack"
		cp.Spec.Services.Keystone.Namespace = &ServiceNamespaceSpec{
			Name: "identity", Lifecycle: ServiceNamespaceLifecycleManaged,
		}
		cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.com/v3"
		cp.Spec.Services.Keystone.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge"}
		return cp
	}

	t.Run("placing a live service", func(t *testing.T) {
		g := NewGomegaWithT(t)
		newCP := placed()
		oldCP := newCP.DeepCopy()
		oldCP.Spec.Services.Keystone.TargetClusterRef = nil

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.keystone.targetClusterRef"))
		g.Expect(err.Error()).To(ContainSubstring("targetClusterRef is immutable"))
	})

	t.Run("unplacing a live service", func(t *testing.T) {
		g := NewGomegaWithT(t)
		oldCP := placed()
		newCP := oldCP.DeepCopy()
		newCP.Spec.Services.Keystone.TargetClusterRef = nil

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("targetClusterRef is immutable"))
	})

	t.Run("re-pointing a live service", func(t *testing.T) {
		g := NewGomegaWithT(t)
		oldCP := placed()
		newCP := oldCP.DeepCopy()
		newCP.Spec.Services.Keystone.TargetClusterRef.Name = "core"

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.keystone.targetClusterRef.name"))
		g.Expect(err.Error()).To(ContainSubstring("targetClusterRef is immutable"))
	})

	t.Run("an unchanged ref is not rejected", func(t *testing.T) {
		g := NewGomegaWithT(t)
		oldCP := placed()
		newCP := oldCP.DeepCopy()
		newCP.Spec.OpenStackRelease = "2026.1"

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).NotTo(HaveOccurred())
	})

	// The serviceDeclaredBefore carve-out: a service the old revision carries
	// neither in spec nor in status is being CREATED by this update, so it names
	// its cluster freely: there is no workload anywhere to strand.
	t.Run("a newly declared service is placed freely", func(t *testing.T) {
		g := NewGomegaWithT(t)
		oldCP := validControlPlane()
		oldCP.Name = "cp"
		oldCP.Namespace = "openstack"
		gl := glanceControlPlane()
		newCP := oldCP.DeepCopy()
		newCP.Spec.Services.Glance = gl.Spec.Services.Glance
		newCP.Spec.Services.Glance.Namespace = &ServiceNamespaceSpec{
			Name: "images", Lifecycle: ServiceNamespaceLifecycleManaged,
		}
		newCP.Spec.Services.Glance.PublicEndpoint = "https://glance.example.com"
		newCP.Spec.Services.Glance.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge"}
		publishKeystone(newCP)

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).NotTo(HaveOccurred())
	})
}

// --- spec.infrastructure.messaging (issue #895) ---
//
// The shared message bus is opt-in: the defaulting webhook fills the leaves of a
// declared block but never creates the block itself, and once declared in MANAGED
// mode the block is frozen as a one-way add (a brownfield one provisions nothing,
// so it can still be dropped).

// TestDefault_MessagingNotMaterialized pins the opt-in contract: a ControlPlane
// that declares no messaging must come out of the defaulting webhook with none,
// including the bare CR whose spec.infrastructure the webhook does allocate.
func TestDefault_MessagingNotMaterialized(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	cp := managedControlPlane()
	g.Expect(w.Default(context.Background(), cp)).To(Succeed())
	g.Expect(cp.Spec.Infrastructure.Messaging).To(BeNil())

	bare := &ControlPlane{}
	g.Expect(w.Default(context.Background(), bare)).To(Succeed())
	g.Expect(bare.Spec.Infrastructure).NotTo(BeNil())
	g.Expect(bare.Spec.Infrastructure.Messaging).To(BeNil(),
		"allocating the infrastructure block must not invent a message bus")
}

// TestDefault_MessagingLeaves covers the leaf defaults of a DECLARED messaging
// block: the managed clusterRef is invented only when the brownfield
// discriminator (secretRef) is unset, an explicit value is preserved, and the
// well-known Secret keys are filled.
func TestDefault_MessagingLeaves(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   *commonv1.MessagingSpec
		// An empty want means the corresponding block must stay nil.
		wantClusterRefName string
		wantSecretKey      string
		wantCABundleKey    string
	}{
		{
			name:               "empty block becomes managed",
			in:                 &commonv1.MessagingSpec{},
			wantClusterRefName: DefaultMessagingClusterRefName,
		},
		{
			name:               "empty clusterRef name is filled",
			in:                 &commonv1.MessagingSpec{ClusterRef: &corev1.LocalObjectReference{}},
			wantClusterRefName: DefaultMessagingClusterRefName,
		},
		{
			name:          "brownfield secretRef gets the transport-url key and no clusterRef",
			in:            &commonv1.MessagingSpec{SecretRef: &commonv1.SecretRefSpec{Name: "bus-url"}},
			wantSecretKey: commonv1.DefaultTransportURLSecretKey,
		},
		{
			name: "explicit clusterRef is preserved and the tls key is filled",
			in: &commonv1.MessagingSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "x"},
				TLS:        &commonv1.MessagingTLSSpec{CABundleSecretRef: commonv1.SecretRefSpec{Name: "ca"}},
			},
			wantClusterRefName: "x",
			wantCABundleKey:    DefaultCABundleSecretKey,
		},
		{
			name: "explicit keys are never overwritten",
			in: &commonv1.MessagingSpec{
				SecretRef: &commonv1.SecretRefSpec{Name: "bus-url", Key: "url"},
				TLS:       &commonv1.MessagingTLSSpec{CABundleSecretRef: commonv1.SecretRefSpec{Name: "ca", Key: "bundle.pem"}},
			},
			wantSecretKey:   "url",
			wantCABundleKey: "bundle.pem",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &ControlPlaneWebhook{}
			cp := managedControlPlane()
			cp.Spec.Infrastructure.Messaging = tc.in

			g.Expect(w.Default(context.Background(), cp)).To(Succeed())

			m := cp.Spec.Infrastructure.Messaging
			g.Expect(m).NotTo(BeNil())
			if tc.wantClusterRefName == "" {
				g.Expect(m.ClusterRef).To(BeNil(), "a brownfield bus must not grow a managed clusterRef")
			} else {
				g.Expect(m.ClusterRef).NotTo(BeNil())
				g.Expect(m.ClusterRef.Name).To(Equal(tc.wantClusterRefName))
			}
			if tc.wantSecretKey == "" {
				g.Expect(m.SecretRef).To(BeNil())
			} else {
				g.Expect(m.SecretRef).NotTo(BeNil())
				g.Expect(m.SecretRef.Key).To(Equal(tc.wantSecretKey))
			}
			if tc.wantCABundleKey == "" {
				g.Expect(m.TLS).To(BeNil())
			} else {
				g.Expect(m.TLS).NotTo(BeNil())
				g.Expect(m.TLS.CABundleSecretRef.Key).To(Equal(tc.wantCABundleKey))
			}

			// A second pass must change nothing.
			before := cp.DeepCopy()
			g.Expect(w.Default(context.Background(), cp)).To(Succeed())
			g.Expect(cp.Spec.Infrastructure).To(Equal(before.Spec.Infrastructure))
		})
	}
}

// TestValidateCreate_RejectsMessagingXOR verifies the webhook twin of the CEL
// rule on commonv1.MessagingSpec. The fixtures skip the defaulting webhook on
// purpose: defaulting would turn the neither-set case into a managed block.
func TestValidateCreate_RejectsMessagingXOR(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   *commonv1.MessagingSpec
	}{
		{
			name: "both set",
			in: &commonv1.MessagingSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: DefaultMessagingClusterRefName},
				SecretRef:  &commonv1.SecretRefSpec{Name: "bus-url", Key: commonv1.DefaultTransportURLSecretKey},
			},
		},
		{
			name: "neither set",
			in:   &commonv1.MessagingSpec{Replicas: 3},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &ControlPlaneWebhook{}
			cp := validControlPlane()
			cp.Spec.Infrastructure.Messaging = tc.in

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("spec.infrastructure.messaging"))
			g.Expect(err.Error()).To(ContainSubstring("exactly one of clusterRef or secretRef must be set"))
		})
	}
}

// TestValidateCreate_RejectsMessagingEmptyNames covers the Secret references a
// declared bus points at: an unnamed brownfield Secret or CA bundle resolves to
// nothing at reconcile time, so it is rejected at admission.
func TestValidateCreate_RejectsMessagingEmptyNames(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in       *commonv1.MessagingSpec
		wantPath string
	}{
		{
			name:     "brownfield without a Secret name",
			in:       &commonv1.MessagingSpec{SecretRef: &commonv1.SecretRefSpec{Key: commonv1.DefaultTransportURLSecretKey}},
			wantPath: "spec.infrastructure.messaging.secretRef.name",
		},
		{
			// Brownfield, because tls is only supported there: a managed bus would
			// trip the mode rejection too and stop isolating the empty name.
			name: "tls without a CA bundle Secret name",
			in: &commonv1.MessagingSpec{
				SecretRef: &commonv1.SecretRefSpec{Name: "bus-url", Key: commonv1.DefaultTransportURLSecretKey},
				TLS:       &commonv1.MessagingTLSSpec{CABundleSecretRef: commonv1.SecretRefSpec{Key: DefaultCABundleSecretKey}},
			},
			wantPath: "spec.infrastructure.messaging.tls.caBundleSecretRef.name",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &ControlPlaneWebhook{}
			cp := validControlPlane()
			cp.Spec.Infrastructure.Messaging = tc.in

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.wantPath))
		})
	}
}

// TestValidateCreate_RejectsMessagingTLSInManagedMode pins that client trust is a
// brownfield-only leaf. ensureRabbitMQ projects spec.replicas and nothing else,
// so a managed broker comes up on the RabbitMQ Cluster Operator's default,
// plaintext listener: a tls block beside a clusterRef would ask for an encrypted
// connection nothing provisions, and the mismatch would only surface when the
// first consumer rendered ssl = true against a broker that never had a listener
// for it.
func TestValidateCreate_RejectsMessagingTLSInManagedMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Infrastructure.Messaging = &commonv1.MessagingSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: DefaultMessagingClusterRefName},
		TLS: &commonv1.MessagingTLSSpec{
			CABundleSecretRef: commonv1.SecretRefSpec{Name: "rabbitmq-ca", Key: DefaultCABundleSecretKey},
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.infrastructure.messaging.tls"))
	g.Expect(err.Error()).To(ContainSubstring("tls is supported in brownfield mode only"))
}

// TestValidateCreate_AcceptsMessagingTLSInBrownfield is the acceptance twin: the
// same tls block on a brownfield bus is admitted.
func TestValidateCreate_AcceptsMessagingTLSInBrownfield(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Infrastructure.Messaging = &commonv1.MessagingSpec{
		SecretRef: &commonv1.SecretRefSpec{Name: "bus-url", Key: commonv1.DefaultTransportURLSecretKey},
		TLS: &commonv1.MessagingTLSSpec{
			CABundleSecretRef: commonv1.SecretRefSpec{Name: "rabbitmq-ca", Key: DefaultCABundleSecretKey},
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// managedMessaging returns the managed bus the freeze tests start from.
func managedMessaging() *commonv1.MessagingSpec {
	return &commonv1.MessagingSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: DefaultMessagingClusterRefName},
		Replicas:   3,
	}
}

// TestValidateUpdate_MessagingFreeze pins the one-way-add contract: declaring
// the bus on a live ControlPlane is allowed, dropping it — in EITHER mode — or
// reshaping its addressing is not.
func TestValidateUpdate_MessagingFreeze(t *testing.T) {
	for _, tc := range []struct {
		name string
		oldM *commonv1.MessagingSpec
		newM *commonv1.MessagingSpec
		// An empty wantMsg means the update is accepted.
		wantMsg string
	}{
		{
			name: "declaring the bus on a live ControlPlane",
			oldM: nil,
			newM: managedMessaging(),
		},
		{
			name:    "removing a declared MANAGED bus",
			oldM:    managedMessaging(),
			newM:    nil,
			wantMsg: "cannot be removed once declared",
		},
		{
			// Brownfield provisions nothing, so the removal strands no state on its
			// own — but admitting it laundered the mode freeze into a two-step flip:
			// null the brownfield block, then re-add it with a clusterRef, and every
			// consumer is re-pointed at a fresh, empty RabbitmqCluster without a
			// single admission error. Nothing records the previous mode, so the
			// one-step rejection below only holds while this one does too.
			name: "removing a declared BROWNFIELD bus",
			oldM: &commonv1.MessagingSpec{
				SecretRef: &commonv1.SecretRefSpec{Name: "bus-url", Key: commonv1.DefaultTransportURLSecretKey},
			},
			newM:    nil,
			wantMsg: "cannot be removed once declared",
		},
		{
			name: "managed to brownfield",
			oldM: managedMessaging(),
			newM: &commonv1.MessagingSpec{
				SecretRef: &commonv1.SecretRefSpec{Name: "bus-url", Key: commonv1.DefaultTransportURLSecretKey},
			},
			wantMsg: "messaging mode (managed clusterRef vs brownfield secretRef) is immutable",
		},
		{
			name: "managed clusterRef rename",
			oldM: managedMessaging(),
			newM: &commonv1.MessagingSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "openstack-rabbitmq-2"},
				Replicas:   3,
			},
			wantMsg: "managed messaging clusterRef.name is immutable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &ControlPlaneWebhook{}
			oldCP := managedControlPlane()
			oldCP.Spec.Infrastructure.Messaging = tc.oldM
			newCP := managedControlPlane()
			newCP.Spec.Infrastructure.Messaging = tc.newM

			_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
			if tc.wantMsg == "" {
				g.Expect(err).NotTo(HaveOccurred())
				return
			}
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("spec.infrastructure.messaging"))
			g.Expect(err.Error()).To(ContainSubstring(tc.wantMsg))
		})
	}
}

// TestValidateUpdate_AcceptsMessagingMutableLeaves pins the other half of the
// freeze: everything the reconciler re-reads on every pass stays mutable.
func TestValidateUpdate_AcceptsMessagingMutableLeaves(t *testing.T) {
	w := &ControlPlaneWebhook{}

	// Both directions stay mutable. Growing is what the RabbitMQ Cluster Operator
	// supports in place; shrinking is what ensureRabbitMQ converges by recreating
	// the owned cluster. Rejecting the shrink here would leave an oversized bus on
	// a constrained cluster unrepairable — the broker never reaches its declared
	// replica count, so InfrastructureReady never goes True and the only remaining
	// action is deleting the whole ControlPlane.
	for _, tc := range []struct {
		name     string
		replicas int32
	}{
		{name: "replicas scale up", replicas: 5},
		{name: "replicas scale down", replicas: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			oldCP := managedControlPlane()
			oldCP.Spec.Infrastructure.Messaging = managedMessaging()
			newCP := managedControlPlane()
			newCP.Spec.Infrastructure.Messaging = managedMessaging()
			newCP.Spec.Infrastructure.Messaging.Replicas = tc.replicas

			_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
			g.Expect(err).NotTo(HaveOccurred())
		})
	}

	t.Run("brownfield secretRef key change", func(t *testing.T) {
		g := NewGomegaWithT(t)
		brownfield := func(key string) *commonv1.MessagingSpec {
			return &commonv1.MessagingSpec{SecretRef: &commonv1.SecretRefSpec{Name: "bus-url", Key: key}}
		}
		oldCP := managedControlPlane()
		oldCP.Spec.Infrastructure.Messaging = brownfield(commonv1.DefaultTransportURLSecretKey)
		newCP := managedControlPlane()
		newCP.Spec.Infrastructure.Messaging = brownfield("rotated_transport_url")

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).NotTo(HaveOccurred())
	})

	// Client trust is a brownfield-only leaf: a managed broker is provisioned
	// without a TLS listener, so tls beside a clusterRef is rejected outright (see
	// TestValidateCreate_RejectsMessagingTLSInManagedMode).
	t.Run("adding client trust to a brownfield bus", func(t *testing.T) {
		g := NewGomegaWithT(t)
		brownfield := func() *commonv1.MessagingSpec {
			return &commonv1.MessagingSpec{
				SecretRef: &commonv1.SecretRefSpec{Name: "bus-url", Key: commonv1.DefaultTransportURLSecretKey},
			}
		}
		oldCP := managedControlPlane()
		oldCP.Spec.Infrastructure.Messaging = brownfield()
		newCP := managedControlPlane()
		newCP.Spec.Infrastructure.Messaging = brownfield()
		newCP.Spec.Infrastructure.Messaging.TLS = &commonv1.MessagingTLSSpec{
			CABundleSecretRef: commonv1.SecretRefSpec{Name: "rabbitmq-ca", Key: DefaultCABundleSecretKey},
		}

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).NotTo(HaveOccurred())
	})
}

// --- services.neutron ---

// neutronControlPlane returns a managed ControlPlane with a minimal neutron
// block, so the tests below start from an admissible baseline and vary only the
// field (or the INI content) under test. The bus is declared brownfield: the
// network service is the one service the ControlPlane requires
// spec.infrastructure.messaging for, because the Neutron CRD requires
// spec.messaging on the child projected for it.
func neutronControlPlane() *ControlPlane {
	cp := managedControlPlane()
	cp.Name = "cp"
	cp.Spec.Infrastructure.Messaging = &commonv1.MessagingSpec{
		SecretRef: &commonv1.SecretRefSpec{Name: "bus-url"},
	}
	cp.Spec.Services.Neutron = &ServiceNeutronSpec{
		OVN: NeutronOVNSpec{CentralRef: NeutronOVNCentralRef{Name: "ovn"}},
	}
	return cp
}

// TestDefault_NeutronServiceNamespaceLifecycle verifies a declared neutron
// namespace assignment takes the Managed lifecycle default, exactly as the
// keystone/horizon/glance/placement/barbican ones do, and that no assignment is
// invented for a service that declared none.
func TestDefault_NeutronServiceNamespaceLifecycle(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := neutronControlPlane()
	cp.Spec.Services.Neutron.Namespace = &ServiceNamespaceSpec{Name: "network"}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())
	g.Expect(cp.Spec.Services.Neutron.Namespace.Lifecycle).To(Equal(ServiceNamespaceLifecycleManaged))

	bare := neutronControlPlane()
	g.Expect(w.Default(context.Background(), bare)).To(Succeed())
	g.Expect(bare.Spec.Services.Neutron.Namespace).To(BeNil(),
		"an absent assignment means the service stays in the ControlPlane's namespace")
}

// TestDefault_NeutronDedicatedBackingServicesLeaves verifies a declared neutron
// dedicated block takes the same leaf defaults as the shared one, with a managed
// clusterRef name DERIVED from the ControlPlane and credentialsMode materialized
// to Static (a dedicated managed database cannot draw engine-issued credentials).
// A service that declares no dedicated block gets nothing.
func TestDefault_NeutronDedicatedBackingServicesLeaves(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := neutronControlPlane()
	cp.Name = "prod"
	cp.Spec.Services.Neutron.DedicatedBackingServices = &NeutronDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{},
		Cache:    &commonv1.CacheSpec{},
	}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	db := cp.Spec.Services.Neutron.DedicatedBackingServices.Database
	g.Expect(db.ClusterRef).NotTo(BeNil())
	g.Expect(db.ClusterRef.Name).To(Equal("prod" + DedicatedNeutronDatabaseClusterRefSuffix))
	g.Expect(db.Database).To(Equal(DefaultDatabaseName))
	g.Expect(db.SecretRef.Name).To(Equal(DefaultDatabaseSecretName))
	g.Expect(db.CredentialsMode).To(Equal(commonv1.CredentialsModeStatic),
		"a dedicated managed database is Static-only: no per-instance OpenBao engine role exists")

	cache := cp.Spec.Services.Neutron.DedicatedBackingServices.Cache
	g.Expect(cache.ClusterRef).NotTo(BeNil())
	g.Expect(cache.ClusterRef.Name).To(Equal("prod" + DedicatedNeutronCacheClusterRefSuffix))
	g.Expect(cache.Backend).To(Equal(DefaultCacheBackend))

	// Idempotent on the dedicated leaves too.
	before := cp.DeepCopy()
	g.Expect(w.Default(context.Background(), cp)).To(Succeed())
	g.Expect(cp.Spec.Services).To(Equal(before.Spec.Services))

	shared := neutronControlPlane()
	g.Expect(w.Default(context.Background(), shared)).To(Succeed())
	g.Expect(shared.Spec.Services.Neutron.DedicatedBackingServices).To(BeNil(),
		"an absent block means the service shares the ControlPlane-wide instances")
}

// TestDefault_NeutronOVNCentralRefNamespace pins the one default the OVN
// reference takes: an empty namespace is materialized to the ControlPlane's own,
// the value NeutronOVNCentralNamespace() resolves it to anyway, so a CR that
// bypassed this webhook addresses the same central. An explicit namespace stays
// untouched, and a ControlPlane without the network service gets no neutron
// block invented for it.
func TestDefault_NeutronOVNCentralRefNamespace(t *testing.T) {
	w := &ControlPlaneWebhook{}

	t.Run("an empty namespace takes the ControlPlane's own", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := neutronControlPlane()
		cp.Namespace = "openstack"

		g.Expect(w.Default(context.Background(), cp)).To(Succeed())
		g.Expect(cp.Spec.Services.Neutron.OVN.CentralRef.Namespace).To(Equal("openstack"))
		g.Expect(cp.NeutronOVNCentralNamespace()).To(Equal("openstack"))
	})

	t.Run("an explicit namespace is preserved", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := neutronControlPlane()
		cp.Namespace = "openstack"
		cp.Spec.Services.Neutron.OVN.CentralRef.Namespace = "ovn"

		g.Expect(w.Default(context.Background(), cp)).To(Succeed())
		g.Expect(cp.Spec.Services.Neutron.OVN.CentralRef.Namespace).To(Equal("ovn"))
	})

	t.Run("a ControlPlane without the network service", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := managedControlPlane()
		cp.Namespace = "openstack"

		g.Expect(w.Default(context.Background(), cp)).To(Succeed())
		g.Expect(cp.Spec.Services.Neutron).To(BeNil())
	})
}

// TestValidateCreate_AcceptsNeutronControlPlane pins the admissible baseline and
// the longest ControlPlane name the projected Neutron child still fits into.
func TestValidateCreate_AcceptsNeutronControlPlane(t *testing.T) {
	w := &ControlPlaneWebhook{}

	t.Run("the minimal block beside a brownfield bus", func(t *testing.T) {
		g := NewGomegaWithT(t)
		_, err := w.ValidateCreate(context.Background(), neutronControlPlane())
		g.Expect(err).NotTo(HaveOccurred())
	})

	t.Run("a 32-character ControlPlane name", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := neutronControlPlane()
		cp.Name = strings.Repeat("c", neutronv1alpha1.MaxNeutronNameLength-neutronChildNameOverhead)

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
	})
}

// TestValidateCreate_RejectsNeutronInExternalMode verifies the webhook
// cross-field forbid, mirroring services.placement: no Keystone workload is
// deployed, so Neutron has no identity to validate its tokens against.
func TestValidateCreate_RejectsNeutronInExternalMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := externalControlPlane()
	cp.Spec.Services.Neutron = &ServiceNeutronSpec{
		OVN: NeutronOVNSpec{CentralRef: NeutronOVNCentralRef{Name: "ovn"}},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.neutron"))
	g.Expect(err.Error()).To(ContainSubstring("forbidden when services.keystone.mode is External"))
}

// TestValidateCreate_RejectsNeutronWithoutMessaging pins the one prerequisite no
// other service has. The Neutron CRD requires spec.messaging, and the
// ControlPlane derives the child's transport URL from the shared bus, so a
// ControlPlane that declares the network service without one would project a
// child its own admission rejects on every pass.
func TestValidateCreate_RejectsNeutronWithoutMessaging(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := neutronControlPlane()
	cp.Spec.Infrastructure.Messaging = nil

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.infrastructure.messaging"))
	g.Expect(err.Error()).To(ContainSubstring("is required when services.neutron is set"))
}

// TestValidateCreate_RejectsNeutronOVNCentralRefNameEmpty pins the
// defense-in-depth mirror of the MinLength marker on the ref: the ML2/OVN
// mechanism driver writes every network, subnet and port into the named
// central's Northbound database, so a ref naming nothing leaves the child with
// no database to program.
func TestValidateCreate_RejectsNeutronOVNCentralRefNameEmpty(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := neutronControlPlane()
	cp.Spec.Services.Neutron.OVN.CentralRef.Name = ""

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.services.neutron.ovn.centralRef.name"))
	g.Expect(err.Error()).To(ContainSubstring("must be set: it names the OVNCentral"))
}

// TestValidateCreate_RejectsNeutronOVNCentralRefNamespaceShape pins the
// defense-in-depth mirror of the RFC-1123 Pattern marker: the value names a
// Kubernetes namespace the reconciler reads the central from.
func TestValidateCreate_RejectsNeutronOVNCentralRefNamespaceShape(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := neutronControlPlane()
	cp.Spec.Services.Neutron.OVN.CentralRef.Namespace = "Not_Valid"

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.services.neutron.ovn.centralRef.namespace"))
	g.Expect(err.Error()).To(ContainSubstring("must be a lowercase alphanumeric RFC-1123 label"))
}

// TestValidateCreate_NeutronOVNCentralRefNamespaceStaysInsideThePlane pins the
// reach of the one ControlPlane field that addresses another namespace. Naming a
// central the plane does not already reach is not a read-only act: the
// neutron-operator mirrors that central's client Secret into the Neutron
// namespace, so the reference alone would hand this plane a full mTLS identity
// for a foreign plane's Northbound and Southbound databases.
func TestValidateCreate_NeutronOVNCentralRefNamespaceStaysInsideThePlane(t *testing.T) {
	w := &ControlPlaneWebhook{}
	// A ControlPlane in namespace "openstack" that places the network service in a
	// namespace of its own, so both the claimed and the unclaimed case are one
	// field apart.
	base := func(lifecycle ServiceNamespaceLifecycle) *ControlPlane {
		cp := neutronControlPlane()
		cp.Namespace = "openstack"
		cp.Spec.Services.Neutron.Namespace = &ServiceNamespaceSpec{Name: "networking", Lifecycle: lifecycle}
		return cp
	}

	t.Run("own namespace", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := neutronControlPlane()
		cp.Namespace = "openstack"
		cp.Spec.Services.Neutron.OVN.CentralRef.Namespace = "openstack"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred(),
			"the ControlPlane's own namespace is what the defaulting webhook writes into the ref")
	})

	t.Run("foreign namespace", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := base(ServiceNamespaceLifecycleExternal)
		cp.Spec.Services.Neutron.OVN.CentralRef.Namespace = "other-tenant"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.neutron.ovn.centralRef.namespace"))
		g.Expect(err.Error()).To(ContainSubstring(
			`namespace "other-tenant" is neither this ControlPlane's own nor one it claims`))
		g.Expect(err.Error()).To(ContainSubstring("mirrors that central's client certificate"))
	})

	t.Run("claimed namespace with lifecycle External", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := base(ServiceNamespaceLifecycleExternal)
		cp.Spec.Services.Neutron.OVN.CentralRef.Namespace = "networking"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred(),
			"an External namespace this plane claims is inside its trust boundary and outlives its teardown")
	})

	t.Run("claimed namespace with lifecycle Managed", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := base(ServiceNamespaceLifecycleManaged)
		cp.Spec.Services.Neutron.OVN.CentralRef.Namespace = "networking"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.neutron.ovn.centralRef.namespace"))
		g.Expect(err.Error()).To(ContainSubstring(`namespace "networking" is claimed by this ControlPlane with ` +
			"lifecycle Managed"))
		g.Expect(err.Error()).To(ContainSubstring("the cascade would take the referenced OVNCentral"))
	})

	// The default an unset lifecycle carries is Managed, so a claim that never
	// spelled it out must be refused for the same cascade reason.
	t.Run("claimed namespace with an unset lifecycle", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := base("")
		cp.Spec.Services.Neutron.OVN.CentralRef.Namespace = "networking"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("lifecycle Managed"))
	})
}

// TestValidateCreate_NeutronPublicEndpointMustBeABareOrigin covers the shapes
// the ^https?:// Pattern marker lets through and validateHTTPURL happily parses.
// The Neutron API is served at the root and clients append the API path to the
// catalog endpoint, so "https://neutron.example.com/network" yields
// "https://neutron.example.com/network/v2.0/networks" and 404s every network
// call. A single trailing slash is what OpenStack clients normalize away, so it
// stays admitted.
func TestValidateCreate_NeutronPublicEndpointMustBeABareOrigin(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for name, endpoint := range map[string]string{
		"query":        "https://neutron.example.com?utm=1",
		"fragment":     "https://neutron.example.com#top",
		"path":         "https://neutron.example.com/network",
		"missing host": "https://",
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := neutronControlPlane()
			cp.Spec.Services.Neutron.PublicEndpoint = endpoint

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("services.neutron.publicEndpoint"))
		})
	}

	t.Run("trailing slash", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := neutronControlPlane()
		cp.Spec.Services.Neutron.PublicEndpoint = "https://neutron.example.com/"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred(),
			"clients normalize the catalog endpoint before appending the API path")
	})
}

// TestValidateCreate_NeutronPublicEndpointMustAgreeWithGateway pins the two
// cross-field rules. An http endpoint behind a TLS-terminating listener ships
// the caller's scoped Keystone token in cleartext on every network call; a
// divergent host advertises a catalog URL the Gateway listener never routes,
// which fails client-side with nothing on the ControlPlane recording why.
func TestValidateCreate_NeutronPublicEndpointMustAgreeWithGateway(t *testing.T) {
	w := &ControlPlaneWebhook{}
	gateway := func() *commonv1.GatewaySpec {
		return &commonv1.GatewaySpec{
			Hostname:  "neutron.example.com",
			ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		}
	}

	t.Run("divergent host", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := neutronControlPlane()
		cp.Spec.Services.Neutron.Gateway = gateway()
		cp.Spec.Services.Neutron.PublicEndpoint = "https://networks.example.com"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("services.neutron.publicEndpoint"))
		g.Expect(err.Error()).To(ContainSubstring(
			`must equal services.neutron.gateway.hostname "neutron.example.com"`,
		))
	})

	t.Run("http scheme behind a TLS-terminating gateway", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := neutronControlPlane()
		cp.Spec.Services.Neutron.Gateway = gateway()
		cp.Spec.Services.Neutron.PublicEndpoint = "http://neutron.example.com"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("scheme must be https"))
	})

	t.Run("matching host with a non-default port", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := neutronControlPlane()
		cp.Spec.Services.Neutron.Gateway = gateway()
		cp.Spec.Services.Neutron.PublicEndpoint = "https://neutron.example.com:8443"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred(),
			"Gateway API hostnames carry no port, so the port is the reason the override exists")
	})

	t.Run("wildcard gateway hostname", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := neutronControlPlane()
		cp.Spec.Services.Neutron.Gateway = gateway()
		cp.Spec.Services.Neutron.Gateway.Hostname = "*.example.com"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("services.neutron.gateway.hostname"))
	})

	// The other arm of the same pair, mirroring the MinLength=1 marker on
	// GatewaySpec.Hostname: a gateway without one derives a hostless public
	// endpoint the catalog would register as "https://".
	t.Run("gateway without a hostname", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := neutronControlPlane()
		cp.Spec.Services.Neutron.Gateway = &commonv1.GatewaySpec{
			ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("services.neutron.gateway.hostname"))
		g.Expect(err.Error()).To(ContainSubstring("must be set when a gateway is configured"))
	})
}

// TestValidateCreate_WarnsOnCleartextNeutronPublicEndpoint covers the
// gateway-less network service, where an http endpoint is a legal (if unwise)
// development setup the CRD Pattern deliberately allows. Every network call
// sends a scoped Keystone token to that URL, so the downgrade must at least be
// surfaced.
func TestValidateCreate_WarnsOnCleartextNeutronPublicEndpoint(t *testing.T) {
	w := &ControlPlaneWebhook{}

	t.Run("http warns", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := neutronControlPlane()
		cp.Spec.Services.Neutron.PublicEndpoint = "http://neutron.example.com"

		warnings, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).To(HaveLen(1))
		g.Expect(warnings[0]).To(ContainSubstring("scoped Keystone token"))
	})

	t.Run("https is silent", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := neutronControlPlane()
		cp.Spec.Services.Neutron.PublicEndpoint = "https://neutron.example.com"

		warnings, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).To(BeEmpty())
	})
}

// TestValidateCreate_RejectsNeutronImageTagDigestXOR pins the defense-in-depth
// mirror of the commonv1.ImageSpec XValidation rule for callers that bypass CRD
// schema admission.
func TestValidateCreate_RejectsNeutronImageTagDigestXOR(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for name, img := range map[string]*commonv1.ImageSpec{
		"neither tag nor digest": {Repository: "ghcr.io/c5c3/neutron"},
		"both tag and digest": {
			Repository: "ghcr.io/c5c3/neutron",
			Tag:        "2025.2",
			Digest:     "sha256:" + strings.Repeat("a", 64),
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := neutronControlPlane()
			cp.Spec.Services.Neutron.Image = img

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("services.neutron.image"))
			g.Expect(err.Error()).To(ContainSubstring("exactly one of image.tag or image.digest must be set"))
		})
	}
}

// TestValidateCreate_RejectsNeutronCredentialsModeOverrideDynamicOnDedicated is
// the neutron mirror of the keystone dedicated-database rejection: the override
// retargets the shared database the service does not use, and a dedicated
// database is Static-only.
func TestValidateCreate_RejectsNeutronCredentialsModeOverrideDynamicOnDedicated(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := neutronControlPlane()
	cp.Spec.Services.Neutron.DatabaseCredentialsMode = commonv1.CredentialsModeDynamic
	cp.Spec.Services.Neutron.DedicatedBackingServices = &NeutronDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{
			ClusterRef:      &corev1.LocalObjectReference{Name: "cp-neutron-db"},
			CredentialsMode: commonv1.CredentialsModeStatic,
			Database:        "neutron",
			SecretRef:       commonv1.SecretRefSpec{Name: "neutron-db"},
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.neutron.databaseCredentialsMode"))
	g.Expect(err.Error()).To(ContainSubstring(
		"not supported as an override on a service with a dedicated database",
	))
}

// TestValidateCreate_RejectsNeutronCredentialsModeOverrideDynamicOnBrownfieldShared
// pins the other half of the override rule: the dynamic engine issues per-tenant
// DB users only against a cluster the operator provisions, so a Dynamic override
// on a brownfield shared database is rejected.
func TestValidateCreate_RejectsNeutronCredentialsModeOverrideDynamicOnBrownfieldShared(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := neutronControlPlane()
	cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		Host:      "db.example.com",
		Port:      3306,
		Database:  "openstack",
		SecretRef: commonv1.SecretRefSpec{Name: "db-creds"},
	}
	cp.Spec.Services.Neutron.DatabaseCredentialsMode = commonv1.CredentialsModeDynamic

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.neutron.databaseCredentialsMode"))
	g.Expect(err.Error()).To(ContainSubstring(
		"Dynamic requires the shared database to be managed (clusterRef)",
	))
}

// placedNeutronControlPlane returns a ControlPlane whose network service is
// placed on the "edge" cluster in a namespace of its own, advertising nothing.
// Keystone is published because a service placed away from it validates its
// tokens over that URL.
func placedNeutronControlPlane() *ControlPlane {
	cp := neutronControlPlane()
	cp.Namespace = "openstack"
	cp.Spec.Services.Neutron.Namespace = &ServiceNamespaceSpec{
		Name: "network", Lifecycle: ServiceNamespaceLifecycleManaged,
	}
	cp.Spec.Services.Neutron.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge"}
	publishKeystone(cp)
	return cp
}

// TestValidateCreate_RejectsPlacedNeutronWithoutNamespace pins the
// dedicated-namespace rule for the network service: a namespace maps to exactly
// one cluster, and the ControlPlane's own stays on the local one, so a Neutron
// placed elsewhere without a namespace of its own would have its database, its
// tenant store, and its credential material provisioned on a cluster its
// workload does not run on.
func TestValidateCreate_RejectsPlacedNeutronWithoutNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := placedNeutronControlPlane()
	cp.Spec.Services.Neutron.PublicEndpoint = "https://neutron.example.com"
	cp.Spec.Services.Neutron.Namespace = nil

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.services.neutron.namespace"))
	g.Expect(err.Error()).To(ContainSubstring("a placed service needs a namespace of its own"))
}

// TestValidateCreate_RejectsPlacedNeutronUnpublished pins the reachability rule
// for the network catalog entry: what the ControlPlane registers for an
// unpublished service is its in-cluster Service DNS name, which resolves nowhere
// outside the cluster the service runs on. Either publication satisfies it.
func TestValidateCreate_RejectsPlacedNeutronUnpublished(t *testing.T) {
	w := &ControlPlaneWebhook{}

	t.Run("neither publicEndpoint nor gateway", func(t *testing.T) {
		g := NewGomegaWithT(t)
		_, err := w.ValidateCreate(context.Background(), placedNeutronControlPlane())
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.neutron.publicEndpoint"))
		g.Expect(err.Error()).To(ContainSubstring(
			"one of publicEndpoint or gateway is required when targetClusterRef is set"))
	})

	t.Run("a publicEndpoint satisfies it", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := placedNeutronControlPlane()
		cp.Spec.Services.Neutron.PublicEndpoint = "https://neutron.example.com"

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
	})

	t.Run("a gateway satisfies it", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := placedNeutronControlPlane()
		cp.Spec.Services.Neutron.Gateway = placedGateway("neutron.example.com")

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
	})
}

// The projected Neutron child is bounded far below the 253-byte object-name cap:
// the Neutron CRD caps metadata.name at 40 characters, because the neutron
// operator appends a suffix for the ovn-db-sync CronJob and Kubernetes caps
// CronJob names at 52. Without this guard the ControlPlane admits and the
// projection then fails to apply the child on every pass, with metadata.name
// immutable, so recovery means recreating the whole control plane.
func TestValidateCreate_RejectsOverlongProjectedNeutronChildName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	maxCPName := neutronv1alpha1.MaxNeutronNameLength - neutronChildNameOverhead

	atLimit := neutronControlPlane()
	atLimit.Name = strings.Repeat("c", maxCPName)
	_, err := w.ValidateCreate(context.Background(), atLimit)
	g.Expect(err).NotTo(HaveOccurred(),
		"a name whose projected Neutron child still fits must be accepted")

	tooLong := neutronControlPlane()
	tooLong.Name = strings.Repeat("c", maxCPName+1)
	_, err = w.ValidateCreate(context.Background(), tooLong)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("projected Neutron child CR name would be 41 characters"))
	g.Expect(err.Error()).To(ContainSubstring("must be at most 32 characters"))

	// Without services.neutron no Neutron child is projected, so the bound does
	// not apply and the ControlPlane keeps the full 253-byte budget.
	noNeutron := neutronControlPlane()
	noNeutron.Name = strings.Repeat("c", maxCPName+1)
	noNeutron.Spec.Services.Neutron = nil
	_, err = w.ValidateCreate(context.Background(), noNeutron)
	g.Expect(err).NotTo(HaveOccurred())
}

// Enabling Neutron on an existing over-long ControlPlane is the one update that
// can newly violate the bound, so it is rejected; every other update on a CR
// that already carried Neutron — including the finalizer removal that completes
// its deletion — must still pass, because metadata.name is immutable and a
// rejection would wedge it in Terminating.
func TestValidateUpdate_ProjectedNeutronChildNameBoundIsNewlyEnabledOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	overlong := strings.Repeat("c", neutronv1alpha1.MaxNeutronNameLength-neutronChildNameOverhead+1)

	withoutNeutron := neutronControlPlane()
	withoutNeutron.Name = overlong
	withoutNeutron.Spec.Services.Neutron = nil
	enabling := neutronControlPlane()
	enabling.Name = overlong

	_, err := w.ValidateUpdate(context.Background(), withoutNeutron, enabling)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("projected Neutron child CR name would be"))

	grandfathered := neutronControlPlane()
	grandfathered.Name = overlong
	grandfathered.Finalizers = []string{"c5c3.io/finalizer"}
	deleting := grandfathered.DeepCopy()
	deleting.Finalizers = nil

	_, err = w.ValidateUpdate(context.Background(), grandfathered, deleting)
	g.Expect(err).NotTo(HaveOccurred(),
		"an over-long grandfathered ControlPlane must stay updatable, or its deletion never completes")
}

// The OVNCentral reach check is a newly-enabled-or-moved rule for the same reason
// the child-name bound above is: it can reject a CR a previous operator build
// admitted, and the finalizer-removal update that completes a deletion is an
// update like any other. A grandfathered plane that stays rejectable can never be
// deleted — the webhook is registered for UPDATE with failurePolicy: Fail, so the
// operator's own finalizer removal is refused and the CR hangs in Terminating.
func TestValidateUpdate_NeutronOVNCentralReachCheckIsNewlyEnabledOrMovedOnly(t *testing.T) {
	w := &ControlPlaneWebhook{}
	// A ControlPlane admitted before the reach rule existed, carrying a central in
	// a namespace it neither owns nor claims.
	grandfathered := func() *ControlPlane {
		cp := neutronControlPlane()
		cp.Namespace = "openstack"
		cp.Spec.Services.Neutron.OVN.CentralRef.Namespace = "other-tenant"
		cp.Finalizers = []string{"c5c3.io/finalizer"}
		return cp
	}

	t.Run("routine update on a grandfathered plane", func(t *testing.T) {
		g := NewGomegaWithT(t)
		oldCP := grandfathered()
		newCP := oldCP.DeepCopy()
		newCP.Annotations = map[string]string{"c5c3.io/allow-neutron-deletion": "false"}

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).NotTo(HaveOccurred(),
			"an unrelated update must not be rejected by a rule the CR already violated")
	})

	t.Run("finalizer removal on a grandfathered plane", func(t *testing.T) {
		g := NewGomegaWithT(t)
		oldCP := grandfathered()
		deleting := oldCP.DeepCopy()
		deleting.Finalizers = nil

		_, err := w.ValidateUpdate(context.Background(), oldCP, deleting)
		g.Expect(err).NotTo(HaveOccurred(),
			"rejecting the finalizer removal would wedge the ControlPlane in Terminating")
	})

	t.Run("the update that enables the network service", func(t *testing.T) {
		g := NewGomegaWithT(t)
		enabling := grandfathered()
		withoutNeutron := enabling.DeepCopy()
		withoutNeutron.Spec.Services.Neutron = nil

		_, err := w.ValidateUpdate(context.Background(), withoutNeutron, enabling)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.neutron.ovn.centralRef.namespace"))
		g.Expect(err.Error()).To(ContainSubstring(
			`namespace "other-tenant" is neither this ControlPlane's own nor one it claims`))
	})

	t.Run("the update that moves the ref", func(t *testing.T) {
		g := NewGomegaWithT(t)
		oldCP := neutronControlPlane()
		oldCP.Namespace = "openstack"
		oldCP.Spec.Services.Neutron.OVN.CentralRef.Namespace = "openstack"
		newCP := oldCP.DeepCopy()
		newCP.Spec.Services.Neutron.OVN.CentralRef.Namespace = "other-tenant"

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.neutron.ovn.centralRef.namespace"))
	})
}

// TestValidateUpdate_RejectsNeutronNamespaceChange pins the create-only freeze on
// the neutron namespace assignment.
func TestValidateUpdate_RejectsNeutronNamespaceChange(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := neutronControlPlane()
	oldCP.Namespace = "openstack"
	oldCP.Spec.Services.Neutron.Namespace = &ServiceNamespaceSpec{
		Name: "network", Lifecycle: ServiceNamespaceLifecycleManaged,
	}
	newCP := oldCP.DeepCopy()
	newCP.Spec.Services.Neutron.Namespace.Name = "network-2"

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.services.neutron.namespace.name"))
	g.Expect(err.Error()).To(ContainSubstring("the namespace a service is placed in is immutable"))
}

// TestValidateUpdate_RejectsDroppingANeutronNamespaceAssignment pins that the
// declared-before carve-out does not weaken the move freeze: a live Neutron
// still cannot shed its namespace assignment, since everything scoped to that
// namespace stays where it is.
func TestValidateUpdate_RejectsDroppingANeutronNamespaceAssignment(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := neutronControlPlane()
	oldCP.Namespace = "openstack"
	oldCP.Spec.Services.Neutron.Namespace = &ServiceNamespaceSpec{
		Name: "network", Lifecycle: ServiceNamespaceLifecycleManaged,
	}
	newCP := oldCP.DeepCopy()
	newCP.Spec.Services.Neutron.Namespace = nil

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("the namespace a service is placed in is immutable"))
}

// TestValidateUpdate_RejectsNeutronDedicatedPresenceFlip pins the transition
// freeze on the neutron dedicated block: a live service cannot be moved between
// shared and dedicated backing services.
func TestValidateUpdate_RejectsNeutronDedicatedPresenceFlip(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := neutronControlPlane()
	newCP := neutronControlPlane()
	newCP.Spec.Services.Neutron.DedicatedBackingServices = &NeutronDedicatedBackingServicesSpec{
		Cache: &commonv1.CacheSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "cp-neutron-cache"},
			Backend:    commonv1.DefaultCacheBackend,
		},
	}

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("switching a service between shared and dedicated backing services"))
	g.Expect(err.Error()).To(ContainSubstring("neutron.dedicatedBackingServices"))
}

// TestValidateUpdate_RejectsNeutronTargetClusterChange pins the create-only
// freeze on the neutron placement: re-pointing a live service strands its
// workload, its database, and the material in its tenant store on the cluster it
// came from.
func TestValidateUpdate_RejectsNeutronTargetClusterChange(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := placedNeutronControlPlane()
	oldCP.Spec.Services.Neutron.PublicEndpoint = "https://neutron.example.com"
	newCP := oldCP.DeepCopy()
	newCP.Spec.Services.Neutron.TargetClusterRef.Name = "core"

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.services.neutron.targetClusterRef.name"))
	g.Expect(err.Error()).To(ContainSubstring("targetClusterRef is immutable"))
}

// TestValidateUpdate_AcceptsAddingNeutronInADedicatedNamespace pins the
// declared-before carve-out for the network service: assigning a namespace to a
// service the ControlPlane did not declare before is that service's create, so
// there is no live workload and no credential material stranded in an old
// namespace for the move freeze to protect.
func TestValidateUpdate_AcceptsAddingNeutronInADedicatedNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := neutronControlPlane()
	oldCP.Namespace = "openstack"
	oldCP.Spec.Services.Neutron = nil

	newCP := oldCP.DeepCopy()
	newCP.Spec.Services.Neutron = neutronControlPlane().Spec.Services.Neutron
	newCP.Spec.Services.Neutron.Namespace = &ServiceNamespaceSpec{
		Name: "network", Lifecycle: ServiceNamespaceLifecycleManaged,
	}

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateUpdate_NeutronDeclaredInStatusFreezesTheNamespace pins that the
// carve-out keys on status.services too, not on the old spec alone: a Neutron
// dropped from spec keeps its projected child until the operator observes the
// drop, so re-adding it in a namespace of its own inside that window is the move
// the freeze forbids.
func TestValidateUpdate_NeutronDeclaredInStatusFreezesTheNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := neutronControlPlane()
	oldCP.Namespace = "openstack"
	oldCP.Spec.Services.Neutron = nil
	oldCP.Status.Services = []ServiceStatus{
		{Name: "keystone", Ready: true},
		{Name: "neutron", Ready: true},
	}

	newCP := oldCP.DeepCopy()
	newCP.Spec.Services.Neutron = neutronControlPlane().Spec.Services.Neutron
	newCP.Spec.Services.Neutron.Namespace = &ServiceNamespaceSpec{
		Name: "network", Lifecycle: ServiceNamespaceLifecycleManaged,
	}

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.services.neutron.namespace"))
	g.Expect(err.Error()).To(ContainSubstring("the namespace a service is placed in is immutable"))
}
