// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Sub-reconciler instrumentation helper for the ControlPlane controller
package controller

import (
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/c5c3/cobaltcore/internal/common/instrumentation"
)

// subReconcilerConditionTypes maps a sub_reconciler label value to the
// condition_type it drives. The instrumenter consults this map to attribute
// errors to the correct Ready sub-condition.
//
// Every value MUST be a member of subConditionTypes or of the KeystoneService
// CR's keystoneServiceSubConditionTypes; the drift-guard test
// TestSubReconcilerConditionTypesCoversAllNames asserts this invariant. If a
// sub_reconciler name reaches the instrumenter without a key here, the helper
// falls back to instrumentation.ConditionTypeUnknown ("UNKNOWN") rather than
// an empty label so the drift surfaces in alerts.
var subReconcilerConditionTypes = map[string]string{
	"Namespaces":      conditionTypeNamespacesReady,
	"Infrastructure":  conditionTypeInfrastructureReady,
	"ESOTenantStore":  conditionTypeESOTenantStoreReady,
	"DBCredentials":   conditionTypeDBCredentialsReady,
	"Keystone":        conditionTypeKeystoneReady,
	"Horizon":         conditionTypeHorizonReady,
	"Glance":          conditionTypeGlanceReady,
	"Placement":       conditionTypePlacementReady,
	"Barbican":        conditionTypeBarbicanReady,
	"OVN":             conditionTypeOVNReady,
	"Neutron":         conditionTypeNeutronReady,
	"KORC":            conditionTypeKORCReady,
	"AdminCredential": conditionTypeAdminCredentialReady,
	"AdminPassword":   conditionTypeAdminPasswordReady,
	"Catalog":         conditionTypeCatalogReady,
	"ServiceAccounts": conditionTypeServiceAccountsReady,
	// The per-tenant stores of the allowlisted namespaces standalone registrations
	// come from, kept apart from ESOTenantStore's own series because the two carry
	// different blast radii and one alert should not read as the other.
	"RegistrationTenantStores": conditionTypeRegistrationTenantStoresReady,
	// The KeystoneService controller's two block legs. The names carry the CR
	// kind as a prefix because "Catalog" and "ServiceAccounts" already label the
	// ControlPlane's own legs (the identity row and the registration
	// aggregation), and the two must stay distinguishable per metric series.
	"KeystoneServiceCatalog": conditionTypeKeystoneServiceCatalogReady,
	"KeystoneServiceAccount": conditionTypeKeystoneServiceAccountReady,
}

// instrumenter wraps every sub-reconciler call with the shared duration/error
// instrumentation (c5c3_operator_reconcile_duration_seconds and
// c5c3_operator_reconcile_errors_total). It owns its metric vectors, which
// RegisterMetrics exposes on the controller-runtime registry at startup. The
// var indirection lets unit tests rebind it to an isolated prometheus registry
// without polluting the production registry; production code MUST NOT reassign
// it.
var instrumenter = instrumentation.NewSubReconcilerInstrumenter("c5c3_operator", subReconcilerConditionTypes)

// RegisterMetrics exposes the operator's sub-reconciler duration/error vectors
// on the controller-runtime registry, returning an error on a
// duplicate-registration rather than panicking mid-reconcile so main.go can
// fail startup cleanly. Call it exactly once during operator setup.
func RegisterMetrics() error {
	return instrumenter.Register(ctrlmetrics.Registry)
}
