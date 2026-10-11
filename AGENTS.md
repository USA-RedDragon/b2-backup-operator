# b2-backup-operator - AI Agent Guide

A Kubernetes operator that creates and adopts BorgBase restic repositories and
renders the CronJobs that back up into them. Read the README first: it explains
what the two resources are for and why the defaults are what they are.

## Layout

```
cmd/main.go                    Manager entry and operator-level flags
cmd/kubectl-b2bk/              b2bk CLI entry, also the kubectl plugin
api/v1/*_types.go              CRD schemas (+kubebuilder markers)
api/v1/zz_generated.*          Auto-generated (DO NOT EDIT)
internal/controller/           Reconciliation for both kinds
internal/backup/               Rendering: CronJob, schedule, backup script
internal/borgbase/             GraphQL client for the BorgBase API
internal/healthchecks/         runitor wrapping, no API client of its own
internal/secrets/              Restic password generation
internal/cli/                  b2bk commands, including migrate and parity
internal/cli/runner/           Ephemeral Jobs for one-off restic commands
config/crd/bases/              Generated CRDs (DO NOT EDIT)
config/rbac/role.yaml          Generated RBAC (DO NOT EDIT)
config/rbac/b2bk_user_role.yaml Roles for people running the CLI
config/samples/                Example CRs, checked by make test-crd
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
- **Adoption never creates.** A wrong `existingRepositoryID` must fail loudly
  rather than provision an empty repository beside the real backups.
- **`deletionPolicy` defaults to Retain**, and the credentials Secret only gets
  an ownerReference under `Delete`.
- **The operator only writes to Secrets it created**, identified by the
  `app.kubernetes.io/managed-by` label.
- **The API token is resolved lazily**, so a Retain deletion works without one.
- **Status is written with a merge patch**, never a full update: the informer
  cache lags behind the controller's own writes.
- **The init Job is deleted a pass after success is recorded**, not in the same
  one, or the watch event races the status write.
- **`spec.env` may not name a variable the operator sets.** A container's `env`
  overrides its `envFrom` wholesale, so `RESTIC_REPOSITORY` or `RESTIC_PASSWORD`
  in `spec.env` would send a backup to another repository or encrypt it under
  another key. `buildEnv` rejects the whole set it built, plus those two;
  `RESTIC_GROUP_BY` and `RESTIC_REST_*` stay usable because the image's restic
  wrapper -- not the operator -- reads them.
- **The backup image's `restic` is a wrapper** ahead of the real binary on PATH.
  It supplies `--group-by` from `RESTIC_GROUP_BY` and the `--stdin-filename` a
  database dump lands under from `RESTIC_HOST`, neither of which is in the
  rendered script. `b2bk restore --to-database` reads that filename back, so the
  two have to agree. See the image contract in the README before changing
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
make lint        # golangci-lint with the logcheck plugin
```

Tests are standard `testing`, not Ginkgo, except the e2e suite: it is Ginkgo, and
its `test/e2e/e2e_suite_test.go` is the only `suite_test.go` in the tree. Both it
and the envtest CRD tests sit behind build tags (`e2e` and `envtest`), so `make
test` -- a plain `go test` over everything but `/e2e` -- compiles neither.

**Validation lives in CEL on the CRDs**, so `make test` cannot see it. A rule
that fails to compile makes every CRD install fail, and one that never fires is
invisible. Anything added under `+kubebuilder:validation:XValidation` needs a
case in `internal/controller/crd_validation_envtest_test.go`.

CEL gotcha: `\.` is not a valid escape in a CEL string literal. Use a character
class, `[.]`, instead.

## Migration

`b2bk migrate` reads an app's hand-written `resources/restic` directory and
emits the two resources, converting a hand-jittered cron expression into the
equivalent shorthand so the operator can jitter it instead.

`b2bk parity` then compares the generated script, schedule and time zone
against the original. Migrating must not silently change what gets backed up or
how often, so any change to `internal/backup/script.go` or `schedule.go` has to
keep parity meaningful. It normalises away only the quoting around `--exclude`
patterns and the `--retry-lock` and `--secret-mount` flags -- the pattern is
`ignoredFlags` in `internal/cli/develop.go` -- and compares the schedule by **cadence**,
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
