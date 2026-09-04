// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"fmt"
	"reflect"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/c5c3/cobaltcore/internal/common/config"
	"github.com/c5c3/cobaltcore/internal/common/release"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
	horizonv1alpha1 "github.com/c5c3/cobaltcore/operators/horizon/api/v1alpha1"
	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	placementv1alpha1 "github.com/c5c3/cobaltcore/operators/placement/api/v1alpha1"
)

// MergedExtraConfig merges the ControlPlane-wide globalExtraConfig with one
// service's own extraConfig and returns the effective INI block projected onto
// that service's child CR. Sections are unioned, the per-service value wins per
// key, and a global key with no per-service counterpart stays effective. A
// merge that produces no sections is normalized to nil, so an empty merge
// projects an absent field rather than an empty map.
//
// This is the SINGLE merge function both the reconciler and the validating
// webhook consume: validating one merge while projecting another is exactly the
// drift this helper exists to prevent, so admission and reconciliation always
// agree on the effective config.
func MergedExtraConfig(global, service map[string]map[string]string) map[string]map[string]string {
	// config.MergeDefaults(userConfig, defaults) merges key by key with
	// userConfig winning, into a fresh map without mutating either input, so the
	// per-service block is the userConfig and the global block is the defaults.
	// It never returns nil, hence the length-zero normalization below.
	merged := config.MergeDefaults(service, global)
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// DerivedPublicEndpoint returns the public URL the ControlPlane will derive for
// the dashboard. It is shared by the identity-backend projection and the
// admission-time trusted-dashboard check so both derive the gate identically.
//
// An explicit publicEndpoint wins (with any trailing slash trimmed); otherwise,
// when a gateway is set, it is derived as "https://{gateway.hostname}" (the
// default-443 form). A nil receiver (services.horizon unset) and the
// neither-set case both yield "".
func (s *ServiceHorizonSpec) DerivedPublicEndpoint() string {
	if s == nil {
		return ""
	}
	if s.PublicEndpoint != "" {
		return strings.TrimRight(s.PublicEndpoint, "/")
	}
	if s.Gateway != nil && s.Gateway.Hostname != "" {
		return "https://" + s.Gateway.Hostname
	}
	return ""
}

// iniBlock pairs a free-form INI extraConfig map with the spec field path it
// lives at, so a finding on the MERGED config can be attributed back to the
// concrete block (globalExtraConfig or the per-service block) that contributed
// the offending (section, key). Provenance is by membership: a block contributes
// a pair exactly when block[section][key] is present.
type iniBlock struct {
	config map[string]map[string]string
	path   *field.Path
}

// contributingKeyPaths returns the field paths of the blocks that carry
// (section, key), each anchored at the real leaf (e.g.
// spec.globalExtraConfig[federation][trusted_dashboard]). A pair present in two
// blocks yields two paths, one per block.
func contributingKeyPaths(section, key string, blocks ...iniBlock) []*field.Path {
	var paths []*field.Path
	for _, b := range blocks {
		if _, ok := b.config[section][key]; ok {
			paths = append(paths, b.path.Key(section).Key(key))
		}
	}
	return paths
}

// joinPaths renders a slice of field paths for an admission-warning message.
func joinPaths(paths []*field.Path) string {
	parts := make([]string, len(paths))
	for i, p := range paths {
		parts[i] = p.String()
	}
	return strings.Join(parts, ", ")
}

// validateExtraConfigOwnership runs the shape and ownership checks (Family A) on
// every create and every update, UN-gated: unlike the option-catalog checks it
// depends on nothing a regenerated catalog can invalidate, so a stale-valid CR
// is never at risk. It rejects malformed blocks (empty section/key/setting
// names, non-Python-identifier Horizon settings), forbids overrides of the
// operator-owned keys the ControlPlane projects, and warns on honored-but-owned
// overrides — attributing every finding to the concrete block(s) that carry it.
func validateExtraConfigOwnership(cp *ControlPlane) (admission.Warnings, field.ErrorList) {
	var warnings admission.Warnings
	var errs field.ErrorList

	specPath := field.NewPath("spec")
	globalPath := specPath.Child("globalExtraConfig")
	keystonePath := specPath.Child("services", "keystone", "extraConfig")
	glancePath := specPath.Child("services", "glance", "extraConfig")
	placementPath := specPath.Child("services", "placement", "extraConfig")
	barbicanPath := specPath.Child("services", "barbican", "extraConfig")
	neutronPath := specPath.Child("services", "neutron", "extraConfig")
	horizonPath := specPath.Child("services", "horizon", "extraConfig")

	// --- Shape checks -----------------------------------------------------
	errs = append(errs, validateINIShape(globalPath, cp.Spec.GlobalExtraConfig)...)
	if ks := cp.Spec.Services.Keystone; ks != nil {
		errs = append(errs, validateINIShape(keystonePath, ks.ExtraConfig)...)
	}
	if gl := cp.Spec.Services.Glance; gl != nil {
		errs = append(errs, validateINIShape(glancePath, gl.ExtraConfig)...)
	}
	if pl := cp.Spec.Services.Placement; pl != nil {
		errs = append(errs, validateINIShape(placementPath, pl.ExtraConfig)...)
	}
	if bn := cp.Spec.Services.Barbican; bn != nil {
		errs = append(errs, validateINIShape(barbicanPath, bn.ExtraConfig)...)
	}
	if nn := cp.Spec.Services.Neutron; nn != nil {
		errs = append(errs, validateINIShape(neutronPath, nn.ExtraConfig)...)
	}
	if hz := cp.Spec.Services.Horizon; hz != nil {
		for name := range hz.ExtraConfig {
			if name == "" {
				errs = append(errs, field.Invalid(horizonPath, name,
					"extraConfig setting name must not be empty"))
				continue
			}
			if !horizonv1alpha1.PythonSettingName.MatchString(name) {
				errs = append(errs, field.Invalid(horizonPath.Key(name), name,
					"extraConfig setting name must be a valid Python identifier ([A-Za-z_][A-Za-z0-9_]*)"))
			}
		}
	}

	// --- Keystone merged-result ownership ---------------------------------
	// Skipped in External mode: no Keystone workload is deployed, so there is no
	// config the merged block would project (the extraConfig field itself is
	// rejected by validateKeystoneMode there).
	if ks := cp.Spec.Services.Keystone; ks != nil && !cp.IsExternalKeystone() {
		blocks := []iniBlock{{cp.Spec.GlobalExtraConfig, globalPath}, {ks.ExtraConfig, keystonePath}}
		merged := MergedExtraConfig(cp.Spec.GlobalExtraConfig, ks.ExtraConfig)
		for _, owned := range config.FindOwnedOverrides(merged, keystonev1alpha1.OwnedConfigKeys) {
			paths := contributingKeyPaths(owned.Section, owned.Key, blocks...)
			// The sole Rejected keystone key is [federation] trusted_dashboard.
			// The ControlPlane owns it only when it derives a dashboard endpoint
			// from services.horizon; when none is derived, an externally-run
			// dashboard doing WebSSO against the managed Keystone is legitimate, so
			// the override is honored-but-reported rather than forbidden.
			if owned.Rejected && cp.Spec.Services.Horizon.DerivedPublicEndpoint() != "" {
				for _, p := range paths {
					errs = append(errs, field.Forbidden(p, rejectedKeystoneMessage(owned)))
				}
				continue
			}
			warnings = append(warnings, ownedINIWarning(owned, paths))
		}
	}

	// --- Glance merged-result ownership -----------------------------------
	if gl := cp.Spec.Services.Glance; gl != nil {
		blocks := []iniBlock{{cp.Spec.GlobalExtraConfig, globalPath}, {gl.ExtraConfig, glancePath}}
		merged := MergedExtraConfig(cp.Spec.GlobalExtraConfig, gl.ExtraConfig)
		for _, owned := range config.FindOwnedOverrides(merged, glancev1alpha1.OwnedConfigKeys) {
			paths := contributingKeyPaths(owned.Section, owned.Key, blocks...)
			// Every Rejected glance key is always forbidden: rendering
			// [keystone_authtoken] password would leak the service password into the
			// namespace-readable ConfigMap, rendering an [import_filtering_opts]
			// key would loosen the web-download URI filter past every gate
			// services.glance.importFiltering puts in front of it, rendering a
			// [DEFAULT] image_cache_* key ends in an evicted glance-api pod or in
			// database rows nothing reclaims, and rendering an
			// [image_import_opts] image_import_plugins, [image_conversion]
			// output_format or [inject_metadata_properties] inject /
			// ignore_user_roles key bypasses what services.glance.importPlugins
			// guarantees about them: the fixed decompression-before-conversion
			// order, the output-format enum, and the oslo Dict syntax the injected
			// property names are validated against.
			if owned.Rejected {
				for _, p := range paths {
					errs = append(errs, field.Forbidden(p, rejectedOwnedKeyMessage(owned)))
				}
				continue
			}
			warnings = append(warnings, ownedINIWarning(owned, paths))
		}
	}

	// --- Placement merged-result ownership --------------------------------
	if pl := cp.Spec.Services.Placement; pl != nil {
		blocks := []iniBlock{{cp.Spec.GlobalExtraConfig, globalPath}, {pl.ExtraConfig, placementPath}}
		merged := MergedExtraConfig(cp.Spec.GlobalExtraConfig, pl.ExtraConfig)
		for _, owned := range config.FindOwnedOverrides(merged, placementv1alpha1.OwnedConfigKeys) {
			paths := contributingKeyPaths(owned.Section, owned.Key, blocks...)
			// Every Rejected placement key is always forbidden: rendering
			// [keystone_authtoken] password would leak the service password into the
			// namespace-readable ConfigMap, and rendering auth_strategy (the [api]
			// spelling placement reads, or the deprecated [DEFAULT] alias oslo.config
			// still honors) puts the API on the named auth middleware, where noauth2
			// drops token validation entirely and takes project and role from the
			// x-auth-token header, on an endpoint services.placement.gateway can
			// publish outside the cluster.
			if owned.Rejected {
				for _, p := range paths {
					errs = append(errs, field.Forbidden(p, rejectedOwnedKeyMessage(owned)))
				}
				continue
			}
			warnings = append(warnings, ownedINIWarning(owned, paths))
		}
	}

	// --- Barbican merged-result ownership ---------------------------------
	if bn := cp.Spec.Services.Barbican; bn != nil {
		blocks := []iniBlock{{cp.Spec.GlobalExtraConfig, globalPath}, {bn.ExtraConfig, barbicanPath}}
		merged := MergedExtraConfig(cp.Spec.GlobalExtraConfig, bn.ExtraConfig)
		for _, owned := range config.FindOwnedOverrides(merged, barbicanv1alpha1.OwnedConfigKeys) {
			paths := contributingKeyPaths(owned.Section, owned.Key, blocks...)
			// The three Rejected barbican keys are the credential ones:
			// [keystone_authtoken] password and the two [vault_plugin] secrets
			// (approle_secret_id, root_token_id). None is read from the file at
			// runtime — each arrives through an env override — so rendering one
			// gains nothing and copies credential material into the config Secret
			// every API pod mounts. root_token_id additionally replaces the
			// mount-scoped AppRole with an unscoped credential.
			if owned.Rejected {
				for _, p := range paths {
					errs = append(errs, field.Forbidden(p, rejectedOwnedKeyMessage(owned)))
				}
				continue
			}
			warnings = append(warnings, ownedINIWarning(owned, paths))
		}
	}

	// --- Neutron merged-result ownership ----------------------------------
	if nn := cp.Spec.Services.Neutron; nn != nil {
		blocks := []iniBlock{{cp.Spec.GlobalExtraConfig, globalPath}, {nn.ExtraConfig, neutronPath}}
		merged := MergedExtraConfig(cp.Spec.GlobalExtraConfig, nn.ExtraConfig)
		for _, owned := range config.FindOwnedOverrides(merged, neutronv1alpha1.OwnedConfigKeys) {
			paths := contributingKeyPaths(owned.Section, owned.Key, blocks...)
			// Every Rejected neutron key is always forbidden. The [ovn] Northbound
			// and Southbound connection strings point the ML2/OVN mechanism driver
			// at a logical network model the ControlPlane does not own, and the six
			// TLS file paths beside them (the two client keypairs and the two CA
			// bundles, per database) either name a file the pods do not carry or
			// drop the client identity the OVNCentral issuer signed. [DEFAULT]
			// transport_url, [database] connection and [keystone_authtoken] password
			// arrive through env overrides at runtime, so rendering one is inert and
			// only copies credential material into the config Secret every pod
			// mounts. [DEFAULT] auth_strategy and api_paste_config select the WSGI
			// pipeline, and anything but the keystone one serves the API without
			// token validation, on an endpoint services.neutron.gateway can publish
			// outside the cluster. [securitygroup] enable_security_group is what
			// makes the mechanism driver write the ACLs a port's security groups
			// describe, so disabling it leaves every instance port reachable from
			// every other.
			if owned.Rejected {
				for _, p := range paths {
					errs = append(errs, field.Forbidden(p, rejectedOwnedKeyMessage(owned)))
				}
				continue
			}
			warnings = append(warnings, ownedINIWarning(owned, paths))
		}
	}

	// --- Horizon ownership (flat Django settings) -------------------------
	if hz := cp.Spec.Services.Horizon; hz != nil && len(hz.ExtraConfig) > 0 {
		names := make([]string, 0, len(hz.ExtraConfig))
		for name := range hz.ExtraConfig {
			names = append(names, name)
		}
		for _, owned := range config.FindOwnedSettingOverrides(names, horizonv1alpha1.OwnedConfigKeys) {
			settingPath := horizonPath.Key(owned.Key)
			// Every Rejected Horizon setting (SECRET_KEY and the websso /
			// multi-domain settings) is forbidden UNCONDITIONALLY — stricter than
			// the child webhook, which gates the websso/multiDomain collisions on
			// the typed blocks being present. The ControlPlane projects
			// services.websso / services.multiDomain dynamically from the attached
			// identity backends, so whether a collision materializes is not
			// decidable at admission; admitting the key would plant a runtime
			// HorizonProjectionRejected wedge.
			if owned.Rejected {
				errs = append(errs, field.Forbidden(settingPath, rejectedHorizonMessage(owned)))
				continue
			}
			warnings = append(warnings, ownedSettingWarning(owned, settingPath))
		}
	}

	return warnings, errs
}

// validateINIShape rejects an empty section name or an empty option key in one
// free-form INI block, mirroring the glance child's messages so a rendered
// service config never carries a nameless [<section>] or a bare "= value" line.
//
// It also rejects a newline or carriage return in any section name, key, OR
// value. RenderINIMulti writes each verbatim ("[%s]" for the section, "%s = %s"
// per option), so such a character injects arbitrary additional config lines —
// smuggling a whole [section]/key past the (section, key)-name-keyed ownership
// (FindOwnedOverrides) and catalog (FindUnknownOptions) gates, which inspect map
// structure only and never look inside a value. A value like
// "false\n[federation]\ntrusted_dashboard = https://attacker" would otherwise
// render a real [federation] section the gates never saw.
func validateINIShape(blockPath *field.Path, block map[string]map[string]string) field.ErrorList {
	var errs field.ErrorList
	for section, opts := range block {
		if section == "" {
			errs = append(errs, field.Invalid(blockPath, section,
				"extraConfig section name must not be empty"))
			continue
		}
		if strings.ContainsAny(section, "\n\r") {
			errs = append(errs, field.Invalid(blockPath, section,
				"extraConfig section name must not contain a newline or carriage return"))
			continue
		}
		for key, value := range opts {
			if key == "" {
				errs = append(errs, field.Invalid(blockPath.Key(section), key,
					"extraConfig key must not be empty"))
				continue
			}
			if strings.ContainsAny(key, "\n\r") || strings.ContainsAny(value, "\n\r") {
				errs = append(errs, field.Invalid(blockPath.Key(section).Key(key), key,
					"extraConfig key and value must not contain a newline or carriage return: "+
						"the rendered INI writes them verbatim, so a newline injects arbitrary config lines"))
			}
		}
	}
	return errs
}

// rejectedKeystoneMessage renders the Forbidden detail for the merged Keystone
// [federation] trusted_dashboard override, which the ControlPlane owns via its
// Horizon-derived trusted-dashboards projection.
func rejectedKeystoneMessage(owned config.OwnedKey) string {
	return fmt.Sprintf("[%s] %s is managed by the ControlPlane's Horizon-derived trusted-dashboards "+
		"projection and must not be set in extraConfig (the merged value wins and would silently drop the "+
		"projected dashboard origins)", owned.Section, owned.Key)
}

// rejectedOwnedKeyMessage mirrors a child's own Forbidden message for a Rejected
// owned key, including its Impact. The glance and placement registries both carry
// service-specific prose in OwnedBy/Impact, so the rendering is shared rather
// than duplicated per service (unlike the keystone and horizon messages, whose
// prose is written here).
func rejectedOwnedKeyMessage(owned config.OwnedKey) string {
	msg := fmt.Sprintf("%s is managed via %s and must not be set in extraConfig", owned.Key, owned.OwnedBy)
	if owned.Impact != "" {
		msg += fmt.Sprintf(" (%s)", owned.Impact)
	}
	return msg
}

// rejectedHorizonMessage renders the Forbidden detail for a Rejected Horizon
// setting: SECRET_KEY is managed via services.horizon.secretKeyRef, and the
// websso / multi-domain settings are owned by the identity-backend projection.
func rejectedHorizonMessage(owned config.OwnedKey) string {
	if owned.Key == "SECRET_KEY" {
		return "SECRET_KEY is managed via services.horizon.secretKeyRef and must not be set in extraConfig"
	}
	return fmt.Sprintf("%s is owned by the ControlPlane's identity-backend projection and must not be set in "+
		"extraConfig (services.websso and services.multiDomain are projected dynamically from the attached "+
		"identity backends, so the override cannot be reconciled)", owned.Key)
}

// ownedINIWarning renders the admission warning for a honored-but-owned INI
// override, naming the section/key, the OwnedBy source, the Impact when set, and
// the contributing block path(s).
func ownedINIWarning(owned config.OwnedKey, paths []*field.Path) string {
	msg := fmt.Sprintf("[%s] %s overrides an operator-owned key (managed by %s)",
		owned.Section, owned.Key, owned.OwnedBy)
	if owned.Impact != "" {
		msg += fmt.Sprintf(" (%s)", owned.Impact)
	}
	return msg + fmt.Sprintf("; the override is honored but reported (set at %s)", joinPaths(paths))
}

// ownedSettingWarning renders the admission warning for a honored-but-owned
// flat Horizon setting.
func ownedSettingWarning(owned config.OwnedKey, path *field.Path) string {
	msg := fmt.Sprintf("%s overrides an operator-owned Django setting (managed by %s)", owned.Key, owned.OwnedBy)
	if owned.Impact != "" {
		msg += fmt.Sprintf(" (%s)", owned.Impact)
	}
	return msg + fmt.Sprintf("; the override is honored but reported (set at %s)", path.String())
}

// validateExtraConfigCatalogs validates the MERGED INI config of each declared
// INI service (Keystone unless External, Glance, Placement, Barbican, Neutron)
// against
// the option catalog embedded for the resolved release (Family B). It fails
// open — exactly one warning, no error — when no catalog resolves for a
// non-empty merged config, so a digest pin, an unparseable tag, or a release the
// build ships no catalog for never blocks admission. Every unknown
// option/section becomes a field error attributed to the concrete block(s) that
// carry it; every deprecated-but-accepted option becomes a warning.
func validateExtraConfigCatalogs(cp *ControlPlane) (admission.Warnings, field.ErrorList) {
	var warnings admission.Warnings
	var errs field.ErrorList

	specPath := field.NewPath("spec")
	globalPath := specPath.Child("globalExtraConfig")

	// --- Keystone ---------------------------------------------------------
	if ks := cp.Spec.Services.Keystone; ks != nil && !cp.IsExternalKeystone() {
		merged := MergedExtraConfig(cp.Spec.GlobalExtraConfig, ks.ExtraConfig)
		// Mirror reconcile_keystone.go: the release tag is
		// services.keystone.image.Tag when the image is overridden, else
		// spec.openStackRelease.
		tag := cp.Spec.OpenStackRelease
		releaseField := "spec.openStackRelease"
		if ks.Image != nil {
			tag = ks.Image.Tag
			releaseField = "services.keystone.image"
		}
		catalog, ok := keystonev1alpha1.OptionCatalogForRelease(tag)
		if !ok {
			if w := failOpenCatalogWarning("keystone", releaseField, tag, merged); w != "" {
				warnings = append(warnings, w)
			}
		} else {
			// No plugin sections: the projection never sets spec.plugins on the
			// child. Only the operator-owned keys are exempt.
			ex := config.CatalogExemptions{Keys: config.KeyExemptionsFromRegistry(keystonev1alpha1.OwnedConfigKeys)}
			w, e := attributeCatalogFindings("keystone", catalog, merged, ex,
				iniBlock{cp.Spec.GlobalExtraConfig, globalPath},
				iniBlock{ks.ExtraConfig, specPath.Child("services", "keystone", "extraConfig")})
			warnings = append(warnings, w...)
			errs = append(errs, e...)
		}
	}

	// --- Glance -----------------------------------------------------------
	if gl := cp.Spec.Services.Glance; gl != nil {
		merged := MergedExtraConfig(cp.Spec.GlobalExtraConfig, gl.ExtraConfig)
		catalog, ok := glancev1alpha1.OptionCatalogForRelease(cp.Spec.OpenStackRelease)
		if !ok {
			if w := failOpenCatalogWarning("glance", "spec.openStackRelease", cp.Spec.OpenStackRelease, merged); w != "" {
				warnings = append(warnings, w)
			}
		} else {
			sections := make(map[string]struct{}, len(glancev1alpha1.CatalogExemptSections))
			for _, s := range glancev1alpha1.CatalogExemptSections {
				sections[s] = struct{}{}
			}
			ex := config.CatalogExemptions{
				Sections: sections,
				Keys:     config.KeyExemptionsFromRegistry(glancev1alpha1.OwnedConfigKeys),
			}
			w, e := attributeCatalogFindings("glance", catalog, merged, ex,
				iniBlock{cp.Spec.GlobalExtraConfig, globalPath},
				iniBlock{gl.ExtraConfig, specPath.Child("services", "glance", "extraConfig")})
			warnings = append(warnings, w...)
			errs = append(errs, e...)
		}
	}

	// --- Placement --------------------------------------------------------
	if pl := cp.Spec.Services.Placement; pl != nil {
		merged := MergedExtraConfig(cp.Spec.GlobalExtraConfig, pl.ExtraConfig)
		catalog, ok := placementv1alpha1.OptionCatalogForRelease(cp.Spec.OpenStackRelease)
		if !ok {
			if w := failOpenCatalogWarning("placement", "spec.openStackRelease", cp.Spec.OpenStackRelease, merged); w != "" {
				warnings = append(warnings, w)
			}
		} else {
			// No exempt sections: placement exports none (every section it configures
			// is statically registered, so the catalog enumerates all of them), which
			// is why the exemptions are keys-only, as they are for keystone.
			ex := config.CatalogExemptions{Keys: config.KeyExemptionsFromRegistry(placementv1alpha1.OwnedConfigKeys)}
			w, e := attributeCatalogFindings("placement", catalog, merged, ex,
				iniBlock{cp.Spec.GlobalExtraConfig, globalPath},
				iniBlock{pl.ExtraConfig, specPath.Child("services", "placement", "extraConfig")})
			warnings = append(warnings, w...)
			errs = append(errs, e...)
		}
	}

	// --- Barbican ---------------------------------------------------------
	if bn := cp.Spec.Services.Barbican; bn != nil {
		merged := MergedExtraConfig(cp.Spec.GlobalExtraConfig, bn.ExtraConfig)
		catalog, ok := barbicanv1alpha1.OptionCatalogForRelease(cp.Spec.OpenStackRelease)
		if !ok {
			if w := failOpenCatalogWarning("barbican", "spec.openStackRelease", cp.Spec.OpenStackRelease, merged); w != "" {
				warnings = append(warnings, w)
			}
		} else {
			ex := config.CatalogExemptions{
				Sections: barbicanExemptSections(merged),
				Keys:     config.KeyExemptionsFromRegistry(barbicanv1alpha1.OwnedConfigKeys),
			}
			w, e := attributeCatalogFindings("barbican", catalog, merged, ex,
				iniBlock{cp.Spec.GlobalExtraConfig, globalPath},
				iniBlock{bn.ExtraConfig, specPath.Child("services", "barbican", "extraConfig")})
			warnings = append(warnings, w...)
			errs = append(errs, e...)
		}
	}

	// --- Neutron ----------------------------------------------------------
	if nn := cp.Spec.Services.Neutron; nn != nil {
		merged := MergedExtraConfig(cp.Spec.GlobalExtraConfig, nn.ExtraConfig)
		catalog, ok := neutronv1alpha1.OptionCatalogForRelease(cp.Spec.OpenStackRelease)
		if !ok {
			if w := failOpenCatalogWarning("neutron", "spec.openStackRelease", cp.Spec.OpenStackRelease, merged); w != "" {
				warnings = append(warnings, w)
			}
		} else {
			// No exempt sections: the neutron catalog is the flat union of the
			// neutron.conf, ml2_conf.ini and neutron_ovn_metadata_agent.ini generator
			// files, so it already enumerates every section the child configures,
			// which is why the exemptions are keys-only, as they are for keystone and
			// placement.
			ex := config.CatalogExemptions{Keys: config.KeyExemptionsFromRegistry(neutronv1alpha1.OwnedConfigKeys)}
			w, e := attributeCatalogFindings("neutron", catalog, merged, ex,
				iniBlock{cp.Spec.GlobalExtraConfig, globalPath},
				iniBlock{nn.ExtraConfig, specPath.Child("services", "neutron", "extraConfig")})
			warnings = append(warnings, w...)
			errs = append(errs, e...)
		}
	}

	return warnings, errs
}

// barbicanExemptSections expands barbican's exempt section PREFIXES against the
// section names the merged configuration actually carries, since
// config.CatalogExemptions.Sections matches exact names only. Unlike glance,
// whose exempt sections are a static list, barbican's are the per-store
// [secretstore:<name>] sections named after the BarbicanSecretStore CRs attached
// to a Barbican, so no static list can spell them. It mirrors the barbican
// child's own catalogExemptions helper.
func barbicanExemptSections(merged map[string]map[string]string) map[string]struct{} {
	sections := make(map[string]struct{})
	for section := range merged {
		for _, prefix := range barbicanv1alpha1.CatalogExemptSectionPrefixes {
			if strings.HasPrefix(section, prefix) {
				sections[section] = struct{}{}
			}
		}
	}
	return sections
}

// failOpenCatalogWarning renders the single fail-open warning for a service
// whose merged config could not be validated against a catalog. It distinguishes
// a release input that does not parse as a release (empty tag, digest pin,
// "latest") from a parseable release the build ships no catalog for, mirroring
// the keystone child. It returns "" when the merged config is empty (nothing to
// validate, so no warning).
func failOpenCatalogWarning(svc, releaseField, tag string, merged map[string]map[string]string) string {
	if len(merged) == 0 {
		return ""
	}
	rel, err := release.ParseRelease(tag)
	if err != nil {
		return fmt.Sprintf("the merged %s extraConfig was not validated against an option catalog: "+
			"%s does not name an OpenStack release", svc, releaseField)
	}
	return fmt.Sprintf("the merged %s extraConfig was not validated against an option catalog: "+
		"no catalog for release %q is embedded in this operator build",
		svc, fmt.Sprintf("%d.%d", rel.Year, rel.Minor))
}

// attributeCatalogFindings runs the unknown-option scan ONCE on the merged map,
// then attributes each finding to every block that carries it — anchoring the
// field error/warning at the real leaf. A pair present in both the global and
// the per-service block yields one error per block.
func attributeCatalogFindings(svc string, catalog *config.OptionCatalog, merged map[string]map[string]string, ex config.CatalogExemptions, blocks ...iniBlock) (admission.Warnings, field.ErrorList) {
	unknown, deprecated := catalog.FindUnknownOptions(merged, ex)
	base := catalog.Release

	var errs field.ErrorList
	for _, u := range unknown {
		for _, b := range blocks {
			if _, ok := b.config[u.Section][u.Key]; !ok {
				continue
			}
			fldPath := b.path.Key(u.Section).Key(u.Key)
			if u.SectionUnknown {
				errs = append(errs, field.Invalid(fldPath, u.Key,
					fmt.Sprintf("no such section in the %s %s option catalog "+
						"(plugin-registered sections are not configurable through the ControlPlane)", svc, base)))
				continue
			}
			errs = append(errs, field.Invalid(fldPath, u.Key,
				fmt.Sprintf("no such option in the %s %s option catalog", svc, base)))
		}
	}

	var warnings admission.Warnings
	for _, d := range deprecated {
		for _, b := range blocks {
			if _, ok := b.config[d.Section][d.Key]; !ok {
				continue
			}
			if d.Replacement == "" {
				warnings = append(warnings, fmt.Sprintf(
					"%s [%s] %s: deprecated option in %s %s with no replacement",
					b.path.String(), d.Section, d.Key, svc, base,
				))
				continue
			}
			warnings = append(warnings, fmt.Sprintf(
				"%s [%s] %s: deprecated option in %s %s, replaced by %s",
				b.path.String(), d.Section, d.Key, svc, base, d.Replacement,
			))
		}
	}
	return warnings, errs
}

// controlPlaneExtraConfigCatalogInputsChanged reports whether anything the
// option-catalog check (Family B) depends on differs between oldObj and newObj:
// the global INI block, any per-service INI block (its presence and its
// extraConfig), the release, or the Keystone image override that selects the
// Keystone catalog. ValidateUpdate gates the catalog re-validation on this so a
// CR whose extraConfig went stale-invalid is not rejected by an otherwise-
// unrelated update. It mirrors the keystone child's extraConfigCatalogInputsChanged.
func controlPlaneExtraConfigCatalogInputsChanged(oldObj, newObj *ControlPlane) bool {
	if !reflect.DeepEqual(oldObj.Spec.GlobalExtraConfig, newObj.Spec.GlobalExtraConfig) {
		return true
	}
	if oldObj.Spec.OpenStackRelease != newObj.Spec.OpenStackRelease {
		return true
	}

	oldKs, newKs := oldObj.Spec.Services.Keystone, newObj.Spec.Services.Keystone
	if (oldKs == nil) != (newKs == nil) {
		return true
	}
	if oldKs != nil && newKs != nil {
		if !reflect.DeepEqual(oldKs.ExtraConfig, newKs.ExtraConfig) {
			return true
		}
		if !reflect.DeepEqual(oldKs.Image, newKs.Image) {
			return true
		}
	}

	oldGl, newGl := oldObj.Spec.Services.Glance, newObj.Spec.Services.Glance
	if (oldGl == nil) != (newGl == nil) {
		return true
	}
	if oldGl != nil && newGl != nil {
		if !reflect.DeepEqual(oldGl.ExtraConfig, newGl.ExtraConfig) {
			return true
		}
	}

	oldPl, newPl := oldObj.Spec.Services.Placement, newObj.Spec.Services.Placement
	if (oldPl == nil) != (newPl == nil) {
		return true
	}
	if oldPl != nil && newPl != nil {
		if !reflect.DeepEqual(oldPl.ExtraConfig, newPl.ExtraConfig) {
			return true
		}
	}

	oldBn, newBn := oldObj.Spec.Services.Barbican, newObj.Spec.Services.Barbican
	if (oldBn == nil) != (newBn == nil) {
		return true
	}
	if oldBn != nil && newBn != nil {
		if !reflect.DeepEqual(oldBn.ExtraConfig, newBn.ExtraConfig) {
			return true
		}
	}

	oldNn, newNn := oldObj.Spec.Services.Neutron, newObj.Spec.Services.Neutron
	if (oldNn == nil) != (newNn == nil) {
		return true
	}
	if oldNn != nil && newNn != nil {
		if !reflect.DeepEqual(oldNn.ExtraConfig, newNn.ExtraConfig) {
			return true
		}
	}

	return false
}
