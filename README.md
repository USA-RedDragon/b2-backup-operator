# b2-backup-operator

[![Release](https://github.com/USA-RedDragon/b2-backup-operator/actions/workflows/release.yaml/badge.svg)](https://github.com/USA-RedDragon/b2-backup-operator/actions/workflows/release.yaml) [![go.mod version](https://img.shields.io/github/go-mod/go-version/USA-RedDragon/b2-backup-operator.svg)](https://github.com/USA-RedDragon/b2-backup-operator) [![License](https://badgen.net/github/license/USA-RedDragon/b2-backup-operator)](https://github.com/USA-RedDragon/b2-backup-operator/blob/main/LICENSE) [![Release](https://img.shields.io/github/release/USA-RedDragon/b2-backup-operator.svg)](https://github.com/USA-RedDragon/b2-backup-operator/releases/)

A Kubernetes operator that backs up apps with [restic](https://restic.net) into Backblaze B2. Each app gets two short custom resources: a Repository that provisions the restic repository and its credentials, and a ScheduledBackup that renders the CronJob that backs up into it.

## What it does

- **Provisions or adopts** a restic repository and records which one in `status.repositoryID`, so the app-to-repository mapping is visible without decrypting anything.
- **Generates the restic password exactly once**, writes it alongside a synthesized `RESTIC_REPOSITORY` into a Secret, and never rotates it.
- **Initializes the repository** with a one-shot Job that probes before initializing, so a genuine failure is not mistaken for success.
- **Renders a CronJob** from a declarative backup spec: sources, retention, database credential wiring, pod affinity and cache volume are all derived.
- **Ships `b2bk`**, a CLI and kubectl plugin that runs backups on demand, explains why one is not running, and browses and restores snapshots. See [docs/b2bk.md](docs/b2bk.md).

## Installing

Every release publishes the rendered manifests as an OCI artifact at `oci://ghcr.io/usa-reddragon/b2-backup-operator-manifests`, with the manager image pinned by digest. Point Flux at it:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: b2-backup-operator
  namespace: flux-system
spec:
  interval: 1h
  url: oci://ghcr.io/usa-reddragon/b2-backup-operator-manifests
  ref:
    semver: ">=0.1.0"
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: b2-backup-operator
  namespace: flux-system
spec:
  interval: 1h
  sourceRef:
    kind: OCIRepository
    name: b2-backup-operator
  path: ./
  prune: true
  wait: true
```

Without Flux, pull the same artifact and apply it:

```sh
flux pull artifact oci://ghcr.io/usa-reddragon/b2-backup-operator-manifests:v0.1.0 --output ./b2-backup-operator
kubectl apply -f ./b2-backup-operator/install.yaml
```

The operator runs in `b2-backup-operator-system`.

## Resources

### Repository

```yaml
apiVersion: b2backup.mcswain.dev/v1
kind: Repository
metadata:
  name: restic
  namespace: myapp-prod
spec:
  passwordSecretRef:
    name: restic-envs
    key: RESTIC_PASSWORD
```

The operator writes the repository credentials, `RESTIC_REPOSITORY` and `RESTIC_PASSWORD`, to a Secret named `<name>-b2` unless `spec.secretName` says otherwise. It only ever writes to Secrets it created, which it marks with `app.kubernetes.io/managed-by: b2-backup-operator`.

`passwordSecretRef` seeds the restic password from a Secret you keep, such as a SOPS-encrypted one in Git. The operator reads it and never writes to it, so it stays the off-cluster copy of the key that decrypts the backups. Without it the operator generates a password once and never generates another for a repository that has been recorded, initialized or reports any usage. Set it whenever the repository may already hold snapshots.

`deletionPolicy` defaults to `Retain`: deleting the resource leaves both the remote repository and the credentials Secret in place. Set it to `Delete` only when destroying every snapshot is what you want. Under `Delete`, the finalizer is released only once the repository is confirmed gone. Annotate the Repository with `b2backup.mcswain.dev/force-finalize=true` to release it anyway; the operator emits a `RepositoryOrphaned` warning naming the repository it left behind.

`kubectl explain repository.spec` lists the storage fields.

### ScheduledBackup

```yaml
apiVersion: b2backup.mcswain.dev/v1
kind: ScheduledBackup
metadata:
  name: restic
  namespace: myapp-prod
spec:
  repositoryRef: {name: restic}
  schedule: "@hourly"          # jittered; a cron expression is used verbatim
  sources:
    - {type: cnpg, tag: db}
    - {type: files, tag: files, path: app, exclude: ["**/temp*"]}
  retention: {hourly: 168, daily: 90, monthly: 24, yearly: 10}
  database: {engine: cnpg, secretName: postgresql-app}
  volume: {existingClaim: myapp-prod-storage}
```

`spec.script` replaces the generated body wholesale for anything the source types do not cover; it still gets the standard preamble, logging and cache cleanup.

### Pod security

Backup pods drop every capability, forbid privilege escalation, run under the `RuntimeDefault` seccomp profile and mount no ServiceAccount token.

No user or group id is imposed. A backup has to read the app's own data, whose ownership varies per app and per cluster, and an `fsGroup` would chown that data as a side effect of backing it up. A namespace enforcing the restricted Pod Security Standard supplies what it needs through `spec.podSecurityContext`, with `spec.containerSecurityContext` for `readOnlyRootFilesystem`. Both replace the default wholesale.

### Database credentials

`spec.database.secretName` chooses the Secret; it does not choose where the Secret is mounted. The operator mounts it at one fixed path, `/var/run/secrets/b2backup/database`, and passes that same path to `dumpdb` and `restoredb` as `--secret-mount`, so where the credentials are put and where they are read from cannot drift apart.

### Schedules

A five-field cron expression is passed through untouched. A shorthand is expanded with a jitter derived from the resource's namespace and name, so copy-pasted backups spread across the period instead of all firing on the hour. The result appears in `status.effectiveSchedule`. The jitter is stable: a backup keeps its slot unless it is renamed.

Accepted shorthands are `@hourly`, `@daily` (`@midnight`), `@weekly`, `@monthly`, `@yearly` (`@annually`) and `@every <duration>`. Stepped schedules are phase-shifted rather than started at zero, so `@every 15m` renders as `7-59/15 * * * *` rather than `*/15 * * * *`.

Steps that do not divide their period evenly (`@every 7h`) are rejected, since cron restarts the sequence each period and would leave an irregular gap.

### Extra environment variables

`spec.env` is a plain map of extra variables for the backup container. It is merged after everything the operator sets, and a container's `env` overrides its `envFrom` as a whole, so a name the operator already uses would win silently, sending a backup to another repository or encrypting it under another key.

So `spec.env` is refused when it names one of those. Which names are reserved depends on the rest of the spec:

| Reserved | When |
| --- | --- |
| `RESTIC_REPOSITORY`, `RESTIC_PASSWORD` | Always. These come from the Repository's Secret |
| `RESTIC_HOST`, `TZ` | Always. Use `spec.resticHost` and `spec.timeZone` |
| `DB_HOST`, `DB_DATABASE`, `DB_USERNAME` | Only under the **mariadb** engine, which is where the operator sets them. A cnpg backup may set `DB_HOST` itself |

The error names each offending variable and the field to use instead. It is raised at reconcile time, so a ScheduledBackup that predates this check goes `Ready=False` rather than quietly shadowing its own credentials. A CEL rule on the CRD rejects `RESTIC_REPOSITORY` and `RESTIC_PASSWORD` at admission as well: those two are unconditional, and a CEL rule cannot be relaxed later without a version bump.

`RESTIC_GROUP_BY` and `RESTIC_REST_USERNAME`/`RESTIC_REST_PASSWORD` are deliberately **not** reserved. The operator never sets them; the image's restic wrapper reads them, and overriding them is a supported thing to do.

### The snapshot host

Every snapshot records a host, and it defaults to the namespace, injected from the downward API. `spec.resticHost` pins it to something else.

It is more than restic's `--host`. The image's wrapper also derives the dump's `--stdin-filename` from it, so `resticHost` names the database dump *inside* a snapshot as well. That is why it is a field rather than a `RESTIC_HOST` entry in `spec.env`, and why it is validated as a host name.

Set it when a namespace is renamed and the snapshots should stay in one series rather than splitting into two hosts at the rename. `b2bk` reads the same field: `b2bk snapshots` defaults its `--host` filter to it, and `b2bk restore --to-database` looks for the dump under that name, so a pinned `resticHost` needs no extra flags.

### Where the backup pod runs

The operator derives a pod affinity when the spec implies one. The rules, in order:

| Spec | Affinity |
| --- | --- |
| `spec.affinity` set | Used verbatim. `{}` is a valid value and means "no affinity at all" |
| `spec.volume` | **Required**: same node as pods labelled `app.kubernetes.io/name: <namespace>`, other than the operator's own backup pods |
| `spec.database`, no volume | **Preferred** (weight 100): same node as the database pod |
| Neither | None |

The **volume** case stays a hard requirement on purpose. An RWO claim can only be mounted on the node already holding it, so a preference there would let the backup pod be scheduled somewhere the claim cannot follow, and it would sit Pending. The selector is the sharp edge: it matches on the *namespace* as the app name, which is what bjw-s app-template sets when the HelmRelease is named after its namespace. It does not look at `app.kubernetes.io/controller`, so it matches every controller in that release, and in a namespace running more than one app it can pin the backup beside the wrong one. Set `spec.affinity` explicitly there.

The **database** case is a preference. Co-location makes a dump faster, but it is not required for correctness, and as a hard rule it left backups Pending whenever the database happened to be on a full or cordoned node. Set `spec.database.requireSameNode: true` to get the hard form back.

Which database pod is matched depends on the engine. mariadb selects `app.kubernetes.io/name: mariadb` plus `app.kubernetes.io/controller: mariadb`; cnpg selects `cnpg.io/instanceRole: primary` in the cluster named by `spec.database.cnpgCluster`, which defaults to `postgresql`. A cluster under any other name has to say so, or the affinity matches nothing. That is harmless now that it is a preference, but the affinity silently stops doing anything.

## Configuration

| Flag | Default | Purpose |
| --- | --- | --- |
| `--backup-image` | `ghcr.io/usa-reddragon/b2-backup-operator-restic:<operator version>` | The image every backup pod runs. More than a list of binaries; see the contract below |
| `--cache-storage-class` | *(none)* | StorageClass for restic cache volumes. **Cache claims request `ReadWriteMany`**, which most cluster default classes cannot satisfy; see below |

`/manager --help` lists the rest, including the storage credentials.

### The backup image contract

`spec.image` and `--backup-image` are not just "an image with restic in it". The default image satisfies a contract the operator and `b2bk` both rely on, and a substitute that does not will fail quietly rather than loudly.

The default image is `ghcr.io/usa-reddragon/b2-backup-operator-restic`, tagged with the operator's own version. It is built from `Dockerfile.restic`: [`ghcr.io/gabe565/restic`](https://github.com/gabe565/docker-restic) with this repository's `restoredb` added.

It must provide `restic`, `ts` and `dumpdb` on PATH. `b2bk restore --to-database` additionally needs `restoredb`, which it probes for, and a `/bin/sh` that supports `set -o pipefail`, so a dump that dies mid-stream cannot be reported as a successful restore. BusyBox ash, bash, zsh and Debian trixie's dash do; older dash exits with `Illegal option -o pipefail`.

The part that is easy to miss: **`restic` in this image is a wrapper** sitting ahead of the real binary, and it injects two things that appear nowhere in the script the operator renders.

- `--group-by=$RESTIC_GROUP_BY`. Without it restic groups a `forget` by `host,paths`, and every database source has the same empty path, so two database sources in one repository collapse into a single retention group and expire each other's snapshots.
- `--stdin-filename` derived from `$RESTIC_HOST`. That is what names the dump inside a snapshot (`<host>.dmp` for cnpg, `<host>.sql` for mariadb), and `b2bk restore --to-database` reads it back by that name. Change how it is derived and restores stop finding their dump.

So a custom image without the wrapper does not error. It changes retention grouping and restore paths, and you find out at the point you need the snapshot. If you pin `spec.image`, pin a tag of the same image rather than a different one.

### Cache claims and ReadWriteMany

`spec.cache.accessMode` defaults to `ReadWriteMany` unconditionally, and the claim is created whether or not `--cache-storage-class` names a class that can provide it. Leaving the flag empty falls back to the cluster's default StorageClass, which usually cannot: the claim stays `Pending`, the backup pod never schedules, and nothing about the ScheduledBackup looks wrong: it is `Ready`, with a CronJob, firing on time into a pod that cannot start.

Two ways out: point `--cache-storage-class` at a class that provides RWX, or set `spec.cache.accessMode: ReadWriteOnce` per backup. `spec.cache.enabled: false` drops the claim entirely, at the cost of re-fetching metadata every run.

## Optional monitoring manifests

`config/prometheus` and `config/network-policy` are deliberately **not** part of `config/default`, so neither `make deploy` nor the published manifest bundle installs them. The ServiceMonitor needs the Prometheus Operator's CRDs, and the NetworkPolicy needs a CNI that enforces policy; including either by default would break a plain cluster. Apply them yourself, after the operator:

```sh
kustomize build config/prometheus | kubectl apply -f -
kustomize build config/network-policy | kubectl apply -f -
```

Both pin `b2-backup-operator-system`, matching `config/default`. The ServiceMonitor has no `namespaceSelector`, so it discovers the metrics Service in its own namespace and must stay in `b2-backup-operator-system`; if you move it, add a `namespaceSelector` pointing back. Prometheus must also be configured to select ServiceMonitors from that namespace.

The NetworkPolicy admits `:8443` only from namespaces labelled `metrics: enabled` (label the one Prometheus runs in) and `:8081` from anywhere, because kubelet probes arrive from the node rather than from a pod that any selector can match. `:8081` serves only `/healthz` and `/readyz`.

## Development

```sh
make manifests generate        # regenerate CRDs, RBAC and deepcopy
make test                      # unit tests, no cluster needed
make test-crd                  # CRD validation against a real API server (envtest)
make test-e2e                  # full deploy against a throwaway Kind cluster
make lint                      # golangci-lint with the logcheck plugin
make build-installer IMG=...   # render dist/install.yaml
```

Most of the validation lives in CEL rules on the CRDs, which no Go test can reach: a rule that fails to compile makes every CRD install fail, and one that never fires is invisible. `make test-crd` runs them against a real API server.

Pushes to `main` publish `beta` images. A `v*` tag publishes the manager image to `ghcr.io/usa-reddragon/b2-backup-operator`, the backup image to `ghcr.io/usa-reddragon/b2-backup-operator-restic`, the rendered manifests to `ghcr.io/usa-reddragon/b2-backup-operator-manifests`, and the `b2bk` archives and checksums to the GitHub release.

## b2bk, the CLI

`b2bk` runs backups on demand, explains why one is not running, and restores snapshots. It is the same binary as the `kubectl b2bk` plugin: install it under both names, and kubectl presents anything called `kubectl-b2bk` on PATH as a subcommand of its own.

```sh
b2bk doctor -n myapp-prod        # or: kubectl b2bk doctor -n myapp-prod
```

`b2bk` talks to the cluster as you do, and needs more than the operator's own ServiceAccount holds: `config/rbac/b2bk_user_role.yaml` defines `b2bk-viewer` for read-only use and `b2bk-operator` for triggering backups and restoring.

See [docs/b2bk.md](docs/b2bk.md) for the full command reference.

## License

b2-backup-operator is licensed under the [MIT License](LICENSE).
