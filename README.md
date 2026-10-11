# b2-backup-operator

[![Release](https://github.com/USA-RedDragon/b2-backup-operator/actions/workflows/release.yaml/badge.svg)](https://github.com/USA-RedDragon/b2-backup-operator/actions/workflows/release.yaml) [![go.mod version](https://img.shields.io/github/go-mod/go-version/USA-RedDragon/b2-backup-operator.svg)](https://github.com/USA-RedDragon/b2-backup-operator) [![License](https://badgen.net/github/license/USA-RedDragon/b2-backup-operator)](https://github.com/USA-RedDragon/b2-backup-operator/blob/main/LICENSE) [![Release](https://img.shields.io/github/release/USA-RedDragon/b2-backup-operator.svg)](https://github.com/USA-RedDragon/b2-backup-operator/releases/)

A Kubernetes operator that backs up apps with [restic](https://restic.net) into [Backblaze B2](https://www.backblaze.com/cloud-storage). Each app gets two short custom resources: a Repository that provisions the restic repository and its credentials, and a ScheduledBackup that renders the CronJob that backs up into it.

## What it does

- **Provisions or adopts** a restic repository in B2, either in a bucket of its own or under a path in a shared bucket, and records the bucket and path in status, so the app-to-repository mapping is visible without decrypting anything.
- **Gives every repository its own B2 application key**, restricted to its bucket and path, and replaces the key if it is lost or revoked.
- **Generates the restic password exactly once**, writes it with the key and a synthesized `RESTIC_REPOSITORY` into a Secret, and never rotates it.
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

The operator runs in `b2-backup-operator-system`. Before it can do anything it needs a B2 application key in a Secret there; see [B2 credentials](#b2-credentials).

## Resources

### Repository

A Repository has one of two layouts, chosen by `spec.mode`:

| Mode | Bucket | Path | Application key restricted to |
| --- | --- | --- | --- |
| `Bucket` | Its own private bucket. Created if this account does not have it, adopted if it does | Bucket root, or `spec.path` | The bucket, and `spec.path` when set |
| `Prefix` | An existing bucket shared with other repositories. Never created or deleted by the operator | `spec.path`, default `<namespace>/<name>` | The bucket and the path |

Both `mode` and `bucket` are required and cannot change once set. `path` can be set only when the Repository is created. Bucket names are global across all of B2: 6 to 63 lowercase letters, digits and dashes, starting and ending with a letter or digit, and never starting with `b2-`.

A Prefix mode repository with its own bucket path, created fresh:

```yaml
apiVersion: b2backup.mcswain.dev/v1
kind: Repository
metadata:
  name: restic
  namespace: myapp-prod
spec:
  mode: Prefix
  bucket: acme-restic
```

It lives at `acme-restic/myapp-prod/restic`. The default path is `<namespace>/<name>` rather than `<namespace>` because namespace and name together are unique in the cluster, so two Repositories in one namespace never collide by default. Set `spec.path` to put it anywhere else.

A Bucket mode repository, which gets a private bucket of its own:

```yaml
apiVersion: b2backup.mcswain.dev/v1
kind: Repository
metadata:
  name: restic
  namespace: otherapp-prod
spec:
  mode: Bucket
  bucket: acme-otherapp-prod-restic
```

Adopting a restic repository that already exists at `s3:https://s3.us-east-005.backblazeb2.com/acme-restic/restic/myapp-prod`:

```yaml
apiVersion: b2backup.mcswain.dev/v1
kind: Repository
metadata:
  name: restic
  namespace: myapp-prod
spec:
  mode: Prefix
  bucket: acme-restic
  path: restic/myapp-prod
  passwordSecretRef:
    name: restic-envs
    key: RESTIC_PASSWORD
```

`b2bk migrate` writes this for an app that was backed up by hand; see [docs/b2bk.md](docs/b2bk.md).

#### The credentials Secret

The operator writes a Secret named `<name>-credentials`, or `spec.secretName`, with everything restic needs to reach the repository over B2's S3-compatible API:

| Key | Value |
| --- | --- |
| `RESTIC_REPOSITORY` | `s3:<endpoint>/<bucket>/<path>`, with the endpoint taken from the account |
| `RESTIC_PASSWORD` | The restic password |
| `AWS_ACCESS_KEY_ID` | The repository's own application key ID |
| `AWS_SECRET_ACCESS_KEY` | The repository's own application key |
| `AWS_DEFAULT_REGION` | The region in the endpoint, such as `us-east-005` |

It only ever writes to Secrets it created, which it marks with `app.kubernetes.io/managed-by: b2-backup-operator`.

restic always talks to B2 through the S3-compatible API and never through its native `b2:` backend. restic 0.19.1 pins kurin/blazer v0.7.2, whose upload path retries a B2 upload URL that refuses connections forever, which has hung backups for hours while they held their locks. The region has to be set: without it the S3 client asks B2 for the bucket location first, and on a denied answer it signs for `us-east-1`.

#### The application key

Each Repository gets its own application key, named `b2bk-<namespace>-<name>-<first 12 hex digits of the UID>`, restricted to its bucket, and to its path with a trailing `/` when it has one. Its capabilities are `listBuckets`, `listFiles`, `readFiles` and `writeFiles`. It has no `deleteFiles`: restic's S3 backend never names a version when it deletes, so on B2 every restic delete only hides the file, which needs `writeFiles` alone. A key without `deleteFiles` cannot destroy a file version, so whatever a leaked key hides stays recoverable with the account key until the lifecycle rule below deletes it.

The key's secret half is shown only once, when the key is created, and the operator writes it into the Secret at once and records the key ID in `status.applicationKeyID`. If the Secret is lost, or the key no longer exists in B2, or its restrictions no longer match, the operator creates a new key and writes it. On a later pass, one interval on, it deletes every other key carrying this Repository's name, so a backup that started with the old key can finish. The name ends in the Repository's UID, so this never touches a key that another Repository, or another cluster sharing the account, relies on. The same rule means a Repository that is deleted under `Retain` and created again gets a new key and leaves the old one in B2; delete it by hand once nothing uses the old Secret.

#### The restic password

`passwordSecretRef` seeds the restic password from a Secret you keep, such as a SOPS-encrypted one in Git. The operator reads it and never writes to it, so it stays the off-cluster copy of the key that decrypts the backups.

Without it, the operator generates a password once, and only for a path that holds nothing. Before the repository is initialized it checks for `<path>/config` and for any file version under the path, hidden ones included. If a config exists the repository is adopted (`status.adopted`); if anything at all exists, or this Repository has written credentials before, no password is ever generated and the Repository goes `Ready=False` with reason `PasswordUnavailable` until `passwordSecretRef` is set. Set it whenever the path may already hold snapshots.

#### Old file versions

Because restic's deletes only hide files, a bucket keeps every pack restic has ever pruned until something deletes the hidden versions. By default the operator keeps a bucket lifecycle rule that deletes hidden files under the repository's path one day after they are hidden: scoped to `<path>/` in Prefix mode, and to the whole bucket in Bucket mode without a path.

The rule is merged into the bucket's existing rules. Rules for every other prefix are left exactly as they are, and a rule on a parent prefix that already deletes hidden files at least as soon covers the repository, so no rule is added. The result is in the `LifecycleRule` condition. A rule that hides live files under the path after some days (`daysFromUploadingToHiding`) would eventually hide data that snapshots still need, so it turns the condition `False` with reason `HidesLiveFiles` and `Ready` `False` with reason `LifecycleHidesData`.

```yaml
spec:
  lifecycle:
    daysFromHidingToDeleting: 7   # keep hidden versions a week; default 1
    manage: false                 # or leave the bucket's rules alone entirely
```

A longer `daysFromHidingToDeleting` is the B2 form of append-only: B2 has no restic-server-style append-only mode, and a key without `deleteFiles` does not make one, because restic's lock removal, `forget` and `prune` all succeed by hiding. What the setting buys is a window in which anything a compromised client hid can be restored. For a hard guarantee use B2 Object Lock, which this operator does not manage.

#### Usage

`status.currentUsage` is the total size of the files restic can see under the path, measured at most every six hours by listing them with `b2_list_file_names`, and stopped after 200,000 files. Hidden versions still awaiting deletion are billed but not counted. When the listing stops early, or has never run, the field is empty rather than a guess. `b2bk stats` gives restic's own view.

#### Deletion

`deletionPolicy` defaults to `Retain`: deleting the resource leaves the data, the application key and the credentials Secret in place.

Under `Delete`, deleting the Repository deletes its application key, then every file version and unfinished large file under its path, in batches, and in Prefix mode its lifecycle rule. In Bucket mode it then deletes the bucket, unless the bucket holds files outside the repository's path, in which case it leaves the bucket and emits `BucketRetained`. Nothing outside the path is ever touched, and if another Repository has recorded an overlapping path in the same bucket, only the application key is deleted and the data is left with a `DataRetained` warning. The finalizer is released only once that is done. Annotate the Repository with `b2backup.mcswain.dev/force-finalize=true` to release it anyway; the operator emits a `RepositoryOrphaned` warning naming what it left behind, and takes its ownership off the credentials Secret first, so garbage collection does not delete the only password for the data left behind.

#### Conflicts

Two Repositories whose paths in the same bucket are equal or nested, or one of which covers the whole bucket, would write into one repository. One of them goes `Ready=False` with reason `RepositoryConflict` and makes no B2 calls at all until the conflict is resolved: the one that has not yet recorded the location in status, or, when both or neither have, the newer one. A path that a Repository under `deletionPolicy: Delete` is deleting blocks every other Repository from it until the deletion finishes.

`kubectl explain repository.spec` lists every field.

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
| `RESTIC_REPOSITORY`, `RESTIC_PASSWORD`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_DEFAULT_REGION` | Always. These come from the Repository's Secret |
| `RESTIC_HOST`, `TZ` | Always. Use `spec.resticHost` and `spec.timeZone` |
| `DB_HOST`, `DB_DATABASE`, `DB_USERNAME` | Only under the **mariadb** engine, which is where the operator sets them. A cnpg backup may set `DB_HOST` itself |

The error names each offending variable and the field to use instead. It is raised at reconcile time, so a ScheduledBackup that predates this check goes `Ready=False` rather than quietly shadowing its own credentials. A CEL rule on the CRD rejects the five names from the Repository's Secret at admission as well: they are unconditional, and a CEL rule cannot be relaxed later without a version bump.

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
| `--b2-credentials-secret` | `b2-credentials` | Secret holding the operator's B2 application key, as `name` in the operator's namespace or `namespace/name`. See below |
| `--b2-auth-url` | `https://api.backblazeb2.com` | Where `b2_authorize_account` is called. Only tests change it |
| `--backup-image` | `ghcr.io/usa-reddragon/b2-backup-operator-restic:<operator version>` | The image every backup pod runs. More than a list of binaries; see the contract below |
| `--cache-storage-class` | *(none)* | StorageClass for restic cache volumes. **Cache claims request `ReadWriteMany`**, which most cluster default classes cannot satisfy; see below |

`/manager --help` lists the rest.

### B2 credentials

The operator manages buckets, keys and lifecycle rules through the B2 native API with one application key, read from the Secret named by `--b2-credentials-secret` under `B2_APPLICATION_KEY_ID` and `B2_APPLICATION_KEY`. A Repository can name a Secret of its own in its namespace with `spec.credentialsSecretRef`, with the same two keys.

```sh
kubectl -n b2-backup-operator-system create secret generic b2-credentials \
  --from-literal=B2_APPLICATION_KEY_ID=<keyID> \
  --from-literal=B2_APPLICATION_KEY=<applicationKey>
```

The key needs these capabilities:

| Capability | Used for |
| --- | --- |
| `listBuckets` | Finding the bucket by name |
| `writeBuckets` | Creating a Bucket mode bucket and keeping the lifecycle rule |
| `deleteBuckets` | Deleting a Bucket mode bucket under `deletionPolicy: Delete`. Not needed otherwise |
| `listKeys`, `writeKeys`, `deleteKeys` | Creating, checking and replacing each repository's own key |
| `listFiles` | Detecting an existing repository, measuring usage, and listing what to delete |
| `writeFiles`, `deleteFiles` | Cancelling unfinished large files and deleting file versions under `deletionPolicy: Delete`. Not needed otherwise |

Backblaze documents `writeKeys` as granting full access to the account, since a key that can create keys can create one with any capability. Treat this Secret as the account itself.

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
