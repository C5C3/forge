// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// TestTargetClusterRefForNamespace pins the namespace-to-cluster resolution every
// placed sub-reconciler routes its writes through: the ControlPlane's own
// namespace stays local, a dedicated namespace answers with the ref of the
// service that declared it, and a namespace nobody declared is local too.
func TestTargetClusterRefForNamespace(t *testing.T) {
	placed := func() *c5c3v1alpha1.ControlPlane {
		return &c5c3v1alpha1.ControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
			Spec: c5c3v1alpha1.ControlPlaneSpec{
				Services: c5c3v1alpha1.ServicesSpec{
					Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
						Namespace:        &c5c3v1alpha1.ServiceNamespaceSpec{Name: "identity"},
						TargetClusterRef: &commonv1.TargetClusterRefSpec{Name: "edge-a"},
					},
					Horizon: &c5c3v1alpha1.ServiceHorizonSpec{
						Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{Name: "dashboard"},
					},
					Neutron: &c5c3v1alpha1.ServiceNeutronSpec{
						Namespace:        &c5c3v1alpha1.ServiceNamespaceSpec{Name: "network"},
						TargetClusterRef: &commonv1.TargetClusterRefSpec{Name: "edge-b"},
					},
				},
			},
		}
	}

	// A webhook-bypassed CR: the ref is set, the namespace block that must
	// accompany it is not, so the service resolves to the ControlPlane's own
	// namespace — which never moves.
	bypassed := placed()
	bypassed.Spec.Services.Keystone.Namespace = nil

	// The same shape on the network service: a co-located Neutron contributes the
	// ControlPlane's own namespace to the table, which the early return has
	// already answered.
	bypassedNeutron := placed()
	bypassedNeutron.Spec.Services.Neutron.Namespace = nil

	tests := []struct {
		name      string
		cp        *c5c3v1alpha1.ControlPlane
		namespace string
		want      string // "" = expect nil
	}{
		{name: "the ControlPlane's own namespace is never placed", cp: placed(), namespace: "openstack"},
		{name: "a placed service's namespace answers with its ref", cp: placed(), namespace: "identity", want: "edge-a"},
		{name: "an unplaced service's namespace is local", cp: placed(), namespace: "dashboard"},
		{name: "a namespace no service declares is local", cp: placed(), namespace: "unknown"},
		{name: "a ref without a namespace block leaves the own namespace local", cp: bypassed, namespace: "openstack"},
		{name: "the network service's namespace answers with its ref", cp: placed(), namespace: "network", want: "edge-b"},
		{name: "a co-located neutron leaves the own namespace local", cp: bypassedNeutron, namespace: "openstack"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			if ref := targetClusterRefForNamespace(tc.cp, tc.namespace); ref != nil {
				got = ref.Name
			}
			if got != tc.want {
				t.Errorf("targetClusterRefForNamespace(%q) = %q, want %q", tc.namespace, got, tc.want)
			}
		})
	}
}

// TestSameTargetCluster covers the four combinations of the placement
// comparison, nil (the local cluster) included: it is compared against a ref as
// often as two refs are compared against each other.
func TestSameTargetCluster(t *testing.T) {
	edgeA := &commonv1.TargetClusterRefSpec{Name: "edge-a"}
	edgeB := &commonv1.TargetClusterRefSpec{Name: "edge-b"}

	tests := []struct {
		name string
		a, b *commonv1.TargetClusterRefSpec
		want bool
	}{
		{name: "both local", want: true},
		{name: "local against a placed ref", b: edgeA},
		{name: "a placed ref against local", a: edgeA},
		{name: "the same cluster", a: edgeA, b: &commonv1.TargetClusterRefSpec{Name: "edge-a"}, want: true},
		{name: "different clusters", a: edgeA, b: edgeB},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameTargetCluster(tc.a, tc.b); got != tc.want {
				t.Errorf("sameTargetCluster(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestIntervalToCron(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		want     string
		wantErr  bool
	}{
		{
			name:     "168h maps to weekly Sunday midnight",
			interval: 168 * time.Hour,
			want:     "0 0 * * 0",
		},
		{
			name:     "24h maps to daily midnight",
			interval: 24 * time.Hour,
			want:     "0 0 * * *",
		},
		{
			name:     "multiple of 24h maps to daily midnight",
			interval: 72 * time.Hour,
			want:     "0 0 * * *",
		},
		{
			name:     "unsupported interval returns an error",
			interval: 5 * time.Hour,
			wantErr:  true,
		},
		{
			name:     "zero interval returns an error",
			interval: 0,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := intervalToCron(tt.interval)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("intervalToCron(%v) = %q, want error", tt.interval, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("intervalToCron(%v) returned unexpected error: %v", tt.interval, err)
			}
			if got != tt.want {
				t.Errorf("intervalToCron(%v) = %q, want %q", tt.interval, got, tt.want)
			}
		})
	}
}

func TestIntervalToCronErrorNamesUnsupportedValue(t *testing.T) {
	const interval = 5 * time.Hour
	_, err := intervalToCron(interval)
	if err == nil {
		t.Fatalf("intervalToCron(%v) = nil error, want error naming the value", interval)
	}
	if !strings.Contains(err.Error(), interval.String()) {
		t.Errorf("error %q does not name unsupported value %q", err.Error(), interval.String())
	}
}

// TestEffectiveBackingServices pins the shared-by-default / dedicated-on-request
// resolution every consumer of a backing service routes through: a service that
// opted in resolves to ITS instance, one that did not resolves to the
// ControlPlane-wide one, and an unresolvable instance (External mode, or a
// webhook-bypassed CR with no infrastructure block) resolves to nil so callers
// fail closed instead of dereferencing it.
func TestEffectiveBackingServices(t *testing.T) {
	sharedInfra := func() *c5c3v1alpha1.InfrastructureSpec {
		return &c5c3v1alpha1.InfrastructureSpec{
			Database: commonv1.DatabaseSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "openstack-db"},
				Database:   "keystone",
			},
			Cache: commonv1.CacheSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "openstack-memcached"},
				Backend:    commonv1.DefaultCacheBackend,
			},
		}
	}

	tests := []struct {
		name              string
		cp                *c5c3v1alpha1.ControlPlane
		wantKeystoneDB    string // "" = expect nil
		wantKeystoneCache string
		wantHorizonCache  string
		wantNeutronDB     string
		wantNeutronCache  string
	}{
		{
			name: "no dedicated blocks: every service shares the ControlPlane-wide instances",
			cp: &c5c3v1alpha1.ControlPlane{Spec: c5c3v1alpha1.ControlPlaneSpec{
				Infrastructure: sharedInfra(),
				Services: c5c3v1alpha1.ServicesSpec{
					Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{},
					Horizon:  &c5c3v1alpha1.ServiceHorizonSpec{},
					Neutron:  &c5c3v1alpha1.ServiceNeutronSpec{},
				},
			}},
			wantKeystoneDB:    "openstack-db",
			wantKeystoneCache: "openstack-memcached",
			wantHorizonCache:  "openstack-memcached",
			wantNeutronDB:     "openstack-db",
			wantNeutronCache:  "openstack-memcached",
		},
		{
			name: "keystone takes a dedicated database only: its cache stays shared",
			cp: &c5c3v1alpha1.ControlPlane{Spec: c5c3v1alpha1.ControlPlaneSpec{
				Infrastructure: sharedInfra(),
				Services: c5c3v1alpha1.ServicesSpec{
					Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
						DedicatedBackingServices: &c5c3v1alpha1.KeystoneDedicatedBackingServicesSpec{
							Database: &commonv1.DatabaseSpec{
								ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-db"},
								Database:   "keystone",
							},
						},
					},
					Horizon: &c5c3v1alpha1.ServiceHorizonSpec{},
				},
			}},
			wantKeystoneDB:    "cp-keystone-db",
			wantKeystoneCache: "openstack-memcached",
			wantHorizonCache:  "openstack-memcached",
			wantNeutronDB:     "openstack-db",
			wantNeutronCache:  "openstack-memcached",
		},
		{
			name: "each service takes its own dedicated cache",
			cp: &c5c3v1alpha1.ControlPlane{Spec: c5c3v1alpha1.ControlPlaneSpec{
				Infrastructure: sharedInfra(),
				Services: c5c3v1alpha1.ServicesSpec{
					Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
						DedicatedBackingServices: &c5c3v1alpha1.KeystoneDedicatedBackingServicesSpec{
							Cache: &commonv1.CacheSpec{
								ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-cache"},
								Backend:    commonv1.DefaultCacheBackend,
							},
						},
					},
					Horizon: &c5c3v1alpha1.ServiceHorizonSpec{
						DedicatedBackingServices: &c5c3v1alpha1.HorizonDedicatedBackingServicesSpec{
							Cache: &commonv1.CacheSpec{
								ClusterRef: &corev1.LocalObjectReference{Name: "cp-horizon-cache"},
								Backend:    commonv1.DefaultCacheBackend,
							},
						},
					},
				},
			}},
			wantKeystoneDB:    "openstack-db",
			wantKeystoneCache: "cp-keystone-cache",
			wantHorizonCache:  "cp-horizon-cache",
			wantNeutronDB:     "openstack-db",
			wantNeutronCache:  "openstack-memcached",
		},
		{
			name: "neutron takes both instances dedicated: keystone keeps the shared ones",
			cp: &c5c3v1alpha1.ControlPlane{Spec: c5c3v1alpha1.ControlPlaneSpec{
				Infrastructure: sharedInfra(),
				Services: c5c3v1alpha1.ServicesSpec{
					Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{},
					Neutron: &c5c3v1alpha1.ServiceNeutronSpec{
						DedicatedBackingServices: &c5c3v1alpha1.NeutronDedicatedBackingServicesSpec{
							Database: &commonv1.DatabaseSpec{
								ClusterRef: &corev1.LocalObjectReference{Name: "cp-neutron-db"},
								Database:   "neutron",
							},
							Cache: &commonv1.CacheSpec{
								ClusterRef: &corev1.LocalObjectReference{Name: "cp-neutron-cache"},
								Backend:    commonv1.DefaultCacheBackend,
							},
						},
					},
				},
			}},
			wantKeystoneDB:    "openstack-db",
			wantKeystoneCache: "openstack-memcached",
			wantHorizonCache:  "openstack-memcached",
			wantNeutronDB:     "cp-neutron-db",
			wantNeutronCache:  "cp-neutron-cache",
		},
		{
			name: "no infrastructure block and no dedicated instances: nothing resolves",
			cp: &c5c3v1alpha1.ControlPlane{Spec: c5c3v1alpha1.ControlPlaneSpec{
				Services: c5c3v1alpha1.ServicesSpec{Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{}},
			}},
		},
	}

	clusterRefName := func(ref *corev1.LocalObjectReference) string {
		if ref == nil {
			return ""
		}
		return ref.Name
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotDB string
			if db := effectiveKeystoneDatabase(tc.cp); db != nil {
				gotDB = clusterRefName(db.ClusterRef)
			}
			if gotDB != tc.wantKeystoneDB {
				t.Errorf("effectiveKeystoneDatabase() = %q, want %q", gotDB, tc.wantKeystoneDB)
			}

			var gotKSCache string
			if cache := effectiveKeystoneCache(tc.cp); cache != nil {
				gotKSCache = clusterRefName(cache.ClusterRef)
			}
			if gotKSCache != tc.wantKeystoneCache {
				t.Errorf("effectiveKeystoneCache() = %q, want %q", gotKSCache, tc.wantKeystoneCache)
			}

			var gotHZCache string
			if cache := effectiveHorizonCache(tc.cp); cache != nil {
				gotHZCache = clusterRefName(cache.ClusterRef)
			}
			if gotHZCache != tc.wantHorizonCache {
				t.Errorf("effectiveHorizonCache() = %q, want %q", gotHZCache, tc.wantHorizonCache)
			}

			var gotNTDB string
			if db := effectiveNeutronDatabase(tc.cp); db != nil {
				gotNTDB = clusterRefName(db.ClusterRef)
			}
			if gotNTDB != tc.wantNeutronDB {
				t.Errorf("effectiveNeutronDatabase() = %q, want %q", gotNTDB, tc.wantNeutronDB)
			}

			var gotNTCache string
			if cache := effectiveNeutronCache(tc.cp); cache != nil {
				gotNTCache = clusterRefName(cache.ClusterRef)
			}
			if gotNTCache != tc.wantNeutronCache {
				t.Errorf("effectiveNeutronCache() = %q, want %q", gotNTCache, tc.wantNeutronCache)
			}
		})
	}
}
