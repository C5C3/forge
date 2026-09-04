#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the c5c3 ControlPlane invalid-CR Chainsaw fixtures.

Single source of truth for the minimal ControlPlane scaffold used by every
``invalid-cr`` rejection (and acceptance-base) test, mirroring
``tests/e2e/keystone/invalid-cr/_generate.py`` and the leaner
``tests/e2e/horizon/invalid-cr/_generate.py``. Each fixture mutates exactly one
aspect of the canonical scaffold so the surrounding CR passes every rule OTHER
than the one under test, making the admission error attributable to that field.

The fixtures deliberately carry NO metadata.namespace: Chainsaw runs each Test in
its own ephemeral namespace, which isolates the one-ControlPlane-per-namespace
webhook from the parallel c5c3 suites pinned to the shared ``openstack``
namespace, and makes the two accepted transition bases (which persist) coexist
without colliding.

Usage:

    # Regenerate all fixtures from this single source of truth.
    python3 _generate.py

    # CI-friendly drift check: exit non-zero if any on-disk fixture diverges
    # from the regenerated content (or an orphan fixture file exists).
    python3 _generate.py --check
"""

from __future__ import annotations

import re
import sys
from dataclasses import dataclass
from pathlib import Path

# Matches every two-digit-prefixed fixture in this directory. Used by the
# orphan-detection sweep in main() so a fixture removed from FIXTURES but left
# on disk is reported as drift (both directions are guarded).
_FIXTURE_FILENAME_PATTERN = re.compile(r"^[0-9]{2}-.+\.yaml$")

LICENSE_HEADER = """\
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""

# Canonical ControlPlane scaffold. Any future required field on ControlPlaneSpec
# must be added below AND verified against every fixture. Placeholders:
#   {name}                metadata.name
#   {global_extra_config} the spec.globalExtraConfig block (indent 2) or ""
#   {infrastructure}      the whole spec.infrastructure block (indent 2) or ""
#   {keystone}            the spec.services.keystone body (indent 6) or "" for nil
#   {horizon}             the spec.services.horizon entry (indent 4) or ""
#   {glance}              the spec.services.glance entry (indent 4) or ""
#   {placement}           the spec.services.placement entry (indent 4) or ""
#   {barbican}            the spec.services.barbican entry (indent 4) or ""
#   {neutron}             the spec.services.neutron entry (indent 4) or ""
#   {service_registrations}
#                         the spec.korc.serviceRegistrations block (indent 4) or ""
#
# korc.adminCredential.applicationCredential is intentionally omitted: the
# defaulting webhook materializes it (rotation.mode etc.) before the CRD's
# required-field check runs, exactly as the minimal managed fixtures rely on.
SCAFFOLD = """\
apiVersion: c5c3.io/v1alpha1
kind: ControlPlane
metadata:
  name: {name}
spec:
  openStackRelease: "2025.2"
{global_extra_config}{infrastructure}  services:
    keystone:
{keystone}{horizon}{glance}{placement}{barbican}{neutron}  korc:
    adminCredential:
      cloudCredentialsRef:
        cloudName: admin
      passwordSecretRef:
        name: external-admin
        key: password
{service_registrations}"""

# A valid External keystone service body (indent 6): the issue's sketch shape.
VALID_EXTERNAL_KEYSTONE = (
    "      mode: External\n"
    "      external:\n"
    "        authURL: https://keystone.example.com/v3\n"
)

# A valid brownfield infrastructure block (indent 2, trailing newline). Used by
# the Managed-mode fixtures and the transition bases so infrastructure is present
# where the mode requires it.
MANAGED_INFRA = (
    "  infrastructure:\n"
    "    database:\n"
    "      host: db.example.com\n"
    "      database: openstack\n"
    "      secretRef:\n"
    "        name: db-creds\n"
    "    cache:\n"
    "      backend: dogpile.cache.pymemcache\n"
    "      servers:\n"
    "      - mc:11211\n"
)

# MANAGED_INFRA plus a brownfield messaging block (indent 2, trailing newline).
# The transition-wave-G base declares the bus brownfield so the persisting base
# provisions nothing while still carrying a declared block to freeze.
MANAGED_INFRA_WITH_BROWNFIELD_MESSAGING = MANAGED_INFRA + (
    "    messaging:\n"
    "      secretRef:\n"
    "        name: bus-url\n"
)


# A valid glance service body (indent 4): one S3 backend promoted to the default
# store. The three glance-block fixtures below mutate exactly one aspect of it,
# and the External-mode forbid fixture reuses it whole.
VALID_GLANCE = (
    "    glance:\n"
    "      backends:\n"
    "      - name: primary\n"
    "        type: S3\n"
    "        isDefault: true\n"
    "        s3:\n"
    "          endpoint: https://s3.example.com\n"
    "          bucket: images\n"
    "          credentialsSecretRef:\n"
    "            name: glance-s3-creds\n"
)


# A valid barbican service body (indent 4): the minimal shape, a dedicated
# secret store. secretStore is the one REQUIRED field of the block, and the
# dedicated mode carries no fields of its own. The barbican fixtures that mutate
# the store spell their block out instead of appending to this one.
VALID_BARBICAN = (
    "    barbican:\n"
    "      secretStore:\n"
    "        dedicated: {}\n"
)


# A valid neutron service body (indent 4): the minimal shape, a reference to the
# OVNCentral the projected child programs. ovn.centralRef.name is the one
# REQUIRED field of the block, because the ML2/OVN mechanism driver has no
# logical network model to write to without a central; everything else the child
# needs (its database, its cache, its bus, its Keystone endpoint) is derived from
# the ControlPlane. The neutron fixture that mutates the reference spells its
# block out instead of appending to this one.
VALID_NEUTRON = (
    "    neutron:\n"
    "      ovn:\n"
    "        centralRef:\n"
    "          name: ovn\n"
)


# A valid, MANAGED dedicated backing-services block for the Keystone service
# (indent 6, to be appended to a Managed keystone body). Every dedicated fixture
# below mutates exactly one aspect of it.
VALID_DEDICATED_KEYSTONE = (
    "      dedicatedBackingServices:\n"
    "        database:\n"
    "          clusterRef:\n"
    "            name: cp-dedicated-db\n"
    "          database: keystone\n"
    "          secretRef:\n"
    "            name: keystone-db\n"
    "        cache:\n"
    "          clusterRef:\n"
    "            name: cp-dedicated-cache\n"
    "          backend: dogpile.cache.pymemcache\n"
)


@dataclass(frozen=True)
class Fixture:
    """One generated fixture (a rejection case or a transition base)."""

    filename: str
    comment: str
    name: str
    keystone: str = VALID_EXTERNAL_KEYSTONE
    infrastructure: str = ""
    horizon: str = ""
    glance: str = ""
    placement: str = ""
    barbican: str = ""
    neutron: str = ""
    # The spec.globalExtraConfig block (indent 2, trailing newline) or "".
    global_extra_config: str = ""
    # The spec.korc.serviceRegistrations block (indent 4, trailing newline) or "".
    service_registrations: str = ""

    def render(self) -> str:
        body = SCAFFOLD.format(
            name=self.name,
            global_extra_config=self.global_extra_config,
            infrastructure=self.infrastructure,
            keystone=self.keystone,
            horizon=self.horizon,
            glance=self.glance,
            placement=self.placement,
            barbican=self.barbican,
            neutron=self.neutron,
            service_registrations=self.service_registrations,
        )
        comment_lines = "".join(f"# {line}\n" for line in self.comment.splitlines())
        return LICENSE_HEADER + comment_lines + body


FIXTURES: tuple[Fixture, ...] = (
    # --- create-rejection matrix (Test: c5c3-invalid-cr) ---
    Fixture(
        filename="19-federation-proxy-image-in-external.yaml",
        comment=(
            "services.keystone.federationProxyImage in External mode violates the CEL rule:\n"
            "no Keystone workload is deployed, so there is no sidecar to image."
        ),
        name="cp-external-proxy-image",
        keystone=(
            "      mode: External\n"
            "      external:\n"
            "        authURL: https://keystone.example.com/v3\n"
            "      federationProxyImage:\n"
            "        repository: ghcr.io/c5c3/keystone-federation-proxy\n"
            "        tag: dev\n"
        ),
    ),
    Fixture(
        filename="20-horizon-public-endpoint-not-a-url.yaml",
        comment=(
            "services.horizon.publicEndpoint with a non-http(s) scheme violates the CRD\n"
            "pattern. Keystone matches the derived WebSSO origin verbatim, so an\n"
            "unusable endpoint could never match any dashboard."
        ),
        name="cp-horizon-bad-endpoint",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        horizon=(
            "    horizon:\n"
            "      publicEndpoint: ftp://horizon.example.com\n"
        ),
    ),
    Fixture(
        filename="21-horizon-gateway-hostname-wildcard.yaml",
        comment=(
            "services.horizon.gateway.hostname must be a concrete DNS name. Gateway API\n"
            "permits a wildcard here, but the reconciler derives the WebSSO origin from it\n"
            "and Keystone compares that origin verbatim, so a wildcard would match no\n"
            "dashboard and silently break every federated login."
        ),
        name="cp-horizon-wildcard-gateway",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        horizon=(
            "    horizon:\n"
            "      gateway:\n"
            "        hostname: '*.example.com'\n"
            "        parentRef:\n"
            "          name: openstack-gw\n"
        ),
    ),
    Fixture(
        filename="22-horizon-public-endpoint-host-mismatch.yaml",
        comment=(
            "services.horizon.publicEndpoint must name the same host as\n"
            "services.horizon.gateway.hostname. Django derives the WebSSO origin it sends\n"
            "from the request Host header — i.e. from the gateway hostname — and Keystone\n"
            "compares it verbatim, so a divergent host is rejected only AFTER the user has\n"
            "already entered their corporate credentials at the identity provider."
        ),
        name="cp-horizon-endpoint-host-mismatch",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        horizon=(
            "    horizon:\n"
            "      gateway:\n"
            "        hostname: horizon.example.com\n"
            "        parentRef:\n"
            "          name: openstack-gw\n"
            "      publicEndpoint: https://dashboard.example.com\n"
        ),
    ),
    Fixture(
        filename="23-horizon-public-endpoint-with-query.yaml",
        comment=(
            "services.horizon.publicEndpoint must be a bare origin. The ^https?:// CRD\n"
            "pattern anchors only the prefix, so a query string is schema-legal — and the\n"
            "derived origin https://horizon.example.com?utm=1/auth/websso/ is accepted by\n"
            "Keystone's own trusted_dashboard validation and then matches nothing, failing\n"
            "every federated login after the user has authenticated at the identity\n"
            "provider. Only the webhook rejects it."
        ),
        name="cp-horizon-endpoint-with-query",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        horizon=(
            "    horizon:\n"
            "      publicEndpoint: https://horizon.example.com?utm=1\n"
        ),
    ),
    Fixture(
        filename="00-external-in-managed-explicit.yaml",
        comment="services.keystone.external set with explicit mode: Managed violates the CEL rule.",
        name="cp-external-in-managed",
        keystone=(
            "      mode: Managed\n"
            "      external:\n"
            "        authURL: https://keystone.example.com/v3\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="01-external-in-managed-unset.yaml",
        comment=(
            "services.keystone.external set with mode unset (defaults Managed)\n"
            "violates the CEL rule after the mode default is applied."
        ),
        name="cp-external-unset-mode",
        keystone=(
            "      external:\n"
            "        authURL: https://keystone.example.com/v3\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="02-external-mode-without-external.yaml",
        comment="mode: External without the external block violates the CEL rule.",
        name="cp-external-no-block",
        keystone="      mode: External\n",
    ),
    Fixture(
        filename="03-external-with-infrastructure.yaml",
        comment="spec.infrastructure set in External mode is forbidden by the webhook (cross-field).",
        name="cp-external-with-infra",
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="04-external-with-horizon.yaml",
        comment="services.horizon set in External mode is forbidden by the webhook (P2, cross-field).",
        name="cp-external-with-horizon",
        horizon="    horizon: {}\n",
    ),
    Fixture(
        filename="05-external-replicas.yaml",
        comment="services.keystone.replicas is forbidden in External mode (CEL).",
        name="cp-external-replicas",
        keystone=VALID_EXTERNAL_KEYSTONE + "      replicas: 3\n",
    ),
    Fixture(
        filename="06-external-image.yaml",
        comment="services.keystone.image is forbidden in External mode (CEL).",
        name="cp-external-image",
        keystone=(
            VALID_EXTERNAL_KEYSTONE
            + "      image:\n"
            + "        repository: ghcr.io/c5c3/keystone\n"
            + '        tag: "2025.2"\n'
        ),
    ),
    Fixture(
        filename="07-external-policy-overrides.yaml",
        comment="services.keystone.policyOverrides is forbidden in External mode (CEL).",
        name="cp-external-policy",
        keystone=(
            VALID_EXTERNAL_KEYSTONE
            + "      policyOverrides:\n"
            + "        rules:\n"
            + '          "identity:get_user": "role:admin"\n'
        ),
    ),
    Fixture(
        filename="08-external-rotation-interval.yaml",
        comment="services.keystone.rotationInterval is forbidden in External mode (CEL).",
        name="cp-external-rotation",
        keystone=VALID_EXTERNAL_KEYSTONE + "      rotationInterval: 24h\n",
    ),
    Fixture(
        filename="09-external-gateway.yaml",
        comment="services.keystone.gateway is forbidden in External mode (CEL).",
        name="cp-external-gateway",
        keystone=(
            VALID_EXTERNAL_KEYSTONE
            + "      gateway:\n"
            + "        parentRef:\n"
            + "          name: openstack-gw\n"
            + "        hostname: keystone.example.com\n"
        ),
    ),
    Fixture(
        filename="10-external-public-endpoint.yaml",
        comment="services.keystone.publicEndpoint is forbidden in External mode (CEL, P2).",
        name="cp-external-public",
        keystone=VALID_EXTERNAL_KEYSTONE + "      publicEndpoint: https://keystone.example.com/v3\n",
    ),
    Fixture(
        filename="11-external-authurl-missing.yaml",
        comment="external without authURL violates the CRD required-field check.",
        name="cp-external-no-authurl",
        keystone=(
            "      mode: External\n"
            "      external: {}\n"
        ),
    ),
    Fixture(
        filename="12-external-authurl-not-url.yaml",
        comment="external.authURL without an http(s) scheme violates the CRD pattern.",
        name="cp-external-bad-authurl",
        keystone=(
            "      mode: External\n"
            "      external:\n"
            "        authURL: keystone.example.com\n"
        ),
    ),
    Fixture(
        filename="13-external-endpoint-type-invalid.yaml",
        comment="external.endpointType outside the enum violates the CRD enum.",
        name="cp-external-bad-endpoint",
        keystone=(
            "      mode: External\n"
            "      external:\n"
            "        authURL: https://keystone.example.com/v3\n"
            "        endpointType: gopher\n"
        ),
    ),
    Fixture(
        filename="14-external-cabundle-empty-name.yaml",
        comment="external.caBundleSecretRef.name empty violates the SecretRefSpec MinLength marker.",
        name="cp-external-empty-ca",
        keystone=(
            "      mode: External\n"
            "      external:\n"
            "        authURL: https://keystone.example.com/v3\n"
            "        caBundleSecretRef:\n"
            '          name: ""\n'
        ),
    ),
    # Catalog stewardship (still the create-rejection matrix). The numbering picks
    # up at 19 because 15-18 were already taken by the transition waves below.
    Fixture(
        filename="29-external-catalog-identity-service-name-invalid.yaml",
        comment=(
            "external.catalog.identityServiceName carrying a comma is rejected (CRD\n"
            "pattern): it is cast to K-ORC's OpenStackName on the Service import\n"
            "filter, whose own pattern is ^[^,]+$."
        ),
        name="cp-external-catalog-bad-identity-name",
        keystone=(
            VALID_EXTERNAL_KEYSTONE
            + "        catalog:\n"
            + "          identityServiceName: keystone,v3\n"
        ),
    ),
    # Per-service dedicated backing services (still the create-rejection matrix).
    Fixture(
        filename="35-dedicated-in-external.yaml",
        comment=(
            "services.keystone.dedicatedBackingServices in External mode violates the CEL\n"
            "rule: an External ControlPlane provisions no backing services at all, so there\n"
            "is nothing to make dedicated."
        ),
        name="cp-dedicated-external",
        keystone=(
            VALID_EXTERNAL_KEYSTONE
            + "      dedicatedBackingServices:\n"
            + "        cache:\n"
            + "          clusterRef:\n"
            + "            name: cp-dedicated-cache\n"
            + "          backend: dogpile.cache.pymemcache\n"
        ),
    ),
    Fixture(
        filename="36-dedicated-empty-block.yaml",
        comment=(
            "An empty dedicatedBackingServices block violates the CEL rule: it requests no\n"
            "backing-service class at all. Omit the block entirely to share the\n"
            "ControlPlane-wide instances (the default)."
        ),
        name="cp-dedicated-empty",
        keystone=(
            "      mode: Managed\n"
            "      dedicatedBackingServices: {}\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="37-dedicated-database-dynamic-credentials.yaml",
        comment=(
            "credentialsMode Dynamic on a DEDICATED database is rejected by the webhook: the\n"
            "OpenBao database engine carries one connection and one role per namespace,\n"
            "bootstrapped against the SHARED cluster, so no engine role can issue credentials\n"
            "for a dedicated instance — an admitted CR would wedge on an ExternalSecret that\n"
            "can never sync. The CRD CEL rule does not catch it (clusterRef IS set, so the\n"
            "Dynamic-requires-managed-mode rule passes)."
        ),
        name="cp-dedicated-dynamic",
        keystone=(
            "      mode: Managed\n"
            "      dedicatedBackingServices:\n"
            "        database:\n"
            "          clusterRef:\n"
            "            name: cp-dedicated-db\n"
            "          credentialsMode: Dynamic\n"
            "          database: keystone\n"
            "          secretRef:\n"
            "            name: keystone-db\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="38-dedicated-database-replicas-two.yaml",
        comment=(
            "A two-replica DEDICATED database is rejected by the webhook, exactly as a\n"
            "two-replica shared one is: the managed-MariaDB projection turns any replicas>1\n"
            "into a Galera cluster, and a two-node Galera cluster cannot hold a majority. The\n"
            "CRD marker only enforces Minimum=1, so the webhook is the enforcement point."
        ),
        name="cp-dedicated-replicas-two",
        keystone=(
            "      mode: Managed\n"
            "      dedicatedBackingServices:\n"
            "        database:\n"
            "          clusterRef:\n"
            "            name: cp-dedicated-db\n"
            "          database: keystone\n"
            "          secretRef:\n"
            "            name: keystone-db\n"
            "          replicas: 2\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="39-dedicated-clusterref-collision.yaml",
        comment=(
            "Two services' dedicated caches naming the same managed clusterRef are rejected\n"
            "by the webhook: both would resolve to a single Memcached child CR that the two\n"
            "projections then fight over, silently voiding the very isolation the opt-in\n"
            "exists for."
        ),
        name="cp-dedicated-collision",
        keystone="      mode: Managed\n" + VALID_DEDICATED_KEYSTONE,
        infrastructure=MANAGED_INFRA,
        horizon=(
            "    horizon:\n"
            "      dedicatedBackingServices:\n"
            "        cache:\n"
            "          clusterRef:\n"
            "            name: cp-dedicated-cache\n"
            "          backend: dogpile.cache.pymemcache\n"
        ),
    ),
    # --- per-service Glance (issue #672, still the create-rejection matrix) ---
    Fixture(
        filename="47-external-with-glance.yaml",
        comment=(
            "services.glance set in External mode is forbidden by the webhook (cross-field,\n"
            "mirrors services.horizon): Glance needs its own External-mode design. The glance\n"
            "block itself is valid, so the ONLY violation is the cross-field forbid."
        ),
        name="cp-external-with-glance",
        glance=VALID_GLANCE,
    ),
    Fixture(
        filename="48-glance-backends-empty.yaml",
        comment=(
            "services.glance.backends with no entry violates the CRD MinItems floor: an empty\n"
            "list projects no GlanceBackend, so Glance has no image store at all."
        ),
        name="cp-glance-backends-empty",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=(
            "    glance:\n"
            "      backends: []\n"
        ),
    ),
    Fixture(
        filename="49-glance-two-defaults.yaml",
        comment=(
            "Two backends both setting isDefault violate the single-default CEL rule: the\n"
            "Glance default_backend would be ambiguous. The webhook mirrors the rule."
        ),
        name="cp-glance-two-defaults",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=(
            "    glance:\n"
            "      backends:\n"
            "      - name: primary\n"
            "        type: S3\n"
            "        isDefault: true\n"
            "        s3:\n"
            "          endpoint: https://s3.example.com\n"
            "          bucket: images\n"
            "          credentialsSecretRef:\n"
            "            name: glance-s3-creds\n"
            "      - name: secondary\n"
            "        type: S3\n"
            "        isDefault: true\n"
            "        s3:\n"
            "          endpoint: https://s3-2.example.com\n"
            "          bucket: images2\n"
            "          credentialsSecretRef:\n"
            "            name: glance-s3-creds-2\n"
        ),
    ),
    Fixture(
        filename="50-glance-s3-block-missing.yaml",
        comment=(
            "A backend of type S3 without the s3 block violates the type/s3 union CEL rule:\n"
            "the driver parameters live in that block. The webhook mirrors the rule."
        ),
        name="cp-glance-s3-missing",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=(
            "    glance:\n"
            "      backends:\n"
            "      - name: primary\n"
            "        type: S3\n"
            "        isDefault: true\n"
        ),
    ),
    Fixture(
        filename="51-glance-public-endpoint-not-a-url.yaml",
        comment=(
            "services.glance.publicEndpoint with a non-http(s) scheme violates the CRD\n"
            "pattern. The value is advertised verbatim as the public image catalog\n"
            "Endpoint, so an unusable scheme could never serve external clients."
        ),
        name="cp-glance-bad-endpoint",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=VALID_GLANCE + "      publicEndpoint: ftp://glance.example.com\n",
    ),
    Fixture(
        filename="52-glance-public-endpoint-host-mismatch.yaml",
        comment=(
            "services.glance.publicEndpoint must name the same host as\n"
            "services.glance.gateway.hostname. The Gateway listener is what routes that\n"
            "hostname to the Glance API, so a divergent host advertises a catalog endpoint\n"
            "that never reaches it — and the value is projected into no child CR, so this\n"
            "webhook is the only gate on the URL every image client resolves."
        ),
        name="cp-glance-endpoint-host-mismatch",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=(
            VALID_GLANCE
            + "      gateway:\n"
            + "        hostname: glance.example.com\n"
            + "        parentRef:\n"
            + "          name: openstack-gw\n"
            + "      publicEndpoint: https://images.example.com\n"
        ),
    ),
    Fixture(
        filename="53-glance-public-endpoint-with-query.yaml",
        comment=(
            "services.glance.publicEndpoint must be a bare origin. The ^https?:// pattern\n"
            "anchors only the prefix, so a query string is schema-legal — and since the\n"
            "Glance API is served at the root, clients append the API path to the catalog\n"
            "endpoint and 404 on every image call."
        ),
        name="cp-glance-endpoint-with-query",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=VALID_GLANCE + "      publicEndpoint: https://glance.example.com?utm=1\n",
    ),
    # --- per-service databaseCredentialsMode override (issue #683, still the
    #     create-rejection matrix) ---
    Fixture(
        filename="54-keystone-credentials-mode-override-in-external.yaml",
        comment=(
            "services.keystone.databaseCredentialsMode in External mode violates the CEL\n"
            "rule: no managed database is provisioned, so there is no credentials mode to\n"
            "override. A Static value is otherwise valid, so the ONLY violation is the\n"
            "External-mode forbid."
        ),
        name="cp-external-credentials-mode",
        keystone=VALID_EXTERNAL_KEYSTONE + "      databaseCredentialsMode: Static\n",
    ),
    Fixture(
        filename="55-keystone-override-dynamic-on-dedicated.yaml",
        comment=(
            "databaseCredentialsMode Dynamic on the Keystone service is rejected by the\n"
            "webhook when Keystone declares a DEDICATED database: the override retargets the\n"
            "shared database this service does not use, and a dedicated database is\n"
            "Static-only. The CRD Enum admits the value (Static|Dynamic) and no CEL rule\n"
            "spans the dedicated block, so the webhook is the enforcement point."
        ),
        name="cp-override-dynamic-dedicated-ks",
        keystone=(
            "      mode: Managed\n"
            "      databaseCredentialsMode: Dynamic\n"
            "      dedicatedBackingServices:\n"
            "        database:\n"
            "          clusterRef:\n"
            "            name: cp-dedicated-db\n"
            "          database: keystone\n"
            "          secretRef:\n"
            "            name: keystone-db\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="56-glance-override-dynamic-on-dedicated.yaml",
        comment=(
            "databaseCredentialsMode Dynamic on the Glance service is rejected by the webhook\n"
            "when Glance declares a DEDICATED database, mirroring the Keystone case: the\n"
            "override retargets the shared database this service does not use, and a\n"
            "dedicated database is Static-only. The defaulting webhook injects the glance\n"
            "service account, so the only violation is the credentials-mode override."
        ),
        name="cp-override-dynamic-dedicated-gl",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=(
            "    glance:\n"
            "      databaseCredentialsMode: Dynamic\n"
            "      backends:\n"
            "      - name: primary\n"
            "        type: S3\n"
            "        isDefault: true\n"
            "        s3:\n"
            "          endpoint: https://s3.example.com\n"
            "          bucket: images\n"
            "          credentialsSecretRef:\n"
            "            name: glance-s3-creds\n"
            "      dedicatedBackingServices:\n"
            "        database:\n"
            "          clusterRef:\n"
            "            name: cp-dedicated-db\n"
            "          database: glance\n"
            "          secretRef:\n"
            "            name: glance-db\n"
        ),
    ),
    Fixture(
        filename="57-override-dynamic-on-brownfield.yaml",
        comment=(
            "databaseCredentialsMode Dynamic on a service using the SHARED database is\n"
            "rejected by the webhook when that shared database is brownfield (host set, no\n"
            "clusterRef), mirroring the commonv1 Dynamic-requires-clusterRef contract one\n"
            "level up: the dynamic engine issues per-tenant DB users only against a cluster\n"
            "the operator provisions. The CRD Enum admits the value and the keystone CEL rule\n"
            "forbids the field only in External mode, so the webhook is the enforcement point."
        ),
        name="cp-override-dynamic-brownfield",
        keystone=(
            "      mode: Managed\n"
            "      databaseCredentialsMode: Dynamic\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    # --- transition wave C: shared -> dedicated (Test: c5c3-invalid-cr-shared-to-dedicated) ---
    Fixture(
        filename="40-transition-base-shared.yaml",
        comment=(
            "Accepted base for the shared->dedicated transition test: a Managed ControlPlane\n"
            "whose Keystone service shares the ControlPlane-wide backing services (the\n"
            "default — no dedicatedBackingServices block)."
        ),
        name="cp-transition-c",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="41-transition-to-dedicated.yaml",
        comment=(
            "UPDATE of the accepted shared base onto dedicated backing services is rejected:\n"
            "the flip would re-point the consuming child's (immutable) database fields at a\n"
            "different instance while the previously-provisioned one keeps running with the\n"
            "data still on it. The freeze is webhook-only — no CEL transition rule — so a\n"
            "later transition feature can relax it to a gated migration."
        ),
        name="cp-transition-c",
        keystone="      mode: Managed\n" + VALID_DEDICATED_KEYSTONE,
        infrastructure=MANAGED_INFRA,
    ),
    # --- per-service namespaces (issue #646) ---
    Fixture(
        filename="42-namespace-in-external-mode.yaml",
        comment=(
            "services.keystone.namespace in External mode violates the CEL rule: no Keystone\n"
            "workload is deployed, so there is nothing to place in a namespace of its own."
        ),
        name="cp-external-namespace",
        keystone=(
            "      mode: External\n"
            "      external:\n"
            "        authURL: https://keystone.example.com/v3\n"
            "      namespace:\n"
            "        name: identity\n"
        ),
    ),
    Fixture(
        filename="43-namespace-lifecycle-conflict.yaml",
        comment=(
            "Two services co-located in ONE namespace must agree on its lifecycle. They share\n"
            "that namespace's backing services and its tenant store, so they cannot disagree\n"
            "on whether the operator owns it: the Managed declaration would have the teardown\n"
            "delete the namespace the External one declared untouchable."
        ),
        name="cp-ns-lifecycle-conflict",
        keystone=(
            "      mode: Managed\n"
            "      namespace:\n"
            "        name: shared-services\n"
            "        lifecycle: Managed\n"
        ),
        horizon=(
            "    horizon:\n"
            "      namespace:\n"
            "        name: shared-services\n"
            "        lifecycle: External\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    # --- transition wave D: namespace assignment freeze (Test: c5c3-invalid-cr-namespace-freeze) ---
    Fixture(
        filename="44-transition-base-namespaced.yaml",
        comment=(
            "Accepted base for the namespace-assignment freeze test: a Managed ControlPlane\n"
            "whose Keystone service is placed in a pre-existing namespace it does not own.\n"
            "The External lifecycle is deliberate — the operator never creates that\n"
            "namespace, so the CR parks on NamespacesReady=False/NamespaceNotFound and\n"
            "provisions nothing, leaving no side effects for the rejection step to clean up."
        ),
        name="cp-transition-d",
        keystone=(
            "      mode: Managed\n"
            "      namespace:\n"
            "        name: invalid-cr-preexisting\n"
            "        lifecycle: External\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="45-transition-remove-namespace.yaml",
        comment=(
            "UPDATE dropping the namespace assignment from the accepted base is rejected: the\n"
            "assignment is create-only. Moving a live service across namespaces would leave\n"
            "its backing services, its secret store, and every OpenBao path scoped to the old\n"
            "namespace behind with no migration path. namespace is explicitly nulled, not\n"
            "merely omitted: Chainsaw applies an UPDATE as an RFC 7386 JSON merge patch, so\n"
            "an omitted block would simply be retained and the update would be admitted."
        ),
        name="cp-transition-d",
        keystone=(
            "      mode: Managed\n"
            "      namespace: null\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    # --- transition wave A: Managed -> External (Test: c5c3-invalid-cr-managed-to-external) ---
    Fixture(
        filename="15-transition-base-managed.yaml",
        comment=(
            "Accepted base for the Managed->External transition test: a brownfield,\n"
            "keystone-unset (staged-adoption) ControlPlane — not External, so\n"
            "infrastructure is required and present."
        ),
        name="cp-transition-a",
        keystone="",
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="16-transition-to-external.yaml",
        comment=(
            "UPDATE of the accepted base to a valid External shape is rejected outright:\n"
            "adopting an existing installation must be a fresh External-mode ControlPlane."
        ),
        name="cp-transition-a",
    ),
    # --- transition wave B: External -> Managed (Test: c5c3-invalid-cr-external-to-managed) ---
    Fixture(
        filename="17-transition-base-external.yaml",
        comment=(
            "Accepted base for the External->Managed transition test: the issue's minimal\n"
            "External sketch CR (mode + external.authURL + passwordSecretRef, no\n"
            "infrastructure). Doubles as the sketch-CR acceptance proof."
        ),
        name="cp-transition-b",
    ),
    Fixture(
        filename="18-transition-to-managed.yaml",
        comment=(
            "UPDATE of the accepted External base to a Managed shape is rejected with the\n"
            "reserved phase-3 takeover message. external is explicitly nulled, not merely\n"
            "omitted: Chainsaw applies an UPDATE as an RFC 7386 JSON merge patch, so an\n"
            "omitted external would be RETAINED from the External base and trip the\n"
            "intra-struct CEL rule (external forbidden in Managed mode) at CRD validation,\n"
            "before the validating webhook's transition gate ever runs. Nulling external\n"
            "removes the block, yielding the clean Managed shape whose only remaining\n"
            "violation is the External->Managed transition the webhook rejects with phase-3."
        ),
        name="cp-transition-b",
        keystone=(
            "      mode: Managed\n"
            "      external: null\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    # --- freeform extraConfig admission (issue #704, create-rejection matrix) ---
    Fixture(
        filename="58-global-extraconfig-unknown-option.yaml",
        comment=(
            "spec.globalExtraConfig sets an unknown option (providr, a typo of the\n"
            "keystone [token] provider option) in a known section: rejected by the webhook\n"
            "because it is absent from the keystone 2025.2 option catalog. The finding is\n"
            "attributed back to the global block that carried it, so the message names\n"
            "spec.globalExtraConfig[token][providr]."
        ),
        name="cp-global-extraconfig-unknown",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        global_extra_config=(
            "  globalExtraConfig:\n"
            "    token:\n"
            "      providr: fernet\n"
        ),
    ),
    Fixture(
        filename="59-glance-extraconfig-unknown-option.yaml",
        comment=(
            "services.glance.extraConfig sets an unknown option (default_backend_typo) in\n"
            "the known [glance_store] section: rejected by the webhook because it is absent\n"
            "from the glance 2025.2 option catalog. The rest of the glance block is valid, so\n"
            "the ONLY violation is the per-service catalog check."
        ),
        name="cp-glance-extraconfig-unknown",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=(
            VALID_GLANCE
            + "      extraConfig:\n"
            + "        glance_store:\n"
            + "          default_backend_typo: rbd\n"
        ),
    ),
    Fixture(
        filename="60-keystone-extraconfig-in-external.yaml",
        comment=(
            "services.keystone.extraConfig in External mode violates the CEL rule: no\n"
            "Keystone workload is deployed, so there is no child config the merged block\n"
            "would project. The value is otherwise a shape-valid INI block, so the ONLY\n"
            "violation is the External-mode forbid."
        ),
        name="cp-external-extraconfig",
        keystone=(
            VALID_EXTERNAL_KEYSTONE
            + "      extraConfig:\n"
            + "        DEFAULT:\n"
            + '          debug: "true"\n'
        ),
    ),
    Fixture(
        filename="61-horizon-extraconfig-secret-key.yaml",
        comment=(
            "SECRET_KEY in services.horizon.extraConfig is forbidden by the webhook\n"
            "(Family A, unconditional): it is managed via services.horizon.secretKeyRef, and\n"
            "an override in extraConfig would collide with the operator-projected Django\n"
            "session key."
        ),
        name="cp-horizon-extraconfig-secret-key",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        horizon=(
            "    horizon:\n"
            "      extraConfig:\n"
            "        SECRET_KEY: hunter2\n"
        ),
    ),
    Fixture(
        filename="62-glance-importfiltering-allow-and-deny-hosts.yaml",
        comment=(
            "services.glance.importFiltering sets allowedHosts AND disallowedHosts, which\n"
            "violates the CEL mutual-exclusivity rule: glance evaluates the deny-list only\n"
            "while the allow-list is empty, so the deny-list would be silently dropped. The\n"
            "rest of the glance block is valid, so the ONLY violation is the host pair."
        ),
        name="cp-glance-importfiltering-hosts",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=(
            VALID_GLANCE
            + "      importFiltering:\n"
            + "        allowedHosts:\n"
            + "        - mirror.example.com\n"
            + "        disallowedHosts:\n"
            + "        - 169.254.169.254\n"
        ),
    ),
    Fixture(
        filename="63-glance-staging-sizelimit-zero.yaml",
        comment=(
            "services.glance.staging.sizeLimit of 0 is rejected by the validating webhook,\n"
            "which reuses the glance module's exported validator: a resource.Quantity\n"
            "renders as x-kubernetes-int-or-string in the CRD schema, which carries no\n"
            "Minimum marker, so admission is the sole gate against an unusable\n"
            "scratch-volume bound — a 1Mi floor, since the schema pattern also admits\n"
            "the sub-byte milli suffix (`100m` for `100Mi`). The rest of the glance\n"
            "block is valid, so the ONLY violation is the size limit."
        ),
        name="cp-glance-staging-sizelimit",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=(
            VALID_GLANCE
            + "      staging:\n"
            + "        sizeLimit: 0\n"
        ),
    ),
    Fixture(
        filename="64-glance-imagecache-sizelimit-zero.yaml",
        comment=(
            "services.glance.imageCache.sizeLimit of 0 is rejected by the validating\n"
            "webhook, which reuses the glance module's exported validator: a\n"
            "resource.Quantity renders as x-kubernetes-int-or-string in the CRD schema,\n"
            "which carries no Minimum marker, so admission is the sole gate against an\n"
            "unusable cache bound — a 1Mi floor, since the schema pattern also admits\n"
            "the sub-byte milli suffix (`100m` for `100Mi`). The rest of the glance\n"
            "block is valid, so the ONLY violation is the size limit."
        ),
        name="cp-glance-imagecache-sizelimit",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=(
            VALID_GLANCE
            + "      imageCache:\n"
            + "        sizeLimit: 0\n"
        ),
    ),
    Fixture(
        filename="65-glance-importplugins-inject-key-colon.yaml",
        comment=(
            "services.glance.importPlugins.injectMetadata.properties carries a property\n"
            "name with a colon, rejected by the validating webhook through the glance\n"
            "module's exported ValidateImportPlugins. A map key has no CRD schema\n"
            "counterpart, so admission is the sole gate on this one: the rendered\n"
            "[inject_metadata_properties] inject value is an oslo Dict whose parser\n"
            "splits each pair on the first colon, so the name would be truncated there\n"
            "and its remainder injected as part of the value. The rest of the glance\n"
            "block is valid, so the ONLY violation is the property name."
        ),
        name="cp-glance-importplugins-inject-colon",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=(
            VALID_GLANCE
            + "      importPlugins:\n"
            + "        injectMetadata:\n"
            + "          properties:\n"
            + '            "hw:disk_bus": virtio\n'
        ),
    ),
    # --- per-service Placement (still the create-rejection matrix) ---
    Fixture(
        filename="66-external-with-placement.yaml",
        comment=(
            "services.placement set in External mode is forbidden by the webhook (cross-field,\n"
            "mirrors services.glance): Placement needs its own External-mode design. Every\n"
            "ServicePlacementSpec field is optional, so the empty block is valid and the ONLY\n"
            "violation is the cross-field forbid."
        ),
        name="cp-external-with-placement",
        placement="    placement: {}\n",
    ),
    Fixture(
        filename="67-placement-public-endpoint-not-a-url.yaml",
        comment=(
            "services.placement.publicEndpoint with a non-http(s) scheme violates the CRD\n"
            "pattern. The value is advertised verbatim as the public placement catalog\n"
            "Endpoint, so an unusable scheme could never serve the compute services that\n"
            "place their allocations through it."
        ),
        name="cp-placement-bad-endpoint",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        placement=(
            "    placement:\n"
            "      publicEndpoint: ftp://placement.example.com\n"
        ),
    ),
    Fixture(
        filename="68-placement-public-endpoint-host-mismatch.yaml",
        comment=(
            "services.placement.publicEndpoint must name the same host as\n"
            "services.placement.gateway.hostname. The Gateway listener is what routes that\n"
            "hostname to the Placement API, so a divergent host advertises a catalog endpoint\n"
            "that never reaches it. The value is projected into no child CR, so this webhook\n"
            "is the only gate on the URL every compute service resolves to place its\n"
            "allocations."
        ),
        name="cp-placement-endpoint-host-mismatch",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        placement=(
            "    placement:\n"
            "      gateway:\n"
            "        hostname: placement.example.com\n"
            "        parentRef:\n"
            "          name: openstack-gw\n"
            "      publicEndpoint: https://allocations.example.com\n"
        ),
    ),
    Fixture(
        filename="69-placement-override-dynamic-on-dedicated.yaml",
        comment=(
            "databaseCredentialsMode Dynamic on the Placement service is rejected by the\n"
            "webhook when Placement declares a DEDICATED database, mirroring the Keystone and\n"
            "Glance cases: the override retargets the shared database this service does not\n"
            "use, and a dedicated database is Static-only. The defaulting webhook injects the\n"
            "placement service account, so the only violation is the credentials-mode override."
        ),
        name="cp-override-dynamic-dedicated-pl",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        placement=(
            "    placement:\n"
            "      databaseCredentialsMode: Dynamic\n"
            "      dedicatedBackingServices:\n"
            "        database:\n"
            "          clusterRef:\n"
            "            name: cp-dedicated-db\n"
            "          database: placement\n"
            "          secretRef:\n"
            "            name: placement-db\n"
        ),
    ),
    # --- per-service Barbican (still the create-rejection matrix). Every
    #     ControlPlane name below stays at or under 34 characters: the projected
    #     Barbican child is "{cp}-barbican", and the Barbican CRD caps its own
    #     metadata.name at 43. ---
    Fixture(
        filename="70-external-with-barbican.yaml",
        comment=(
            "services.barbican set in External mode is forbidden by the webhook (cross-field,\n"
            "mirrors services.glance and services.placement): Barbican needs its own\n"
            "External-mode design. The barbican block is the minimal valid one (a dedicated\n"
            "secret store), so the ONLY violation is the cross-field forbid."
        ),
        name="cp-external-with-barbican",
        barbican=VALID_BARBICAN,
    ),
    Fixture(
        filename="71-barbican-secret-store-both-modes.yaml",
        comment=(
            "services.barbican.secretStore setting BOTH dedicated and external violates the\n"
            "type-level CEL union rule: the two modes address different servers with\n"
            "different credentials, so a block naming both leaves the projection no way to\n"
            "tell which server barbican writes its secret material to. The external block is\n"
            "itself valid, so the ONLY violation is the union."
        ),
        name="cp-barbican-store-both",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        barbican=(
            "    barbican:\n"
            "      secretStore:\n"
            "        dedicated: {}\n"
            "        external:\n"
            "          url: https://openbao.example.com:8200\n"
            "          credentialsSecretRef:\n"
            "            name: barbican-approle\n"
        ),
    ),
    Fixture(
        filename="72-barbican-secret-store-no-mode.yaml",
        comment=(
            "services.barbican.secretStore naming NEITHER mode violates the same CEL union\n"
            "rule from the other side. A Barbican with no store attached parks on\n"
            "SecretStoresReady=False/NoDefaultSecretStore for as long as it exists, so the\n"
            "block would only ever project a child that can never reach Ready. The webhook\n"
            "mirrors the rule for an API server old enough to skip x-kubernetes-validations."
        ),
        name="cp-barbican-store-neither",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        barbican=(
            "    barbican:\n"
            "      secretStore: {}\n"
        ),
    ),
    Fixture(
        filename="73-barbican-external-store-plaintext-url.yaml",
        comment=(
            "services.barbican.secretStore.external.url without an https scheme violates the\n"
            "CRD pattern (^https://), which is stricter than the ^https?:// the endpoint\n"
            "fields carry: the operator's AppRole login and every secret barbican stores\n"
            "travel this URL, so a plaintext scheme would put the role ID, the secret ID, and\n"
            "the keys and certificates the service exists to protect on the wire in the\n"
            "clear. The webhook mirrors the pattern."
        ),
        name="cp-barbican-store-plaintext-url",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        barbican=(
            "    barbican:\n"
            "      secretStore:\n"
            "        external:\n"
            "          url: http://openbao.example.com:8200\n"
            "          credentialsSecretRef:\n"
            "            name: barbican-approle\n"
        ),
    ),
    Fixture(
        filename="74-barbican-external-store-empty-credentials-name.yaml",
        comment=(
            "services.barbican.secretStore.external.credentialsSecretRef.name empty violates\n"
            "the SecretNameRefSpec MinLength marker, mirroring the keystone caBundleSecretRef\n"
            "fixture: the external store authenticates with the AppRole credentials that\n"
            "Secret holds, so an unnamed reference resolves to nothing. Schema validation runs\n"
            "before the validating webhook, so the marker is what rejects this CR and the\n"
            "chainsaw step anchors on its message; the webhook mirrors the rule for callers\n"
            "that bypass the schema."
        ),
        name="cp-barbican-store-empty-creds",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        barbican=(
            "    barbican:\n"
            "      secretStore:\n"
            "        external:\n"
            "          url: https://openbao.example.com:8200\n"
            "          credentialsSecretRef:\n"
            '            name: ""\n'
        ),
    ),
    Fixture(
        filename="75-barbican-public-endpoint-host-mismatch.yaml",
        comment=(
            "services.barbican.publicEndpoint must name the same host as\n"
            "services.barbican.gateway.hostname. The Gateway listener is what routes that\n"
            "hostname to the Barbican API, so a divergent host advertises a catalog endpoint\n"
            "that never reaches it. The value is projected into no child CR, so this webhook\n"
            "is the only gate on the URL every client resolves to store and read its secret\n"
            "material. The endpoint keeps the https scheme the gateway rule also demands, so\n"
            "the ONLY violation is the host."
        ),
        name="cp-barbican-endpoint-mismatch",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        barbican=(
            VALID_BARBICAN
            + "      gateway:\n"
            + "        hostname: barbican.example.com\n"
            + "        parentRef:\n"
            + "          name: openstack-gw\n"
            + "      publicEndpoint: https://secrets.example.com\n"
        ),
    ),
    Fixture(
        filename="76-barbican-override-dynamic-on-dedicated.yaml",
        comment=(
            "databaseCredentialsMode Dynamic on the Barbican service is rejected by the\n"
            "webhook when Barbican declares a DEDICATED database, mirroring the keystone,\n"
            "glance, and placement twins: the override retargets the shared database this\n"
            "service does not use, and a dedicated database is Static-only. The defaulting\n"
            "webhook injects the barbican service account, so the only violation is the\n"
            "credentials-mode override."
        ),
        name="cp-override-dynamic-dedicated-bn",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        barbican=(
            VALID_BARBICAN
            + "      databaseCredentialsMode: Dynamic\n"
            + "      dedicatedBackingServices:\n"
            + "        database:\n"
            + "          clusterRef:\n"
            + "            name: cp-dedicated-db\n"
            + "          database: barbican\n"
            + "          secretRef:\n"
            + "            name: barbican-db\n"
        ),
    ),
    # --- per-service Neutron (still the create-rejection matrix). Every
    #     ControlPlane name below stays at or under 32 characters, except the one
    #     fixture that pins the bound: the projected Neutron child is
    #     "{cp}-neutron" and the Neutron CRD caps its own metadata.name at 40.
    #     The numbering runs 96-99 and then fills 24-26, three of the gaps left
    #     below the two-digit ceiling every fixture filename sits under. ---
    Fixture(
        filename="96-external-with-neutron.yaml",
        comment=(
            "services.neutron set in External mode is forbidden by the webhook (cross-field,\n"
            "mirroring its glance, placement and barbican siblings): Neutron needs its own\n"
            "External-mode design. The neutron block is the minimal valid one (a reference to\n"
            "the OVNCentral the child programs), so the ONLY violation is the cross-field\n"
            "forbid, and the chainsaw step anchors on `forbidden when services.keystone.mode\n"
            "is External`."
        ),
        name="cp-external-with-neutron",
        neutron=VALID_NEUTRON,
    ),
    Fixture(
        filename="97-neutron-without-messaging.yaml",
        comment=(
            "spec.infrastructure.messaging is required as soon as services.neutron is set,\n"
            "and the webhook is the only layer that can say so: the Neutron CRD requires\n"
            "spec.messaging and the ControlPlane derives the child's transport URL from the\n"
            "shared bus, so a ControlPlane declaring the network service without a bus would\n"
            "project a child its own admission rejects on every pass. The infrastructure block\n"
            "is the brownfield one every Managed fixture carries, minus the messaging entry, so\n"
            "the missing bus is the only violation and the step anchors on `is required when\n"
            "services.neutron is set`."
        ),
        name="cp-neutron-no-messaging",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        neutron=VALID_NEUTRON,
    ),
    Fixture(
        filename="98-neutron-ovn-centralref-name-empty.yaml",
        comment=(
            "services.neutron.ovn.centralRef.name is empty. The ML2/OVN mechanism driver\n"
            "writes every network, subnet and port into that central's Northbound database, so\n"
            "a reference naming no OVNCentral leaves the child no database to program. The\n"
            "field carries MinLength=1, which rejects the CR at the CRD schema layer before\n"
            "the webhook mirror in validateNeutron runs, so the chainsaw step anchors on the\n"
            "API server's marker message rather than the webhook's."
        ),
        name="cp-neutron-empty-central-name",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA_WITH_BROWNFIELD_MESSAGING,
        neutron=(
            "    neutron:\n"
            "      ovn:\n"
            "        centralRef:\n"
            '          name: ""\n'
        ),
    ),
    Fixture(
        filename="99-neutron-public-endpoint-host-mismatch.yaml",
        comment=(
            "services.neutron.publicEndpoint must name the same host as\n"
            "services.neutron.gateway.hostname (webhook-only). The Gateway listener is what\n"
            "routes that hostname to the Neutron API, so a divergent host advertises a catalog\n"
            "endpoint that never reaches it. The value is projected into no child CR, so this\n"
            "webhook is the only gate on the URL every client resolves to create its networks,\n"
            "subnets and ports. The endpoint keeps the https scheme the gateway rule also\n"
            "demands, so the ONLY violation is the host and the step anchors on `must equal\n"
            "services.neutron.gateway.hostname`."
        ),
        name="cp-neutron-endpoint-mismatch",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA_WITH_BROWNFIELD_MESSAGING,
        neutron=(
            VALID_NEUTRON
            + "      gateway:\n"
            + "        hostname: neutron.example.com\n"
            + "        parentRef:\n"
            + "          name: openstack-gw\n"
            + "      publicEndpoint: https://other.example.com\n"
        ),
    ),
    Fixture(
        filename="24-neutron-override-dynamic-on-dedicated.yaml",
        comment=(
            "databaseCredentialsMode Dynamic on the Neutron service is rejected by the webhook\n"
            "when Neutron declares a DEDICATED database, mirroring the keystone, glance,\n"
            "placement and barbican twins: the override retargets the shared database this\n"
            "service does not use, and a dedicated database is Static-only. The defaulting\n"
            "webhook materializes that dedicated database's own Static mode, so the only\n"
            "violation is the service-level override and the step anchors on `not supported as\n"
            "an override on a service with a dedicated database`."
        ),
        name="cp-override-dynamic-dedicated-nn",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA_WITH_BROWNFIELD_MESSAGING,
        neutron=(
            VALID_NEUTRON
            + "      databaseCredentialsMode: Dynamic\n"
            + "      dedicatedBackingServices:\n"
            + "        database:\n"
            + "          clusterRef:\n"
            + "            name: cp-dedicated-db\n"
            + "          database: neutron\n"
            + "          secretRef:\n"
            + "            name: neutron-db\n"
        ),
    ),
    Fixture(
        filename="25-placed-neutron-unpublished.yaml",
        comment=(
            "A placed CATALOG service must advertise a publicEndpoint or a gateway\n"
            "(webhook-only), the rule the placed-Glance fixture pins for the image service and\n"
            "this one for the network service: what the ControlPlane registers for an\n"
            "unpublished Neutron is its in-cluster Service DNS name, which resolves nowhere\n"
            "outside the cluster Neutron runs on, so every client that reads the catalog from\n"
            "anywhere else gets an address it cannot connect to. The namespace block is\n"
            "present, so the co-requisite rule of the same validator stays silent and the step\n"
            "anchors on `one of publicEndpoint or gateway is required when targetClusterRef is\n"
            "set`. Keystone stays unplaced in the ControlPlane's own namespace, which the\n"
            "co-location rule does not compare against Neutron's; it is published all the same,\n"
            "because a service placed away from Keystone reaches it over the public URL."
        ),
        name="cp-placed-neutron-unpublished",
        keystone=(
            "      mode: Managed\n"
            "      publicEndpoint: https://keystone.example.com/v3\n"
        ),
        infrastructure=MANAGED_INFRA_WITH_BROWNFIELD_MESSAGING,
        neutron=(
            VALID_NEUTRON
            + "      namespace:\n"
            + "        name: network\n"
            + "      targetClusterRef:\n"
            + "        name: edge\n"
        ),
    ),
    Fixture(
        filename="26-neutron-name-too-long.yaml",
        comment=(
            "metadata.name is 33 characters, so the Neutron child this ControlPlane projects\n"
            "would be named \"{cp}-neutron\" at 41 — one over the 40 the Neutron CRD admits,\n"
            "because its ovn-db-sync CronJob appends a 12-character suffix and Kubernetes caps\n"
            "CronJob names at 52. The webhook rejects the ControlPlane up front (a create-only\n"
            "rule, since metadata.name is immutable): admitted, it would fail to apply the\n"
            "child on every pass, with NeutronReady stuck False and no recovery short of\n"
            "recreating the whole control plane. The step anchors on `the projected Neutron\n"
            "child CR name would be 41 characters`."
        ),
        name="cp-neutron-name-is-thirty-three-x",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA_WITH_BROWNFIELD_MESSAGING,
        neutron=VALID_NEUTRON,
    ),
    Fixture(
        filename="27-neutron-ovn-central-foreign-namespace.yaml",
        comment=(
            "services.neutron.ovn.centralRef.namespace names a namespace this ControlPlane\n"
            "neither owns nor claims through a services.<service>.namespace assignment\n"
            "(webhook-only). The reference is not read-only: the neutron-operator mirrors the\n"
            "named central's client Secret into the Neutron namespace, so admitting it would\n"
            "hand this plane a full mTLS identity for another plane's Northbound and\n"
            "Southbound databases. The fixture carries no metadata.namespace — Chainsaw runs\n"
            "each Test in an ephemeral one — so any namespace spelled out here is foreign, and\n"
            "the step anchors on `is neither this ControlPlane`."
        ),
        name="cp-neutron-foreign-central-ns",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA_WITH_BROWNFIELD_MESSAGING,
        neutron=(
            "    neutron:\n"
            "      ovn:\n"
            "        centralRef:\n"
            "          name: ovn\n"
            "          namespace: other-tenant\n"
        ),
    ),
    # --- transition wave E: barbican secret-store addressing freeze
    #     (Test: c5c3-invalid-cr-barbican-store-freeze) ---
    Fixture(
        filename="77-transition-base-barbican-dedicated.yaml",
        comment=(
            "Accepted base for the barbican secret-store freeze test: a Managed ControlPlane\n"
            "whose Barbican service takes the dedicated secret store the operator provisions."
        ),
        name="cp-transition-e",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        barbican=VALID_BARBICAN,
    ),
    Fixture(
        filename="78-transition-barbican-store-to-external.yaml",
        comment=(
            "UPDATE of the accepted dedicated base onto an external store is rejected. The\n"
            "secret material barbican has already written lives on the server the current\n"
            "mode names, and the BarbicanSecretStore CRD freezes the instanceRef/server\n"
            "discriminator for exactly that reason. Without the ControlPlane-side freeze the\n"
            "reconciler would answer the drift by deleting and recreating the store against\n"
            "the new server, stranding that material together with the OpenBao instance, its\n"
            "raft volume, and its seal key. dedicated is explicitly nulled, not merely\n"
            "omitted: Chainsaw applies an UPDATE as an RFC 7386 JSON merge patch, so an\n"
            "omitted dedicated would be RETAINED from the base, and the resulting\n"
            "both-modes shape would trip the CEL union rule (exactly one of dedicated or\n"
            "external) at CRD validation, before the webhook's mode freeze ever runs."
        ),
        name="cp-transition-e",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        barbican=(
            "    barbican:\n"
            "      secretStore:\n"
            "        dedicated: null\n"
            "        external:\n"
            "          url: https://openbao.example.com:8200\n"
            "          credentialsSecretRef:\n"
            "            name: barbican-approle\n"
        ),
    ),
    # --- per-service target clusters (issue #840, create-rejection matrix) ---
    Fixture(
        filename="79-target-cluster-in-external-mode.yaml",
        comment=(
            "services.keystone.targetClusterRef in External mode violates the CEL rule: no\n"
            "Keystone workload is deployed, so there is nothing to place on another cluster.\n"
            "The rest of the CR is the minimal External sketch, so the ref is the only\n"
            "violation."
        ),
        name="cp-external-target-cluster",
        keystone=(
            "      mode: External\n"
            "      external:\n"
            "        authURL: https://keystone.example.com/v3\n"
            "      targetClusterRef:\n"
            "        name: edge\n"
        ),
    ),
    Fixture(
        filename="80-target-cluster-empty-name.yaml",
        comment=(
            "services.keystone.targetClusterRef.name is empty. The shared\n"
            "TargetClusterRefSpec.Name carries MinLength=1, so a ref naming no cluster is\n"
            "rejected at the CRD schema layer before the webhook mirror in\n"
            "validation.TargetClusterRef runs. Everything else a placed service needs is\n"
            "present (a namespace of its own, a public endpoint), so the empty name is the\n"
            "only violation."
        ),
        name="cp-target-cluster-empty-name",
        keystone=(
            "      mode: Managed\n"
            "      namespace:\n"
            "        name: identity\n"
            "      publicEndpoint: https://keystone.example.com/v3\n"
            "      targetClusterRef:\n"
            '        name: ""\n'
        ),
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="81-placed-service-without-namespace.yaml",
        comment=(
            "A placed service must declare a namespace of its own (webhook-only): every\n"
            "namespace maps to exactly one cluster and the ControlPlane's own stays on the\n"
            "local one, so a Keystone placed elsewhere without a namespace block would have\n"
            "its database, its tenant store, and its credential material provisioned in a\n"
            "namespace that lives on another cluster than the ref names. The publicEndpoint\n"
            "is set, so the reachability rule of the same validator stays silent and the\n"
            "missing namespace is the only violation."
        ),
        name="cp-placed-without-namespace",
        keystone=(
            "      mode: Managed\n"
            "      publicEndpoint: https://keystone.example.com/v3\n"
            "      targetClusterRef:\n"
            "        name: edge\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="82-placed-glance-unpublished.yaml",
        comment=(
            "A placed CATALOG service must advertise a publicEndpoint or a gateway\n"
            "(webhook-only): what the ControlPlane registers for an unpublished Glance is its\n"
            "in-cluster Service DNS name, which resolves nowhere outside the cluster Glance\n"
            "runs on, so every client that reads the catalog from anywhere else gets an\n"
            "address it cannot connect to. The namespace block is present, so the co-requisite\n"
            "rule of the same validator stays silent and the unreachable catalog entry is the\n"
            "only violation. Keystone stays unplaced in the ControlPlane's own namespace,\n"
            "which the co-location rule does not compare against Glance's; it is published\n"
            "all the same, because a service placed away from Keystone reaches it over the\n"
            "public URL."
        ),
        name="cp-placed-glance-unpublished",
        keystone=(
            "      mode: Managed\n"
            "      publicEndpoint: https://keystone.example.com/v3\n"
        ),
        infrastructure=MANAGED_INFRA,
        glance=(
            VALID_GLANCE
            + "      namespace:\n"
            + "        name: images\n"
            + "      targetClusterRef:\n"
            + "        name: edge\n"
        ),
    ),
    Fixture(
        filename="83-target-cluster-disagreement.yaml",
        comment=(
            "Two services co-located in ONE namespace must name the SAME target cluster. That\n"
            "namespace exists on exactly one cluster, together with the backing services, the\n"
            "tenant store, and the credential material scoped to it, so the services in it\n"
            "cannot disagree on which one it is. Both blocks are otherwise complete (the\n"
            "shared namespace with one lifecycle, a public endpoint each), so the\n"
            "disagreement is the only violation."
        ),
        name="cp-target-cluster-disagreement",
        keystone=(
            "      mode: Managed\n"
            "      namespace:\n"
            "        name: shared-services\n"
            "        lifecycle: External\n"
            "      publicEndpoint: https://keystone.example.com/v3\n"
            "      targetClusterRef:\n"
            "        name: edge\n"
        ),
        infrastructure=MANAGED_INFRA,
        glance=(
            VALID_GLANCE
            + "      namespace:\n"
            + "        name: shared-services\n"
            + "        lifecycle: External\n"
            + "      publicEndpoint: https://glance.example.com\n"
            + "      targetClusterRef:\n"
            + "        name: core\n"
        ),
    ),
    Fixture(
        filename="86-placed-service-unpublished-keystone.yaml",
        comment=(
            "Keystone must advertise a publicEndpoint or a gateway as soon as ANOTHER\n"
            "service is placed on a target cluster (webhook-only). That service validates\n"
            "its tokens against Keystone and cannot resolve Keystone's in-cluster Service\n"
            "DNS name from another cluster, so the operator would project an EMPTY\n"
            "spec.keystoneEndpoint into the placed child — which the child's own CRD refuses\n"
            "(MinLength=1 plus ^https?://) on every pass, with nothing on the ControlPlane\n"
            "naming the field to fix. The per-service rule above only reaches a service\n"
            "carrying a ref of its OWN, so an unplaced Keystone falls outside it. Glance\n"
            "carries its namespace and its public endpoint, so the unpublished Keystone is\n"
            "the only violation."
        ),
        name="cp-glance-unpublished-keystone",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=(
            VALID_GLANCE
            + "      namespace:\n"
            + "        name: images\n"
            + "      publicEndpoint: https://glance.example.com\n"
            + "      targetClusterRef:\n"
            + "        name: edge\n"
        ),
    ),
    Fixture(
        filename="87-placed-keystone-plaintext-endpoint.yaml",
        comment=(
            "Keystone's publicEndpoint must use https as soon as a cluster boundary\n"
            "separates Keystone from any service, here by placing Keystone itself\n"
            "(webhook-only). With nothing placed, that URL only feeds the Keystone bootstrap\n"
            "and the catalog's public identity row; across a boundary it becomes the auth_url\n"
            "the operator renders the admin password and every service-account password NEXT\n"
            "TO, and those credentials cross that boundary on every mint, re-mint and\n"
            "delivery. The ^https?:// pattern on the field admits http:// on purpose for the\n"
            "all-local case, so the scheme is checked here instead. Everything else a placed\n"
            "service needs is present, so the plaintext endpoint is the only violation."
        ),
        name="cp-placed-keystone-plaintext",
        keystone=(
            "      mode: Managed\n"
            "      namespace:\n"
            "        name: identity\n"
            "      publicEndpoint: http://keystone.example.com:5000/v3\n"
            "      targetClusterRef:\n"
            "        name: edge\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    # --- service-registration allowlist (still the create-rejection matrix) ---
    Fixture(
        filename="88-registration-namespace-invalid.yaml",
        comment=(
            "spec.korc.serviceRegistrations.allowedNamespaces[] outside the RFC-1123\n"
            "label shape is rejected (CRD items pattern): each entry names a Kubernetes\n"
            "namespace the registration gate admits KeystoneService CRs from, so a value\n"
            "no namespace can carry would sit in the allowlist admitting nothing."
        ),
        name="cp-registration-bad-namespace",
        service_registrations=(
            "    serviceRegistrations:\n"
            "      allowedNamespaces:\n"
            "      - Tenant_A\n"
        ),
    ),
    Fixture(
        filename="89-registration-namespace-duplicate.yaml",
        comment=(
            "A duplicate spec.korc.serviceRegistrations.allowedNamespaces entry is\n"
            "rejected by the apiserver's listType=set semantics. Consent is a set: a\n"
            "namespace listed twice cannot be admitted twice, and the duplicate would\n"
            "survive the edit that removes the entry an operator meant to revoke."
        ),
        name="cp-registration-duplicate-namespace",
        service_registrations=(
            "    serviceRegistrations:\n"
            "      allowedNamespaces:\n"
            "      - tenant-a\n"
            "      - tenant-a\n"
        ),
    ),
    # --- transition wave F: target-cluster assignment freeze
    #     (Test: c5c3-invalid-cr-target-cluster-freeze) ---
    Fixture(
        filename="84-transition-base-unplaced.yaml",
        comment=(
            "Accepted base for the target-cluster freeze test: a Managed ControlPlane whose\n"
            "Keystone service carries everything a placement needs (a namespace of its own,\n"
            "a public endpoint) but names no target cluster, so it stays on the local one.\n"
            "The External lifecycle is deliberate, as on the namespace-freeze base: the\n"
            "operator never creates that namespace, so the CR parks on\n"
            "NamespacesReady=False/NamespaceNotFound and provisions nothing, leaving no side\n"
            "effects for the rejection step to clean up. The namespace name differs from the\n"
            "namespace-freeze base's because a namespace belongs to at most one ControlPlane\n"
            "and both bases persist for the length of the run."
        ),
        name="cp-transition-f",
        keystone=(
            "      mode: Managed\n"
            "      namespace:\n"
            "        name: invalid-cr-preexisting-placed\n"
            "        lifecycle: External\n"
            "      publicEndpoint: https://keystone.example.com/v3\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="85-transition-add-target-cluster.yaml",
        comment=(
            "UPDATE placing the accepted base's Keystone service on a target cluster is\n"
            "rejected: the assignment is create-only. Re-pointing a live service at another\n"
            "cluster strands everything the previous one holds (its workload, its database,\n"
            "its tenant store, and the credential material in it), none of which the\n"
            "reconcile that follows the edit moves or reaps. The freeze is webhook-only, with\n"
            "no CEL transition rule, so moving a service between clusters can be relaxed to a\n"
            "gated migration later. Everything a placed service needs is already on the base,\n"
            "so the added ref is the only change."
        ),
        name="cp-transition-f",
        keystone=(
            "      mode: Managed\n"
            "      namespace:\n"
            "        name: invalid-cr-preexisting-placed\n"
            "        lifecycle: External\n"
            "      publicEndpoint: https://keystone.example.com/v3\n"
            "      targetClusterRef:\n"
            "        name: edge\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    # --- shared message bus (issue #895, create-rejection matrix) ---
    Fixture(
        filename="90-messaging-both-modes.yaml",
        comment=(
            "spec.infrastructure.messaging naming both a clusterRef and a secretRef violates\n"
            "the type-level CEL rule on commonv1.MessagingSpec (exactly one of clusterRef or\n"
            "secretRef must be set). The two refs are the managed and the brownfield mode:\n"
            "one has the ControlPlane provision a RabbitmqCluster, the other reads a\n"
            "transport URL off a Secret, and a CR asking for both leaves the reconciler no\n"
            "way to decide which broker the control plane talks to. The webhook mirror in\n"
            "validation.MessagingXOR is the twin that catches this when the schema is\n"
            "bypassed."
        ),
        name="cp-messaging-both-modes",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA
        + (
            "    messaging:\n"
            "      clusterRef:\n"
            "        name: openstack-rabbitmq\n"
            "      secretRef:\n"
            "        name: bus-url\n"
        ),
    ),
    Fixture(
        filename="91-messaging-secretref-empty-name.yaml",
        comment=(
            "spec.infrastructure.messaging.secretRef.name is empty. Brownfield messaging\n"
            "reads the whole rabbit:// transport URL out of that Secret, so a ref naming no\n"
            "Secret addresses no broker at all. The shared SecretRefSpec.Name carries\n"
            "MinLength=1, which rejects it at the CRD schema layer; the webhook's\n"
            "field.Required on spec.infrastructure.messaging.secretRef.name is the twin for\n"
            "a bypassed schema. Only the brownfield mode is declared, so the empty name is\n"
            "the only violation."
        ),
        name="cp-messaging-empty-secret-name",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA
        + (
            "    messaging:\n"
            "      secretRef:\n"
            '        name: ""\n'
        ),
    ),
    Fixture(
        filename="92-messaging-tls-in-managed-mode.yaml",
        comment=(
            "spec.infrastructure.messaging.tls beside a managed clusterRef is rejected.\n"
            "The block carries CLIENT trust only, and the reconciler projects nothing but\n"
            "spec.replicas onto the owned RabbitmqCluster, so a managed broker comes up on\n"
            "the RabbitMQ Cluster Operator's default, plaintext listener. Admitting the\n"
            "pair would promise an encrypted connection nothing provisions, and the\n"
            "mismatch would only surface once the first consumer rendered ssl = true.\n"
            "tls is supported in brownfield mode, where the broker's listeners are\n"
            "someone else's concern. Webhook-only: the shared commonv1.MessagingSpec must\n"
            "not carry a c5c3-specific CEL rule."
        ),
        name="cp-messaging-tls-managed",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA
        + (
            "    messaging:\n"
            "      clusterRef:\n"
            "        name: openstack-rabbitmq\n"
            "      tls:\n"
            "        caBundleSecretRef:\n"
            "          name: rabbitmq-ca\n"
        ),
    ),
    # --- transition wave G: shared message-bus freeze
    #     (Test: c5c3-invalid-cr-messaging-freeze) ---
    Fixture(
        filename="93-transition-base-messaging.yaml",
        comment=(
            "Accepted base for the messaging freeze test: a Managed ControlPlane that\n"
            "declares the shared message bus in brownfield mode. Brownfield throughout\n"
            "(database, cache and messaging all address endpoints outside the cluster), so\n"
            "the base provisions nothing and leaves no side effects for the rejection steps\n"
            "to clean up, while still carrying a declared messaging block for them to\n"
            "freeze."
        ),
        name="cp-transition-g",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA_WITH_BROWNFIELD_MESSAGING,
    ),
    Fixture(
        filename="94-transition-messaging-mode-flip.yaml",
        comment=(
            "UPDATE flipping the accepted brownfield base onto a managed clusterRef is\n"
            "rejected: the messaging mode is immutable, like the cache one. The two modes\n"
            "address different brokers, so the flip would re-point every consumer at a\n"
            "RabbitmqCluster the ControlPlane provisions fresh and empty while the queues\n"
            "the control plane has been using stay on the brownfield broker. secretRef is\n"
            "explicitly nulled for the reason the barbican store flip states: Chainsaw\n"
            "applies an UPDATE as an RFC 7386 JSON merge patch, so an omitted secretRef\n"
            "would be RETAINED from the base and the resulting both-modes shape would trip\n"
            "the CEL XOR rule at CRD validation, before the webhook's mode freeze ever\n"
            "runs."
        ),
        name="cp-transition-g",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA
        + (
            "    messaging:\n"
            "      clusterRef:\n"
            "        name: openstack-rabbitmq\n"
            "      secretRef: null\n"
        ),
    ),
    Fixture(
        filename="95-transition-remove-messaging.yaml",
        comment=(
            "UPDATE dropping the BROWNFIELD messaging block from the accepted base is\n"
            "rejected: the block is a one-way add in BOTH modes. Brownfield provisions\n"
            "nothing - managedInfraInstances returns early on a nil clusterRef - so the\n"
            "removal strands no state on its own, but admitting it would launder the mode\n"
            "freeze the previous step pins into a two-step flip: null the block here, then\n"
            "re-add it with a clusterRef as an ordinary opt-in, and every consumer is\n"
            "re-pointed at a fresh, empty RabbitmqCluster while the queues stay on the\n"
            "brownfield broker - without a single admission error. Nothing in spec or\n"
            "status remembers the mode a previous revision declared, so the one-step\n"
            "rejection only holds while this one does too. The MANAGED removal is the same\n"
            "rule seen from the other side, pinned against a live owned broker by the\n"
            "messaging e2e suite and by TestValidateUpdate_MessagingFreeze. messaging is\n"
            "explicitly nulled, not merely omitted: Chainsaw applies an UPDATE as an RFC\n"
            "7386 JSON merge patch, so an omitted block would simply be retained and the\n"
            "step would assert nothing."
        ),
        name="cp-transition-g",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA + "    messaging: null\n",
    ),
)


def main() -> int:
    check = "--check" in sys.argv[1:]
    here = Path(__file__).resolve().parent
    drift = False

    for fixture in FIXTURES:
        target = here / fixture.filename
        content = fixture.render()
        if check:
            on_disk = target.read_text(encoding="utf-8") if target.exists() else None
            if on_disk != content:
                print(f"DRIFT: {fixture.filename}")
                drift = True
        else:
            target.write_text(content, encoding="utf-8")
            print(f"wrote {fixture.filename}")

    # Orphan sweep (both directions): a fixture file on disk that is not declared
    # in FIXTURES is drift too.
    declared = {fixture.filename for fixture in FIXTURES}
    for path in sorted(here.iterdir()):
        if not _FIXTURE_FILENAME_PATTERN.match(path.name):
            continue
        if path.name in declared:
            continue
        if check:
            print(f"DRIFT: orphan fixture {path.name} not declared in FIXTURES")
            drift = True
        else:
            path.unlink()
            print(f"removed orphan {path.name}")

    if check and drift:
        print("run `python3 tests/e2e/c5c3/invalid-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
