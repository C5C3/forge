// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"regexp"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
)

func TestSchemeBuilderRegistersControlPlane(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme failed: %v", err)
	}

	for _, kind := range []string{"ControlPlane", "ControlPlaneList"} {
		gvk := schema.GroupVersionKind{Group: "c5c3.io", Version: "v1alpha1", Kind: kind}
		if _, err := s.New(gvk); err != nil {
			t.Fatalf("scheme.New(%v) failed: %v", gvk, err)
		}
	}
}

// controlPlaneReleasePattern mirrors the +kubebuilder:validation:Pattern marker
// on ControlPlaneSpec.OpenStackRelease. The CRD schema is the enforcement point
// at admission time; this test pins the contract so a marker change is caught.
const controlPlaneReleasePattern = `^\d{4}\.[12]$`

func TestOpenStackReleasePattern(t *testing.T) {
	re := regexp.MustCompile(controlPlaneReleasePattern)
	tests := []struct {
		release string
		valid   bool
	}{
		{"2024.1", true},
		{"2025.2", true},
		{"2023.1", true},
		{"", false},
		{"2025", false},
		{"2025.", false},
		{"25.2", false},
		{"2025.22", false},
		{"v2025.2", false},
		{"2025.2 ", false},
		// Non-cadence minors: OpenStack ships only YYYY.1 and YYYY.2, so the
		// [12] class must reject any other single digit — keeping the CRD
		// pattern in agreement with release.ParseRelease.
		{"2025.0", false},
		{"2025.3", false},
		{"2025.9", false},
	}
	for _, tt := range tests {
		got := re.MatchString(tt.release)
		if got != tt.valid {
			t.Errorf("release %q: pattern match = %v, want %v", tt.release, got, tt.valid)
		}
	}
}

// TestControlPlaneSpecReusesCommonTypes asserts the ControlPlane reuses the
// canonical commonv1 shapes for infrastructure and policy, so the
// aggregate and the per-service CRs validate the same way. Assigning the
// commonv1 zero values to the spec fields is a compile-time type assertion.
func TestControlPlaneSpecReusesCommonTypes(t *testing.T) {
	spec := ControlPlaneSpec{Infrastructure: &InfrastructureSpec{}}

	// These assignments only compile if the field types are exactly the
	// commonv1 types — guarding against an accidental local copy.
	spec.Infrastructure.Database = commonv1.DatabaseSpec{Database: "ks", SecretRef: commonv1.SecretRefSpec{Name: "s"}}
	spec.Infrastructure.Cache = commonv1.CacheSpec{Backend: "dogpile.cache.pymemcache"}
	spec.GlobalPolicyOverrides = &commonv1.PolicySpec{}
	spec.Services.Keystone = &ServiceKeystoneSpec{}
	spec.Services.Keystone.Image = &commonv1.ImageSpec{Repository: "r", Tag: "t"}
	spec.Services.Keystone.PolicyOverrides = &commonv1.PolicySpec{}
	spec.Services.Keystone.Gateway = &commonv1.GatewaySpec{}
	spec.KORC.AdminCredential.PasswordSecretRef = commonv1.SecretRefSpec{Name: "admin"}

	if spec.Infrastructure.Database.Database != "ks" {
		t.Errorf("unexpected database name %q", spec.Infrastructure.Database.Database)
	}
	if spec.Infrastructure.Cache.Backend != "dogpile.cache.pymemcache" {
		t.Errorf("unexpected cache backend %q", spec.Infrastructure.Cache.Backend)
	}
}

// TestServiceKeystoneSpecDeepCopy verifies the shared keystone subset
// round-trips through DeepCopy with independent pointer storage (plan decision #2).
func TestServiceKeystoneSpecDeepCopy(t *testing.T) {
	replicas := int32(5)
	spec := ServiceKeystoneSpec{
		Replicas:         &replicas,
		Image:            &commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"},
		RotationInterval: &metav1.Duration{},
	}

	clone := spec.DeepCopy()
	if clone.Replicas == spec.Replicas {
		t.Errorf("DeepCopy did not allocate a new *int32 for Replicas")
	}
	if clone.Image == spec.Image {
		t.Errorf("DeepCopy did not allocate a new *ImageSpec for Image")
	}
	if *clone.Replicas != 5 {
		t.Errorf("DeepCopy altered Replicas: got %d", *clone.Replicas)
	}
}

// TestServiceKeystoneSpecExternalDeepCopy verifies the External-mode block
// (mode + the typed external pointer, incl. its optional caBundleSecretRef)
// round-trips through DeepCopy with independent pointer storage.
func TestServiceKeystoneSpecExternalDeepCopy(t *testing.T) {
	spec := ServiceKeystoneSpec{
		Mode: KeystoneModeExternal,
		External: &ExternalKeystoneSpec{
			AuthURL:           "https://keystone.example.com/v3",
			EndpointType:      ExternalEndpointTypePublic,
			CABundleSecretRef: &commonv1.SecretRefSpec{Name: "brownfield-keystone-ca", Key: "ca.crt"},
		},
	}

	clone := spec.DeepCopy()
	if clone.External == spec.External {
		t.Errorf("DeepCopy did not allocate a new *ExternalKeystoneSpec for External")
	}
	if clone.External.CABundleSecretRef == spec.External.CABundleSecretRef {
		t.Errorf("DeepCopy did not allocate a new *SecretRefSpec for CABundleSecretRef")
	}
	if clone.Mode != KeystoneModeExternal {
		t.Errorf("DeepCopy altered Mode: got %q", clone.Mode)
	}
	if clone.External.AuthURL != "https://keystone.example.com/v3" {
		t.Errorf("DeepCopy altered AuthURL: got %q", clone.External.AuthURL)
	}

	// Mutating the clone's external block must not touch the source.
	clone.External.AuthURL = "https://other.example.com/v3"
	if spec.External.AuthURL != "https://keystone.example.com/v3" {
		t.Errorf("DeepCopy aliased External: source AuthURL changed to %q", spec.External.AuthURL)
	}
}

// TestIsExternalKeystone exercises the nil-safe discriminator across the three
// Keystone service states (nil, Managed, External).
func TestIsExternalKeystone(t *testing.T) {
	tests := []struct {
		name string
		ks   *ServiceKeystoneSpec
		want bool
	}{
		{"nil keystone", nil, false},
		{"managed (explicit)", &ServiceKeystoneSpec{Mode: KeystoneModeManaged}, false},
		{"managed (unset mode)", &ServiceKeystoneSpec{}, false},
		{"external", &ServiceKeystoneSpec{Mode: KeystoneModeExternal}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp := &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{Keystone: tt.ks}}}
			if got := cp.IsExternalKeystone(); got != tt.want {
				t.Errorf("IsExternalKeystone() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestKORCSpecShape exercises the KORC/AdminCredential nested shape
// and the application-credential defaults' field types.
func TestKORCSpecShape(t *testing.T) {
	restricted := true
	korc := KORCSpec{
		AdminCredential: AdminCredentialSpec{
			CloudCredentialsRef: CloudCredentialsRef{CloudName: "admin", SecretName: "k-orc-clouds-yaml"},
			PasswordSecretRef:   commonv1.SecretRefSpec{Name: "admin-pw"},
			ApplicationCredential: ApplicationCredentialSpec{
				Restricted: &restricted,
				AccessRules: []AccessRule{
					{Service: "identity", Method: "GET", Path: "/v3/users"},
				},
				Rotation: RotationSpec{Mode: RotationModePasswordDriven},
			},
			BootstrapResources: []BootstrapResourceSpec{{Kind: "Project", Name: "service"}},
		},
	}

	clone := korc.DeepCopy()
	if clone.AdminCredential.ApplicationCredential.Restricted == korc.AdminCredential.ApplicationCredential.Restricted {
		t.Errorf("DeepCopy did not allocate a new *bool for Restricted")
	}
	if clone.AdminCredential.ApplicationCredential.Rotation.Mode != RotationModePasswordDriven {
		t.Errorf("unexpected rotation mode %q", clone.AdminCredential.ApplicationCredential.Rotation.Mode)
	}
}

// TestDedicatedBackingServicesDeepCopy verifies the per-service dedicated blocks
// round-trip through DeepCopy with independent pointer storage. The reconciler
// DeepCopies the effective (dedicated-or-shared) specs onto the service children,
// so an aliased ClusterRef here would let a child projection mutate the
// ControlPlane spec it was derived from.
func TestDedicatedBackingServicesDeepCopy(t *testing.T) {
	spec := ServiceKeystoneSpec{
		DedicatedBackingServices: &KeystoneDedicatedBackingServicesSpec{
			Database: &commonv1.DatabaseSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-db"},
				Database:   "keystone",
			},
			Cache: &commonv1.CacheSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-cache"},
				Backend:    commonv1.DefaultCacheBackend,
			},
		},
	}

	clone := spec.DeepCopy()
	if clone.DedicatedBackingServices == spec.DedicatedBackingServices {
		t.Errorf("DeepCopy did not allocate a new *KeystoneDedicatedBackingServicesSpec")
	}
	if clone.DedicatedBackingServices.Database == spec.DedicatedBackingServices.Database {
		t.Errorf("DeepCopy did not allocate a new *DatabaseSpec for the dedicated database")
	}
	if clone.DedicatedBackingServices.Database.ClusterRef == spec.DedicatedBackingServices.Database.ClusterRef {
		t.Errorf("DeepCopy did not allocate a new *LocalObjectReference for the dedicated database clusterRef")
	}
	if clone.DedicatedBackingServices.Cache.ClusterRef == spec.DedicatedBackingServices.Cache.ClusterRef {
		t.Errorf("DeepCopy did not allocate a new *LocalObjectReference for the dedicated cache clusterRef")
	}

	// Mutating the clone must not touch the source.
	clone.DedicatedBackingServices.Database.ClusterRef.Name = "other-db"
	if spec.DedicatedBackingServices.Database.ClusterRef.Name != "cp-keystone-db" {
		t.Errorf("DeepCopy aliased the dedicated database clusterRef: source name changed to %q",
			spec.DedicatedBackingServices.Database.ClusterRef.Name)
	}
}

// TestDedicatedBackingServicesAccessors exercises the nil-safe reads the webhook
// and the reconciler share, across the three states a service can be in: no
// service block, a service that shares the ControlPlane-wide instances (the
// default), and a service that opted into dedicated instances.
func TestDedicatedBackingServicesAccessors(t *testing.T) {
	tests := []struct {
		name              string
		cp                *ControlPlane
		wantKeystoneDB    bool
		wantKeystoneCache bool
		wantHorizonCache  bool
	}{
		{
			name: "no service blocks",
			cp:   &ControlPlane{},
		},
		{
			name: "services share the ControlPlane-wide instances",
			cp: &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{
				Keystone: &ServiceKeystoneSpec{},
				Horizon:  &ServiceHorizonSpec{},
			}}},
		},
		{
			name: "keystone takes a dedicated cache only",
			cp: &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{
				Keystone: &ServiceKeystoneSpec{
					DedicatedBackingServices: &KeystoneDedicatedBackingServicesSpec{
						Cache: &commonv1.CacheSpec{Backend: commonv1.DefaultCacheBackend},
					},
				},
			}}},
			wantKeystoneCache: true,
		},
		{
			name: "both services take dedicated instances",
			cp: &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{
				Keystone: &ServiceKeystoneSpec{
					DedicatedBackingServices: &KeystoneDedicatedBackingServicesSpec{
						Database: &commonv1.DatabaseSpec{Database: "keystone"},
						Cache:    &commonv1.CacheSpec{Backend: commonv1.DefaultCacheBackend},
					},
				},
				Horizon: &ServiceHorizonSpec{
					DedicatedBackingServices: &HorizonDedicatedBackingServicesSpec{
						Cache: &commonv1.CacheSpec{Backend: commonv1.DefaultCacheBackend},
					},
				},
			}}},
			wantKeystoneDB:    true,
			wantKeystoneCache: true,
			wantHorizonCache:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cp.DedicatedKeystoneDatabase() != nil; got != tc.wantKeystoneDB {
				t.Errorf("DedicatedKeystoneDatabase() present = %v, want %v", got, tc.wantKeystoneDB)
			}
			if got := tc.cp.DedicatedKeystoneCache() != nil; got != tc.wantKeystoneCache {
				t.Errorf("DedicatedKeystoneCache() present = %v, want %v", got, tc.wantKeystoneCache)
			}
			if got := tc.cp.DedicatedHorizonCache() != nil; got != tc.wantHorizonCache {
				t.Errorf("DedicatedHorizonCache() present = %v, want %v", got, tc.wantHorizonCache)
			}
		})
	}
}

func TestServiceNamespaceAccessors(t *testing.T) {
	cpIn := func(services ServicesSpec) *ControlPlane {
		return &ControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
			Spec:       ControlPlaneSpec{Services: services},
		}
	}

	tests := []struct {
		name          string
		cp            *ControlPlane
		wantKeystone  string
		wantHorizon   string
		wantDedicated []string
	}{
		{
			name:         "no service blocks default to the ControlPlane namespace",
			cp:           cpIn(ServicesSpec{}),
			wantKeystone: "openstack",
			wantHorizon:  "openstack",
		},
		{
			name: "service blocks without an assignment default to the ControlPlane namespace",
			cp: cpIn(ServicesSpec{
				Keystone: &ServiceKeystoneSpec{},
				Horizon:  &ServiceHorizonSpec{},
			}),
			wantKeystone: "openstack",
			wantHorizon:  "openstack",
		},
		{
			name: "each service takes a namespace of its own",
			cp: cpIn(ServicesSpec{
				Keystone: &ServiceKeystoneSpec{
					Namespace: &ServiceNamespaceSpec{Name: "identity", Lifecycle: ServiceNamespaceLifecycleManaged},
				},
				Horizon: &ServiceHorizonSpec{
					Namespace: &ServiceNamespaceSpec{Name: "dashboard", Lifecycle: ServiceNamespaceLifecycleExternal},
				},
			}),
			wantKeystone:  "identity",
			wantHorizon:   "dashboard",
			wantDedicated: []string{"identity", "dashboard"},
		},
		{
			name: "co-located services yield one dedicated namespace",
			cp: cpIn(ServicesSpec{
				Keystone: &ServiceKeystoneSpec{Namespace: &ServiceNamespaceSpec{Name: "shared-ns"}},
				Horizon:  &ServiceHorizonSpec{Namespace: &ServiceNamespaceSpec{Name: "shared-ns"}},
			}),
			wantKeystone:  "shared-ns",
			wantHorizon:   "shared-ns",
			wantDedicated: []string{"shared-ns"},
		},
		{
			// Webhook-bypass shape: an assignment naming the ControlPlane's own
			// namespace must never be enumerated as dedicated, or teardown would
			// delete the ControlPlane's own namespace.
			name: "an assignment equal to the ControlPlane namespace is not dedicated",
			cp: cpIn(ServicesSpec{
				Keystone: &ServiceKeystoneSpec{Namespace: &ServiceNamespaceSpec{Name: "openstack"}},
			}),
			wantKeystone: "openstack",
			wantHorizon:  "openstack",
		},
		{
			// Webhook-bypass shape: an empty name is not an assignment.
			name: "an empty assignment name falls back to the ControlPlane namespace",
			cp: cpIn(ServicesSpec{
				Keystone: &ServiceKeystoneSpec{Namespace: &ServiceNamespaceSpec{}},
			}),
			wantKeystone: "openstack",
			wantHorizon:  "openstack",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cp.KeystoneNamespace(); got != tc.wantKeystone {
				t.Errorf("KeystoneNamespace() = %q, want %q", got, tc.wantKeystone)
			}
			if got := tc.cp.HorizonNamespace(); got != tc.wantHorizon {
				t.Errorf("HorizonNamespace() = %q, want %q", got, tc.wantHorizon)
			}
			var gotNames []string
			for _, ns := range tc.cp.DedicatedServiceNamespaces() {
				gotNames = append(gotNames, ns.Name)
			}
			if len(gotNames) != len(tc.wantDedicated) {
				t.Fatalf("DedicatedServiceNamespaces() = %v, want %v", gotNames, tc.wantDedicated)
			}
			for i := range gotNames {
				if gotNames[i] != tc.wantDedicated[i] {
					t.Errorf("DedicatedServiceNamespaces()[%d] = %q, want %q", i, gotNames[i], tc.wantDedicated[i])
				}
			}
		})
	}
}

func TestDedicatedServiceNamespacesCarriesTheLifecycle(t *testing.T) {
	cp := &ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
		Spec: ControlPlaneSpec{Services: ServicesSpec{
			Horizon: &ServiceHorizonSpec{
				Namespace: &ServiceNamespaceSpec{Name: "dashboard", Lifecycle: ServiceNamespaceLifecycleExternal},
			},
		}},
	}
	got := cp.DedicatedServiceNamespaces()
	if len(got) != 1 {
		t.Fatalf("DedicatedServiceNamespaces() = %v, want one entry", got)
	}
	if got[0].Lifecycle != ServiceNamespaceLifecycleExternal {
		t.Errorf("lifecycle = %q, want %q", got[0].Lifecycle, ServiceNamespaceLifecycleExternal)
	}
}

// TestGlanceNamespace exercises the nil-safe namespace resolver for the Glance
// service across the states the accessor can be in: no service block and a block
// without an assignment (both default to the ControlPlane's namespace), a
// webhook-bypass empty name (also a fallback), and an explicit assignment.
func TestGlanceNamespace(t *testing.T) {
	cpIn := func(glance *ServiceGlanceSpec) *ControlPlane {
		return &ControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
			Spec:       ControlPlaneSpec{Services: ServicesSpec{Glance: glance}},
		}
	}
	tests := []struct {
		name string
		cp   *ControlPlane
		want string
	}{
		{"no glance block defaults to the ControlPlane namespace", cpIn(nil), "openstack"},
		{"glance block without an assignment defaults to the ControlPlane namespace", cpIn(&ServiceGlanceSpec{}), "openstack"},
		{"an empty assignment name falls back to the ControlPlane namespace", cpIn(&ServiceGlanceSpec{Namespace: &ServiceNamespaceSpec{}}), "openstack"},
		{"glance takes a namespace of its own", cpIn(&ServiceGlanceSpec{Namespace: &ServiceNamespaceSpec{Name: "images"}}), "images"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cp.GlanceNamespace(); got != tc.want {
				t.Errorf("GlanceNamespace() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDedicatedServiceNamespacesIncludesGlance asserts the Glance, Placement,
// Barbican, and Neutron assignments are enumerated alongside keystone and
// horizon, in the stable keystone→horizon→glance→placement→barbican→neutron
// order, and that co-located services collapse to a single entry (services
// sharing a namespace share its backing services and tenant store).
func TestDedicatedServiceNamespacesIncludesGlance(t *testing.T) {
	cpIn := func(services ServicesSpec) *ControlPlane {
		return &ControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
			Spec:       ControlPlaneSpec{Services: services},
		}
	}
	tests := []struct {
		name string
		cp   *ControlPlane
		want []string
	}{
		{
			name: "glance takes a namespace of its own",
			cp: cpIn(ServicesSpec{
				Glance: &ServiceGlanceSpec{Namespace: &ServiceNamespaceSpec{Name: "images"}},
			}),
			want: []string{"images"},
		},
		{
			name: "each service in its own namespace enumerates in keystone→horizon→glance→placement→barbican→neutron order",
			cp: cpIn(ServicesSpec{
				Keystone:  &ServiceKeystoneSpec{Namespace: &ServiceNamespaceSpec{Name: "identity"}},
				Horizon:   &ServiceHorizonSpec{Namespace: &ServiceNamespaceSpec{Name: "dashboard"}},
				Glance:    &ServiceGlanceSpec{Namespace: &ServiceNamespaceSpec{Name: "images"}},
				Placement: &ServicePlacementSpec{Namespace: &ServiceNamespaceSpec{Name: "placement"}},
				Barbican:  &ServiceBarbicanSpec{Namespace: &ServiceNamespaceSpec{Name: "barbican"}},
				Neutron:   &ServiceNeutronSpec{Namespace: &ServiceNamespaceSpec{Name: "neutron"}},
			}),
			want: []string{"identity", "dashboard", "images", "placement", "barbican", "neutron"},
		},
		{
			name: "neutron takes a namespace of its own",
			cp: cpIn(ServicesSpec{
				Neutron: &ServiceNeutronSpec{Namespace: &ServiceNamespaceSpec{Name: "neutron"}},
			}),
			want: []string{"neutron"},
		},
		{
			name: "neutron co-located with barbican yields one entry",
			cp: cpIn(ServicesSpec{
				Barbican: &ServiceBarbicanSpec{Namespace: &ServiceNamespaceSpec{Name: "shared-ns"}},
				Neutron:  &ServiceNeutronSpec{Namespace: &ServiceNamespaceSpec{Name: "shared-ns"}},
			}),
			want: []string{"shared-ns"},
		},
		{
			name: "a neutron assignment naming the ControlPlane namespace contributes nothing",
			cp: cpIn(ServicesSpec{
				Neutron: &ServiceNeutronSpec{Namespace: &ServiceNamespaceSpec{Name: "openstack"}},
			}),
			want: nil,
		},
		{
			name: "barbican takes a namespace of its own",
			cp: cpIn(ServicesSpec{
				Barbican: &ServiceBarbicanSpec{Namespace: &ServiceNamespaceSpec{Name: "barbican"}},
			}),
			want: []string{"barbican"},
		},
		{
			name: "barbican co-located with placement yields one entry",
			cp: cpIn(ServicesSpec{
				Placement: &ServicePlacementSpec{Namespace: &ServiceNamespaceSpec{Name: "shared-ns"}},
				Barbican:  &ServiceBarbicanSpec{Namespace: &ServiceNamespaceSpec{Name: "shared-ns"}},
			}),
			want: []string{"shared-ns"},
		},
		{
			name: "a barbican assignment naming the ControlPlane namespace contributes nothing",
			cp: cpIn(ServicesSpec{
				Barbican: &ServiceBarbicanSpec{Namespace: &ServiceNamespaceSpec{Name: "openstack"}},
			}),
			want: nil,
		},
		{
			name: "placement takes a namespace of its own",
			cp: cpIn(ServicesSpec{
				Placement: &ServicePlacementSpec{Namespace: &ServiceNamespaceSpec{Name: "placement"}},
			}),
			want: []string{"placement"},
		},
		{
			name: "glance co-located with keystone yields one entry",
			cp: cpIn(ServicesSpec{
				Keystone: &ServiceKeystoneSpec{Namespace: &ServiceNamespaceSpec{Name: "shared-ns"}},
				Glance:   &ServiceGlanceSpec{Namespace: &ServiceNamespaceSpec{Name: "shared-ns"}},
			}),
			want: []string{"shared-ns"},
		},
		{
			name: "placement co-located with glance yields one entry",
			cp: cpIn(ServicesSpec{
				Glance:    &ServiceGlanceSpec{Namespace: &ServiceNamespaceSpec{Name: "shared-ns"}},
				Placement: &ServicePlacementSpec{Namespace: &ServiceNamespaceSpec{Name: "shared-ns"}},
			}),
			want: []string{"shared-ns"},
		},
		{
			name: "a placement assignment naming the ControlPlane namespace contributes nothing",
			cp: cpIn(ServicesSpec{
				Placement: &ServicePlacementSpec{Namespace: &ServiceNamespaceSpec{Name: "openstack"}},
			}),
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, ns := range tc.cp.DedicatedServiceNamespaces() {
				got = append(got, ns.Name)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("DedicatedServiceNamespaces() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("DedicatedServiceNamespaces()[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestDedicatedGlanceBackingServicesAccessors exercises the nil-safe reads for
// the Glance service across the three states it can be in: no service block
// (services.glance nil), a block that shares the ControlPlane-wide instances
// (dedicatedBackingServices nil), and one that opted into dedicated instances.
func TestDedicatedGlanceBackingServicesAccessors(t *testing.T) {
	tests := []struct {
		name         string
		cp           *ControlPlane
		wantDatabase bool
		wantCache    bool
	}{
		{
			name: "no glance block",
			cp:   &ControlPlane{},
		},
		{
			name: "glance shares the ControlPlane-wide instances",
			cp: &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{
				Glance: &ServiceGlanceSpec{},
			}}},
		},
		{
			name: "glance takes a dedicated database only",
			cp: &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{
				Glance: &ServiceGlanceSpec{
					DedicatedBackingServices: &GlanceDedicatedBackingServicesSpec{
						Database: &commonv1.DatabaseSpec{Database: "glance"},
					},
				},
			}}},
			wantDatabase: true,
		},
		{
			name: "glance takes both dedicated instances",
			cp: &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{
				Glance: &ServiceGlanceSpec{
					DedicatedBackingServices: &GlanceDedicatedBackingServicesSpec{
						Database: &commonv1.DatabaseSpec{Database: "glance"},
						Cache:    &commonv1.CacheSpec{Backend: commonv1.DefaultCacheBackend},
					},
				},
			}}},
			wantDatabase: true,
			wantCache:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cp.DedicatedGlanceDatabase() != nil; got != tc.wantDatabase {
				t.Errorf("DedicatedGlanceDatabase() present = %v, want %v", got, tc.wantDatabase)
			}
			if got := tc.cp.DedicatedGlanceCache() != nil; got != tc.wantCache {
				t.Errorf("DedicatedGlanceCache() present = %v, want %v", got, tc.wantCache)
			}
		})
	}
}

// TestServiceGlanceSpecDeepCopy verifies the curated Glance subset round-trips
// through DeepCopy with independent pointer storage, in particular the backends
// slice and the S3 pointer nested in each entry: the reconciler DeepCopies the
// projected spec onto the Glance child, so an aliased S3 block here would let a
// child projection mutate the ControlPlane spec it was derived from.
func TestServiceGlanceSpecDeepCopy(t *testing.T) {
	replicas := int32(2)
	spec := ServiceGlanceSpec{
		Replicas: &replicas,
		Image:    &commonv1.ImageSpec{Repository: "ghcr.io/c5c3/glance", Tag: "2026.1"},
		Backends: []GlanceBackendEntry{
			{
				Name: "primary",
				Type: "S3",
				S3: &GlanceBackendS3Spec{
					Endpoint:             "https://s3.example.com",
					Bucket:               "images",
					CredentialsSecretRef: SecretNameRef{Name: "glance-s3"},
				},
				IsDefault: true,
			},
		},
		DedicatedBackingServices: &GlanceDedicatedBackingServicesSpec{
			Database: &commonv1.DatabaseSpec{Database: "glance"},
		},
	}

	clone := spec.DeepCopy()
	if clone.Replicas == spec.Replicas {
		t.Errorf("DeepCopy did not allocate a new *int32 for Replicas")
	}
	if &clone.Backends[0] == &spec.Backends[0] {
		t.Errorf("DeepCopy did not allocate a new backends slice")
	}
	if clone.Backends[0].S3 == spec.Backends[0].S3 {
		t.Errorf("DeepCopy did not allocate a new *GlanceBackendS3Spec for the backend")
	}

	// Mutating the clone's backend must not touch the source.
	clone.Backends[0].S3.Bucket = "other"
	if spec.Backends[0].S3.Bucket != "images" {
		t.Errorf("DeepCopy aliased the backend S3 block: source bucket changed to %q", spec.Backends[0].S3.Bucket)
	}
}

// TestPlacementNamespace exercises the nil-safe namespace resolver for the
// Placement service across the states the accessor can be in: no service block
// and a block without an assignment (both default to the ControlPlane's
// namespace), a webhook-bypass empty name (also a fallback), and an explicit
// assignment.
func TestPlacementNamespace(t *testing.T) {
	cpIn := func(placement *ServicePlacementSpec) *ControlPlane {
		return &ControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
			Spec:       ControlPlaneSpec{Services: ServicesSpec{Placement: placement}},
		}
	}
	tests := []struct {
		name string
		cp   *ControlPlane
		want string
	}{
		{"no placement block defaults to the ControlPlane namespace", cpIn(nil), "openstack"},
		{"placement block without an assignment defaults to the ControlPlane namespace", cpIn(&ServicePlacementSpec{}), "openstack"},
		{"an empty assignment name falls back to the ControlPlane namespace", cpIn(&ServicePlacementSpec{Namespace: &ServiceNamespaceSpec{}}), "openstack"},
		{"placement takes a namespace of its own", cpIn(&ServicePlacementSpec{Namespace: &ServiceNamespaceSpec{Name: "placement"}}), "placement"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cp.PlacementNamespace(); got != tc.want {
				t.Errorf("PlacementNamespace() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDedicatedPlacementBackingServicesAccessors exercises the nil-safe reads for
// the Placement service across the states it can be in: no service block
// (services.placement nil), a block that shares the ControlPlane-wide instances
// (dedicatedBackingServices nil), and one that opted into a dedicated database, a
// dedicated cache, or both.
func TestDedicatedPlacementBackingServicesAccessors(t *testing.T) {
	tests := []struct {
		name         string
		cp           *ControlPlane
		wantDatabase bool
		wantCache    bool
	}{
		{
			name: "no placement block",
			cp:   &ControlPlane{},
		},
		{
			name: "placement shares the ControlPlane-wide instances",
			cp: &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{
				Placement: &ServicePlacementSpec{},
			}}},
		},
		{
			name: "placement takes a dedicated database only",
			cp: &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{
				Placement: &ServicePlacementSpec{
					DedicatedBackingServices: &PlacementDedicatedBackingServicesSpec{
						Database: &commonv1.DatabaseSpec{Database: "placement"},
					},
				},
			}}},
			wantDatabase: true,
		},
		{
			name: "placement takes a dedicated cache only",
			cp: &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{
				Placement: &ServicePlacementSpec{
					DedicatedBackingServices: &PlacementDedicatedBackingServicesSpec{
						Cache: &commonv1.CacheSpec{Backend: commonv1.DefaultCacheBackend},
					},
				},
			}}},
			wantCache: true,
		},
		{
			name: "placement takes both dedicated instances",
			cp: &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{
				Placement: &ServicePlacementSpec{
					DedicatedBackingServices: &PlacementDedicatedBackingServicesSpec{
						Database: &commonv1.DatabaseSpec{Database: "placement"},
						Cache:    &commonv1.CacheSpec{Backend: commonv1.DefaultCacheBackend},
					},
				},
			}}},
			wantDatabase: true,
			wantCache:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cp.DedicatedPlacementDatabase() != nil; got != tc.wantDatabase {
				t.Errorf("DedicatedPlacementDatabase() present = %v, want %v", got, tc.wantDatabase)
			}
			if got := tc.cp.DedicatedPlacementCache() != nil; got != tc.wantCache {
				t.Errorf("DedicatedPlacementCache() present = %v, want %v", got, tc.wantCache)
			}
		})
	}
}

// TestServicePlacementSpecDeepCopy verifies the curated Placement subset
// round-trips through DeepCopy with independent storage, in particular the
// extraConfig map and the dedicated-backing-services pointer: the reconciler
// DeepCopies the projected spec onto the Placement child, so an aliased nested
// map here would let a child projection mutate the ControlPlane spec it was
// derived from.
func TestServicePlacementSpecDeepCopy(t *testing.T) {
	replicas := int32(2)
	spec := ServicePlacementSpec{
		Replicas:    &replicas,
		Image:       &commonv1.ImageSpec{Repository: "ghcr.io/c5c3/placement", Tag: "2026.1"},
		ExtraConfig: map[string]map[string]string{"placement": {"randomize_allocation_candidates": "true"}},
		DedicatedBackingServices: &PlacementDedicatedBackingServicesSpec{
			Database: &commonv1.DatabaseSpec{Database: "placement"},
		},
		Namespace: &ServiceNamespaceSpec{Name: "placement"},
	}

	clone := spec.DeepCopy()
	if clone.Replicas == spec.Replicas {
		t.Errorf("DeepCopy did not allocate a new *int32 for Replicas")
	}
	if clone.Image == spec.Image {
		t.Errorf("DeepCopy did not allocate a new *ImageSpec for Image")
	}
	if clone.DedicatedBackingServices == spec.DedicatedBackingServices {
		t.Errorf("DeepCopy did not allocate a new *PlacementDedicatedBackingServicesSpec")
	}
	if clone.DedicatedBackingServices.Database == spec.DedicatedBackingServices.Database {
		t.Errorf("DeepCopy did not allocate a new *DatabaseSpec for the dedicated database")
	}
	if clone.Namespace == spec.Namespace {
		t.Errorf("DeepCopy did not allocate a new *ServiceNamespaceSpec for Namespace")
	}

	// Mutating the clone's nested extraConfig section must not touch the source.
	clone.ExtraConfig["placement"]["randomize_allocation_candidates"] = "false"
	if spec.ExtraConfig["placement"]["randomize_allocation_candidates"] != "true" {
		t.Errorf("DeepCopy aliased the nested extraConfig section: source value changed to %q",
			spec.ExtraConfig["placement"]["randomize_allocation_candidates"])
	}
}

// TestBarbicanNamespace exercises the nil-safe namespace resolver for the
// Barbican service across the states the accessor can be in: no service block
// and a block without an assignment (both default to the ControlPlane's
// namespace), a webhook-bypass empty name (also a fallback), and an explicit
// assignment.
func TestBarbicanNamespace(t *testing.T) {
	cpIn := func(barbican *ServiceBarbicanSpec) *ControlPlane {
		return &ControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
			Spec:       ControlPlaneSpec{Services: ServicesSpec{Barbican: barbican}},
		}
	}
	tests := []struct {
		name string
		cp   *ControlPlane
		want string
	}{
		{"no barbican block defaults to the ControlPlane namespace", cpIn(nil), "openstack"},
		{"barbican block without an assignment defaults to the ControlPlane namespace", cpIn(&ServiceBarbicanSpec{}), "openstack"},
		{"an empty assignment name falls back to the ControlPlane namespace", cpIn(&ServiceBarbicanSpec{Namespace: &ServiceNamespaceSpec{}}), "openstack"},
		{"barbican takes a namespace of its own", cpIn(&ServiceBarbicanSpec{Namespace: &ServiceNamespaceSpec{Name: "barbican"}}), "barbican"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cp.BarbicanNamespace(); got != tc.want {
				t.Errorf("BarbicanNamespace() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDedicatedBarbicanBackingServicesAccessors exercises the nil-safe reads for
// the Barbican service across the states it can be in: no service block
// (services.barbican nil), a block that shares the ControlPlane-wide instances
// (dedicatedBackingServices nil), and one that opted into a dedicated database, a
// dedicated cache, or both.
func TestDedicatedBarbicanBackingServicesAccessors(t *testing.T) {
	tests := []struct {
		name         string
		cp           *ControlPlane
		wantDatabase bool
		wantCache    bool
	}{
		{
			name: "no barbican block",
			cp:   &ControlPlane{},
		},
		{
			name: "barbican shares the ControlPlane-wide instances",
			cp: &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{
				Barbican: &ServiceBarbicanSpec{},
			}}},
		},
		{
			name: "barbican takes a dedicated database only",
			cp: &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{
				Barbican: &ServiceBarbicanSpec{
					DedicatedBackingServices: &BarbicanDedicatedBackingServicesSpec{
						Database: &commonv1.DatabaseSpec{Database: "barbican"},
					},
				},
			}}},
			wantDatabase: true,
		},
		{
			name: "barbican takes a dedicated cache only",
			cp: &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{
				Barbican: &ServiceBarbicanSpec{
					DedicatedBackingServices: &BarbicanDedicatedBackingServicesSpec{
						Cache: &commonv1.CacheSpec{Backend: commonv1.DefaultCacheBackend},
					},
				},
			}}},
			wantCache: true,
		},
		{
			name: "barbican takes both dedicated instances",
			cp: &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{
				Barbican: &ServiceBarbicanSpec{
					DedicatedBackingServices: &BarbicanDedicatedBackingServicesSpec{
						Database: &commonv1.DatabaseSpec{Database: "barbican"},
						Cache:    &commonv1.CacheSpec{Backend: commonv1.DefaultCacheBackend},
					},
				},
			}}},
			wantDatabase: true,
			wantCache:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cp.DedicatedBarbicanDatabase() != nil; got != tc.wantDatabase {
				t.Errorf("DedicatedBarbicanDatabase() present = %v, want %v", got, tc.wantDatabase)
			}
			if got := tc.cp.DedicatedBarbicanCache() != nil; got != tc.wantCache {
				t.Errorf("DedicatedBarbicanCache() present = %v, want %v", got, tc.wantCache)
			}
		})
	}
}

// TestServiceBarbicanSpecDeepCopy verifies the curated Barbican subset
// round-trips through DeepCopy with independent storage, in particular the
// secret-store block and its nested Secret references: the reconciler DeepCopies
// the projected spec onto the Barbican child, so an aliased pointer here would
// let a child projection mutate the ControlPlane spec it was derived from.
func TestServiceBarbicanSpecDeepCopy(t *testing.T) {
	replicas := int32(2)
	spec := ServiceBarbicanSpec{
		Replicas:    &replicas,
		Image:       &commonv1.ImageSpec{Repository: "ghcr.io/c5c3/barbican", Tag: "2026.1"},
		ExtraConfig: map[string]map[string]string{"DEFAULT": {"debug": "true"}},
		SecretStore: ServiceBarbicanSecretStoreSpec{
			External: &BarbicanExternalSecretStoreSpec{
				URL:                  "https://openbao.example.com:8200",
				CredentialsSecretRef: barbicanv1alpha1.SecretNameRefSpec{Name: "barbican-approle"},
				CABundleSecretRef:    &barbicanv1alpha1.SecretNameRefSpec{Name: "openbao-ca"},
			},
		},
		DedicatedBackingServices: &BarbicanDedicatedBackingServicesSpec{
			Database: &commonv1.DatabaseSpec{Database: "barbican"},
		},
		Namespace: &ServiceNamespaceSpec{Name: "barbican"},
	}

	clone := spec.DeepCopy()
	if clone.Replicas == spec.Replicas {
		t.Errorf("DeepCopy did not allocate a new *int32 for Replicas")
	}
	if clone.SecretStore.External == spec.SecretStore.External {
		t.Errorf("DeepCopy did not allocate a new *BarbicanExternalSecretStoreSpec")
	}
	if clone.SecretStore.External.CABundleSecretRef == spec.SecretStore.External.CABundleSecretRef {
		t.Errorf("DeepCopy did not allocate a new *SecretNameRefSpec for the CA bundle reference")
	}
	if clone.DedicatedBackingServices == spec.DedicatedBackingServices {
		t.Errorf("DeepCopy did not allocate a new *BarbicanDedicatedBackingServicesSpec")
	}
	if clone.Namespace == spec.Namespace {
		t.Errorf("DeepCopy did not allocate a new *ServiceNamespaceSpec for Namespace")
	}

	// Mutating the clone's nested store must not touch the source.
	clone.SecretStore.External.CABundleSecretRef.Name = "other-ca"
	if spec.SecretStore.External.CABundleSecretRef.Name != "openbao-ca" {
		t.Errorf("DeepCopy aliased the CA bundle reference: source name changed to %q",
			spec.SecretStore.External.CABundleSecretRef.Name)
	}
}

// TestNeutronNamespace exercises the nil-safe namespace resolver for the
// network service across the states the accessor can be in: no service block
// and a block without an assignment (both default to the ControlPlane's
// namespace), a webhook-bypass empty name (also a fallback), and an explicit
// assignment.
func TestNeutronNamespace(t *testing.T) {
	cpIn := func(neutron *ServiceNeutronSpec) *ControlPlane {
		return &ControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
			Spec:       ControlPlaneSpec{Services: ServicesSpec{Neutron: neutron}},
		}
	}
	tests := []struct {
		name string
		cp   *ControlPlane
		want string
	}{
		{"no neutron block defaults to the ControlPlane namespace", cpIn(nil), "openstack"},
		{"neutron block without an assignment defaults to the ControlPlane namespace", cpIn(&ServiceNeutronSpec{}), "openstack"},
		{"an empty assignment name falls back to the ControlPlane namespace", cpIn(&ServiceNeutronSpec{Namespace: &ServiceNamespaceSpec{}}), "openstack"},
		{"neutron takes a namespace of its own", cpIn(&ServiceNeutronSpec{Namespace: &ServiceNamespaceSpec{Name: "neutron"}}), "neutron"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cp.NeutronNamespace(); got != tc.want {
				t.Errorf("NeutronNamespace() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDedicatedNeutronBackingServicesAccessors exercises the nil-safe reads for
// the network service across the states it can be in: no service block
// (services.neutron nil), a block that shares the ControlPlane-wide instances
// (dedicatedBackingServices nil), and one that opted into a dedicated database, a
// dedicated cache, or both.
func TestDedicatedNeutronBackingServicesAccessors(t *testing.T) {
	tests := []struct {
		name         string
		cp           *ControlPlane
		wantDatabase bool
		wantCache    bool
	}{
		{
			name: "no neutron block",
			cp:   &ControlPlane{},
		},
		{
			name: "neutron shares the ControlPlane-wide instances",
			cp: &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{
				Neutron: &ServiceNeutronSpec{},
			}}},
		},
		{
			name: "neutron takes a dedicated database only",
			cp: &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{
				Neutron: &ServiceNeutronSpec{
					DedicatedBackingServices: &NeutronDedicatedBackingServicesSpec{
						Database: &commonv1.DatabaseSpec{Database: "neutron"},
					},
				},
			}}},
			wantDatabase: true,
		},
		{
			name: "neutron takes a dedicated cache only",
			cp: &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{
				Neutron: &ServiceNeutronSpec{
					DedicatedBackingServices: &NeutronDedicatedBackingServicesSpec{
						Cache: &commonv1.CacheSpec{Backend: commonv1.DefaultCacheBackend},
					},
				},
			}}},
			wantCache: true,
		},
		{
			name: "neutron takes both dedicated instances",
			cp: &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{
				Neutron: &ServiceNeutronSpec{
					DedicatedBackingServices: &NeutronDedicatedBackingServicesSpec{
						Database: &commonv1.DatabaseSpec{Database: "neutron"},
						Cache:    &commonv1.CacheSpec{Backend: commonv1.DefaultCacheBackend},
					},
				},
			}}},
			wantDatabase: true,
			wantCache:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cp.DedicatedNeutronDatabase() != nil; got != tc.wantDatabase {
				t.Errorf("DedicatedNeutronDatabase() present = %v, want %v", got, tc.wantDatabase)
			}
			if got := tc.cp.DedicatedNeutronCache() != nil; got != tc.wantCache {
				t.Errorf("DedicatedNeutronCache() present = %v, want %v", got, tc.wantCache)
			}
		})
	}
}

// TestServiceNeutronSpecDeepCopy verifies the curated Neutron subset round-trips
// through DeepCopy with independent storage, in particular the extraConfig map,
// the worker replica count, and the dedicated-backing-services pointer: the
// reconciler DeepCopies the projected spec onto the Neutron child, so an aliased
// nested map here would let a child projection mutate the ControlPlane spec it
// was derived from.
func TestServiceNeutronSpecDeepCopy(t *testing.T) {
	replicas, workerReplicas := int32(2), int32(1)
	spec := ServiceNeutronSpec{
		Replicas:       &replicas,
		WorkerReplicas: &workerReplicas,
		Image:          &commonv1.ImageSpec{Repository: "ghcr.io/c5c3/neutron", Tag: "2026.1"},
		ExtraConfig:    map[string]map[string]string{"ml2": {"tenant_network_types": "geneve"}},
		OVN: NeutronOVNSpec{
			CentralRef: NeutronOVNCentralRef{Name: "ovn", Namespace: "networking"},
		},
		DedicatedBackingServices: &NeutronDedicatedBackingServicesSpec{
			Database: &commonv1.DatabaseSpec{Database: "neutron"},
		},
		Namespace: &ServiceNamespaceSpec{Name: "neutron"},
	}

	clone := spec.DeepCopy()
	if clone.Replicas == spec.Replicas {
		t.Errorf("DeepCopy did not allocate a new *int32 for Replicas")
	}
	if clone.WorkerReplicas == spec.WorkerReplicas {
		t.Errorf("DeepCopy did not allocate a new *int32 for WorkerReplicas")
	}
	if clone.Image == spec.Image {
		t.Errorf("DeepCopy did not allocate a new *ImageSpec for Image")
	}
	if clone.DedicatedBackingServices == spec.DedicatedBackingServices {
		t.Errorf("DeepCopy did not allocate a new *NeutronDedicatedBackingServicesSpec")
	}
	if clone.DedicatedBackingServices.Database == spec.DedicatedBackingServices.Database {
		t.Errorf("DeepCopy did not allocate a new *DatabaseSpec for the dedicated database")
	}
	if clone.Namespace == spec.Namespace {
		t.Errorf("DeepCopy did not allocate a new *ServiceNamespaceSpec for Namespace")
	}

	// Mutating the clone's nested extraConfig section and its OVN reference must
	// not touch the source.
	clone.ExtraConfig["ml2"]["tenant_network_types"] = "vlan"
	if spec.ExtraConfig["ml2"]["tenant_network_types"] != "geneve" {
		t.Errorf("DeepCopy aliased the nested extraConfig section: source value changed to %q",
			spec.ExtraConfig["ml2"]["tenant_network_types"])
	}
	clone.OVN.CentralRef.Namespace = "other"
	if spec.OVN.CentralRef.Namespace != "networking" {
		t.Errorf("DeepCopy aliased the OVN central reference: source namespace changed to %q",
			spec.OVN.CentralRef.Namespace)
	}

	var nilSpec *ServiceNeutronSpec
	if got := nilSpec.DeepCopy(); got != nil {
		t.Errorf("(*ServiceNeutronSpec)(nil).DeepCopy() = %v, want nil", got)
	}
}

// TestNeutronOVNCentralNamespace pins the resolution of the OVNCentral reference
// across the states it can be in: a ref carrying a namespace, a webhook-bypass
// ref with none (the ControlPlane's own namespace, the value the defaulting
// webhook writes), and no neutron block at all.
func TestNeutronOVNCentralNamespace(t *testing.T) {
	cpIn := func(neutron *ServiceNeutronSpec) *ControlPlane {
		return &ControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
			Spec:       ControlPlaneSpec{Services: ServicesSpec{Neutron: neutron}},
		}
	}
	central := func(namespace string) *ServiceNeutronSpec {
		return &ServiceNeutronSpec{OVN: NeutronOVNSpec{
			CentralRef: NeutronOVNCentralRef{Name: "ovn", Namespace: namespace},
		}}
	}
	tests := []struct {
		name string
		cp   *ControlPlane
		want string
	}{
		{"an explicit namespace is returned as written", cpIn(central("networking")), "networking"},
		{"an empty namespace falls back to the ControlPlane namespace", cpIn(central("")), "openstack"},
		{"no neutron block falls back to the ControlPlane namespace", cpIn(nil), "openstack"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cp.NeutronOVNCentralNamespace(); got != tc.want {
				t.Errorf("NeutronOVNCentralNamespace() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestServiceTargetClusterRefAccessors exercises the nil-safe target-cluster
// accessors across the states each can be in: no service block at all and a
// service block without a ref (both mean the service stays on the local cluster)
// and a block naming a cluster.
func TestServiceTargetClusterRefAccessors(t *testing.T) {
	ref := func(name string) *commonv1.TargetClusterRefSpec {
		return &commonv1.TargetClusterRefSpec{Name: name}
	}
	cpIn := func(services ServicesSpec) *ControlPlane {
		return &ControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
			Spec:       ControlPlaneSpec{Services: services},
		}
	}

	tests := []struct {
		name string
		cp   *ControlPlane
		// want maps the accessor name to the cluster it must resolve to; an
		// empty string demands a nil ref.
		want map[string]string
	}{
		{
			name: "no service blocks resolve to no placement",
			cp:   cpIn(ServicesSpec{}),
			want: map[string]string{},
		},
		{
			name: "service blocks without a ref resolve to no placement",
			cp: cpIn(ServicesSpec{
				Keystone:  &ServiceKeystoneSpec{},
				Horizon:   &ServiceHorizonSpec{},
				Glance:    &ServiceGlanceSpec{},
				Placement: &ServicePlacementSpec{},
				Barbican:  &ServiceBarbicanSpec{},
				Neutron:   &ServiceNeutronSpec{},
			}),
			want: map[string]string{},
		},
		{
			name: "each service takes a cluster of its own",
			cp: cpIn(ServicesSpec{
				Keystone:  &ServiceKeystoneSpec{TargetClusterRef: ref("edge-identity")},
				Horizon:   &ServiceHorizonSpec{TargetClusterRef: ref("edge-dashboard")},
				Glance:    &ServiceGlanceSpec{TargetClusterRef: ref("edge-images")},
				Placement: &ServicePlacementSpec{TargetClusterRef: ref("edge-placement")},
				Barbican:  &ServiceBarbicanSpec{TargetClusterRef: ref("edge-secrets")},
				Neutron:   &ServiceNeutronSpec{TargetClusterRef: ref("edge-networking")},
			}),
			want: map[string]string{
				"KeystoneTargetClusterRef":  "edge-identity",
				"HorizonTargetClusterRef":   "edge-dashboard",
				"GlanceTargetClusterRef":    "edge-images",
				"PlacementTargetClusterRef": "edge-placement",
				"BarbicanTargetClusterRef":  "edge-secrets",
				"NeutronTargetClusterRef":   "edge-networking",
			},
		},
		{
			// One service placed, the rest local: the accessors must stay
			// independent of each other.
			name: "an unplaced service resolves to nil beside a placed one",
			cp: cpIn(ServicesSpec{
				Keystone: &ServiceKeystoneSpec{TargetClusterRef: ref("edge-identity")},
				Horizon:  &ServiceHorizonSpec{},
			}),
			want: map[string]string{"KeystoneTargetClusterRef": "edge-identity"},
		},
		{
			// A neutron block with no ref beside a placed neutron-less plane:
			// the accessor must not read through the missing block.
			name: "a neutron block without a ref resolves to nil",
			cp: cpIn(ServicesSpec{
				Neutron:   &ServiceNeutronSpec{},
				Placement: &ServicePlacementSpec{TargetClusterRef: ref("edge-placement")},
			}),
			want: map[string]string{"PlacementTargetClusterRef": "edge-placement"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := map[string]*commonv1.TargetClusterRefSpec{
				"KeystoneTargetClusterRef":  tc.cp.KeystoneTargetClusterRef(),
				"HorizonTargetClusterRef":   tc.cp.HorizonTargetClusterRef(),
				"GlanceTargetClusterRef":    tc.cp.GlanceTargetClusterRef(),
				"PlacementTargetClusterRef": tc.cp.PlacementTargetClusterRef(),
				"BarbicanTargetClusterRef":  tc.cp.BarbicanTargetClusterRef(),
				"NeutronTargetClusterRef":   tc.cp.NeutronTargetClusterRef(),
			}
			for accessor, gotRef := range got {
				want := tc.want[accessor]
				if want == "" {
					if gotRef != nil {
						t.Errorf("%s() = %q, want nil", accessor, gotRef.Name)
					}
					continue
				}
				if gotRef == nil {
					t.Errorf("%s() = nil, want %q", accessor, want)
					continue
				}
				if gotRef.Name != want {
					t.Errorf("%s() = %q, want %q", accessor, gotRef.Name, want)
				}
			}
		})
	}
}

// TestTargetClusterNames pins the enumeration of the clusters a ControlPlane
// places services on: deduplicated, in the stable keystone→horizon→glance→
// placement→barbican→neutron order, and empty for a local-only ControlPlane.
func TestTargetClusterNames(t *testing.T) {
	ref := func(name string) *commonv1.TargetClusterRefSpec {
		return &commonv1.TargetClusterRefSpec{Name: name}
	}
	cpIn := func(services ServicesSpec) *ControlPlane {
		return &ControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
			Spec:       ControlPlaneSpec{Services: services},
		}
	}

	tests := []struct {
		name string
		cp   *ControlPlane
		want []string
	}{
		{
			name: "a local-only ControlPlane places nothing",
			cp: cpIn(ServicesSpec{
				Keystone: &ServiceKeystoneSpec{},
				Horizon:  &ServiceHorizonSpec{},
			}),
		},
		{
			// The refs are named against the reverse of the enumeration order,
			// so an accidental sort would show up here.
			name: "placed services are enumerated in service order, not by name",
			cp: cpIn(ServicesSpec{
				Keystone:  &ServiceKeystoneSpec{TargetClusterRef: ref("edge-f")},
				Horizon:   &ServiceHorizonSpec{TargetClusterRef: ref("edge-e")},
				Glance:    &ServiceGlanceSpec{TargetClusterRef: ref("edge-d")},
				Placement: &ServicePlacementSpec{TargetClusterRef: ref("edge-c")},
				Barbican:  &ServiceBarbicanSpec{TargetClusterRef: ref("edge-b")},
				Neutron:   &ServiceNeutronSpec{TargetClusterRef: ref("edge-a")},
			}),
			want: []string{"edge-f", "edge-e", "edge-d", "edge-c", "edge-b", "edge-a"},
		},
		{
			// The neutron ref is enumerated last, and a nil neutron block
			// contributes nothing.
			name: "a placed neutron is enumerated after barbican",
			cp: cpIn(ServicesSpec{
				Barbican: &ServiceBarbicanSpec{TargetClusterRef: ref("edge-secrets")},
				Neutron:  &ServiceNeutronSpec{TargetClusterRef: ref("edge-networking")},
			}),
			want: []string{"edge-secrets", "edge-networking"},
		},
		{
			name: "services sharing a cluster yield one entry, first occurrence winning",
			cp: cpIn(ServicesSpec{
				Keystone: &ServiceKeystoneSpec{TargetClusterRef: ref("edge-one")},
				Glance:   &ServiceGlanceSpec{TargetClusterRef: ref("edge-one")},
				Barbican: &ServiceBarbicanSpec{TargetClusterRef: ref("edge-two")},
			}),
			want: []string{"edge-one", "edge-two"},
		},
		{
			// Webhook-bypass shape: an empty name is not a placement, and must
			// never reach a registry lookup as the empty cluster name.
			name: "an empty ref name is not a placement",
			cp: cpIn(ServicesSpec{
				Keystone: &ServiceKeystoneSpec{TargetClusterRef: &commonv1.TargetClusterRefSpec{}},
				Horizon:  &ServiceHorizonSpec{TargetClusterRef: ref("edge-dashboard")},
			}),
			want: []string{"edge-dashboard"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cp.TargetClusterNames()
			if len(got) != len(tc.want) {
				t.Fatalf("TargetClusterNames() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("TargetClusterNames()[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
