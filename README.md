# b2-backup-operator

[![Release](https://github.com/USA-RedDragon/b2-backup-operator/actions/workflows/release.yaml/badge.svg)](https://github.com/USA-RedDragon/b2-backup-operator/actions/workflows/release.yaml) [![go.mod version](https://img.shields.io/github/go-mod/go-version/USA-RedDragon/b2-backup-operator.svg)](https://github.com/USA-RedDragon/b2-backup-operator) [![License](https://badgen.net/github/license/USA-RedDragon/b2-backup-operator)](https://github.com/USA-RedDragon/b2-backup-operator/blob/main/LICENSE) [![Release](https://img.shields.io/github/release/USA-RedDragon/b2-backup-operator.svg)](https://github.com/USA-RedDragon/b2-backup-operator/releases/)

A Kubernetes operator that backs up apps with [restic](https://restic.net) into [Backblaze B2](https://www.backblaze.com/cloud-storage). Each app gets two short custom resources: a Repository that provisions the restic repository and its credentials, and a ScheduledBackup that renders the CronJob that backs up into it.

## What it does

- **Provisions or adopts** a restic repository in B2, either in a bucket of its own or under a path in a shared bucket, and records the bucket and path in status, so the app-to-repository mapping is visible without decrypting anything. It refuses a path that holds files but no restic repository, and never touches anything outside restic's own files.
- **Gives every repository its own B2 application key**, restricted to its bucket and path, and replaces the key if it is lost or revoked.
- **Generates the restic password exactly once**, writes it with the key and a synthesized `RESTIC_REPOSITORY` into a Secret, and never rotates it.
- **Initializes the repository** with a one-shot Job that probes before initializing, so a genuine failure is not mistaken for success.
- **Renders a CronJob** from a declarative backup spec: sources, retention, database credential wiring, the claims and scratch space to mount, pod affinity and cache volume are all derived.
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

The operator writes a Secret named `<name>-credentials`, or `spec.secretName`, with everything restic needs to reach the repository over B2's S3-compatible API. `spec.secretName` cannot be changed once the Repository exists, other than to or from the default: backups, restores and key rotation all find the repository through that Secret, and a name pointed at another Repository's Secret would send them to its repository.

| Key | Value |
| --- | --- |
| `RESTIC_REPOSITORY` | `s3:<endpoint>/<bucket>/<path>`, with the endpoint taken from the account |
| `RESTIC_PASSWORD` | The restic password |
| `AWS_ACCESS_KEY_ID` | The repository's own application key ID |
| `AWS_SECRET_ACCESS_KEY` | The repository's own application key |
| `AWS_DEFAULT_REGION` | The region in the endpoint, such as `us-east-005` |

It only ever writes to Secrets it created, which it marks with `app.kubernetes.io/managed-by: b2-backup-operator`.

Under `deletionPolicy: Retain`, when the password exists nowhere else, the Secret carries the finalizer `b2backup.mcswain.dev/protect-password`. That is the case when the operator generated the password, and when `passwordSecretRef` names a Secret that now holds a different password, as it does after `b2bk rotate-password`. Deleting the namespace then deletes the Repository but leaves the Secret `Terminating`, with a `PasswordNotArchived` warning on the Repository, and the namespace waits on it. Archive the password, then remove the finalizer:

```sh
kubectl -n myapp-prod get secret restic-credentials -o jsonpath='{.data.RESTIC_PASSWORD}' | base64 -d
kubectl -n myapp-prod patch secret restic-credentials --type=json \
  -p '[{"op":"remove","path":"/metadata/finalizers"}]'
```

A seeded password is taken to be kept off-cluster, which is what seeding is for, so the finalizer is not added, and it is dropped once the seed holds the password again. A seed Secret that does not exist, perhaps not applied yet, confirms nothing, and the finalizer stays. A namespace deletion removes the seed Secret too; at that point the operator goes by what it last saw, and the copy that counts is the one in Git or SOPS. Under `Delete` the Secret is owned by the Repository and carries the finalizer until the operator has deleted the data, so a namespace deletion leaves it `Terminating` while the deletion runs, and a deletion that fails, for example because the B2 credentials went with the namespace, still has the password. Once the data is gone the finalizer comes off and garbage collection deletes the Secret. After a `force-finalize` the Secret is released as under `Retain`.

A Secret left behind this way belongs to the data it was left for. When the Repository releases it, the Secret is annotated with `b2backup.mcswain.dev/released-by` (the Repository's UID) and `b2backup.mcswain.dev/released-repository` (its `RESTIC_REPOSITORY`). While the finalizer stays, no other Repository takes the Secret over: a new Repository whose Secret name points at it, at the same path or another, goes `Ready=False` with reason `SecretHoldsRetainedPassword`, records nothing in status and does not change the Secret. The same goes for an older Secret without those annotations whose `RESTIC_REPOSITORY` is not the new Repository's. Archive the password and remove the finalizer to let the new Repository reuse the Secret, or set `spec.secretName` to another name. A Repository never removes a finalizer that another Repository set; `b2backup.mcswain.dev/protected-by` records which one did.

restic always talks to B2 through the S3-compatible API and never through its native `b2:` backend. restic 0.19.1 pins kurin/blazer v0.7.2, whose upload path retries a B2 upload URL that refuses connections forever, which has hung backups for hours while they held their locks. The region has to be set: without it the S3 client asks B2 for the bucket location first, and on a denied answer it signs for `us-east-1`.

#### The application key

Each Repository gets its own application key, named `b2bk-<namespace>-<name>-<first 12 hex digits of the UID>`, restricted to its bucket, and to its path with a trailing `/` when it has one. Its capabilities are `listBuckets`, `listFiles`, `readFiles` and `writeFiles`. It has no `deleteFiles`: restic's S3 backend never names a version when it deletes, so on B2 every restic delete only hides the file, which needs `writeFiles` alone. A key without `deleteFiles` cannot destroy a file version, so whatever a leaked key hides stays recoverable with the account key until the lifecycle rule below deletes it.

The key's secret half is shown only once, when the key is created, and the operator writes it into the Secret at once and records the key ID in `status.applicationKeyID` and the time in `status.keyRotationTime`. If the Secret is lost, or the key no longer exists in B2, or its restrictions no longer match, the operator creates a new key and writes it. It then deletes every other key carrying this Repository's name, but only once no Job in the namespace that reads the credentials Secret and started before the rotation is still running: scheduled and triggered backups, `b2bk` restores and shells, and the init Job alike. A pod reads the Secret when it starts, so such a Job may still hold the old key, and a pod started in the first moments after the rotation may have read the old key from the kubelet's cache, so no old key is deleted until 10 minutes after the rotation. After 24 hours the old keys are deleted regardless, with a `StaleKeyDeadline` warning naming the Jobs still running. The name ends in the Repository's UID, so this never touches a key that another Repository, or another cluster sharing the account, relies on. The same rule means a Repository that is deleted under `Retain` and created again gets a new key and leaves the old one in B2; delete it by hand once nothing uses the old Secret.

#### The restic password

`passwordSecretRef` seeds the restic password from a Secret you keep, such as a SOPS-encrypted one in Git. The operator reads it and never writes to it, so it stays the off-cluster copy of the key that decrypts the backups. A credentials Secret that is already there wins over the seed only when it is this Repository's own: it names this repository in `RESTIC_REPOSITORY` or was written by this Repository. One left behind by another Repository at another path never supplies the password; the seed does, or a new password when the path is empty.

Without it, the operator generates a password once, and only for a path that holds nothing. Before the repository is initialized it checks for `<path>/config` and for any file version under the path, hidden ones included. If a config exists the repository is adopted (`status.adopted`), and its password must come from `passwordSecretRef`: no password is ever generated for it, or for a path this Repository has written credentials for before, and the Repository goes `Ready=False` with reason `PasswordUnavailable` until `passwordSecretRef` is set. Set it whenever the path may already hold snapshots.

A path that holds any file version but no `config` is not a restic repository the operator can adopt. Perhaps it is the parent of other repositories, or another tool's files. The Repository goes `Ready=False` with reason `ForeignData` and the operator does nothing else there: no key, no Secret, no lifecycle rule, no init Job, and no location recorded in status. The same goes for a Bucket mode Repository whose bucket already holds files but no `config` at its root. Point `spec.path` somewhere else, or empty the path.

#### Old file versions

Because restic's deletes only hide files, a bucket keeps every pack restic has ever pruned until something deletes the hidden versions. The operator relies on a bucket lifecycle rule for that, and is careful never to shorten a window someone else set.

B2 applies every lifecycle rule whose `fileNamePrefix` matches a file, taking the smallest non-null value of each setting. So a rule for a parent prefix already governs a repository below it, and a more specific rule can only shorten the window, never lengthen it. With that in mind:

- **A rule on the repository's path or any parent of it that sets `daysFromHidingToDeleting` covers the repository**, whatever its value. The operator adds no rule, and the `LifecycleRule` condition is `True` with reason `CoveredByExistingRule`, naming the rule and its days. A shared bucket with a rule for `k8s/` of 30 days keeps every repository under `k8s/` at 30 days.
- **Only a path no rule covers gets a rule**, for exactly `<path>/` in Prefix mode or the whole bucket in Bucket mode without a path, deleting hidden files after 30 days. The condition reason is then `Managed`. A rule for the whole bucket would delete the hidden versions of every file in it, so it is added only while every file version in the bucket, hidden ones included, is part of restic's layout (`config`, `keys/`, `data/`, `index/`, `snapshots/`, `locks/`). An adopted bucket holding anything else gets no rule, and the condition is `False` with reason `OtherFilesInBucket`, naming one such file.
- **`spec.lifecycle.daysFromHidingToDeleting` opts in to a rule of the repository's own**, for exactly its path, even under a parent rule. Because B2 takes the minimum, a value shorter than the parent's shortens the window for this repository; a longer one has no effect, and the condition says so.
- A rule for exactly the path that sets nothing but `daysFromHidingToDeleting`, such as one an older release of the operator added, is removed when a parent rule covers the path and no value is set explicitly. Removing it can only lengthen the window. A rule carrying any other setting is someone else's and is left alone.

Every other rule in the bucket, and every field of it, is written back exactly as read, including fields this operator does not know about. A rule that hides live files under the path after some days (`daysFromUploadingToHiding`) would eventually hide data that snapshots still need, so it turns the condition `False` with reason `HidesLiveFiles` and `Ready` `False` with reason `LifecycleHidesData`.

```yaml
spec:
  lifecycle:
    daysFromHidingToDeleting: 7   # a rule of this repository's own; unset, a covering rule or 30 days
    manage: false                 # or leave the bucket's rules alone entirely
```

A longer window is the B2 form of append-only: B2 has no restic-server-style append-only mode, and a key without `deleteFiles` does not make one, because restic's lock removal, `forget` and `prune` all succeed by hiding. What the window buys is time in which anything a compromised client hid can be restored. For a hard guarantee use B2 Object Lock, which this operator does not manage.

#### Usage

`status.currentUsage` is the total size of the files restic can see under the path, measured at most every six hours by listing them with `b2_list_file_names`, and stopped after 200,000 files. Hidden versions still awaiting deletion are billed but not counted. When the listing stops early, or has never run, the field is empty rather than a guess. `b2bk stats` gives restic's own view.

#### Deletion

`deletionPolicy` defaults to `Retain`: deleting the resource leaves the data, the application key and the credentials Secret in place, and takes the Repository's ownership off the Secret first, so garbage collection cannot take the only password.

Under `Delete`, deleting the Repository deletes its application key, then the restic repository at its path, in batches, and in Prefix mode the lifecycle rule it added. Only restic's own layout is deleted: `<path>/config` and everything under `<path>/keys/`, `data/`, `index/`, `snapshots/` and `locks/`, hidden versions and unfinished large files included. Any other file under the path is left, with an `OtherFilesRetained` warning. In Bucket mode the bucket is deleted only if it is then completely empty; otherwise it is left with `BucketRetained`.

Status is not trusted to say what to delete. Before deleting anything the operator checks that `status.bucketName` is `spec.bucket`, that `status.path` is the path the spec resolves to and is not empty in Prefix mode, and that the bucket's ID in B2 is still `status.bucketID`. If any of that does not hold, it deletes nothing, keeps the finalizer, and reports `Ready=False` with reason `DeletionScopeMismatch`. A bucket the credentials cannot find counts as already gone only when they belong to the B2 account recorded in `status.accountID`, since a bucket in another account is invisible rather than deleted; otherwise, and for a Repository that has not recorded its account yet, the reason is `AccountMismatch`. A Repository records its account on its next reconcile, and refuses to reconcile with credentials for another account. If another Repository has recorded an overlapping path in the same bucket, only the application key is deleted and the data is left with a `DataRetained` warning; the credentials Secret is then released as under `Retain`, so the password for the data left behind is kept.

The finalizer is released only once all of that is done. Annotate the Repository with `b2backup.mcswain.dev/force-finalize=true` to release it anyway: the operator emits a `RepositoryOrphaned` warning naming what it left behind and deletes nothing more. It never widens what it deletes to get the finalizer off.

`spec.deletionPolicy` can be changed at any time by anyone who can patch the Repository, including the `b2bk-operator` role, which patches Repositories for `b2bk suspend` and `b2bk reinit`. RBAC cannot restrict which fields a patch changes, and a validation rule that froze the policy would also stop an administrator. Bind `b2bk-operator` only to people trusted with the data, which it already gives them through the credentials Secret.

#### Conflicts

Two Repositories whose paths in the same bucket are equal or nested, or one of which covers the whole bucket, would write into one repository. Two Repositories in one namespace naming the same credentials Secret would overwrite each other's `RESTIC_REPOSITORY` and take each other's password, so they conflict too. One of them goes `Ready=False` with reason `RepositoryConflict` and makes no B2 calls at all until the conflict is resolved: the one that has not yet recorded the location in status, or, when both or neither have, the newer one. A path that a Repository under `deletionPolicy: Delete` is deleting blocks every other Repository from it until the deletion finishes.

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
    - {type: files, tag: files, paths: [app], exclude: ["**/temp*"]}
  retention: {hourly: 168, daily: 90, monthly: 24, yearly: 10}
  database: {engine: cnpg, secretName: postgresql-app}
  volumes:
    - {existingClaim: myapp-prod-storage, mountPath: /data}
```

`spec.volumes` lists the existing claims to mount, each at its own `mountPath` (default `/<existingClaim>`), optionally read-only. `spec.scratch` adds one empty directory, `{mountPath, sizeLimit}`, for a script to stage files in. The container's working directory is `spec.workingDir`, or the first volume's mount path, or the scratch space, or `/`.

A files source backs up `paths` in one snapshot, each absolute or relative to the working directory, and each on one of the volumes or the scratch space. The operator always renders them absolute, as `restic backup --tag=files /data/app`, so restic records `/data/app` and stores the files under it. `b2bk restore` relies on that to put them back where they were. Several paths in one source make one snapshot of all of them, as `restic backup /data /blobs` does.

After the sources, the operator runs `restic forget --prune` with `spec.retention`, scoped with `--host="$RESTIC_HOST"` and one `--tag` per source tag, and grouped with `--group-by=tags`. restic ORs repeated `--tag` flags, and the image groups by tags, so this backup's retention applies to its own snapshots only, and two ScheduledBackups writing into one repository with different retention do not expire each other's snapshots.

Each source needs a tag of its own: the image groups `forget` by tags, so two sources under one tag would share one retention group, and the policy would keep one snapshot per period across both and expire the other source's. A ScheduledBackup whose sources share a tag is refused, and two ScheduledBackups writing into the same Repository on the same host under one tag both go `Ready=False` with reason `TagConflict`, with their CronJobs suspended, until one of them changes. Both stop, not only the newer, because the one edited into the conflict may be the older. A CronJob that cannot be brought in line with its ScheduledBackup, because the Repository is missing or not initialized, the spec is rejected, or the cache claim or CronJob write fails, is suspended too, and resumes once a reconcile succeeds, so it never goes on forgetting under a spec that no longer exists. A CronJob is suspended too, with reason `RepositoryNotItsOwn`, while its Repository is `Ready=False` for a reason that means its credentials Secret or location may belong to something else (`RepositoryConflict`, `SecretHoldsRetainedPassword`, `AccountMismatch`, `SecretNotManaged`, `BucketReplaced`), or while `spec.secretName` is not the Secret the Repository recorded. A `spec.script` backup must be the only ScheduledBackup on its Repository: the shell can build any restic command at run time, with any `--tag` or `--host`, so what its `forget` reaches cannot be read from the script, and it conflicts with any other ScheduledBackup there. Give it a Repository of its own.

The check also covers what the specs no longer show. Each backup Job carries its forget scope (Repository, host and tags) in the `b2backup.mcswain.dev/forget-scope` annotation, copied from the CronJob's job template, so a Job that started before its ScheduledBackup's tags changed still runs its old `forget` once, and a ScheduledBackup that now uses one of those tags is held at `TagConflict`, with its CronJob suspended, until that Job finishes. A CronJob the operator rendered that no live ScheduledBackup owns, as `kubectl delete --cascade=orphan` leaves it, is suspended whenever the operator reconciles a ScheduledBackup in its namespace, since it would otherwise go on backing up and forgetting under a spec that no longer exists; `b2bk prune` and the overwriting restores count one that is still running as a conflict, on its recorded scope or on everything when it has none. One of a ScheduledBackup's own name is instead suspended whenever that spec cannot be rendered, and adopted once it can. A namespace with no ScheduledBackup left is not reconciled, so an orphan there runs until it is deleted. One window remains: a Job the old CronJob starts in the seconds between an edit and the operator re-rendering that CronJob, while the ScheduledBackup that took over the tag is already running its own Job. The old Job's creation makes the operator re-check the other ScheduledBackups, which suspends the newer side's CronJob but cannot stop a Job of it that is already running.

`spec.script` replaces the generated body wholesale for anything the source types do not cover; it still gets the standard preamble, logging and cache cleanup. `spec.retention` is refused beside it: the script forgets on its own, and should scope its `forget` with `--tag`.

The body runs as `set -o pipefail; { set -euo pipefail; ...; restic cache --cleanup; } 2>&1 | ts`, so every line is timestamped, a failure anywhere fails the run with restic's own exit status, a failed command piped into restic fails it too, and the output of a run that fails at once is not lost.

### Pod security

Backup pods drop every capability but `DAC_OVERRIDE`, forbid privilege escalation, run under the `RuntimeDefault` seccomp profile and mount no ServiceAccount token. The backup image runs as root, and `DAC_OVERRIDE` is what lets root read a directory another user owns with mode `0700`, as the app's own container, with the runtime's default capabilities, could; without it such files are left out of every snapshot. `DAC_READ_SEARCH` would be enough, but the baseline Pod Security Standard does not allow it. `b2bk restore --in-place` and `--to-new-pvc` add `CHOWN`, `DAC_OVERRIDE`, `FOWNER` and `FSETID` to their own Job, which restic needs to recreate files with their owners and modes.

No user or group id is imposed. A backup has to read the app's own data, whose ownership varies per app and per cluster, and an `fsGroup` would chown that data as a side effect of backing it up. A namespace enforcing the restricted Pod Security Standard supplies what it needs through `spec.podSecurityContext`, with `spec.containerSecurityContext` for `readOnlyRootFilesystem`. Both replace the default wholesale.

### Database credentials

`spec.database.secretName` chooses the Secret; it does not choose where the Secret is mounted. The operator mounts it at one fixed path, `/var/run/secrets/b2backup/database`, and passes that same path to `dumpdb` and `restoredb` as `--secret-mount`, so where the credentials are put and where they are read from cannot drift apart.

A mariadb restore (`b2bk restore --to-database`) stages the dump in `<db>__b2bk_restore` and swaps it in with one atomic `RENAME TABLE`, keeping the replaced tables in `<db>__b2bk_old`. The user in the Secret therefore needs `ALL` on those two databases as well as `ALTER`, `DROP`, `CREATE` and `INSERT` on `<db>`, and the server must be MariaDB 10.5 or later, since restoredb marks the two databases with a comment and never drops one without it. A database or dump with views or triggers is refused. See [docs/b2bk.md](docs/b2bk.md).

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

`RESTIC_GROUP_BY` and `RESTIC_REST_USERNAME`/`RESTIC_REST_PASSWORD` are deliberately **not** reserved. The operator never sets them; the image's restic wrapper reads them, and overriding them is a supported thing to do. The operator's own `forget` passes `--group-by=tags` regardless, so an override cannot merge two sources into one retention group: grouped by host alone, the policy would keep one snapshot per period across every source and expire the rest.

### The snapshot host

Every snapshot records a host, and it defaults to the namespace, injected from the downward API. `spec.resticHost` pins it to something else.

It is more than restic's `--host`. The image's wrapper also derives the dump's `--stdin-filename` from it, so `resticHost` names the database dump *inside* a snapshot as well. That is why it is a field rather than a `RESTIC_HOST` entry in `spec.env`, and why it is validated as a host name.

Set it when a namespace is renamed and the snapshots should stay in one series rather than splitting into two hosts at the rename. `b2bk` reads the same field: `b2bk snapshots` defaults its `--host` filter to it, and `b2bk restore` picks the latest snapshot taken under it, so a pinned `resticHost` needs no extra flags.

### Where the backup pod runs

The operator derives a pod affinity when the spec implies one. The rules, in order:

| Spec | Affinity |
| --- | --- |
| `spec.affinity` set | Used verbatim. `{}` is a valid value and means "no affinity at all" |
| `spec.volumes` | **Required**: same node as pods labelled `app.kubernetes.io/name: <namespace>`, other than the operator's own backup pods. One rule, however many claims |
| `spec.database`, no volumes | **Preferred** (weight 100): same node as the database pod |
| Neither | None |

The **volumes** case stays a hard requirement on purpose. An RWO claim can only be mounted on the node already holding it, so a preference there would let the backup pod be scheduled somewhere the claim cannot follow, and it would sit Pending. The selector is the sharp edge: it matches on the *namespace* as the app name, which is what bjw-s app-template sets when the HelmRelease is named after its namespace. It does not look at `app.kubernetes.io/controller`, so it matches every controller in that release, and in a namespace running more than one app it can pin the backup beside the wrong one. Set `spec.affinity` explicitly there.

The **database** case is a preference. Co-location makes a dump faster, but it is not required for correctness, and as a hard rule it left backups Pending whenever the database happened to be on a full or cordoned node. Set `spec.database.requireSameNode: true` to get the hard form back.

Which database pod is matched depends on the engine. mariadb selects `app.kubernetes.io/name: mariadb` plus `app.kubernetes.io/controller: mariadb`; cnpg selects `cnpg.io/instanceRole: primary` in the cluster named by `spec.database.cnpgCluster`, which defaults to `postgresql`. A cluster under any other name has to say so, or the affinity matches nothing. That is harmless now that it is a preference, but the affinity silently stops doing anything.

`spec.nodeSelector`, `spec.tolerations` and `spec.priorityClassName` go onto the backup pod as given, beside whatever affinity applies, and onto the one-off Jobs `b2bk` starts from it.

## Migrating hand-written restic CronJobs

`b2bk migrate` turns an app-template HelmRelease with a `restic` CronJob controller into a Repository that adopts the existing repository in place, plus a ScheduledBackup. `b2bk parity` then checks the result against the HelmRelease. Neither changes anything; see [docs/b2bk.md](docs/b2bk.md) for every flag.

```sh
b2bk migrate apps/myapp/resources/restic --from-secret ../secrets/myapp-restic.yaml > generated.yaml
b2bk parity generated.yaml apps/myapp/resources/restic --from-secret ../secrets/myapp-restic.yaml
```

What to expect:

- **The Repository** is in Prefix mode at the bucket and path of the old `RESTIC_REPOSITORY`, reads its password from `restic-envs`, keeps `deletionPolicy: Retain`, and has no `lifecycle` block. A bucket rule over the path, such as one for `k8s/`, covers it, and the operator adds nothing.
- **Every claim** the restic container mounts becomes an entry in `spec.volumes`, with the same path and read-only flag, and an `emptyDir` becomes `spec.scratch`. The working directory is carried over when it is not the default.
- **Each `restic backup`** of absolute paths on those mounts becomes a source, several paths staying one snapshot. Anything else, such as a script that copies a SQLite database into an `emptyDir` first, or a line the shell would rewrite (an escape, a variable, an unquoted glob), is kept verbatim as `spec.script`.
- **The pod's scheduling** (`affinity`, `nodeSelector`, `tolerations`, `priorityClassName`), its labels, `suspend` and the container's `resources` are carried over. Anything `migrate` cannot carry, such as pod annotations, `backoffLimit`, another `envFrom` Secret or an unknown value, is refused rather than dropped. With `--from-secret`, other `RESTIC_*` keys in `restic-envs`, such as `RESTIC_COMPRESSION`, go into `spec.env`; without it, `migrate` and `parity` say those keys went unchecked.
- **The container's security context** is carried over, and `parity` reports `READ ACCESS DIFFERS` if the old container could read every file on its claims and the new one cannot.
- **A hand-jittered daily schedule** becomes `@daily`, which the operator jitters itself. `parity` reports the move as `RESCHEDULED`, with the old and new time of day in the schedule's time zone, and the first run after cutover may come up to a day later.
- **`backup-cnpg.sh` becomes `dumpdb cnpg`.** Old snapshots hold plain SQL stored as `/<namespace>`; new ones hold pg_dump's custom format stored as `/<namespace>.dmp`. `b2bk restore --to-database` restores either, and `parity` reports the switch as `DUMP FORMAT CHANGES`.
- **`forget` becomes scoped** to the backup's own tags and host. `parity` notes it as `FORGET SCOPED`: snapshots under tags the backup no longer takes are not expired by it any more.

`parity` compares the CronJob the operator would build, through its own validation, with the CronJob the HelmRelease renders, which it renders the way app-template does without going through `migrate`. It exits 0 only when everything else matches: the script after normalization, cadence, time zone, image, claims and scratch space, working directory, snapshot host, environment, database Secret, resources, scheduling constraints, labels, other containers and controllers, the rest of the pod spec, the namespace, the Secret and key the Repository reads the password from and, given `--repository` or `--from-secret`, the repository's bucket and path; without them it notes the location went unchecked. Job retry and history settings, the image switch, an added affinity preference and the dropped `restic init` Job are printed as notes.

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

- `--group-by=$RESTIC_GROUP_BY`, for `backup`, `forget` and `snapshots` when no `--group-by` is given. Without it restic groups a `forget` by `host,paths`, and every database source has the same empty path, so two database sources in one repository collapse into a single retention group and expire each other's snapshots. The image sets `RESTIC_GROUP_BY=tags`, and `RESTIC_CACHE_DIR=/cache`, where the operator mounts the cache claim.
- `--stdin-filename` derived from `$RESTIC_HOST`. That is what names the dump inside a snapshot (`<host>.dmp` for cnpg, `<host>.sql` for mariadb). `b2bk restore --to-database` reads the name back from the snapshot itself, so a dump stored under another name, such as the plain SQL `backup-cnpg.sh` wrote as `<host>`, still restores.

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
