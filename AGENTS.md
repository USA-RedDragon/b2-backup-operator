# b2-backup-operator - AI Agent Guide

A Kubernetes operator that provisions or adopts restic repositories in
Backblaze B2 and renders the CronJobs that back up into them. Read the README
first: it explains what the two resources are for and why the defaults are what
they are.

## Layout

```
cmd/main.go                    Manager entry and operator-level flags
cmd/kubectl-b2bk/              b2bk CLI entry, also the kubectl plugin
cmd/restoredb/                 restoredb, added to the backup image
api/v1/*_types.go              CRD schemas (+kubebuilder markers)
api/v1/zz_generated.*          Auto-generated (DO NOT EDIT)
internal/controller/           Reconciliation for both kinds
internal/backup/               Rendering: CronJob, schedule, backup script
internal/b2/                   B2 native API client for management calls
internal/b2/b2fake/            In-memory B2 API, used by unit tests and the e2e stub
internal/dbtools/              Shared code for restoredb
internal/secrets/              Restic password generation
internal/cli/                  b2bk commands, including migrate and parity
internal/cli/runner/           Ephemeral Jobs for one-off restic commands
config/crd/bases/              Generated CRDs (DO NOT EDIT)
config/rbac/role.yaml          Generated RBAC (DO NOT EDIT)
config/rbac/b2bk_user_role.yaml Roles for people running the CLI
config/samples/                Example CRs, checked by make test-crd
Dockerfile                     Manager image
Dockerfile.restic              Backup image: ghcr.io/gabe565/restic plus restoredb
PROJECT                        Kubebuilder metadata (DO NOT EDIT)
```

There is no webhook and no multi-group layout. Both kinds live in `api/v1`.

## What this code is protecting

Nearly every non-obvious choice here exists to avoid destroying or orphaning
backups. Before changing something that looks redundant, check whether it is
one of these:

- **The restic password is generated once and never rotated.** It is the
  encryption key for every snapshot. The controller refuses to invent one for a
  repository that has been recorded, initialized, or reports any usage.
- **Adoption never generates a password.** A path holding a restic `config` gets
  its password from `passwordSecretRef` or not at all. Prefix mode never creates
  a bucket, and a recorded bucket that vanished or changed ID is never recreated.
- **A path with files but no `config` is foreign.** Before the Repository has
  provisioned anything, such a path (or a Bucket mode bucket with files but no
  root `config`) stops it at `Ready=False` reason `ForeignData`: no key, Secret,
  lifecycle rule, init Job or recorded location. It may be the parent of other
  repositories in a shared bucket.
- **restic only talks to B2 over S3.** restic 0.19.1's native `b2:` backend
  (blazer v0.7.2) retries a refused upload URL forever and hangs backups while
  they hold locks. The native API in `internal/b2` is for management calls only.
- **Nothing outside a Repository's own restic repository is touched.**
  `b2.DeleteResticRepository` deletes only `<path>/config` and `keys/`,
  `data/`, `index/`, `snapshots/` and `locks/` under it; a Bucket mode bucket
  is deleted only when it is then empty. Key deletion matches a key name that
  embeds the Repository's UID, never an ID from status.
- **Status is not trusted to scope deletion.** `deletionScope` requires
  `status.bucketName == spec.bucket`, `status.path == EffectivePath()` and a
  non-empty Prefix path, and `deleteRemote` re-reads the bucket and checks its
  ID. A bucket it cannot find is taken as gone only when the credentials
  belong to `status.accountID` (`AccountMismatch` otherwise, including when
  none is recorded). Otherwise nothing is deleted and the finalizer stays;
  `force-finalize` only ever deletes less. Status is written by the operator
  alone: `b2bk reinit` goes through the `reinit-at` annotation and the
  `b2bk-operator` role has no status write access.
- **The lifecycle rule never shortens anyone's window.** A rule on the path or a
  parent that sets `daysFromHidingToDeleting` covers the repository whatever its
  value (`CoveredByExistingRule`); only an explicit
  `spec.lifecycle.daysFromHidingToDeleting` adds a rule under one. B2 takes the
  minimum of overlapping rules. A Bucket mode rule for the whole bucket (empty
  prefix) is added only while every version in the bucket is restic's layout
  (`b2.FirstOutsideLayout`), since it would purge other files' hidden versions;
  otherwise `OtherFilesInBucket`. Prefix mode keeps one rule per path: B2
  allows 100 rules per bucket. `b2.LifecycleRule` keeps unknown fields in
  `Extra` and writes them back, so a read-modify-write never drops another
  tool's settings.
- **`deletionPolicy` defaults to Retain**, and the credentials Secret only gets
  an ownerReference under `Delete`. Every finalize path that leaves data behind
  takes the ownerReference off first. Under Retain, a Secret holding the only
  copy of the password (generated, or a seed that no longer matches) carries the
  `protect-password` finalizer, so a namespace deletion cannot take it. Under
  `Delete` it carries the finalizer until the remote deletion has succeeded.
  A Repository never removes that finalizer when another Repository set it
  (`protected-by`, `released-by` annotations), and never takes over a Secret
  carrying it for another Repository or another `RESTIC_REPOSITORY`: it stops
  at `SecretHoldsRetainedPassword` before recording anything in status.
- **The operator only writes to Secrets it created**, identified by the
  `app.kubernetes.io/managed-by` label.
- **The B2 credentials are resolved lazily**, so a Retain deletion works without them.
- **The per-repository key's secret is shown once.** It is written into the
  Secret straight away and its ID and `keyRotationTime` recorded in status in
  the same pass; a key that could not be saved is deleted again. Superseded keys
  are deleted only once no Job that reads the Secret and started before the
  rotation is running, or after 24 hours.
- **Status is written with a merge patch**, never a full update: the informer
  cache lags behind the controller's own writes.
- **The init Job is deleted a pass after success is recorded**, not in the same
  one, or the watch event races the status write.
- **`spec.env` may not name a variable the operator sets.** A container's `env`
  overrides its `envFrom` wholesale, so `RESTIC_REPOSITORY`, `RESTIC_PASSWORD`
  or an `AWS_*` variable in `spec.env` would send a backup to another repository
  or encrypt it under another key. `buildEnv` rejects the whole set it built,
  plus the five keys of the Repository's Secret;
  `RESTIC_GROUP_BY` and `RESTIC_REST_*` stay usable because the image's restic
  wrapper -- not the operator -- reads them.
- **Files sources are rendered with absolute paths.** restic stores the tree
  under the path as given: `restic backup /data` stores `/data/...`, while
  `cd /data; restic backup .` stores the contents at the root. `b2bk restore`
  restores `<snapshot>:<path>` for each recorded path, which only works for the
  absolute form, so never render a relative path.
- **`forget` is scoped by tag and host, and tags are unique.** The image groups
  by tags, and restic ORs repeated `--tag` flags, so a ScheduledBackup's
  retention only reaches its own snapshots. `b2bk prune` builds the same flags
  in `pruneArgs`. Two sources, or two ScheduledBackups on one Repository, under
  one tag would share a forget group and expire each other's snapshots, so
  `backup.Validate` and the `TagConflict` condition refuse them. A script backup
  must be alone on its Repository: no parser can tell what a shell script will
  forget, under which tag or host, so `tagConflict` does not try.
  `backup.FindConflict` also reads the `forget-scope` annotation of running
  Jobs and of CronJobs no live ScheduledBackup owns, since a Job keeps the
  scope it started with and an orphaned CronJob is reconciled by nobody.
- **Backups read as root with `DAC_OVERRIDE`.** The image runs as root; with
  every capability dropped, root cannot read another user's `0700` directory
  and restic saves an incomplete snapshot. Restore Jobs add `CHOWN`,
  `DAC_OVERRIDE`, `FOWNER` and `FSETID` (`runner.RestoreCapabilities`).
- **Restores never pick the wrong snapshot or delete what was never saved.**
  `restoreOptions.chooseSnapshot` matches tag, host and exact paths; dumps must
  be one file and one series. Restores pass the source's excludes, so
  `--delete` leaves them; restic refuses includes and excludes together, which
  `restoreArgs` handles. Verify any change against restic 0.19.1 in the image.
- **A restore that overwrites is held until its target is whole.** b2bk sets
  `restore-lock` on every ScheduledBackup sharing the claims or database, and
  a failed restore leaves `restore-partial`; `backup.RestoreHold` makes the
  operator suspend their CronJobs and skip triggers whatever `spec.suspend`
  says. Only a complete restore of the same target, or `b2bk resume`, clears a
  partial entry.
- **A plain-SQL restore commits only a complete dump.** psql
  `--single-transaction` commits at end of input, so `restoredb` runs it through
  `dumpdb.RunOpts.Complete` and kills it before closing stdin unless the dump
  ends with pg_dump's `dump complete` trailer. A `COMMIT` inside the dump would
  commit early, so `postgres.PlainFilter` follows the dump with a copy of
  psql's lexer rules, comments out pg_dump's own `BEGIN;`/`COMMIT;` lines around
  large objects and refuses any other transaction control. Verify changes with
  `make test-postgres`, which runs `restoredb` inside a real Postgres container.
- **A MariaDB restore never touches the live database until the swap.**
  mariadb-dump output cannot run in a transaction, so `restoredb mariadb`
  restores into `<db>__b2bk_restore` with the dump's `USE` and `CREATE
  DATABASE` lines left out, checks the `-- Dump completed` trailer, and only
  then moves every table with one multi-table `RENAME TABLE`, which MariaDB
  applies atomically. MariaDB refuses to move views and tables with triggers
  across databases, so those are refused up front. `<db>__b2bk_old` is the
  rollback copy: never drop it unless it is empty or `--drop-old` was given.
  Verify changes with `make test-mariadb`, which runs a real server in Docker.
- **The backup image's `restic` is a wrapper** ahead of the real binary on PATH.
  It supplies `--group-by` from `RESTIC_GROUP_BY` and the `--stdin-filename` a
  database dump lands under from `RESTIC_HOST`, neither of which is in the
  rendered script. `b2bk restore --to-database` reads the filename back from
  the snapshot's recorded path, and `restoredb` tells the dump's format by its
  first bytes. See the image contract in the README before changing
  `spec.image` handling or `internal/backup/script.go`.

## Generated files

After editing `*_types.go` or any marker:

```sh
make manifests generate
```

Never hand-edit `config/crd/bases/*`, `config/rbac/role.yaml` or
`**/zz_generated.*`. Do not delete `// +kubebuilder:scaffold:*` comments.

## Tests

```sh
make test        # unit tests: plain go test, no cluster, no envtest binaries
make test-crd    # CRD validation against a real API server (envtest)
make test-e2e    # full deploy against a throwaway Kind cluster
make test-mariadb # restoredb mariadb against a throwaway MariaDB container
make test-postgres # restoredb postgres against a throwaway Postgres container
make lint        # golangci-lint with the logcheck plugin
```

golangci-lint v2.13.2 cannot read the export data of Go 1.27.2. Run
`GOTOOLCHAIN=go1.27.1 make lint` when the local Go is newer than go.mod.

Controller tests run the real `internal/b2` client against `b2fake` over
httptest, so they exercise the HTTP layer too. A B2 call the operator starts
making needs a handler in `b2fake`; the e2e suite fails on any call the stub
does not recognize.

Tests are standard `testing`, not Ginkgo, except the e2e suite: it is Ginkgo, and
its `test/e2e/e2e_suite_test.go` is the only `suite_test.go` in the tree. Both it
and the envtest CRD tests sit behind build tags (`e2e` and `envtest`), as do the
Docker MariaDB and Postgres tests (`mariadb`, `postgres`), so `make test` -- a
plain `go test` over everything but `/e2e` -- compiles none of them.

**Validation lives in CEL on the CRDs**, so `make test` cannot see it. A rule
that fails to compile makes every CRD install fail, and one that never fires is
invisible. Anything added under `+kubebuilder:validation:XValidation` needs a
case in `internal/controller/crd_validation_envtest_test.go`.

CEL gotcha: `\.` is not a valid escape in a CEL string literal. Use a character
class, `[.]`, instead.

## Migration

`b2bk migrate` reads an app's hand-written app-template HelmRelease (the
`resources/restic` directories in home-cluster-flux) and emits a Prefix mode
Repository that adopts the existing repository plus a ScheduledBackup,
converting a hand-jittered cron expression into the equivalent shorthand so the
operator can jitter it instead. Every claim becomes an entry in
`spec.volumes`, an `emptyDir` becomes `spec.scratch`, and the container's
`workingDir` is kept. The Repository never gets a `lifecycle` block. The
repository URL is only in the SOPS-encrypted `restic-envs`, so it comes from
`--repository` or `--from-secret`.

`b2bk parity` then compares two CronJobs: the one `backup.BuildCronJob` builds
for the generated ScheduledBackup, and the one the HelmRelease renders.
`internal/cli/apptemplate.go` renders the latter from the raw values the way
app-template 4.x does, without `migrate`'s structs, so the two read the
HelmRelease independently. It refuses whatever it does not model. Its testdata
holds CronJobs the real chart rendered; re-check them with
`B2BK_APP_TEMPLATE_CHART=<app-template-4.6.2.tgz> go test ./internal/cli -run TestAppTemplate`.
Migrating must not silently change what gets backed up or how often, so any
change to `internal/backup/script.go`, `schedule.go` or `cronjob.go` has to keep
parity meaningful. It
normalizes away only the quoting around `--exclude` patterns, the
`--retry-lock` and `--secret-mount` flags -- the pattern is `ignoredFlags` in
`internal/cli/develop.go` -- and the forget scoping `normalizeRendered` notes as
`FORGET SCOPED`, and reports a `backup-cnpg.sh` to `dumpdb cnpg` switch as its
own difference, since the dump format and file name change. It
compares the schedule by **cadence**,
expanding both expressions over 364 days and comparing how often they fire.
364 days is exactly 52 weeks, so every weekday fires the same number of times;
a shorter window makes a monthly or yearly schedule fire once or not at all,
which `equal` cannot tell apart from any other schedule that fires as rarely.
Comparing schedules verbatim would report a difference for every app, since
migration moves the time on purpose, and hide the ones that matter.

That comparison lives in `internal/cli/cadence.go`. Note that when both
day-of-month and day-of-week are restricted, real cron ORs them where this
ANDs. Migration only converts expressions whose day, month and weekday are all
`*`, and a pinned schedule is compared against itself, so it cannot bite there
-- but `b2bk parity` is now general-purpose and could be handed something else.

## Logging

Kubernetes conventions, enforced by the logcheck linter: start with a capital,
no trailing period, past tense, name the object type, balanced key-value pairs.

```go
log.Info("Created Deployment", "name", deploy.Name)
log.Error(err, "Failed to create Pod", "name", name)
```

## References

- Kubebuilder Book: https://book.kubebuilder.io
- controller-runtime FAQ: https://github.com/kubernetes-sigs/controller-runtime/blob/main/FAQ.md
- Logging conventions: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-instrumentation/logging.md#message-style-guidelines
- CEL validation rules: https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions/#validation-rules
