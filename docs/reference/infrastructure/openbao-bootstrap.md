---
title: OpenBao Bootstrap Procedure
quadrant: infrastructure
---

# OpenBao Bootstrap Procedure

Reference documentation for the OpenBao deployment and bootstrap procedure.
OpenBao is deployed as a 3-replica HA Raft cluster via FluxCD HelmRelease
into the `shared-services` namespace, then initialized and configured through
a sequence of idempotent bootstrap scripts. The
scripts provision secret engines, authentication backends, least-privilege policies, and
initial credentials required by downstream services.

## Architecture Overview

```text
┌─────────────────────────────────────────────────────────────────────┐
│                        Management Cluster                           │
│                                                                     │
│  ┌──────────────┐   ┌──────────────┐   ┌──────────────┐             │
│  │  openbao-0   │   │  openbao-1   │   │  openbao-2   │             │
│  │  (leader)    │◄──►  (follower)  │◄──►  (follower)  │             │
│  │  Raft peer   │   │  Raft peer   │   │  Raft peer   │             │
│  └──────┬───────┘   └──────────────┘   └──────────────┘             │
│         │                                                           │
│         │  TLS (openbao-tls Secret from cert-manager)               │
│         │                                                           │
│  ┌──────▼────────────────────────────────────────────────────────┐  │
│  │              ClusterSecretStore: openbao-cluster-store        │  │
│  │              (kubernetes/management auth, role eso-management)│  │
│  └──────┬────────────────────────────────────────────────────────┘  │
│         │                                                           │
│  ┌──────▼──────┐  ┌──────────────┐  ┌──────────────────────────┐    │
│  │ ExternalSec │  │ ExternalSec  │  │ ExternalSecret           │    │
│  │ {cp}-       │  │ {cp}-        │  │ (kind overlay shims:     │    │
│  │ keystone-   │  │ keystone-db- │  │  keystone-admin,         │    │
│  │ admin-creds │  │ credentials  │  │  mariadb-root-password)  │    │
│  └─────────────┘  └──────────────┘  └──────────────────────────┘    │
│   operator-projected per-ControlPlane          kind-only             │
└─────────────────────────────────────────────────────────────────────┘
```

The production stack (`deploy/eso/`, included by `deploy/flux-system/`) ships
**no** ExternalSecret resources — its kustomization renders only
`clustersecretstore.yaml`. The per-ControlPlane admin and database
ExternalSecrets are operator-projected, and the standalone `keystone-admin`,
`mariadb-root-password`, and `keystone-db` ExternalSecrets survive only as kind
overlay shims (`deploy/kind/infrastructure/`).

The `{cp}-keystone-admin-credentials` ExternalSecret is created
**per-ControlPlane** by the c5c3 operator's `reconcileAdminPassword`
sub-reconciler (default `controlplane-keystone-admin-credentials`), reading the
per-ControlPlane remote path `bootstrap/{ns}/{name}-keystone/admin`. Likewise
the `{cp}-keystone-db-credentials` ExternalSecret is **not** a static deploy-time
resource; it is created **per-ControlPlane** by the operator's
`reconcileDBCredentials` sub-reconciler (default
`controlplane-keystone-db-credentials`), reading the per-ControlPlane remote path
`openstack/keystone/{ns}/{name}/db`. Standalone Keystone instances (no
ControlPlane CR) instead reference a Secret named `keystone-db`; the **kind
overlay** ships a `keystone-db` ExternalSecret pinned to the default identity's
path (`deploy/kind/infrastructure/keystone-db-externalsecret.yaml`), while the
production stack ships none.

## Prerequisites

The following must be in place before running the bootstrap scripts:

| Prerequisite | Details |
| --- | --- |
| Kubernetes cluster | Management cluster with `kubectl` access configured |
| FluxCD | source-controller and helm-controller installed |
| cert-manager | Deployed and healthy (CRDs installed, webhook ready) |
| Base manifests | `kubectl apply -k deploy/flux-system/` completed successfully |
| Infrastructure manifests | `kubectl apply -k deploy/flux-system/infrastructure/` completed (includes openbao-tls Certificate) |
| OpenBao pods | All 3 replicas (`openbao-0`, `openbao-1`, `openbao-2`) running in `shared-services` namespace |
| CLI tools | `kubectl`, `jq` available on the operator workstation; `openssl` available inside the OpenBao pod (used for in-pod password generation) |

**Verification commands:**

```bash
# Confirm all 3 OpenBao pods are Running
kubectl get pods -n shared-services -l app.kubernetes.io/name=openbao

# Confirm TLS certificate is ready
kubectl get certificate openbao-tls -n shared-services

# Confirm cert-manager ClusterIssuer exists
kubectl get clusterissuer selfsigned-cluster-issuer
```

## Directory Layout

```text
deploy/
├── openbao/
│   ├── bootstrap/
│   │   ├── common.sh                   Shared functions (log, bao_exec) sourced by all scripts
│   │   ├── init-unseal.sh              Initialize and unseal the cluster
│   │   ├── setup-secret-engines.sh     Enable KV v2 (kv-v2/, barbican/), PKI, and MariaDB database engines
│   │   ├── setup-database-tenant.sh     Provision a per-ControlPlane DB engine connection + role
│   │   ├── setup-eso-tenant.sh          Provision a per-ControlPlane ESO identity (SA + mTLS cert + namespaced SecretStore)
│   │   ├── setup-auth.sh              Configure Kubernetes and AppRole auth
│   │   ├── setup-policies.sh           Apply all HCL access control policies
│   │   └── write-bootstrap-secrets.sh  Generate and seed initial credentials
│   └── policies/
│       ├── eso-management.hcl          ESO policy for management cluster
│       ├── eso-control-plane.hcl       ESO policy for control-plane cluster
│       ├── eso-hypervisor.hcl          ESO policy for hypervisor cluster
│       ├── eso-storage.hcl             ESO policy for storage cluster
│       ├── eso-tenant.hcl              Per-tenant ESO identity (namespace-templated Keystone key/bootstrap access)
│       ├── push-ceph-keys.hcl          PushSecret policy for Ceph keys
│       ├── ci-cd-provisioner.hcl       CI/CD pipeline provisioning policy
│       ├── keystone-db-dynamic.hcl     Per-tenant dynamic DB credential read policy
│       ├── glance-db-dynamic.hcl       Per-tenant dynamic Glance DB credential read policy
│       ├── placement-db-dynamic.hcl    Per-tenant dynamic Placement DB credential read policy
│       ├── neutron-db-dynamic.hcl      Per-tenant dynamic Neutron DB credential read policy
│       ├── barbican-secretstore.hcl    Barbican secret-store policy on the KV v2 mount barbican/
│       └── pki-issuer.hcl             cert-manager PKI issuing policy
├── eso/
│   ├── kustomization.yaml              Kustomize entrypoint (renders ONLY the ClusterSecretStore)
│   └── clustersecretstore.yaml         ClusterSecretStore for OpenBao
├── kind/
│   └── infrastructure/                 kind-overlay-only shims (standalone flows)
│       ├── keystone-admin-externalsecret.yaml        Secret keystone-admin
│       ├── mariadb-root-password-externalsecret.yaml Secret mariadb-root-password
│       ├── keystone-db-externalsecret.yaml           Secret keystone-db
│       ├── placement-db-externalsecret.yaml          Secret placement-db
│       ├── neutron-db-externalsecret.yaml            Secret neutron-db
│       └── placement-nip-io-tls-certificate.yaml     Certificate placement-nip-io-tls
└── flux-system/
    ├── releases/
    │   └── openbao.yaml                HelmRelease for OpenBao HA cluster
    └── infrastructure/
        └── openbao-tls-cert.yaml       cert-manager Certificate for TLS
```

**Note:** The static `deploy/eso/externalsecrets/` directory has been removed.
The production ESO kustomization now renders only `clustersecretstore.yaml`, so
the production stack ships **no** ExternalSecret resources. The per-ControlPlane
admin and database ExternalSecrets are projected by the c5c3 operator, and the
`keystone-admin`, `mariadb-root-password`, and `keystone-db` ExternalSecrets
survive only as kind overlay shims under `deploy/kind/infrastructure/`.

**Note:** The flat OpenBao path `openstack/keystone/db` is no longer seeded —
Keystone database credentials live at per-control-plane paths
`openstack/keystone/{ns}/{name}/db`, and the production deploy stack ships no
`keystone-db` ExternalSecret. For each ControlPlane CR the c5c3 operator's
`reconcileDBCredentials` sub-reconciler creates an ExternalSecret named
`{controlplane.Name}-keystone-db-credentials` reading that ControlPlane's own
path. For standalone Keystone instances the **kind overlay** additionally ships
a `keystone-db` ExternalSecret pinned to the default identity's path
(`deploy/kind/infrastructure/keystone-db-externalsecret.yaml`); outside kind a
standalone instance has to materialise the Secret itself.

## Script Execution Order

The bootstrap scripts **must** be executed in the following order. Each script depends
on the successful completion of the previous step:

```text
1. init-unseal.sh           Initialize Shamir keys, unseal all replicas
       │
       ▼
2. setup-secret-engines.sh  Enable KV v2 and PKI engines
       │
       ▼
3. setup-auth.sh            Configure Kubernetes auth + AppRole
       │
       ▼
4. setup-policies.sh        Apply 12 HCL least-privilege policies
       │
       ▼
5. write-bootstrap-secrets.sh  Generate and seed initial passwords
```

**Dependency rationale:**

- `init-unseal.sh` must run first because OpenBao is sealed after initial deployment
  and all subsequent operations require an unsealed vault with a root token.
- `setup-secret-engines.sh` enables the KV v2 engine that `write-bootstrap-secrets.sh`
  writes to, so engines must exist before secrets can be written.
- `setup-auth.sh` creates the auth mounts and roles that `setup-policies.sh` links
  policies to, though technically policies can be written before auth configuration.
- `setup-policies.sh` must run before `write-bootstrap-secrets.sh` to ensure access
  control is in place before credentials are seeded.

## Environment Setup

All bootstrap scripts execute `bao` CLI commands inside the `openbao-0` pod via
`kubectl exec`. No direct network connection to OpenBao is required from the operator
workstation.

### Required Environment Variables

| Variable | Required By | Description |
| --- | --- | --- |
| `BAO_TOKEN` | All scripts except `init-unseal.sh` | Root token obtained from `init-unseal.sh` output |

The `init-unseal.sh` script does not require `BAO_TOKEN` — it produces the root token
as output. All subsequent scripts require the root token to be set as `BAO_TOKEN` in the
shell environment.

### Internal Script Variables

Each script sets the following variables internally via `kubectl exec` environment
injection:

| Variable | Value | Purpose |
| --- | --- | --- |
| `BAO_ADDR` | `https://127.0.0.1:8200` | OpenBao API address (pod-local loopback) |
| `VAULT_CACERT` | `/openbao/tls/ca.crt` | CA certificate path for TLS verification |
| `VAULT_CLIENT_CERT` | `/openbao/client-tls/tls.crt` | Client certificate the in-pod `bao` CLI presents on every API call. Required because `tls_require_and_verify_client_cert = true` is enabled on the listener; without it the TLS handshake fails before any application-layer auth runs. |
| `VAULT_CLIENT_KEY` | `/openbao/client-tls/tls.key` | Matching private key for `VAULT_CLIENT_CERT`. Both files are mounted from the `openbao-client-tls` Secret at `/openbao/client-tls`. |

Defaults are set in `deploy/openbao/bootstrap/common.sh` and forwarded by every
`bao`-invoking wrapper (`bao_exec`, `bao_exec_stdin` in `common.sh`, the private
`kube_exec` and `kube_exec_stdin` in `init-unseal.sh`, and `openbao_kube_exec`
and `openbao_kube_exec_stdin` in `hack/deploy-infra.sh`). The `_stdin` variants
add `kubectl exec -i` so secret material — an unseal share, a policy body — is
piped in rather than passed as an argument, where it would land both in the API
server's audit log (as a `command=` query parameter) and in
`/proc/<pid>/cmdline` inside the container. Operators wishing to run `bao` from
outside the pod must export both vars to a copy of the client keypair extracted
from the `openbao-client-tls` Secret.

### Running the Full Bootstrap

```bash
# Step 1: Initialize and unseal (produces root token)
cd deploy/openbao/bootstrap
./init-unseal.sh

# Retrieve the root token from the Kubernetes Secret
export BAO_TOKEN=$(kubectl get secret openbao-init-keys -n shared-services \
  -o jsonpath='{.data.init-output}' | base64 -d | jq -r '.root_token')

# Step 2-5: Run remaining scripts in order
./setup-secret-engines.sh
./setup-auth.sh
./setup-policies.sh
./write-bootstrap-secrets.sh
```

> **Note:** For a kind-only walkthrough that uses the same `BAO_TOKEN` extraction to open
> the OpenBao web UI, see
> [Quick Start (Extended) — Step 4b: Open the OpenBao UI](../../quick-start-extended.md#step-4b-openbao-ui)
>. The UI is disabled in the production flux-system overlay.

## Script Reference

### init-unseal.sh

**Purpose:** Initialize OpenBao with Shamir secret sharing and unseal all 3 replicas.

**File:** `deploy/openbao/bootstrap/init-unseal.sh`

| Parameter | Value |
| --- | --- |
| Key shares | 5 |
| Key threshold | 3 |
| Output format | JSON |
| Target pods | `openbao-0`, `openbao-1`, `openbao-2` |
| Namespace | `shared-services` |

**Behavior:**

1. Checks if OpenBao is already initialized by running `bao status -format=json` on `openbao-0`
   and parsing the `initialized` field from the JSON output.
2. If not initialized: runs `bao operator init -key-shares=5 -key-threshold=3
   -format=json` and stores the full JSON output (containing unseal keys and root
   token) as a Kubernetes Secret `openbao-init-keys` in the `shared-services` namespace.
3. If already initialized: retrieves existing unseal keys from the `openbao-init-keys`
   Secret.
4. Iterates over all 3 pods (`openbao-0`, `openbao-1`, `openbao-2`) and unseals each
   by providing 3 unseal keys (meeting the threshold).
5. Skips unsealing for pods that are already unsealed.

**Idempotency:** Runs `bao status -format=json` on `openbao-0` (ignoring the exit
code via `|| true`) and uses `jq -e '.initialized == true'` to reliably distinguish
an uninitialized cluster from an initialized-but-sealed one (both return exit code
`2`). If already initialized and unsealed, the script logs a message and exits cleanly.

**Output:** The `openbao-init-keys` Kubernetes Secret contains:

| Key | Description |
| --- | --- |
| `init-output` | Full JSON output from `bao operator init` including `unseal_keys_b64` array and `root_token` |

**Production security:** After bootstrap is complete and the cluster is verified
operational, the `openbao-init-keys` Secret should be exported to secure offline
storage (e.g., hardware security module, air-gapped backup) and deleted from the
cluster. The unseal keys and root token stored in this Secret grant full control
over the vault — leaving them in-cluster increases the blast radius of a
Kubernetes namespace compromise. Re-sealing and unsealing after pod restarts
requires the exported keys, so ensure they are recoverable before deletion.

### setup-secret-engines.sh

**Purpose:** Enable the KV version 2, PKI, and MariaDB `database` secret engines, plus
the Barbican KV v2 mount.

**File:** `deploy/openbao/bootstrap/setup-secret-engines.sh`

**Requires:** `BAO_TOKEN` environment variable.

| Engine | Mount Path | Configuration |
| --- | --- | --- |
| KV v2 | `kv-v2/` | `version=2` |
| PKI | `pki/` | `max-lease-ttl=87600h` (10 years) |
| database | `database/mariadb/` | mount only; per-tenant connections/roles are added later by `setup-database-tenant.sh` |
| KV v2 | `barbican/` | `version=2`, enabled by `enable_barbican_kv`; the mount for the Barbican brownfield secret-store leg, where Barbican attaches to this shared instance instead of a dedicated one |

**Idempotency:** Before enabling each engine, the script checks `bao secrets list
-format=json` for the mount path. If the path already exists, the engine enable is
skipped with a log message.

**Database engine:** the `database` engine is only mounted here — no connection or
role is written, because the managed MariaDB instances do not exist at bootstrap
time. Each managed ControlPlane's connection and per-tenant role are provisioned
later by `setup-database-tenant.sh` once its MariaDB is Ready. The engine issues
short-lived, auto-revoked Keystone service-DB credentials at
`database/mariadb/creds/keystone-{namespace}` (keyed on the namespace alone —
one ControlPlane per namespace makes it a unique, collision-free tenant key).

**Barbican mount:** `enable_barbican_kv` enables a second KV v2 engine at `barbican/`,
the path Barbican's `vault_plugin` reads and writes through castellan. It is the
brownfield half of the Barbican secret store: the same mount name, policy name, and
AppRole role name that the proving `OpenBaoCluster` self-initializes on a dedicated
instance, so the two only differ in which instance Barbican points at. Like every other
engine here the enable is guarded by a `bao secrets list` check.

### setup-database-tenant.sh

**Purpose:** Provision the per-tenant `database` engine connection and role for one
managed ControlPlane's per-service DB users — Keystone always, and Glance and
Placement each when they share the managed database.

**File:** `deploy/openbao/bootstrap/setup-database-tenant.sh`

**Requires:** `BAO_TOKEN` environment variable; the tenant's MariaDB must be Ready.

**Usage:** `setup-database-tenant.sh <namespace> <controlplane>`

It resolves each service leg's MariaDB name, database name, and root credential
from the live ControlPlane and MariaDB CRs — independently and in that service's
own namespace, so Glance's engine plumbing is keystone-independent — then writes
one connection+role pair per leg. The **Keystone** leg is **always** provisioned:

| Object | Path |
| --- | --- |
| Connection | `database/mariadb/config/keystone-{namespace}` |
| Role | `database/mariadb/roles/keystone-{namespace}` |

The **Glance** leg is provisioned **only** when the live ControlPlane declares
`spec.services.glance` on the shared managed database; a Glance that declares a
dedicated database (`spec.services.glance.dedicatedBackingServices.database`) is
`Static`-only and is **skipped** here. Its glance service namespace defaults to
the ControlPlane's own:

| Object | Path |
| --- | --- |
| Connection | `database/mariadb/config/glance-{namespace}` |
| Role | `database/mariadb/roles/glance-{namespace}` |

Each role's `creation_statements` create a short-lived MySQL user with `ALL
PRIVILEGES` on that service's database; `revocation_statements` drop it at lease
end. `default_ttl` (48h) and `max_ttl` (72h) are tunable via `DB_CREDS_DEFAULT_TTL`
/ `DB_CREDS_MAX_TTL`; `default_ttl` stays a full day above the operator's
ExternalSecret refresh interval (24h) so the operator has a wide window to roll
pods onto a fresh credential before the previous, still-in-use lease is revoked —
long enough that a stalled rollout pages on-call before it can become an outage.
Each role name is keyed on its own service namespace alone (`{namespace}` above is
that service's namespace) and stays in sync with `dbDynamicRoleFor` /
`glanceDBDynamicRoleFor` in the c5c3 operator (`reconcile_dbcredentials.go` /
`reconcile_glance_dbcredentials.go`). Config and role writes are upserts, so
re-running is idempotent.

### setup-eso-tenant.sh

**Purpose:** Provision the in-cluster half of a **standalone** (non-ControlPlane)
Keystone/Horizon namespace's per-tenant OpenBao identity — the objects that let
that namespace's ExternalSecrets and PushSecrets reach OpenBao as the
`eso-tenant` role instead of the shared cluster identity. It is the **manual**
onboarding path for standalone CRs; a ControlPlane of **any** mode (Managed or
External) never needs it, because the c5c3 operator provisions the same
ServiceAccount, Certificate, and SecretStore itself and defaults the control
plane onto the store (`reconcileESOTenantStore`). It is the ESO counterpart to
`setup-database-tenant.sh`.

**File:** `deploy/openbao/bootstrap/setup-eso-tenant.sh`

**Requires:** `kubectl` access to the tenant's cluster; cert-manager and the
`openbao-ca-issuer` ClusterIssuer. It does **not** talk to OpenBao directly —
the OpenBao side (the `eso-tenant` role and policy) is created once at bootstrap
by `setup-auth.sh` / `setup-policies.sh`.

**Usage:** `setup-eso-tenant.sh <namespace>`

It fails loudly when the namespace is absent, then idempotently applies into the
tenant namespace:

| Object | Name | Purpose |
| --- | --- | --- |
| ServiceAccount | `eso-tenant-auth` | the identity the SecretStore presents to OpenBao |
| Certificate (cert-manager) | `eso-tenant-client-tls` | the mTLS client certificate (issued by ClusterIssuer `openbao-ca-issuer`); its Secret carries `tls.crt`/`tls.key` and `ca.crt` |
| SecretStore | `openbao-tenant-store` | the namespaced store selected via `spec.secretStoreRef` |

The SecretStore authenticates as the `eso-tenant` role with the
`eso-tenant-auth` ServiceAccount and sources its client cert, key, and CA from
the `eso-tenant-client-tls` Secret (same namespace). After the SecretStore
reports `Ready`, set the **standalone** Keystone/Horizon CR's
`spec.secretStoreRef` to `{kind: SecretStore, name: openbao-tenant-store}` to
route it through the per-tenant identity. On a cluster bootstrapped before this
feature, re-run `setup-auth.sh` / `setup-policies.sh` first so the `eso-tenant`
role and policy exist — otherwise ESO's pushes 403 and `FernetKeysReady` /
`CredentialKeysReady` degrade. Setting `spec.secretStoreRef` on a **ControlPlane**
is the opt-out override for a self-managed store, not an onboarding step. See the
[multi-tenant deployment guide](../../guides/multi-tenant-deployment.md#per-controlplane-secret-stores-and-openbao-identities)
for the full procedure.

### setup-auth.sh

**Purpose:** Configure Kubernetes authentication for 4 cluster contexts and AppRole
authentication for CI/CD pipelines and the Barbican secret store.

**File:** `deploy/openbao/bootstrap/setup-auth.sh`

**Requires:** `BAO_TOKEN` environment variable.

#### Kubernetes Auth Mounts

| Mount Path | Cluster | ESO Role | Bound SA | Bound NS | Policy | TTL | Max TTL |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `kubernetes/management` | Management | `eso-management` | `external-secrets` | `external-secrets` | `eso-management` | 1h | 4h |
| `kubernetes/control-plane` | Control Plane | `eso-control-plane` | `external-secrets` | `external-secrets` | `eso-control-plane` | 1h | 4h |
| `kubernetes/hypervisor` | Hypervisor | `eso-hypervisor` | `external-secrets` | `external-secrets` | `eso-hypervisor` | 1h | 4h |
| `kubernetes/storage` | Storage | `eso-storage` | `external-secrets` | `external-secrets` | `eso-storage` | 1h | 4h |

Each Kubernetes auth mount creates a role named `eso-<cluster>` that binds to the
`external-secrets` service account in the `external-secrets` namespace. The role is
linked to the corresponding `eso-<cluster>` policy.

The management mount additionally carries a `keystone-db`, a `glance-db`, a
`placement-db`, and a `neutron-db` role, each bound to a fixed per-ControlPlane
ServiceAccount (`keystone-db-creds` / `glance-db-creds` / `placement-db-creds` /
`neutron-db-creds`, any namespace) and linked to its own dynamic-credential
policy (`keystone-db-dynamic` / `glance-db-dynamic` / `placement-db-dynamic` /
`neutron-db-dynamic`). The c5c3 operator's per-ControlPlane
`VaultDynamicSecret` generators authenticate with them to read short-lived DB
credentials at `database/mariadb/creds/keystone-{namespace}`,
`database/mariadb/creds/glance-{namespace}`,
`database/mariadb/creds/placement-{namespace}`, and
`database/mariadb/creds/neutron-{namespace}` respectively. All four roles
deliberately bind `namespaces="*"` so any ControlPlane namespace may
authenticate; the fixed SA name is what tells them apart (a `glance-db-creds`
token can never read a Keystone creds path, or vice versa), and cross-tenant
isolation is enforced by each policy, which templates the readable path to the caller's own
`service_account_namespace` (an exact match — a token minted in one namespace
cannot read another namespace's path).

Unlike the `eso-<cluster>` roles, the `keystone-db`, `glance-db`,
`placement-db`, and `neutron-db` token TTLs are pinned to the database engine's
`max_ttl` (`DB_CREDS_MAX_TTL`, 72h): OpenBao
revokes a dynamic-secret lease together with the auth token that minted it, so a
token shorter than the lease silently caps the effective credential lifetime at
the token's — with an eso-style 1h token, every issued DB credential died after
~1h while the ExternalSecret refresh only re-mints every 24h, dropping the
ephemeral MySQL user under a running service. The longer-lived token is bounded by
the read-only `keystone-db-dynamic` / `glance-db-dynamic` /
`placement-db-dynamic` / `neutron-db-dynamic` policy.

| Mount Path | Role | Bound SA | Bound NS | Policy | TTL | Max TTL |
| --- | --- | --- | --- | --- | --- | --- |
| `kubernetes/management` | `keystone-db` | `keystone-db-creds` | `*` | `keystone-db-dynamic` | 72h | 72h |
| `kubernetes/management` | `glance-db` | `glance-db-creds` | `*` | `glance-db-dynamic` | 72h | 72h |
| `kubernetes/management` | `placement-db` | `placement-db-creds` | `*` | `placement-db-dynamic` | 72h | 72h |
| `kubernetes/management` | `neutron-db` | `neutron-db-creds` | `*` | `neutron-db-dynamic` | 72h | 72h |
| `kubernetes/management` | `eso-tenant` | `eso-tenant-auth` | `*` | `eso-tenant` | 1h | 4h |

The management mount also carries an `eso-tenant` role — the per-ControlPlane
ESO identity a namespaced `SecretStore` (created per tenant by
`setup-eso-tenant.sh`) authenticates with. Like `keystone-db` it binds
`namespaces="*"` with a fixed SA name (`eso-tenant-auth`); the cross-tenant
boundary is enforced by the `eso-tenant` policy, which templates every readable
and writable path to the caller's own `service_account_namespace`, so a tenant
token confined by it can only reach its own namespace's Keystone key and
bootstrap material. `token_max_ttl=4h` caps renewal so a leaked tenant token
cannot be renewed indefinitely.

**Note:** The management cluster mount is fully configured — the script explicitly writes
`auth/kubernetes/management/config` with the in-cluster Kubernetes API endpoint and CA
certificate. This requires the OpenBao service account to have the `system:auth-delegator`
ClusterRole (created by the Helm chart when `server.authDelegator.enabled=true`, the
default). The control-plane, hypervisor, and storage cluster mounts have roles created but
their Kubernetes host and CA configuration is deferred until those clusters are provisioned.

#### AppRole Auth

| Mount Path | Role | Policy | Token TTL | Max TTL | Secret ID TTL |
| --- | --- | --- | --- | --- | --- |
| `approle/` | `provisioner` | `ci-cd-provisioner` | 1h | 4h | `8760h` (1 year) |
| `approle/` | `barbican` | `barbican-secretstore` | 1h | 4h | `720h` (30 days) |

The `barbican` role is the identity Barbican's `vault_plugin` logs in with on the
brownfield leg. Minting a secret ID stays out of bootstrap: for a managed deployment that
is runtime work owned by the barbican-operator, and the brownfield e2e mints one at test
time with the root-token `bao` exec it already has.

A secret ID carries no use count and no CIDR bound, so `secret_id_ttl` is the only thing
that bounds a leaked one. On the `barbican` role it is 30 days rather than the year the
older `provisioner` role uses, matching the `barbican` role of the proving instance
([OpenBao Proving Instance](./infrastructure-manifests.md#openbao-proving-instance)).

**Re-minting the barbican secret ID.**
Nothing re-mints the secret ID automatically yet — that becomes runtime work of the
barbican-operator. Until then the 30-day TTL is a fuse on a hand-minted credential, and it
burns silently: Barbican's `vault_plugin` holds one process-global AppRole session, so an
expired secret ID surfaces as every secret-store operation failing at once, with no metric
that warns ahead of it. Re-mint before the 30 days elapse, and after any Barbican
deployment that has been running against a hand-minted credential for a month:

```bash
# BAO_TOKEN must be exported first — see Running the Full Bootstrap above.
cd deploy/openbao/bootstrap
source ./common.sh

# The role ID is stable across re-mints; only the secret ID rotates.
bao_exec bao read -field=role_id auth/approle/role/barbican/role-id

# Mint a fresh secret ID straight into the Secret the deployment reads it from.
# Replace <secret> and <namespace> with the Secret backing
# vault_plugin.approle_secret_id. -force is required because the write carries
# no data.
kubectl create secret generic <secret> \
  --namespace <namespace> \
  --from-literal=approle_secret_id="$(bao_exec bao write \
    -field=secret_id -force auth/approle/role/barbican/secret-id)" \
  --dry-run=client -o yaml | kubectl apply -f -
```

The secret ID stays inside that pipeline on purpose. `bao_exec` is a `kubectl exec`, so
printing the credential first would put it in the terminal's scrollback, in a recorded
session, or — if the procedure is ever wrapped in automation — in job logs that outlive
the 30-day TTL by a wide margin and are readable by everyone with access to them. The
credential carries no use count and no CIDR bound, so that TTL is the only thing bounding
a leaked copy.

Restart the Barbican API pods afterwards: the plugin reads the credential once at process
start.

To see how much of the fuse is left, list the role's live secret-ID accessors and look one
up — the response carries its `expiration_time`:

```bash
bao_exec bao list auth/approle/role/barbican/secret-id

bao_exec bao write auth/approle/role/barbican/secret-id-accessor/lookup \
  secret_id_accessor=<accessor-from-the-list-above>
```

Destroy the accessor of the credential the restarted pods no longer use, so a copy of the
old secret ID cannot log in for the rest of its 30 days:

```bash
bao_exec bao write auth/approle/role/barbican/secret-id-accessor/destroy \
  secret_id_accessor=<accessor-of-the-replaced-secret-id>
```

`secret_id_num_uses` is deliberately left unset on the role. castellan's Vault key manager
re-logs in whenever its cached token ages out (`token_ttl` is 1h), so a use cap would
expire the credential mid-operation rather than bound it.

**Idempotency:** Before enabling each auth method, the script checks `bao auth list
-format=json` for the mount path. If the path already exists, the auth enable is
skipped. Role creation uses `bao write` which is an upsert operation (creates or
updates).

### setup-policies.sh

**Purpose:** Apply all HCL access control policies from the `policies/` directory.

**File:** `deploy/openbao/bootstrap/setup-policies.sh`

**Requires:** `BAO_TOKEN` environment variable.

The script iterates over all `.hcl` files in `deploy/openbao/policies/` and applies
each one via `bao policy write <name> -` (reading from stdin). The policy name is
derived from the filename without the `.hcl` extension. Because the loop globs
`*.hcl`, adding a new policy file (such as `eso-tenant.hcl`) needs no script
change. The `KUBERNETES_MANAGEMENT_ACCESSOR` placeholder in the templated
policies is substituted with the live `kubernetes/management` auth-mount accessor
at apply time.

Five policies are **namespace-templated** rather than statically scoped, so a
single policy backs every tenant while confining each token to its own namespace:

- `keystone-db-dynamic` — read on the caller's own dynamic Keystone DB-credential path.
- `glance-db-dynamic` — read on the caller's own dynamic Glance DB-credential path.
- `placement-db-dynamic` — read on the caller's own dynamic Placement DB-credential path.
- `neutron-db-dynamic` — read on the caller's own dynamic Neutron DB-credential path.
- `eso-tenant` — the per-ControlPlane ESO identity and the **sole write path**
  for per-ControlPlane Keystone key material: read on the caller's own
  `openstack/keystone/{ns}/*` and `bootstrap/{ns}/*` subtrees, and
  create/update/read/delete on that namespace's fernet-keys, credential-keys,
  admin bootstrap, admin application-credential, and service-account backup
  paths. Every path is scoped to the caller's own `service_account_namespace`,
  so a tenant token cannot touch another tenant's Keystone key material. It
  deliberately grants **no** `infrastructure/*` access — the static
  infrastructure ExternalSecrets stay on the shared cluster store.

**Idempotency:** `bao policy write` is an upsert operation — it creates a new policy
or overwrites an existing one with the same name. Re-running with the same policy
content is inherently idempotent.

### write-bootstrap-secrets.sh

**Purpose:** Generate cryptographically secure passwords and seed initial credentials
into the KV v2 secret engine.

**File:** `deploy/openbao/bootstrap/write-bootstrap-secrets.sh`

**Requires:** `BAO_TOKEN` environment variable.

| KV v2 Path | Secret Keys | Description |
| --- | --- | --- |
| `kv-v2/bootstrap/<namespace>/<keystone>/admin` | `password` | Keystone admin user password, scoped per **Managed-mode** ControlPlane. One entry per `KORC_CONTROLPLANES` identity; the default `openstack/controlplane` seeds `kv-v2/bootstrap/openstack/controlplane-keystone/admin`. |
| `kv-v2/bootstrap/<namespace>/<horizon>/secret-key` | `secret-key` | Horizon Django `SECRET_KEY`, scoped per **Managed-mode** ControlPlane. One entry per `KORC_CONTROLPLANES` identity; the default seeds `kv-v2/bootstrap/openstack/controlplane-horizon/secret-key`, read by the kind-only `horizon-secret-key` ExternalSecret. |
| `kv-v2/infrastructure/mariadb` | `root-password` | MariaDB root password |
| `kv-v2/bootstrap/openstack/garage/admin-token` | `token` | Garage Admin API token — the operator authenticates to Garage's Admin API with it. Read by the kind-only `garage-admin-token` ExternalSecret in `shared-services` (`GarageCluster.spec.admin.adminTokenSecretRef`). Seeded unconditionally (Garage is base infrastructure, not per-ControlPlane). The `openstack` path segment names the consuming tenant, not the workload's namespace: Garage itself runs in `shared-services`. |
| `kv-v2/bootstrap/openstack/garage/s3-credentials` | `access-key-id`, `secret-access-key` | Garage S3 key pair, **imported** by the `GarageKey` (not minted by the operator). `access-key-id` is `GK`-prefixed (`@generate-gk`), as Garage requires. Read by two kind-only `garage-s3-credentials` ExternalSecrets on this one path: the copy in `shared-services` feeds the `GarageKey` import, the copy in `openstack` serves the Glance consumers, which resolve `credentialsSecretRef` in their own namespace. |
| `kv-v2/bootstrap/openstack/openbao-instance/unseal-key` | `key` | Static unseal key of the proving `OpenBaoCluster` (kind-only). Seeded unconditionally and **never rotated**: `write_secret_if_missing` guards the seed side and the consuming ExternalSecret's `refreshPolicy: CreatedOnce` guards the other, because a static seal whose key material changes seals the instance permanently. |
| `kv-v2/openstack/keystone/openstack/standalone/db` | `username`, `password` | Static Keystone DB credential for **standalone** (non-ControlPlane) Keystone demos only (username is `keystone`), read by the kind-only `keystone-db` ExternalSecret. Brownfield-only. |

**Mode note:** `KORC_CONTROLPLANES` lists **Managed-mode** ControlPlane
identities only. An **External-mode** ControlPlane
(`spec.services.keystone.mode: External`) is **never** seeded here — its admin
password is owned out-of-band in the user-supplied `passwordSecretRef` Secret, the
c5c3 operator's `reconcileAdminPassword` short-circuits without reading any
bootstrap path, and `services.horizon` is rejected in External mode. Listing an
External identity would write a generated admin password that nothing reads. See
[OpenBao paths per ControlPlane mode](#openbao-paths-per-controlplane-mode).

**Retired (#439):** the stage-(a) per-ControlPlane static DB seed
`kv-v2/openstack/keystone/{ns}/{name}/db` is **no longer written**. Managed-mode
ControlPlanes now draw short-lived, engine-issued credentials from the MariaDB
`database` engine (`database/mariadb/creds/keystone-{ns}`), so no long-lived
static DB password is seeded at rest. Only the single standalone credential above
remains.

**Password generation:** Each password is generated **inside the OpenBao pod** using
`openssl rand -base64 32` via `sh -c` within `kubectl exec`, producing a 32-byte
(256-bit) cryptographically secure random value encoded as base64 (44 characters).
Generating passwords inside the pod prevents cleartext passwords from appearing in
host `/proc/<pid>/cmdline` process argument lists.

**Generated-value markers:** A key whose value is the marker `@generate` is replaced
with a freshly generated random value in-pod (as above). The `@generate-gk` marker is a
variant for Garage S3 access-key IDs: it emits the literal prefix `GK` followed by 24 hex
characters (12 random bytes), the format Garage's key import requires. Both markers keep
the cleartext off the host process argument list.

**Security:** Generated passwords are never echoed to stdout or stderr and never
appear as command-line arguments visible to host process listings. The script
outputs only status messages (e.g., `Writing kv-v2/bootstrap/openstack/controlplane-keystone/admin...` or
`Skipping kv-v2/bootstrap/openstack/controlplane-keystone/admin (already exists)`).

**Idempotency:** Before writing each secret, the script checks `bao kv get` for the
path. If the secret already exists, the write is skipped to prevent overwriting
existing credentials. This is critical — overwriting would create a mismatch between
the credentials stored in OpenBao and those already provisioned to consuming services.

**ESO PushSecret adoption marker:** After seeding, the script stamps
`custom_metadata.managed-by=external-secrets` onto the per-ControlPlane admin
paths via `bao kv metadata put`. ESO refuses to push to a pre-existing secret
whose metadata lacks this marker (it fails with "secret not managed by
external-secrets"), so without the stamp the scheduled admin-password rotation
backup PushSecret (per-CR RemoteKey `bootstrap/{namespace}/{keystone}/admin`)
could never mirror a rotated password back into OpenBao. The stamp runs
unconditionally — it also adopts paths a prior deploy created without the
marker — and `bao kv metadata put` touches only metadata, leaving the stored
password versions untouched.

## HCL Access Control Policies

Twelve HCL policies enforce least-privilege access for each consumer type. All policy
paths under the KV v2 engine include the `data/` prefix, which is required by the
OpenBao/Vault KV v2 API for read and write operations.

### ESO Policies (Read-Only)

These policies grant the External Secrets Operator read-only access to pull secrets
from OpenBao into Kubernetes Secrets.

| Policy | Paths | Capabilities |
| --- | --- | --- |
| `eso-management` | `kv-v2/data/bootstrap/*`, `kv-v2/data/infrastructure/*` | `read` |
| `eso-control-plane` | `kv-v2/data/bootstrap/*`, `kv-v2/data/infrastructure/*`, `kv-v2/data/ceph/*` | `read` |
| `eso-hypervisor` | `kv-v2/data/ceph/client-nova`, `kv-v2/data/openstack/nova/compute-*` | `read` |
| `eso-storage` | `kv-v2/data/ceph/*` | `read`, `create`, `update` |

**Note:** `eso-storage` is the only ESO policy with write capabilities. This allows
the Ceph cluster to write its own keys back to OpenBao via PushSecret.

**Note:** `eso-hypervisor` has the narrowest scope — it can only access the specific
Ceph client key for Nova and Nova compute configuration, not broader secret paths.

### Operational Policies

| Policy | Paths | Capabilities | Purpose |
| --- | --- | --- | --- |
| `eso-tenant` | `kv-v2/{data,metadata}/openstack/keystone/{ns}/…` (fernet-keys, credential-keys, admin app-credential, service-accounts) and `kv-v2/{data,metadata}/bootstrap/{ns}/…/admin`, plus `read` on `kv-v2/data/openstack/keystone/{ns}/*` and `kv-v2/data/bootstrap/{ns}/*` | `create`, `update`, `read`, `delete` | Per-tenant **sole write path** for per-ControlPlane Keystone key material (fernet/credential-key backups, admin bootstrap, admin Application Credential, service-account passwords). Every path is namespace-templated to the caller's own `service_account_namespace` (bound to the `eso-tenant` role), so a tenant token cannot reach another tenant's key material. |
| `push-ceph-keys` | `kv-v2/data/ceph/*` | `create`, `update`, `read` | PushSecret for Ceph client keys |
| `ci-cd-provisioner` | `kv-v2/data/*` (create/update/read), `kv-v2/metadata/*` (read/list) | `create`, `update`, `read`, `list` | CI/CD pipeline secret provisioning |
| `pki-issuer` | `pki/issue/*`, `pki/sign/*` | `create`, `update` | cert-manager PKI certificate issuing |
| `barbican-secretstore` | `barbican/data/*` (plus `barbican/metadata/*` without `delete`) | `create`, `read`, `update`, `delete`, `list` | Barbican's secret store on the shared instance, bound to the `barbican` AppRole role. The consumer is Barbican's `vault_plugin` via castellan, which speaks the Vault-compatible HTTP API that OpenBao keeps compatible. No `delete` on `metadata/`: in KV v2 that verb permanently destroys every version of a secret, the only in-store recovery path tenant key material has, while castellan deletes through `data/*` where the delete is a recoverable soft delete. Same grants as the `barbican-secretstore` policy the proving `OpenBaoCluster` self-initializes. |
| `keystone-db-dynamic` | <code v-pre>database/mariadb/creds/keystone-{{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}</code> | `read` | Per-tenant dynamic Keystone DB credential reads (bound to the `keystone-db` role). The path is scoped by ACL identity templating to the caller's own service-account namespace (exact match, no wildcard), so a token minted in one namespace cannot read another tenant's creds path. Read-only: a dynamic engine has no static password to push, so the deferred `push-keystone-db.hcl` is unnecessary and not created (#439). |
| `glance-db-dynamic` | <code v-pre>database/mariadb/creds/glance-{{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}</code> | `read` | Per-tenant dynamic Glance DB credential reads (bound to the `glance-db` role) — the Glance analogue of `keystone-db-dynamic`, and fully keystone-independent. The path is scoped by ACL identity templating to the caller's own service-account namespace (exact match, no wildcard), so a token minted in one namespace cannot read another tenant's creds path. Read-only: a dynamic engine has no static password to push. |
| `placement-db-dynamic` | <code v-pre>database/mariadb/creds/placement-{{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}</code> | `read` | Per-tenant dynamic Placement DB credential reads (bound to the `placement-db` role), on the same terms as the Glance policy. The path is scoped by ACL identity templating to the caller's own service-account namespace (exact match, no wildcard). The `placement-db-creds` SA name is what keeps a Placement generator off a Keystone creds path when both services share a namespace. Read-only: a dynamic engine has no static password to push. |
| `neutron-db-dynamic` | <code v-pre>database/mariadb/creds/neutron-{{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}</code> | `read` | Per-tenant dynamic Neutron DB credential reads (bound to the `neutron-db` role), on the same terms as the Placement policy. The path is scoped by ACL identity templating to the caller's own service-account namespace (exact match, no wildcard). The `neutron-db-creds` SA name is what keeps a Neutron generator off a Keystone creds path when both services share a namespace. Read-only: a dynamic engine has no static password to push. |

**Note:** `ci-cd-provisioner` intentionally lacks `delete` capability. The CI/CD
pipeline can create, update, and read secrets but cannot delete them, preventing
accidental secret removal during automated deployments.

**Note:** `push-ceph-keys` and `eso-tenant` include `read` capability
so that ESO's PushSecret controller can check the current remote value during
reconciliation and only write when the secret has actually changed.

**Note:** `eso-tenant` carries the per-ControlPlane grants for the c5c3
operator's admin Application Credential
(`kv-v2/{data,metadata}/openstack/keystone/{ns}/+/admin/app-credential`) and its
declarative service-account passwords
(`kv-v2/{data,metadata}/openstack/keystone/{ns}/+/service-accounts/+`), with
`delete` on the data leaves so the `DeletionPolicy: Delete` PushSecrets can purge
the KV leaf on teardown. These paths are namespace-templated — `{ns}` resolves to
the caller's own `service_account_namespace` — so a tenant's PushSecret cannot
write another tenant's KV leaf. See the
[infrastructure manifests reference](./infrastructure-manifests.md).

## Secret Paths

All secrets are stored under the `kv-v2/` mount point (KV version 2 engine).

### Bootstrap Secrets

| Path | Keys | Provisioned By | Consumed By |
| --- | --- | --- | --- |
| `kv-v2/bootstrap/<namespace>/<keystone>/admin` | `password` | `write-bootstrap-secrets.sh` (per **Managed-mode** ControlPlane; default `.../openstack/controlplane-keystone/admin`) | Operator-created ExternalSecret `{controlplane.Name}-keystone-admin-credentials` (default `controlplane-keystone-admin-credentials`); on kind additionally the overlay's `keystone-admin` ExternalSecret (default identity) |
| `kv-v2/bootstrap/<namespace>/<horizon>/secret-key` | `secret-key` | `write-bootstrap-secrets.sh` (per **Managed-mode** ControlPlane; default `.../openstack/controlplane-horizon/secret-key`) | On kind the overlay's `horizon-secret-key` ExternalSecret (default identity); per-CR ExternalSecrets in test namespaces |
| `kv-v2/infrastructure/mariadb` | `root-password` | `write-bootstrap-secrets.sh` | On kind the overlay's `mariadb-root-password` ExternalSecret; in production a non-kind Flux MariaDB baseline provides the `mariadb-root-password` Secret itself |
| `kv-v2/bootstrap/openstack/garage/admin-token` | `token` | `write-bootstrap-secrets.sh` (unconditional; Garage is base infra) | The kind overlay's `garage-admin-token` ExternalSecret in `shared-services` → `GarageCluster.spec.admin.adminTokenSecretRef` |
| `kv-v2/bootstrap/openstack/garage/s3-credentials` | `access-key-id` (`GK`-prefixed), `secret-access-key` | `write-bootstrap-secrets.sh` (unconditional) | The kind overlay's `garage-s3-credentials` ExternalSecret in `shared-services` → `GarageKey.spec.importKey.secretRef`, plus a second copy in `openstack` for the Glance consumers |
| `kv-v2/bootstrap/openstack/openbao-instance/unseal-key` | `key` | `write-bootstrap-secrets.sh` (unconditional) | The kind overlay's `openbao-instance-unseal-key` ExternalSecret (`refreshPolicy: CreatedOnce`) → the Secret the openbao-operator mounts as the proving instance's static seal. Never rotated |
| `kv-v2/openstack/keystone/openstack/standalone/db` | `username`, `password` | `write-bootstrap-secrets.sh` (standalone/brownfield only) | The kind overlay's `keystone-db` ExternalSecret, serving standalone (non-ControlPlane) Keystone demos |

**Note:** The stage-(a) per-ControlPlane static path
`openstack/keystone/{ns}/{name}/db` (c5c3 operator helper
`dbCredentialRemoteKeyFor`) is **no longer seeded** (#439): managed-mode
ControlPlanes draw short-lived, engine-issued credentials from
`database/mariadb/creds/keystone-{ns}` instead. The static path derivation is
retained only for the `credentialsMode: Static` opt-out (brownfield migration),
whose KV path must then be seeded manually — see
`docs/guides/keystone/migrate-keystone-db-to-dynamic-credentials.md`.

### OpenBao paths per ControlPlane mode

Which OpenBao paths exist for a ControlPlane depends on its Keystone identity
mode (`spec.services.keystone.mode`). Seeding is a **Managed-mode** concern only;
an **External-mode** ControlPlane wraps a pre-existing Keystone whose credentials
were never minted by this operator, so no seeding script runs for it. In both
modes the per-tenant `openbao-tenant-store` SecretStore the c5c3 operator
provisions for the ControlPlane (`reconcileESOTenantStore`) is what reaches these
paths, authenticating as the namespace-templated `eso-tenant` identity.

**Both modes** — created on demand by ESO, never seeded. ESO's first PushSecret
CREATES the KV leaf and stamps `custom_metadata.managed-by=external-secrets`
itself (the managed-by guard only rejects a *pre-existing* leaf lacking that
marker, so a first push to a never-seeded path always succeeds):

| Path (under the `kv-v2` mount) | Written by | Read back by |
| --- | --- | --- |
| `openstack/keystone/{ns}/{cp}/admin/app-credential` | c5c3 operator admin-AC backup PushSecret (`adminAppCredentialRemoteKeyFor`, `DeletionPolicy: Delete`) | per-CR `k-orc-clouds-yaml` ExternalSecret (`ensureKORCCloudsYAMLExternalSecret`) |
| `openstack/keystone/{ns}/{name}/service-accounts/credentials` | c5c3 operator per-registration backup PushSecret (`keystoneServiceRemoteKeyFor`, one per `KeystoneService` CR — `{ns}/{name}` is that CR's) | the matching per-registration ExternalSecret |

**Managed mode additionally** — seeded and/or backed up because the operator owns
the Keystone lifecycle:

| Path (under the `kv-v2` mount) | Provisioned |
| --- | --- |
| `bootstrap/{ns}/{cp}-keystone/admin` | seeded by `write-bootstrap-secrets.sh` (ESO-adoption-marked), later overwritten by the keystone-operator Model B rotation PushSecret |
| `bootstrap/{ns}/{cp}-horizon/secret-key` | seeded by `write-bootstrap-secrets.sh` |
| `openstack/keystone/{ns}/{cp}-keystone/fernet-keys` | keystone-operator backup PushSecret (`reconcile_fernet.go`) |
| `openstack/keystone/{ns}/{cp}-keystone/credential-keys` | keystone-operator backup PushSecret (`reconcile_credential.go`) |
| `database/mariadb/creds/keystone-{ns}` (database engine, not KV) | dynamic, short-lived leases issued by the `database` engine role provisioned by `setup-database-tenant.sh` |

**External mode** — no seeding script runs for the ControlPlane and no bootstrap
path is created for it. The **only** OpenBao-side prerequisites are the one-time
cluster bootstrap:

- the secret engines (`setup-secret-engines.sh`),
- the `eso-tenant` Kubernetes-auth role (`setup-auth.sh`),
- the templated `eso-tenant` policy (`setup-policies.sh`),

plus the infra-stack `openbao-ca-issuer` ClusterIssuer that signs the tenant
store's mTLS client certificate. Neither `setup-eso-tenant.sh` nor
`setup-database-tenant.sh` is needed: the c5c3 operator provisions the per-tenant
store itself, and an External-mode ControlPlane has no managed database. The
app-credential and declared service-account paths above are still created on the
operator's first push, exactly as in Managed mode.

### ESO Integration

The ClusterSecretStore `openbao-cluster-store` connects the External Secrets Operator
to OpenBao. ExternalSecret resources reference this store to pull secrets into
Kubernetes.

**Note:** The shared `openbao-cluster-store` is now namespace-restricted. Its
`spec.conditions` (`deploy/eso/clustersecretstore.yaml`) limit which namespaces
may reference it to `openstack` and `shared-services`. The first hosts the static
infrastructure ExternalSecrets listed below; the second hosts the Garage ones that
sit next to the object store. Per-ControlPlane Keystone key material is
no longer read or written through this shared store; each ControlPlane uses its
own namespaced `openbao-tenant-store` (backed by the per-tenant `eso-tenant`
identity, provisioned by `setup-eso-tenant.sh`). The exact allowed namespace is
deployment-specific.

| ExternalSecret | Namespace | Remote Path | Remote Property | K8s Secret Name | K8s Secret Key |
| --- | --- | --- | --- | --- | --- |
| `{controlplane.Name}-keystone-admin-credentials` | `openstack` | `bootstrap/{ns}/{name}-keystone/admin` (default `bootstrap/openstack/controlplane-keystone/admin`) | `password` | `{controlplane.Name}-keystone-admin-credentials` | `password` |
| `{controlplane.Name}-keystone-db-credentials` | `openstack` | Dynamic (default): generator-backed via `VaultDynamicSecret` reading `database/mariadb/creds/keystone-{ns}` (no KV remote path); Static opt-out: `openstack/keystone/{ns}/{name}/db` | `username`, `password` | `{controlplane.Name}-keystone-db-credentials` | `username`, `password` |
| `{controlplane.Name}-glance-db-credentials` | `openstack` | Dynamic (default): generator-backed via `VaultDynamicSecret` reading `database/mariadb/creds/glance-{ns}` (no KV remote path); Static opt-out: `openstack/glance/{ns}/{name}/db` | `username`, `password` | `{controlplane.Name}-glance-db-credentials` | `username`, `password` |
| `{controlplane.Name}-placement-db-credentials` | `openstack` | Dynamic (default): generator-backed via `VaultDynamicSecret` reading `database/mariadb/creds/placement-{ns}` (no KV remote path); Static opt-out: `openstack/placement/{ns}/{name}/db` | `username`, `password` | `{controlplane.Name}-placement-db-credentials` | `username`, `password` |
| `{controlplane.Name}-neutron-db-credentials` | `openstack` | Dynamic (default): generator-backed via `VaultDynamicSecret` reading `database/mariadb/creds/neutron-{ns}` (no KV remote path); Static opt-out: `openstack/neutron/{ns}/{name}/db` | `username`, `password` | `{controlplane.Name}-neutron-db-credentials` | `username`, `password` |
| `keystone-admin` (kind only) | `openstack` | `bootstrap/openstack/controlplane-keystone/admin` | `password` | `keystone-admin` | `password` |
| `mariadb-root-password` (kind only) | `openstack` | `infrastructure/mariadb` | `root-password` | `mariadb-root-password` | `password` |
| `keystone-db` (kind only) | `openstack` | `openstack/keystone/openstack/standalone/db` | `username`, `password` | `keystone-db` | `username`, `password` |
| `neutron-db` (kind only) | `openstack` | `openstack/neutron/openstack/standalone/db` | `username`, `password` | `neutron-db` | `username`, `password` |

**Note:** The static `deploy/eso/externalsecrets/` directory has been removed, so
the production stack ships **no** ExternalSecret resources — its ESO
kustomization renders only `clustersecretstore.yaml`. The admin and database
ExternalSecrets are now projected **per-ControlPlane** by the c5c3 operator: the
`{controlplane.Name}-keystone-admin-credentials` ExternalSecret by the
`reconcileAdminPassword` sub-reconciler (remote key
`bootstrap/{ns}/{name}-keystone/admin`), and the
`{controlplane.Name}-keystone-db-credentials` ExternalSecret by the
`reconcileDBCredentials` sub-reconciler (Dynamic default: backed by a
`VaultDynamicSecret` generator reading `database/mariadb/creds/keystone-{ns}`;
Static opt-out: KV remote key `openstack/keystone/{ns}/{name}/db`, seeded
manually). Neither the flat database path `openstack/keystone/db` nor the
stage-(a) per-ControlPlane static path is seeded anymore.

**Note:** The `keystone-admin`, `mariadb-root-password`, and `keystone-db`
ExternalSecrets survive only as **kind-overlay-only** resources under
`deploy/kind/infrastructure/`
(`keystone-admin-externalsecret.yaml`, `mariadb-root-password-externalsecret.yaml`,
`keystone-db-externalsecret.yaml`). They keep the standalone flows — the Quick
Start and the keystone/infrastructure e2e, tempest, and chaos suites that
reference plain Secret names — working, and are **not** deployed in production.
Outside kind, a standalone Keystone instance has to materialise the
`keystone-admin` and `keystone-db` Secrets itself, and a non-kind Flux MariaDB
baseline is expected to provide the `mariadb-root-password` Secret itself.

**Note:** The ExternalSecret `remoteRef.key` is the path **under** the store's mount
path. The ClusterSecretStore already sets `path: kv-v2`, so ExternalSecrets use
`infrastructure/mariadb` (not `kv-v2/infrastructure/mariadb`).

**Note:** The `mariadb-root-password` ExternalSecret maps the OpenBao key
`root-password` to the Kubernetes Secret key `password`. The MariaDB CR
references this Secret with `rootPasswordSecretKeyRef.key: password`, which
reads the exact key specified — the MariaDB CRD uses a standard
`SecretKeySelector` with no key remapping.

## OpenBao HelmRelease

**File:** `deploy/flux-system/releases/openbao.yaml`

| Property | Value |
| --- | --- |
| Target namespace | `shared-services` |
| Chart | `openbao` |
| Version constraint | `>=0.5.0 <1.0.0` |
| Source | `openbao` HelmRepository |
| Dependencies | `cert-manager` in `cert-manager` namespace |

### HA Raft Configuration

| Setting | Value |
| --- | --- |
| Replicas | 3 |
| Storage backend | Raft |
| Raft data path | `/openbao/data` |
| PVC size | `10Gi` |
| PVC storage class | `local-path` |
| Leader election | Automatic via Raft consensus |

All 3 replicas are configured with `retry_join` stanzas pointing to each other's
headless service DNS names (`openbao-0.openbao-internal`, `openbao-1.openbao-internal`,
`openbao-2.openbao-internal`), enabling automatic cluster formation at startup.

### TLS Configuration

| Setting | Value |
| --- | --- |
| TLS certificate | `/openbao/tls/tls.crt` |
| TLS key | `/openbao/tls/tls.key` |
| Listener address | `[::]:8200` (dual-stack) |
| Cluster address | `[::]:8201` |
| TLS disabled | `false` |
| Certificate source | `openbao-tls` Secret (cert-manager) |
| Certificate duration | `8760h` (1 year) |
| Renewal window | `720h` (30 days before expiry) |
| `tls_client_ca_file` | `/openbao/tls/ca.crt` — CA the listener uses to verify presented client certs. The file resolves to the `openbao-ca` CA bundle because the server cert (`openbao-tls`) and every client cert are signed by the same `openbao-ca-issuer` (see below) |
| `tls_require_and_verify_client_cert` | `true` — every TLS handshake on `:8200` must present a valid client cert; the listener rejects any connection that does not, before any application-layer auth (Kubernetes JWT, AppRole, root token) runs |

The TLS certificate is issued by the `openbao-ca-issuer` (a CA-type ClusterIssuer)
via a cert-manager Certificate resource at
`deploy/flux-system/infrastructure/openbao-tls-cert.yaml`. The CA keypair itself
is bootstrapped by `selfsigned-cluster-issuer` in
`deploy/flux-system/infrastructure/openbao-ca-issuer.yaml` — a SelfSigned issuer
cannot sign leaves for a separate trust chain, so the openbao trust domain owns
its own CA (mirrors the `openstack-db-ca` precedent).

**Client certificates.** Two additional `cert-manager.io/v1` Certificates
issue *client*-auth keypairs from the same `openbao-ca-issuer`, both
declared in `deploy/flux-system/infrastructure/openbao-client-tls-cert.yaml`:

| Certificate | Secret | Consumer | Mount / Reference |
| --- | --- | --- | --- |
| `openbao-client-tls` | `openbao-client-tls` (namespace `shared-services`) | OpenBao pods themselves — Raft `retry_join` peer auth + in-pod `bao` exec via `bootstrap/*.sh` | StatefulSet volume `client-tls` mounted read-only at `/openbao/client-tls`, distinct from the server-cert mount at `/openbao/tls` |
| `eso-openbao-client-tls` | `eso-openbao-client-tls` (namespace `shared-services`) | External Secrets Operator `ClusterSecretStore/openbao-cluster-store` | `spec.provider.vault.tls.certSecretRef` / `keySecretRef` (`deploy/eso/clustersecretstore.yaml`); Kubernetes-token `auth.kubernetes` block is unchanged — mTLS is purely a transport-layer admission gate |

Both client Certificates carry `usages: ["client auth"]` and share the
`openbao-tls` duration / `renewBefore` so server and client rotation cadences
align. The `commonName` / `dnsNames` on the client certs are identifiers only —
the OpenBao listener does not verify SANs on client auth, only the issuing CA.

**Certificate SANs:**

| SAN | Type | Cert | Usages | Purpose |
| --- | --- | --- | --- | --- |
| `openbao-0.openbao-internal` | DNS | `openbao-tls` (server) | `server auth` | StatefulSet pod 0 |
| `openbao-1.openbao-internal` | DNS | `openbao-tls` (server) | `server auth` | StatefulSet pod 1 |
| `openbao-2.openbao-internal` | DNS | `openbao-tls` (server) | `server auth` | StatefulSet pod 2 |
| `openbao.shared-services.svc` | DNS | `openbao-tls` (server) | `server auth` | Kubernetes Service endpoint |
| `127.0.0.1` | IP | `openbao-tls` (server) | `server auth` | Pod-local loopback (bootstrap scripts, `bao_exec`) |
| `::1` | IP | `openbao-tls` (server) | `server auth` | IPv6 loopback |
| `openbao-client.shared-services.svc` | DNS | `openbao-client-tls` | `client auth` | Identifier only; presented by OpenBao pods on Raft `retry_join` and in-pod `bao` exec. SANs are not verified by the listener for client auth — chain-to-CA is. |
| `eso-openbao-client.shared-services.svc` | DNS | `eso-openbao-client-tls` | `client auth` | Identifier only; presented by ESO `ClusterSecretStore/openbao-cluster-store` on every Vault call. SANs are not verified. |

### Resource Limits

| Resource | Request | Limit |
| --- | --- | --- |
| Memory | 256Mi | 512Mi |
| CPU | 250m | — |

### Disabled Features

| Feature | Value | Reason |
| --- | --- | --- |
| `injector.enabled` | `false` | Secrets are managed via ESO, not sidecar injection |
| `ui` | `false` | No web UI required for headless secret management |

## Idempotency Guarantees

All bootstrap scripts are designed to be safely re-run without side effects. This
table summarizes the idempotency mechanism for each script:

| Script | Guard Mechanism | Behavior on Re-run |
| --- | --- | --- |
| `init-unseal.sh` | `bao status -format=json` JSON parsing | Skips initialization if already initialized; skips unseal for already-unsealed pods |
| `setup-secret-engines.sh` | `bao secrets list` path check | Skips engine enable if mount path already exists |
| `setup-auth.sh` | `bao auth list` path check | Skips auth enable if mount path already exists; role write is upsert |
| `setup-policies.sh` | `bao policy write` upsert semantics | Overwrites existing policy with same content (no-op if unchanged) |
| `write-bootstrap-secrets.sh` | `bao kv get` existence check | Skips secret write if path already contains data |

**Critical invariant:** `write-bootstrap-secrets.sh` never overwrites existing secrets.
This prevents credential rotation from being accidentally triggered by a re-run. To
rotate credentials, existing secrets must be explicitly deleted first.

## Error Handling

All scripts use `set -euo pipefail` for strict error handling:

| Flag | Behavior |
| --- | --- |
| `-e` | Exit immediately on any command failure |
| `-u` | Treat unset variables as errors |
| `-o pipefail` | Propagate failures through pipes (not just the last command) |

Scripts log timestamped status messages to stdout using ISO 8601 format. Error messages
from `bao` CLI commands are propagated to stderr by `kubectl exec`.

## Troubleshooting

### OpenBao pods not starting

Verify the TLS certificate Secret exists and is populated:

```bash
kubectl get secret openbao-tls -n shared-services -o jsonpath='{.data.tls\.crt}' | wc -c
```

If the Secret is empty or missing, check cert-manager logs:

```bash
kubectl logs -n cert-manager -l app.kubernetes.io/name=cert-manager
```

### Unseal keys lost

If the `openbao-init-keys` Secret is deleted, the unseal keys cannot be recovered.
OpenBao must be completely redeployed (delete PVCs, delete pods, re-run init):

```bash
kubectl delete pvc -n shared-services -l app.kubernetes.io/name=openbao
kubectl delete pods -n shared-services -l app.kubernetes.io/name=openbao
# Wait for pods to restart, then re-run init-unseal.sh
```

### Script fails with "permission denied"

Ensure scripts have execute permissions:

```bash
chmod +x deploy/openbao/bootstrap/*.sh
```

### ExternalSecrets stuck in "SecretSyncedError"

Verify the store is healthy. For a ControlPlane on the default shared store,
check the ClusterSecretStore; for one switched to a per-tenant identity via
`spec.secretStoreRef: {kind: SecretStore, name: openbao-tenant-store}`, check
the namespaced SecretStore in the tenant's namespace instead:

```bash
# Default (shared) store:
kubectl get clustersecretstore openbao-cluster-store -o jsonpath='{.status.conditions}'

# Per-tenant store (in the ControlPlane's namespace):
kubectl get secretstore openbao-tenant-store -n <namespace> -o jsonpath='{.status.conditions}'
```

For a per-tenant store, a `403`/permission-denied on push usually means the
`eso-tenant` role or the `eso-tenant` policy is missing — re-run `setup-auth.sh`
and `setup-policies.sh` (bootstrap predating this feature does not create them).

Common causes:

- OpenBao is sealed (re-run `init-unseal.sh`)
- ESO service account missing (verify `external-secrets` SA exists in `external-secrets` namespace)
- TLS trust failure (verify `openbao-tls` Secret contains `ca.crt` key)
- Missing client certificate: verify the `eso-openbao-client-tls`
  Secret exists in `shared-services` and the `ClusterSecretStore`
  `spec.provider.vault.tls.certSecretRef` / `keySecretRef` point at it. With
  `tls_require_and_verify_client_cert = true` on the listener, an absent or
  mis-referenced client cert appears as a generic TLS handshake error in the
  ESO controller log (`remote error: tls: bad certificate` or similar), not as
  an HTTP 401/403.

### Verify mTLS enforcement

Confirm the listener rejects a client that does not present a valid certificate.
The probe runs entirely inside the pod (so the CA bundle is on the
filesystem and reaches the loopback listener) and exits **non-zero** on success,
because the handshake must fail:

```bash
# Expected output ends with:
#   curl: (35) ... alert certificate required        OR
#   curl: (56) ... tls: certificate required         OR
#   exit code 35/56 from curl with no body returned.
# Exit code MUST be non-zero. A 200 OK from this command would indicate that
# tls_require_and_verify_client_cert is NOT being enforced and is a P0 incident.
kubectl exec -n shared-services openbao-0 -- \
  sh -c 'curl --cacert /openbao/tls/ca.crt -sS -o /dev/null \
             -w "http_code=%{http_code}\n" \
             https://127.0.0.1:8200/v1/sys/health; echo "exit=$?"'
```

Then confirm the same call succeeds **with** the client cert (this is what
`bao_exec` does on every reconcile):

```bash
# Expected: http_code=200 (or 429 if standby), exit=0.
kubectl exec -n shared-services openbao-0 -- \
  sh -c 'curl --cacert /openbao/tls/ca.crt \
             --cert   /openbao/client-tls/tls.crt \
             --key    /openbao/client-tls/tls.key \
             -sS -o /dev/null -w "http_code=%{http_code}\n" \
             https://127.0.0.1:8200/v1/sys/health; echo "exit=$?"'
```

If the first command unexpectedly returns `http_code=200`, the listener is not
enforcing client-cert auth — re-check
`deploy/flux-system/releases/openbao.yaml` for `tls_client_ca_file` and
`tls_require_and_verify_client_cert = true`, and that the HelmRelease has
reconciled (`kubectl get helmrelease openbao -n shared-services`).

## Related Resources

- [Infrastructure Manifests](./infrastructure-manifests.md) — FluxCD base deployment
- `deploy/flux-system/releases/openbao.yaml` — OpenBao HelmRelease
- `deploy/flux-system/infrastructure/openbao-tls-cert.yaml` — server TLS Certificate
- `deploy/flux-system/infrastructure/openbao-client-tls-cert.yaml` — client TLS Certificates
- `deploy/eso/clustersecretstore.yaml` — ClusterSecretStore configuration (now uses client-cert mTLS); the only resource the production ESO kustomization renders
- `deploy/kind/infrastructure/` — kind-overlay-only ExternalSecret shims (`keystone-admin`, `mariadb-root-password`, `keystone-db`)
