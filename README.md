# b2-backup-operator

A Kubernetes operator that creates and adopts [BorgBase](https://www.borgbase.com)
restic repositories, and renders the CronJobs that back up into them.

It replaces per-app, hand-copied backup manifests with two short custom
resources. Only restic-format repositories are in scope; BorgBase also serves
borg repositories, which this operator deliberately does not manage.

## What it does

- **Creates or adopts** a BorgBase repository, recording its opaque ID in
  `status.repositoryID` so the app-to-repository mapping is visible without
  decrypting anything.
- **Generates the restic password exactly once**, writes it alongside a
  synthesized `RESTIC_REPOSITORY` into a Secret, and never rotates it.
- **Initializes the repository** with a one-shot Job that probes before
  initializing, so a genuine failure is not mistaken for success.
- **Renders a CronJob** from a declarative backup spec: sources, retention,
  database credential wiring, pod affinity, cache volume and healthchecks
  reporting are all derived.

## Resources

### Repository

```yaml
apiVersion: b2backup.mcswain.dev/v1
kind: Repository
metadata:
  name: restic
  namespace: myapp-prod
spec:
  # Adopt an existing repository. The operator only ever looks it up: a wrong
  # ID fails loudly rather than provisioning an empty repo beside real backups.
  existingRepositoryID: a1b2c3d4
  passwordSecretRef:
    name: restic-envs
    key: RESTIC_PASSWORD

  # Or omit both of the above to create a new one:
  # region: us
  # quotaGiB: 100
```

#### Two ways to adopt

A repository is adopted whenever it pre-existed the resource, and
`status.adopted` reports it either way:

- **By ID.** `spec.existingRepositoryID` names it outright. The CRD refuses the
  resource unless `passwordSecretRef` comes with it.
- **By name.** With no ID given, the operator looks up `repositoryName`
  (default: the namespace) in BorgBase and adopts the match instead of creating
  a second repository under the same name. Only a lookup: two repositories
  sharing the name are refused rather than picked between.

The CRD cannot require `passwordSecretRef` for the second kind — the match
happens against the BorgBase API at reconcile time, and CEL only sees the spec —
so the operator refuses at reconcile time instead. An adopted repository with no
password in either the credentials Secret or `passwordSecretRef` leaves the
resource `Ready=False` with `has already been provisioned but no password is
available`, rather than generating one the existing snapshots would not open.

So set `passwordSecretRef` whenever the name might already exist in BorgBase:
after a namespace is recreated, after a Repository is deleted under the default
`Retain`, or when `repositoryName` is set by hand to something an older setup
created.

`deletionPolicy` defaults to `Retain`: deleting the resource leaves both the
BorgBase repository and the credentials Secret in place. Set it to `Delete`
only when destroying every snapshot is genuinely what you want.

Under `Delete`, the finalizer is only released once BorgBase has confirmed the
repository is gone, either by accepting the delete or by no longer returning the
repository when asked for it. A repository that cannot be deleted -- an API token
without the scope, say -- holds the finalizer and blocks the namespace from
terminating. Annotate the Repository with
`b2backup.mcswain.dev/force-finalize=true` to release it anyway; the operator
emits a `RepositoryOrphaned` warning naming the repository it left behind, which
is still billed until it is deleted by hand.

`quotaGiB` is binary gibibytes, as the name says. BorgBase's API takes decimal
megabytes, so the operator converts: `quotaGiB: 100` is sent as `107374` MB, and
`status.quota` renders it back as `100.0GiB`. `status.currentUsage` is the same
conversion applied to what BorgBase reports, in MiB below a gibibyte and TiB
above a tebibyte. Expect the BorgBase UI, which shows the raw decimal figure, to
disagree with `b2bk status` by about 7%.

`appendOnly` rejects deletes, so it cannot be combined with a ScheduledBackup
that sets `spec.retention`: the rendered `restic forget --prune` is refused.
Such a backup keeps running and stays `Ready=False` with reason
`AppendOnlyWithRetention`, alongside a Warning event, until one or the other is
cleared. The refusal itself only surfaces once retention first expires a
snapshot, which can be days after the change that caused it.

A backup that supplies its own `spec.script` is not flagged, because
`spec.retention` is not rendered into a script — it is silently unused there,
whether or not the repository is append-only. `b2bk prune` refuses on an
append-only repository regardless.

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
  healthchecks:
    pingKeySecretRef: {name: healthchecks-ping-key, key: PING_KEY}
```

`spec.script` replaces the generated body wholesale for anything the source
types do not cover; it still gets the standard preamble, logging and cache
cleanup.

Two backups in one namespace must set distinct `healthchecks.slug` values. The
slug defaults to the namespace, and sharing one check would let either
backup's success hide the other's failure, so the newer of the two is held
back with `SlugConflict` rather than silently reporting into the same place.

### Pod security

Backup pods drop every capability, forbid privilege escalation, run under the
`RuntimeDefault` seccomp profile and mount no ServiceAccount token.

No user or group id is imposed. A backup has to read the app's own data, whose
ownership varies per app and per cluster, and an `fsGroup` would chown that
data as a side effect of backing it up. A namespace enforcing the restricted
Pod Security Standard supplies what it needs through `spec.podSecurityContext`,
with `spec.containerSecurityContext` for `readOnlyRootFilesystem`. Both replace
the default wholesale.

### Database credentials

`spec.database.secretName` chooses the Secret; it does not choose where the
Secret is mounted. The operator mounts it at one fixed path,
`/var/run/secrets/b2backup/database`, and passes that same path to `dumpdb` and
`restoredb` as `--secret-mount`, so where the credentials are put and where
they are read from cannot drift apart.

#### Schedules

A five-field cron expression is passed through untouched. A shorthand is
expanded with a jitter derived from the resource's namespace and name, so
copy-pasted backups spread across the period instead of all firing on the hour.
The result appears in `status.effectiveSchedule`. The jitter is stable: a
backup keeps its slot unless it is renamed.

Accepted shorthands are `@hourly`, `@daily` (`@midnight`), `@weekly`,
`@monthly`, `@yearly` (`@annually`) and `@every <duration>`. Stepped schedules
are phase-shifted rather than started at zero, so `@every 15m` renders as
`7-59/15 * * * *` rather than `*/15 * * * *`.

Steps that do not divide their period evenly (`@every 7h`) are rejected, since
cron restarts the sequence each period and would leave an irregular gap.

#### Healthchecks

The operator contains no healthchecks API client. `runitor`, already in the
backup image, pings `<apiURL>/<pingKey>/<slug>?create=1`, and healthchecks
auto-provisions the check on first ping with the project's notification
channels attached.

The ping key is **per resource, never cluster-wide**: separate client projects
have separate ping keys, and a shared one would file checks into the wrong
project. The slug defaults to the namespace.

Reporting is best-effort. Misconfigured reporting degrades a backup rather than
blocking it: the CronJob is still created, just without the `runitor` wrapper,
and `Ready=False` with reason `HealthchecksNotConfigured` plus a Warning event
are the only signs that monitoring is gone. It happens when reporting is on but
there is no API URL, when neither `pingKeySecretRef` nor `uuidSecretRef`
resolves, or when no slug can be determined.

`SlugConflict` is the exception among the healthchecks failures: it holds the
CronJob back instead of degrading it, because two backups reporting into one
check is worse than one backup not running yet.

Both degraded reasons are on the same `Ready` condition, so a backup that hits
`AppendOnlyWithRetention` *and* `HealthchecksNotConfigured` keeps the first
reason and gets both messages, joined by `; `. Read the message, not just the
reason.

Auto-created checks get the healthchecks defaults of a one-day period and
one-hour grace. Adopting an existing check preserves its tuned values: set its
`slug` to match, and the first ping returns `200` rather than `201`.

### Extra environment variables

`spec.env` is a plain map of extra variables for the backup container. It is
merged after everything the operator sets, and a container's `env` overrides its
`envFrom` as a whole, so a name the operator already uses would win silently —
sending a backup to another repository, encrypting it under another key, or
redirecting its healthchecks pings.

So `spec.env` is refused when it names one of those. Which names are reserved
depends on the rest of the spec:

| Reserved | When |
| --- | --- |
| `RESTIC_REPOSITORY`, `RESTIC_PASSWORD` | Always. These come from the Repository's Secret |
| `RESTIC_HOST`, `TZ` | Always. Use `spec.resticHost` and `spec.timeZone` |
| `HC_API_URL`, `CHECK_SLUG`, `PING_KEY`, `CHECK_UUID` | Only while healthchecks reporting is on for this backup |
| `DB_HOST`, `DB_DATABASE`, `DB_USERNAME` | Only under the **mariadb** engine, which is where the operator sets them. A cnpg backup may set `DB_HOST` itself |

The error names each offending variable and the field to use instead. It is
raised at reconcile time, so a ScheduledBackup that predates this check goes
`Ready=False` rather than quietly shadowing its own credentials. A CEL rule on
the CRD rejects `RESTIC_REPOSITORY` and `RESTIC_PASSWORD` at admission as well:
those two are unconditional, and a CEL rule cannot be relaxed later without a
version bump.

`RESTIC_GROUP_BY` and `RESTIC_REST_USERNAME`/`RESTIC_REST_PASSWORD` are
deliberately **not** reserved. The operator never sets them; the image's restic
wrapper reads them, and overriding them is a supported thing to do.

### The snapshot host

Every snapshot records a host, and it defaults to the namespace, injected from
the downward API. `spec.resticHost` pins it to something else.

It is more than restic's `--host`. The image's wrapper also derives the dump's
`--stdin-filename` from it, so `resticHost` names the database dump *inside* a
snapshot as well — which is why it is a field rather than a `RESTIC_HOST` entry
in `spec.env`, and why it is validated as a host name.

Set it when a namespace is renamed and the snapshots should stay in one series
rather than splitting into two hosts at the rename. `b2bk` reads the same field:
`b2bk snapshots` defaults its `--host` filter to it, and `b2bk restore
--to-database` looks for the dump under that name, so a pinned `resticHost`
needs no extra flags.

### Where the backup pod runs

The operator derives a pod affinity when the spec implies one. The rules, in
order:

| Spec | Affinity |
| --- | --- |
| `spec.affinity` set | Used verbatim. `{}` is a valid value and means "no affinity at all" |
| `spec.volume` | **Required**: same node as pods labelled `app.kubernetes.io/name: <namespace>`, `app.kubernetes.io/controller: app` |
| `spec.database`, no volume | **Preferred** (weight 100): same node as the database pod |
| Neither | None |

The **volume** case stays a hard requirement on purpose. An RWO claim can only
be mounted on the node already holding it, so a preference there would let the
backup pod be scheduled somewhere the claim cannot follow, and it would sit
Pending. The selector is the sharp edge: it matches on the *namespace* as the
app name, which is the convention these deployments follow, but in a namespace
running more than one app it can pin the backup beside the wrong one. Set
`spec.affinity` explicitly there.

The **database** case is a preference. Co-location makes a dump faster, but it
is not required for correctness, and as a hard rule it left backups Pending
whenever the database happened to be on a full or cordoned node. Set
`spec.database.requireSameNode: true` to get the hard form back.

Which database pod is matched depends on the engine. mariadb selects
`app.kubernetes.io/name: mariadb` plus `app.kubernetes.io/controller: mariadb`;
cnpg selects `cnpg.io/instanceRole: primary` in the cluster named by
`spec.database.cnpgCluster`, which defaults to `postgresql`. A cluster under any
other name has to say so, or the affinity matches nothing — harmless now that it
is a preference, but it silently stops doing anything.

## Configuration

| Flag | Default | Purpose |
| --- | --- | --- |
| `--api-token-secret` | `borgbase-api` | Default BorgBase API token. A bare name resolves in the operator's own namespace, taken from `POD_NAMESPACE` |
| `--api-token-key` | `token` | Key within that Secret |
| `--backup-image` | `ghcr.io/usa-reddragon/b2-backup-operator-restic:<operator version>` | The image every backup pod runs. More than a list of binaries — see the contract below |
| `--cache-storage-class` | *(none)* | StorageClass for restic cache volumes. **Cache claims request `ReadWriteMany`**, which most cluster default classes cannot satisfy — see below |
| `--healthchecks-enabled` | `true` | Report runs via runitor |
| `--healthchecks-api-url` | `http://healthchecks.healthchecks:8000/ping` | Ping endpoint |
| `--healthchecks-auto-create` | `true` | Auto-provision checks on first ping |
| `--borgbase-endpoint` | *(public API)* | Override, for testing |

The BorgBase token needs **Full Access**. `repoAdd` alone is insufficient:
`quotaGiB`, `alertDays` and `appendOnly` are reconciled on every pass with
`repoEdit`, and `repoDelete` is used under the `Delete` policy. Only fields
that actually differ are sent, so a setting changed in the BorgBase UI that
the spec says nothing about is left alone.

### The backup image contract

`spec.image` and `--backup-image` are not just "an image with restic in it". The
default image satisfies a contract the operator and `b2bk` both rely on, and a
substitute that does not will fail quietly rather than loudly.

It must provide `restic`, `runitor`, `ts` and `dumpdb` on PATH. `b2bk restore
--to-database` additionally needs `restoredb`, which it probes for, and a
`/bin/sh` that supports `set -o pipefail`, so a dump that dies mid-stream cannot
be reported as a successful restore. BusyBox ash, bash, zsh and Debian trixie's
dash do; older dash exits with `Illegal option -o pipefail`.

The part that is easy to miss: **`restic` in this image is a wrapper** sitting
ahead of the real binary, and it injects two things that appear nowhere in the
script the operator renders.

- `--group-by=$RESTIC_GROUP_BY`. Without it restic groups a `forget` by
  `host,paths`, and every database source has the same empty path — so two
  database sources in one repository collapse into a single retention group and
  expire each other's snapshots.
- `--stdin-filename` derived from `$RESTIC_HOST`. That is what names the dump
  inside a snapshot, and `b2bk restore --to-database` reads it back by that name.
  Change how it is derived and restores stop finding their dump.

So a custom image without the wrapper does not error. It changes retention
grouping and restore paths, and you find out at the point you need the snapshot.
If you pin `spec.image`, pin a tag of the same image rather than a different one.

### Cache claims and ReadWriteMany

`spec.cache.accessMode` defaults to `ReadWriteMany` unconditionally, and the
claim is created whether or not `--cache-storage-class` names a class that can
provide it. Leaving the flag empty falls back to the cluster's default
StorageClass, which usually cannot: the claim stays `Pending`, the backup pod
never schedules, and nothing about the ScheduledBackup looks wrong — it is
`Ready`, with a CronJob, firing on time into a pod that cannot start.

Two ways out: point `--cache-storage-class` at a class that provides RWX, or set
`spec.cache.accessMode: ReadWriteOnce` per backup. `spec.cache.enabled: false`
drops the claim entirely, at the cost of re-fetching metadata every run.

## Migrating an existing backup

`b2bk migrate` reads an app's hand-written `resources/restic` directory,
decrypts its secret to recover the BorgBase repository ID, and emits the two
resources with the current retention and sources pinned verbatim, and the
schedule handed back to the operator (see below).

```sh
b2bk migrate ../fleet-infra/apps/fennec/myapp/prod/resources/restic > generated.yaml
b2bk parity generated.yaml ../fleet-infra/apps/fennec/myapp/prod/resources/restic/helmrelease.yaml
```

```
RESCHEDULED: "20 0 * * *" -> "33 20 * * *" (every 24h0m0s either way; the operator jittered it)
EQUIVALENT
```

`b2bk parity` renders the generated resource and compares it against the
original. The script must match, ignoring only the optional quoting around
`--exclude` patterns and the `--retry-lock` and `--secret-mount` flags, which
the operator renders and the hand-written script does not. The schedule is compared by
**cadence** rather than by the minute it lands on, because migration
deliberately moves the time; see below. It prints `IDENTICAL` when nothing
moved, `EQUIVALENT` when only the schedule was re-jittered, and fails on
anything else. Run it for every app before cutting it over: migrating must not
silently change what gets backed up, or how often.

### Schedules are handed back to the operator

The hand-written schedules are hand-jittered: someone picked a minute to keep
apps off the top of the hour, and copy-paste meant several apps ended up
sharing one anyway. `b2bk migrate` converts them to the equivalent shorthand, so
the operator derives the offset from a hash of the resource and the spread is
maintained without anyone maintaining it.

| Hand-written | Migrated to |
| --- | --- |
| `36 * * * *` | `@hourly` |
| `20 0 * * *` | `@daily` |
| `27 */6 * * *` | `@every 6h` |

Anything that does not map exactly, such as a specific weekday, is pinned
verbatim and left alone.

This moves when a backup runs, which is the point, but note two things on
cutover day. A backup whose new slot has already passed will wait until the
next one, so a daily backup can go up to a day longer than usual before its
first run under the operator. If its healthchecks check was auto-created with
the default one-day period and one-hour grace, that can trip a late alarm once.
Neither loses data; both are worth expecting rather than being surprised by.

Keep the existing SOPS secret, reduced to `RESTIC_PASSWORD`. The operator reads
it as a seed and never writes to it, so it stays the off-cluster copy of the key
that decrypts the backups. `RESTIC_REPOSITORY` is synthesized by the operator
and `CHECK_UUID` is replaced by slug pinging, so both can be dropped.

## Optional monitoring manifests

`config/prometheus` and `config/network-policy` are deliberately **not** part of
`config/default`, so neither `make deploy` nor the published manifest bundle
installs them. The ServiceMonitor needs the Prometheus Operator's CRDs, and the
NetworkPolicy needs a CNI that enforces policy; including either by default
would break a plain cluster. Apply them yourself, after the operator:

```sh
kustomize build config/prometheus | kubectl apply -f -
kustomize build config/network-policy | kubectl apply -f -
```

Both pin `b2-backup-operator-system`, matching `config/default`. The
ServiceMonitor has no `namespaceSelector`, so it discovers the metrics Service
in its own namespace and must stay in `b2-backup-operator-system`; if you move it,
add a `namespaceSelector` pointing back. Prometheus must also be configured to
select ServiceMonitors from that namespace.

The NetworkPolicy admits `:8443` only from namespaces labelled `metrics: enabled`
— label the one Prometheus runs in — and `:8081` from anywhere, because kubelet
probes arrive from the node rather than from a pod that any selector can match.
`:8081` serves only `/healthz` and `/readyz`.

## Development

```sh
make manifests generate   # regenerate CRDs, RBAC and deepcopy
make test                 # unit tests, no cluster needed
make test-crd             # CRD validation against a real API server (envtest)
make test-e2e             # full deploy against a throwaway Kind cluster
make build-installer IMG=...   # render dist/install.yaml
```

Most of the validation lives in CEL rules on the CRDs, which no Go test can
reach: a rule that fails to compile makes every CRD install fail, and one that
never fires is invisible. `make test-crd` runs them against a real API server.

Releases publish a container image to `ghcr.io/usa-reddragon/b2-backup-operator`
and the rendered manifests as an OCI artifact at
`ghcr.io/usa-reddragon/b2-backup-operator-manifests`, which Flux consumes via an
`OCIRepository`. The same tag builds `b2bk` from `.goreleaser.yml`: archives and
checksums are attached to the GitHub release.

## b2bk, the CLI

`b2bk` runs backups on demand, explains why one is not running, and restores
snapshots. It is the same binary as the `kubectl b2bk` plugin: install it
under both names, and kubectl presents anything called `kubectl-b2bk` on PATH
as a subcommand of its own.

```sh
b2bk doctor -n myapp-prod        # or: kubectl b2bk doctor -n myapp-prod
```

`b2bk` talks to the cluster as you do, and needs more than the operator's own
ServiceAccount holds: `config/rbac/b2bk_user_role.yaml` defines `b2bk-viewer`
for read-only use and `b2bk-operator` for triggering backups and restoring.

See [docs/b2bk.md](docs/b2bk.md) for the full command reference.
