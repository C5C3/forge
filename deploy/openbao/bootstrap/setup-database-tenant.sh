#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# setup-database-tenant.sh — Provision the per-tenant MariaDB database-engine
# connection and role for one MANAGED ControlPlane's per-service DB users
# (Keystone always; Glance, Placement, Barbican, and Neutron when they share the
# managed database).
#
# MODE: this is a managed-database onboarding step. An External-mode ControlPlane
# (spec.services.keystone.mode: External) has NO managed database — the c5c3
# operator's reconcileDBCredentials is skipped for it — so it must not be
# onboarded here; there is no MariaDB CR to read and no DB credential path to
# issue.
#
# This is the stage-(b) counterpart to the bootstrap engine mount performed by
# setup-secret-engines.sh (enable_database). The database secrets engine is
# mounted at database/mariadb during bootstrap, but per-tenant connection and
# role configuration cannot happen there: the managed MariaDB instances do not
# exist at bootstrap time. This script is therefore run once per ControlPlane,
# after its MariaDB is Ready, to configure — per service (see
# provision_service_tenant) — one connection+role pair:
#
#   - database/mariadb/config/<service>-<service-namespace>
#       the connection to the service's MariaDB, authenticated as root.
#   - database/mariadb/roles/<service>-<service-namespace>
#       a role that issues short-lived MySQL users with ALL PRIVILEGES on the
#       service database and auto-revokes them at lease end.
#
# It ALWAYS provisions the Keystone pair. It also provisions a Glance pair when
# the ControlPlane declares spec.services.glance on the SHARED managed database,
# and Placement, Barbican and Neutron pairs under the same condition for
# spec.services.placement, spec.services.barbican and spec.services.neutron; a
# service that declares a dedicated database
# (spec.services.<service>.dedicatedBackingServices.database) is Static-only and
# is skipped here. Each service's pair is keyed and root-resolved independently
# in ITS OWN service namespace, so the Glance, Placement, Barbican, and Neutron
# engine plumbing is keystone-independent.
#
# The role is keyed on the KEYSTONE SERVICE NAMESPACE alone — the namespace the
# MariaDB lives in and the generator's ServiceAccount authenticates from. That is
# the ControlPlane's own namespace unless spec.services.keystone.namespace places
# the Keystone service elsewhere, in which case its database and its credential
# generator follow it there, and a role keyed on the ControlPlane's namespace
# would be outside the reach of the keystone-db-dynamic templated policy — which
# grants exactly the caller's OWN namespace.
#
# A namespace is a unique tenant key (at most one ControlPlane occupies it), and
# cluster-unique namespaces keep the role name collision-free (a hyphen-joined
# <namespace>-<controlplane> would be ambiguous — e.g. ns=a-b/name=c and
# ns=a/name=b-c both flatten to keystone-a-b-c and would overwrite each other's
# connection config on the second onboarding). Namespace-only keying is also what
# lets the keystone-db-dynamic policy scope reads to the caller's OWN namespace
# with an exact ACL-template match (no over-matching wildcard).
#
# Engine-issued credentials are then read at database/mariadb/creds/<role> by
# the c5c3 operator's per-ControlPlane VaultDynamicSecret generator
# (reconcile_dbcredentials.go). The role name derivation below MUST stay in sync
# with dbDynamicRoleFor in operators/c5c3/internal/controller/reconcile_dbcredentials.go.
#
# Idempotent: config and role writes are upserts, so re-running refreshes a
# rotated root password or an updated database name.
#
# The arguments still name the ControlPlane (<namespace> is where the CR lives);
# the Keystone service namespace is resolved from its spec.
#
# Usage: setup-database-tenant.sh <namespace> <controlplane>
# Requires: BAO_TOKEN in the environment (kubectl access to the openbao-0 pod).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

###############################################################################
# Configuration
###############################################################################
BAO_TOKEN="${BAO_TOKEN:?BAO_TOKEN must be set}"

# Lease TTLs for issued credentials. default_ttl MUST stay above the c5c3
# operator's DB-credential ExternalSecret refreshInterval (24h) so ESO re-issues a
# fresh lease before the previous one expires; the default_ttl − refreshInterval
# gap (48h − 24h = 24h) is the window the keystone operator has to roll the pods
# onto a fresh credential before the previous, still-in-use lease is revoked. A
# full-day gap means a stalled rollout (bad image, resource pressure) has to
# persist for a day — long enough to page on-call — before a running Keystone
# loses DB access, instead of silently arming an outage within a couple of hours.
# max_ttl caps the absolute lifetime of any issued credential. Override via the
# environment to tune the rotation cadence.
#
# INVARIANT: these TTLs only bind while the minting auth token outlives them.
# OpenBao revokes a dynamic-secret lease together with the token that created
# it, so the keystone-db auth role (setup-auth.sh) pins its token_ttl/
# token_max_ttl to DB_CREDS_MAX_TTL. Raising DB_CREDS_* beyond 72h without
# raising the role's token TTLs silently caps the effective credential
# lifetime at the token's — the failure mode that once killed running
# Keystones hourly under a 1h token.
DB_CREDS_DEFAULT_TTL="${DB_CREDS_DEFAULT_TTL:-48h}"
DB_CREDS_MAX_TTL="${DB_CREDS_MAX_TTL:-72h}"

CP_NS="${1:?usage: setup-database-tenant.sh <namespace> <controlplane>}"
CP_NAME="${2:?usage: setup-database-tenant.sh <namespace> <controlplane>}"

# get_controlplane_field prints a jsonpath field from the live ControlPlane CR,
# or the supplied default when the field is empty/absent. Callers MUST verify the
# ControlPlane CR exists first (see main): this fallback is only meaningful for a
# genuinely-unset field, not a failed lookup — the two are indistinguishable here.
get_controlplane_field() {
  local jsonpath="$1"
  local default="$2"
  local value
  value="$(kubectl get controlplane "${CP_NAME}" -n "${CP_NS}" \
    -o "jsonpath=${jsonpath}" 2>/dev/null || true)"
  if [[ -z "${value}" ]]; then
    echo "${default}"
  else
    echo "${value}"
  fi
}

###############################################################################
# provision_service_tenant <service> <svc_ns> <mariadb_name> <database_name>
###############################################################################
# Write the database-engine connection+role pair for one service tenant:
#   database/mariadb/config/<service>-<svc_ns>
#   database/mariadb/roles/<service>-<svc_ns>
# resolving the MariaDB root credential from the <mariadb_name> CR in <svc_ns>
# and issuing short-lived users with ALL PRIVILEGES on <database_name>. Fails
# loudly if that namespace's MariaDB root Secret is missing.
provision_service_tenant() {
  local service="$1"
  local svc_ns="$2"
  local mariadb_name="$3"
  local database_name="$4"

  # Keyed on the service namespace alone (see header): unique + collision-free,
  # and matched exactly by the <service>-db-dynamic templated policy, whose ACL
  # template resolves to the caller's own namespace — the generator's
  # ServiceAccount in exactly this namespace.
  local role_name="${service}-${svc_ns}"
  local config_name="${role_name}"

  log "Service   : ${service}"
  log "Service NS: ${svc_ns}"
  log "Role      : database/mariadb/roles/${role_name}"
  log "MariaDB   : ${mariadb_name}.${svc_ns}.svc:3306"
  log "Database  : ${database_name}"

  # Resolve the MariaDB root credential from the effective
  # spec.rootPasswordSecretKeyRef on the live MariaDB CR. mariadb-operator
  # webhook-defaults this field when the CR is created without it, so reading it
  # back from the live CR avoids hardcoding the operator's Secret-naming
  # convention.
  local root_secret_name root_secret_key
  root_secret_name="$(kubectl get mariadb "${mariadb_name}" -n "${svc_ns}" \
    -o 'jsonpath={.spec.rootPasswordSecretKeyRef.name}' 2>/dev/null || true)"
  if [[ -z "${root_secret_name}" ]]; then
    log "ERROR: could not resolve spec.rootPasswordSecretKeyRef.name on MariaDB '${mariadb_name}' in namespace '${svc_ns}'."
    exit 1
  fi
  root_secret_key="$(kubectl get mariadb "${mariadb_name}" -n "${svc_ns}" \
    -o 'jsonpath={.spec.rootPasswordSecretKeyRef.key}' 2>/dev/null || true)"
  if [[ -z "${root_secret_key}" ]]; then
    root_secret_key="password"
  fi

  local root_password_b64 root_password
  root_password_b64="$(kubectl get secret "${root_secret_name}" -n "${svc_ns}" \
    -o "jsonpath={.data.${root_secret_key}}" 2>/dev/null || true)"
  if [[ -z "${root_password_b64}" ]]; then
    log "ERROR: MariaDB root Secret '${root_secret_name}' (key '${root_secret_key}') not found or empty in namespace '${svc_ns}'."
    exit 1
  fi
  root_password="$(echo "${root_password_b64}" | base64 -d)"

  # Write the connection config. The password is piped via stdin
  # (password=-) so the cleartext root password never appears in a process
  # argument list. verify_connection=false because the MariaDB may not yet be
  # accepting connections when this runs on a fresh cluster; the first
  # credential issuance validates the connection instead.
  log "Writing database/mariadb/config/${config_name} ..."
  printf '%s' "${root_password}" | bao_exec_stdin bao write "database/mariadb/config/${config_name}" \
    plugin_name=mysql-database-plugin \
    connection_url="{{username}}:{{password}}@tcp(${mariadb_name}.${svc_ns}.svc:3306)/" \
    allowed_roles="${role_name}" \
    username=root \
    password=- \
    verify_connection=false
  log "Connection config written."

  # Write the role. creation_statements creates a short-lived MySQL user with
  # ALL PRIVILEGES on the service database; revocation_statements drops it at
  # lease end. The database identifier is backtick-quoted (escaped \` for the
  # bash double-quoted string) so a hyphenated database name is valid MySQL.
  local creation_stmt revocation_stmt
  creation_stmt="CREATE USER '{{name}}'@'%' IDENTIFIED BY '{{password}}'; GRANT ALL PRIVILEGES ON \`${database_name}\`.* TO '{{name}}'@'%';"
  revocation_stmt="DROP USER IF EXISTS '{{name}}'@'%';"

  log "Writing database/mariadb/roles/${role_name} (default_ttl=${DB_CREDS_DEFAULT_TTL}, max_ttl=${DB_CREDS_MAX_TTL}) ..."
  bao_exec bao write "database/mariadb/roles/${role_name}" \
    db_name="${config_name}" \
    creation_statements="${creation_stmt}" \
    revocation_statements="${revocation_stmt}" \
    default_ttl="${DB_CREDS_DEFAULT_TTL}" \
    max_ttl="${DB_CREDS_MAX_TTL}"
  log "Role written."
}

###############################################################################
# Main
###############################################################################
main() {
  log "=== Provisioning MariaDB database-engine tenant '${CP_NS}/${CP_NAME}' ==="
  log "Namespace : ${NAMESPACE}"
  log "BAO_ADDR  : ${BAO_ADDR}"

  # Fail loudly if the ControlPlane CR cannot be read. get_controlplane_field
  # below falls back to the projection defaults (openstack-db / keystone) on an
  # empty result, which is correct for a genuinely-unset field but would silently
  # mask a missing CR or an unreachable cluster — the script would then provision
  # a database-engine tenant against those defaults for a ControlPlane that does
  # not exist. Verifying existence up front keeps the empty-vs-absent fallback
  # unambiguous.
  if ! kubectl get controlplane "${CP_NAME}" -n "${CP_NS}" >/dev/null 2>&1; then
    log "ERROR: ControlPlane '${CP_NAME}' not found in namespace '${CP_NS}' (or the cluster is unreachable)."
    exit 1
  fi

  # --- Keystone (always) ------------------------------------------------------
  # Resolve the KEYSTONE SERVICE NAMESPACE: the namespace the MariaDB, the
  # credential generator, and its ServiceAccount all live in. It defaults to the
  # ControlPlane's own namespace, and is spec.services.keystone.namespace.name
  # when the Keystone service is placed in a namespace of its own. The MariaDB
  # cluster name and Keystone database name resolve from the live ControlPlane
  # spec; defaults mirror the c5c3 operator's projection defaults (openstack-db /
  # keystone) so a ControlPlane that leaves them unset resolves to the same values
  # the operator projects. The role name derivation MUST stay in sync with
  # dbDynamicRoleFor in operators/c5c3/internal/controller/reconcile_dbcredentials.go.
  local keystone_ns keystone_mariadb keystone_db
  keystone_ns="$(get_controlplane_field '{.spec.services.keystone.namespace.name}' "${CP_NS}")"
  keystone_mariadb="$(get_controlplane_field '{.spec.infrastructure.database.clusterRef.name}' 'openstack-db')"
  keystone_db="$(get_controlplane_field '{.spec.infrastructure.database.database}' 'keystone')"
  provision_service_tenant keystone "${keystone_ns}" "${keystone_mariadb}" "${keystone_db}"

  # --- Glance, Placement, Barbican, Neutron (shared managed database only) ---
  # Each of these services gets its OWN keystone-independent engine pair when the
  # ControlPlane declares spec.services.<service>. A service that declares a
  # dedicated database (spec.services.<service>.dedicatedBackingServices.database)
  # is Static-only — there is no engine role for a dedicated service DB — so it is
  # skipped. Its service namespace defaults to the ControlPlane's own namespace
  # and is spec.services.<service>.namespace.name when the service is placed in a
  # namespace of its own; its MariaDB is the shared managed cluster resolved in
  # THAT namespace, and its database name is the fixed '<service>' schema.
  #
  # MUST STAY IN SYNC (glance, placement): the <service>-<namespace> role name
  # below is the derivation glanceDBDynamicRoleFor / placementDBDynamicRoleFor
  # assert in
  # operators/c5c3/internal/controller/reconcile_<service>_dbcredentials.go, and
  # the fixed database name mirrors defaultGlanceDatabaseName /
  # defaultPlacementDatabaseName in
  # operators/c5c3/internal/controller/reconcile_<service>.go.
  #
  # MUST STAY IN SYNC (barbican): the barbican-<namespace> role name below is the
  # derivation barbicanDBDynamicRoleFor asserts in
  # operators/c5c3/internal/controller/reconcile_barbican_dbcredentials.go, and the
  # schema is the fixed 'barbican' one. This leg is the engine half of the barbican
  # onboarding, next to the presence-independent barbican-db auth role in
  # setup-auth.sh.
  #
  # MUST STAY IN SYNC (neutron): the neutron-<namespace> role name below is the
  # derivation neutronDBDynamicRoleFor asserts in
  # operators/c5c3/internal/controller/reconcile_neutron_dbcredentials.go, and the
  # schema is the fixed 'neutron' one. This leg is the engine half of the neutron
  # onboarding; the auth half is in setup-auth.sh, where the neutron-db role binds
  # the neutron-db-dynamic policy that grants exactly this creds path.
  local svc svc_ns svc_mariadb
  for svc in glance placement barbican neutron; do
    if [[ -z "$(get_controlplane_field "{.spec.services.${svc}}" '')" ]]; then
      log "ControlPlane declares no spec.services.${svc} — skipping the ${svc} database-engine tenant."
      continue
    fi
    if [[ -n "$(get_controlplane_field "{.spec.services.${svc}.dedicatedBackingServices.database}" '')" ]]; then
      log "ControlPlane declares a dedicated ${svc} database (Static-only) — skipping the ${svc} database-engine tenant."
      continue
    fi
    svc_ns="$(get_controlplane_field "{.spec.services.${svc}.namespace.name}" "${CP_NS}")"
    svc_mariadb="$(get_controlplane_field '{.spec.infrastructure.database.clusterRef.name}' 'openstack-db')"
    provision_service_tenant "${svc}" "${svc_ns}" "${svc_mariadb}" "${svc}"
  done

  log "=== Done ==="
}

main "$@"
