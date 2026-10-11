# b2bk

Operating the backups that `b2-backup-operator` schedules.

The same binary works either way:

```
kubectl b2bk <command>    # installed as a kubectl plugin
b2bk <command>            # standalone
```

## What a command does to your data

Every command below is marked. It is the first thing to check at 3am.

| Marker | Meaning |
| --- | --- |
| `reads` | Changes nothing. Safe against production while you think. |
| `writes` | Alters Kubernetes objects — a schedule, an annotation, a Job. Reversible. |
| `DESTROYS` | Can overwrite or delete what is being protected. Every one of these is gated, but not all in the same way. |

How a `DESTROYS` command is gated:

- **A typed confirmation.** `unlock --remove-all`, `prune`, `restore --in-place`
  and `restore --to-database` print what they are about to overwrite and ask you
  to type the resource name. With no terminal they refuse outright unless `--yes`
  is passed, so a script cannot fall through the prompt.
- **A flag that names the risk.** `rotate-password` never prompts. It refuses
  unless `--i-have-somewhere-to-archive-the-new-password` is passed, because the
  danger is losing the new password afterwards, not running the command.
- **Nothing.** `shell --mount-data` and `exec --mount-data` mount the live source
  volume writable and then hand it to you. Nothing asks, because nothing yet
  knows what you are going to do with it.

## Naming what you mean

A bare name matching both a ScheduledBackup and a Repository is refused rather
than guessed at.

| Form | Resolves to |
| --- | --- |
| `web-files` | Whichever kind has that name — an error if both do |
| `sb/web-files` | A ScheduledBackup. Also `scheduledbackup/`, `scheduledbackups/`, `backup/`, `backups/` |
| `repo/prod` | A Repository. Also `repository/`, `repositories/`, `repos/` |

Standard kubectl connection flags work everywhere: `-n/--namespace`,
`--context`, `--kubeconfig`. `-A` and `-o` appear only on `get`, where they
mean something.

## Commands that start a pod

Nine commands do their work inside a throwaway pod built from the backup image:
`snapshots`, `restore`, `unlock`, `check`, `prune`, `stats`, `rotate-password`,
`shell` and `exec`. Those nine, and only those nine, take
`--pod-start-timeout` — how long to wait for that pod to reach Running, default
**10m**.

While the pod is Pending they print `still waiting: ...` to **stderr** every 15
seconds, so a slow start is visible rather than silent, and restic's own output
on stdout stays pipeable. A pod that cannot start at all — an unschedulable
affinity, a claim that will not bind, an image that will not pull — fails
immediately with the reason rather than waiting out the budget. Raise the
timeout when the image is large or the node is cold; the timeout message names
the flag.

## Inspect

### `doctor [name]` — `reads`

**Start here when something is wrong.** Checks the whole chain in one pass:
credentials Secret, CronJob ownership, repository initialization, cache claim,
and whether the most recent run actually succeeded. Names the object to fix and
the command to run next. With no argument, checks every resource in the
namespace. Exits non-zero if any check fails.

### `get [repositories|backups]` — `reads`

Both kinds by default. Pairs usage against quota, and last-success against
whether the schedule is suspended, so "are backups happening" is one command.
The argument takes any of the prefixes above without the slash, so `get sb` and
`get repos` work as well as `get backups`.

| Flag | |
| --- | --- |
| `-A` | Every namespace |
| `-o wide` | Repositories add server, secret, adopted, suspended; backups add timezone and concurrency policy |
| `-o json\|yaml\|name` | Machine-readable. `json` and `yaml` wrap both kinds at once in a `v1.List` |

### `status <name>` — `reads`

One resource in detail: usage against quota, the **effective** schedule with its
timezone, conditions with reasons, and a table of recent runs with result and
duration.

`Adopted` covers both kinds of adoption: a repository named by
`spec.existingRepositoryID`, and one the operator found in BorgBase already
carrying the repository name. Either way it means the snapshots predate this
resource, so `spec.passwordSecretRef` is what makes them readable.

| Flag | |
| --- | --- |
| `--limit` | Runs to show (default 10) |

### `logs <name>` — `reads`

The most recent run's output. Finds runs by ownership, so triggered and
scheduled runs both appear. Jobs are deleted an hour after they finish; past
that, `status` still has the history.

| Flag | |
| --- | --- |
| `-f` | Follow |
| `-p` | The run before the most recent |
| `--tail` | Trailing lines only |

### `version` — `reads`

Prints the b2bk version and nothing else. Needs no cluster and no arguments, so
it is also the quickest check that the binary on PATH is the one you think it
is. A release build has the tag stamped in; a `go install` build reports the
module version; anything else reports `unknown`.

`version` is the only command outside the groups above, so it appears under
"Additional Commands" in `b2bk --help`.

## Run

### `backup <name>` — `writes` — alias `run`

**Runs even when the schedule is suspended** — suspending and then backing up by
hand is the main reason to suspend. Refused only if a run is already in flight
and `concurrencyPolicy: Forbid`. A backup that is not Ready reports that the run
is deferred rather than appearing to hang.

| Flag | |
| --- | --- |
| `--wait` | Block until it finishes; non-zero if it fails |
| `-f` | Stream the logs (implies `--wait`) |
| `--timeout` | Default 2h |

Equivalent without the CLI:

```sh
kubectl annotate scheduledbackup/web-files \
  b2backup.mcswain.dev/trigger-at="$(date -Is)" --overwrite
```

### `cancel <name>` — `writes`

Deletes the Job of a run in flight, leaving completed ones for history. A killed
restic leaves a lock behind; `unlock` clears it.

### `wait <name>` — `reads`

Blocks until the current run finishes. Exits non-zero on failure, so it can gate
a deploy or a migration.

| Flag | |
| --- | --- |
| `--timeout` | Default 2h |

## Snapshots and restore

### `snapshots <name>` — `reads` — alias `snaps`

The backup's own snapshots. One repository can serve several backups, so the
listing is filtered to this backup's source tags. A `spec.script` backup has no
sources to filter by and needs `--all-tags`.

| Flag | |
| --- | --- |
| `--all-tags` | The whole repository |
| `--host` | Override; empty string lifts the filter |
| `--latest` | Newest N only |
| `--json` | restic's JSON output |

`--host` defaults to the host the backup records its snapshots under:
`spec.resticHost`, or the **namespace** when that is unset. Pass `--host=""` to
lift the filter.

### `restore <name>` — `DESTROYS`

Four destinations. **With a terminal and no target, b2bk asks**, offering only
what this backup can do. Without a terminal it lists them and exits, so a script
never restores somewhere it did not name.

| Target | | |
| --- | --- | --- |
| `--to DIR` | `reads` | Streams the files to this machine. Nothing staged on disk at either end |
| `--to-new-pvc NAME` | `writes` | A fresh claim, taking its size, access modes and storage class from the source, to inspect before committing. It outlives the restore and is never garbage collected |
| `--in-place` | `DESTROYS` | Over the source volume. Suspends the schedule for the duration |
| `--to-database` | `DESTROYS` | Streams the dump back through `restoredb` |

The filtering flags do not mean the same thing at every target, because the
three targets run three different restic commands: `--to` runs `restic dump`,
the two volume targets run `restic restore`, and `--to-database` runs
`restic dump | restoredb` over one file.

| Flag | Targets | |
| --- | --- | --- |
| `--snapshot` | all | Default `latest` |
| `--size` | `--to-new-pvc` | Override the new claim's size |
| `--yes` | `--in-place`, `--to-database` | Skip the typed confirmation. The other two never ask |
| `--path` | `--to`, `--to-new-pvc`, `--in-place` | With `--to` it is the path `restic dump` reads, defaulting to `/`. On the volume targets it becomes a `restic restore --include` |
| `--include`, `--exclude` | `--to-new-pvc`, `--in-place` | Passed through to `restic restore`. `restic dump` cannot filter, so `--to` refuses them rather than ignoring them |
| `--delete` | `--to-new-pvc`, `--in-place` | Remove files at the target that are not in the snapshot. Meaningless for a download, so `--to` refuses it |
| `--dry-run` | all, but see below | Report without restoring |

`--to-database` restores exactly one file — the database dump the snapshot was
taken from — so it has nothing to select within. `--path`, `--include`,
`--exclude` and `--delete` are accepted there and then ignored; do not read a
successful run as having honoured them.

`--dry-run` is not uniformly free of side effects:

| Target | What `--dry-run` does |
| --- | --- |
| `--to` | Lists the snapshot with `restic ls` instead of downloading it. Nothing is written |
| `--to-new-pvc` | **Still creates the claim**, then runs `restic restore --dry-run` into it. The claim is left behind |
| `--in-place` | Skips both the typed confirmation and the schedule suspension, then reports what `restic restore` would write |
| `--to-database` | Skips the typed confirmation and passes `--dry-run` to `restoredb` |

## Maintenance

### `unlock <name>` — `writes`

**The usual 3am fix.** A backup killed mid-run leaves a lock, and every later run
fails waiting on it. Removes only what restic considers stale.

| Flag | |
| --- | --- |
| `--remove-all` | **`DESTROYS`** Every lock, stale or not. Only safe when nothing is running. Confirms first |
| `--yes` | Skip the confirmation |

### `check <name>` — `reads`

Verifies repository structure. Metadata only by default, which is fast.

| Flag | |
| --- | --- |
| `--read-data-subset` | Also re-read a share of the data (`10%`, `1/5`). The only way to catch bit rot, at the cost of downloading that share |

### `prune <name>` — `DESTROYS`

Applies the ScheduledBackup's own retention policy, so a manual prune does what
the scheduled one does. Backups already prune themselves after each run; this is
for reclaiming space after a retention change. Refuses on an append-only
repository.

| Flag | |
| --- | --- |
| `--dry-run` | What would be removed. Skips the confirmation |
| `--yes` | Skip the confirmation |

The retention policy comes from the ScheduledBackup, so a backup with no
`spec.retention` has nothing to forget and is refused.

### `stats <name>` — `reads`

restic's own view of repository size, which differs from BorgBase's reported
usage in `status` because of deduplication and compression.

| Flag | |
| --- | --- |
| `--mode` | `restore-size` (default), `raw-data`, `files-by-contents`, `blobs-per-file` |

## Lifecycle

### `suspend <name>` / `resume <name>` — `writes`

Sets or clears `spec.suspend`. On a ScheduledBackup the CronJob is kept but stops
firing; on a Repository the operator stops reconciling it entirely.
Re-suspending is a no-op, not a pointless write.

### `reinit <repository>` — `writes`

A failed init Job is retried **indefinitely**, on a five-minute delay: the
operator waits out the delay, deletes the failed Job, and builds a fresh one on
the pass after that. It never gives up on its own, so a repository stuck on
`Initialized=False` with reason `InitFailed` is still trying.

`reinit` is how you skip the wait. It deletes the Job now and clears
`status.initialized`, so a fix — usually a `passwordSecretRef` holding the wrong
password for an adopted repository — is retried immediately rather than up to
five minutes from now. Does not touch repository contents: the init Job probes
with `restic cat config` first, and `restic init` refuses a repository that
already holds a config rather than re-keying it.

### `rotate-password <repository>` — `DESTROYS`

**Adds** a key rather than replacing one, so snapshots from before the rotation
stay readable and the change is undoable by restoring the old Secret. Remove the
old key yourself once you are sure:

```sh
b2bk exec web-files -- restic key list
b2bk exec web-files -- restic key remove <id>
```

There is no typed confirmation here. It refuses to run at all without
`--i-have-somewhere-to-archive-the-new-password`, and there is no `--yes`: the
password is the only thing that can decrypt this repository, so the gate is on
having somewhere to put the new one, not on the keystroke.

Archive it as soon as the command returns:

```sh
b2bk env repo/prod --show-password
```

## Escape hatch

The pod is deleted when you exit. The app's data is not mounted unless you ask,
and the shared restic cache is replaced with scratch space, so none of this can
interfere with a running backup.

### `shell <name>` — `writes` — alias `sh`

A pod from the backup image with `restic`, `runitor`, `ts` and `dumpdb` on PATH
and the credentials already in the environment, so `restic snapshots` just
works — and so does `restic mount /mnt`.

| Flag | |
| --- | --- |
| `--mount-data` | **`DESTROYS`** Mount the source volume, writable. Nothing confirms |
| `--mount-cache` | Use the shared cache instead of scratch space |
| `--image` | Override the image |
| `--keep` | Leave the pod running |
| `--shell` | Default `sh` |
| `--pod-start-timeout` | How long to wait for the pod to start. Default 10m |

### `exec <name> -- <command>...` — `writes`

One command in the same environment, output printed. Reaches restic subcommands
the CLI does not wrap.

```sh
b2bk exec web-files -- restic find --tag=files 'config.php'
b2bk exec web-files -- restic cat config
```

Everything after `--` is passed through verbatim; the separator is required.

| Flag | |
| --- | --- |
| `--mount-data` | **`DESTROYS`** Mount the source volume, writable. Nothing confirms |
| `--mount-cache` | Use the shared cache instead of scratch space |
| `--image` | Override the image |
| `--pod-start-timeout` | How long to wait for the pod to start. Default 10m |

### `env <name>` — `reads`

The credentials as shell exports. Takes a Repository, or a ScheduledBackup,
which resolves to the Repository it writes into.

Redacted by default, because `RESTIC_REPOSITORY` embeds the password too: the
password inside the URL is replaced with `***`, leaving the server and path
readable, and anything that is not a `rest:` URL is replaced wholesale. The
"redacted" notice goes to stderr, so stdout is still a valid script — but it
exports `***`, so only the `--show-password` form is worth evaluating.

```sh
eval "$(b2bk env repo/prod --show-password)"
```

## Development

### `render <file.yaml|name>` — `reads`

The exact shell script a backup runs. Takes a local manifest or a resource in
the cluster, so it answers both "what would this change do" and "what is this
running". `--cronjob` prints the whole object instead.

### `validate <file.yaml>` — `reads`

Catches what the operator would reject at reconcile time — an unresolvable
schedule, a source it cannot render, a missing database field — with no cluster.
Does not run the CRD's CEL rules; the API server owns those.

### `parity <generated.yaml> <helmrelease.yaml>` — `reads`

Confirms that migrating an app does not change what gets backed up. Needs
nothing on PATH: both sides are parsed in-process, the HelmRelease with
`sigs.k8s.io/yaml`. `migrate` is the one command that shells out, and only to
`sops`.

The script comparison ignores the quoting around `--exclude` patterns and the
`--retry-lock` and `--secret-mount` flags, which the operator renders and the
hand-written script does not. Everything else has to match.

**Schedules are compared by cadence, not verbatim.** Migration hands a
hand-jittered expression back to the operator as a shorthand, which deliberately
moves the minute. Both expressions are expanded over 364 days — exactly 52
weeks, so no weekday is over-counted and monthly and yearly schedules are still
distinguishable — and checked for firing equally often with the same spacing, so
"hourly became daily" fails while a re-jittered minute does not.

| Output | Exit | |
| --- | --- | --- |
| `IDENTICAL` | 0 | Nothing moved |
| `RESCHEDULED` + `EQUIVALENT` | 0 | The time moved, the cadence did not |
| `SCRIPT DIFFERS` | 1 | |
| `CADENCE DIFFERS` | 1 | It no longer runs as often |
| `TIMEZONE DIFFERS` | 1 | |
| `error: ...` | 1 | Either side could not be read or rendered |

These are lines, not verdicts: a run prints every one that applies. `RESCHEDULED`
is a note rather than a difference, so it can precede `TIMEZONE DIFFERS` on a run
that still exits 1. `IDENTICAL` and `EQUIVALENT` print only when no difference
was found, and `SCRIPT DIFFERS` is followed by both scripts.

### `migrate <resources/restic directory>` — `reads`

Converts an app's hand-written backup into operator resources, printed to
stdout. Nothing is applied and nothing in fleet-infra is modified.

The resources are marshalled from the real API types, so the output cannot be
shaped in a way the CRDs would reject.

| Flag | |
| --- | --- |
| `--repository-id` | Use this ID instead of decrypting `secret.yaml` |

The BorgBase repository ID exists only inside the encrypted
`RESTIC_REPOSITORY` value, so by default `migrate` runs `sops` to read it —
that is where the cloud credentials for it live, and linking the sops library
instead would add every KMS backend it supports (61MB on a 93MB binary) to read
one field once per app. `--repository-id` skips that path entirely. Everything
else is parsed and emitted in Go.

Before applying, reduce the app's `secret.yaml` to just `RESTIC_PASSWORD`:
`RESTIC_REPOSITORY` is synthesized by the operator and `CHECK_UUID` is replaced
by healthchecks slug pinging. Do not remove `RESTIC_PASSWORD` — it is the only
off-cluster copy of the key that decrypts these backups.

## Recipes

**A backup stopped working**

```sh
b2bk doctor web-files
# fix what it names, then prove it
b2bk backup web-files --follow
```

**Last night's run failed**

```sh
b2bk status web-files   # when, and how often
b2bk logs web-files     # why
b2bk unlock web-files   # if it died mid-run
```

**Recover one file**

```sh
b2bk snapshots web-files
b2bk restore web-files --snapshot 4f2a1b0c \
  --path app/config.php --to ./recovered
```

**Full recovery, carefully**

```sh
# stage it first and look at it
b2bk restore web-files --to-new-pvc web-files-check
# pvc/web-files-check now holds the snapshot; mount it wherever you want to
# read it. b2bk shell --mount-data would mount the live source claim instead
# only then, over the live volume
b2bk restore web-files --in-place
```

**Quiet a backup during maintenance**

```sh
b2bk suspend web-files
# … do the work …
b2bk backup web-files --wait
b2bk resume web-files
```

**Migrate an app onto the operator**

```sh
b2bk migrate apps/snipe-it/resources/restic > generated.yaml
b2bk validate generated.yaml
b2bk parity generated.yaml helmrelease.yaml
```

## Installing

Download the archive for your platform from the
[GitHub releases](https://github.com/USA-RedDragon/b2-backup-operator/releases),
or build it from source:

```sh
go install github.com/USA-RedDragon/b2-backup-operator/cmd/kubectl-b2bk@latest
ln -s "$(go env GOPATH)/bin/kubectl-b2bk" "$(go env GOPATH)/bin/b2bk"
```

kubectl runs any `kubectl-b2bk` on PATH as `kubectl b2bk`. Link or copy the
binary under both names to use it both ways.

### Permissions

Apply `config/rbac/b2bk_user_role.yaml` and bind one of:

- **`b2bk-viewer`** — read-only. Cannot read the credentials Secret.
- **`b2bk-operator`** — runs backups and restores. **Reads and writes the
  credentials Secret**, which holds the key that decrypts every snapshot. Bind
  it only to people who should hold that key.

The operator's own role does not cover the CLI. `manager-role` never reads pods
and never execs, both of which `b2bk` needs, so `b2bk` runs as you and needs a
grant of its own.

The two roles part company over claims, in opposite directions:

- The **operator** may delete a PersistentVolumeClaim, but only ever deletes the
  restic cache claim it created: `deleteCache` re-reads the claim and returns
  without touching it unless the ScheduledBackup is its controller. It never
  deletes an app's data claim.
- **`b2bk-operator`** may create a claim but not delete one. That is why the
  staging claim from `restore --to-new-pvc` outlives the restore and is never
  garbage collected — deleting it is a `kubectl delete pvc` you run yourself.

## After a migration

Two things change on cutover, and neither is a fault:

- `spec.schedule` becomes a shorthand such as `@hourly`, and the real cron
  expression exists only in `status.effectiveSchedule`. `get` and `status` show
  the effective one.
- A backup whose new slot has already passed waits for the next one, so a daily
  backup can go up to a day longer before its first run. `doctor` reports this
  as "no backup has run yet", not as a missed backup. Take one now with
  `b2bk backup <name>`.
