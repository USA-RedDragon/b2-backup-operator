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
- **A current, uncontested backup.** `prune`, `restore --in-place` and
  `restore --to-database` act on the ScheduledBackup's spec, so they run only
  when the operator has caught up with it: `status.observedGeneration` equals
  `metadata.generation`, `Ready` is `True` at that generation, and the CronJob
  is controlled by the ScheduledBackup and records, in its
  `b2backup.mcswain.dev/rendered-from` and `forget-scope` annotations, that it
  was rendered from the ScheduledBackup and Repository generations of now and
  forgets under the same tags and host. They also re-check the tag conflict
  themselves, against every ScheduledBackup, CronJob and running backup Job in
  the namespace, rather than trust a `Ready` written before a sibling changed.
  Otherwise they refuse and say to wait for the operator. `--to DIR` and
  `--to-new-pvc` write nothing live and are not held back, so a restore stays
  possible while the operator is down.
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

A command that runs in a pod builds it from a ScheduledBackup's CronJob, so a
Repository stands for the one ScheduledBackup that uses it. When several do, the
command refuses and lists them: each has its own retention, tags, claims and
database, and picking one would prune or restore under the wrong one.

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

For a repository it shows the bucket and path, checks that the credentials
Secret holds all five keys (`RESTIC_REPOSITORY`, `RESTIC_PASSWORD`,
`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_DEFAULT_REGION`), that the
key ID in it is the one the operator recorded in `status.applicationKeyID`, and
reports a lifecycle rule problem from the `LifecycleRule` condition. `doctor`
never calls B2 and needs no B2 credentials: whether the key still exists in B2
is the operator's check, and it shows up as the `Ready` condition.

### `get [repositories|backups]` — `reads`

Both kinds by default. Shows where each repository lives and how big it is, and
last-success against whether the schedule is suspended, so "are backups
happening" is one command.
The argument takes any of the prefixes above without the slash, so `get sb` and
`get repos` work as well as `get backups`.

| Flag | |
| --- | --- |
| `-A` | Every namespace |
| `-o wide` | Repositories add mode, key ID, secret, adopted, suspended; backups add timezone and concurrency policy |
| `-o json\|yaml\|name` | Machine-readable. `json` and `yaml` wrap both kinds at once in a `v1.List` |

### `status <name>` — `reads`

One resource in detail: mode, bucket, path, endpoint, application key ID and
usage with when it was measured, the **effective** schedule with its timezone,
conditions with reasons, and a table of recent runs with result and duration.

`Adopted` means the operator found a restic `config` at the path before it
initialized anything, so the snapshots predate this resource and
`spec.passwordSecretRef` is what makes them readable.

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
hand is the main reason to suspend. Refused if a run is already in flight and
`concurrencyPolicy: Forbid`, and while a restore holds the backup (see
`restore`), so a half-restored claim or database is never saved. A backup that is not Ready reports that the run
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
| `--to-new-pvc NAME` | `writes` | A fresh claim, laid out like the source claim, taking its size, access modes and storage class, to inspect before committing. It is created only once the snapshot is found, outlives the restore and is never garbage collected |
| `--in-place` | `DESTROYS` | Over the source claims. Held like `--to-database`, below |
| `--to-database` | `DESTROYS` | Streams the dump back through `restoredb` |

Both overwriting targets work on every ScheduledBackup in the namespace that
backs up the same thing: for `--in-place`, each one with one of the restored
claims in `spec.volumes`; for `--to-database`, each one whose `spec.database`
has the same engine and credentials Secret. They are refused while a Job of any
of them is running: a scheduled or triggered backup, or a b2bk restore, which
b2bk labels `b2backup.mcswain.dev/b2bk-purpose`. A b2bk Job without that label,
from an older b2bk, counts as a restore.

The restore then holds them all with the `b2backup.mcswain.dev/restore-lock`
annotation, written with the resourceVersion it read so that of two restores
only one wins. While it is set the operator keeps their CronJobs suspended and
starts no triggered backup, `b2bk backup` and `b2bk resume` are refused, and a
second overwriting restore is refused. `spec.suspend` is left as you set it.
Just before the first write, from the restore pod, b2bk checks that the
ScheduledBackup still has the spec the restore was planned from and the
operator has rendered it, that every held CronJob is suspended, and that no
backup started in the meantime, and stops without writing if not.

A restore that fails after it started writing records what it was writing in
`b2backup.mcswain.dev/restore-partial` on every held ScheduledBackup, as
`pvc/<claim>:<path>` or `db/<engine>/<secret>/<database>` entries. While any
entry is recorded the hold stays: the next backup would save the partial
restore, and retention could then keep it over good snapshots. A later restore
of the whole of the same target that succeeds removes its entry; one narrowed
with `--path`, `--include` or `--exclude`, one of another target, and one that
stops before writing do not. `b2bk resume` removes them all once you have
checked the data. `b2bk status` and `b2bk doctor` show the hold. A restore
killed before it lets go leaves the lock behind: once it is gone, remove it
with `kubectl annotate scheduledbackup NAME b2backup.mcswain.dev/restore-lock-`.

**Which snapshot.** Every target restores one source. A files target takes the
backup's files source, or the one `--tag` names when there are several. The
database target takes the database source, or the one `--tag` names when there
are several, or, for a `spec.script` backup that has none, the dump tagged
`--tag`. With several sources of the kind and no `--tag`, the restore is refused
with the tags listed. `--to-database` asks for confirmation only after it has
found the dump, and the prompt names the dump's path, tag, snapshot, time and
host as well as the database it overwrites. Without `--snapshot`, b2bk lists the
repository's snapshots and picks the newest one that carries the source's tag,
was taken on the backup's host (`spec.resticHost` or the namespace) and, for
files, holds exactly the source's paths. A database dump is a snapshot of one
file. A snapshot named with `--snapshot` must carry the source's tag, and every
path in it must lie under one of the source's paths, or hold one dump for
`--to-database`; anything else is refused before a byte is written, so a
database dump is never restored over a volume and a files snapshot never into a
database. A named snapshot taken under another host is accepted, which is how a
backup from before a namespace rename is restored. If the latest dumps under the
tag are of more than one file, such as two databases a script tags `db`, the
restore is refused until `--tag` or `--snapshot` picks one; an old plain dump
stored as `<host>` and a new one stored as `<host>.dmp` count as one database.
The dump must be a file in the snapshot, so a files snapshot carrying the same
tag is never piped into `restoredb`.

**Where files go.** restic records the absolute path each snapshot was taken
from, such as `/data`, and stores the files under it. b2bk restores each path
the snapshot holds as `restic restore <snapshot>:<path>`, so `/data` from the
snapshot lands in `/data` on the claim mounted there, and a snapshot of `/data`
and `/blobs` restores each into its own claim. A path that is a single file,
such as `/data/app.db`, is restored from its directory with an include for the
file, since restic cannot use a file as the subfolder.

**What `--delete` leaves alone.** The source's own `exclude` patterns are passed
to `restic restore` as excludes, rewritten relative to the restored path, so
`--delete` never removes what the backup deliberately left out, such as a cache.
An absolute exclude is matched against the restored path segment by segment, so
`/*/cache` under a source of `/data` becomes `/cache`. A relative one such as
`data/cache` matched the end of the backed-up path, so where its first segments
match the end of the restored path, the rest is added anchored at the restore
root: `/cache`. A re-include (`!pattern`) is carried over the same way. One
that cannot be carried over, such as `/**/cache`, refuses `--delete`. restic refuses
includes and excludes together, so with `--path` or `--include` the source's
excludes are dropped, and `--delete` is then refused if the source excludes
anything; `--exclude` together with `--path` or `--include` is refused for the
same reason. `--include` is also refused beside `--path`, and on a source that is
a single file: restic ORs includes, so it would widen what is restored, and
deleted, beyond the path.

| Target | Where `<snapshot>:/data` goes |
| --- | --- |
| `--in-place` | `/data` on the claim mounted there |
| `--to-new-pvc` | The same place relative to the new claim's root, so the new claim can stand in for the old one. A source spanning two claims is refused; restore it `--in-place` or `--to DIR` |
| `--to DIR` | `DIR/data/...` |

`--to-database` streams the dump's file out of the snapshot into `restoredb`,
which tells pg_dump's custom format from plain SQL by its first bytes. The dump
goes into the database the source dumped, named with `--database`; with no
database on the source, into the one the credentials Secret names in `dbname`,
or `spec.database.name` for mariadb. The confirmation names which. A Secret
whose `dbname` is not a single database, such as a superuser Secret, needs
`--database`, and so does a dump a `spec.script` took, which does not record
which database it holds. Dumps
taken by `dumpdb cnpg` (stored as `<host>.dmp`) go through `pg_restore`; old
ones taken by `backup-cnpg.sh` (plain SQL, stored as `<host>` with no
extension) go through `psql`. Both run in a single transaction, so a failed
restore leaves the database as it was. psql would commit whatever it had read if
the download stopped partway, so `restoredb` holds back the end of a plain dump
until it has seen pg_dump's `dump complete` trailer at the very end, with the
same key on its `\unrestrict` line as on the `\restrict` line at its head, and
stops psql before it can commit a dump that ends without it. A plain dump from a
pg_dump older than 17.6 has no `\restrict` line, so only the trailer is checked;
every dump `backup-cnpg.sh` took has one, since it always passed `--restrict-key`.

A mariadb restore is all or nothing too, although a mariadb-dump cannot run in
one transaction: its `DROP TABLE` and `CREATE TABLE` commit on their own. So
`restoredb` streams the dump into a staging database, `<db>__b2bk_restore`,
which it drops first if an earlier run left it behind. Only when the whole dump
has gone in cleanly and ends with mariadb-dump's `-- Dump completed` trailer does
it run one `RENAME TABLE` that moves every live table into `<db>__b2bk_old` and
every staged table into `<db>`. MariaDB applies a multi-table `RENAME TABLE`
atomically: if any part fails, none of it happens. So the database changes all
at once or not at all. A dump cut short, a statement that fails, or anything
refused below drops the staging database and leaves `<db>` as it was. Tables
only in `<db>` move to `<db>__b2bk_old`; tables only in the dump appear in
`<db>`. The dump is not staged on disk, so a large dump needs no room in the
pod, but the database server holds two copies of the data until
`<db>__b2bk_old` is dropped.
A dump taken with `--skip-comments` or `--compact` has no trailer and is always
refused. The swap waits at most 60 seconds (`lock_wait_timeout`) for
transactions that hold a live table, then fails as a whole.

`<db>__b2bk_old` keeps the replaced tables for a rollback. `restoredb` prints
the `DROP DATABASE` that removes it and the statements that swap it back.
`restoredb mariadb --drop-old` drops it after a successful swap instead; b2bk
does not pass that flag. While `<db>__b2bk_old` still holds tables, the next
restore is refused before anything is touched, so a rollback copy is never
dropped silently. Writes the app makes to `<db>` while the dump loads land in
the tables that move to `<db>__b2bk_old`, so stop the app first.

`USE`, `CREATE DATABASE` and `DROP DATABASE` lines, which a dump taken with
`--databases` or `--add-drop-database` has, are left out, so the dump always
lands in the staging database and never in the database it names. The client
reads the dump with `--binary-mode`, so a client command such as `\u` reaches the
server as text and fails instead of switching databases. A dump of
more than one database is refused. `RENAME TABLE` cannot move a view (MariaDB
error 1450) or a table that has triggers (error 1435) to another database, and
nothing moves stored routines or events, so the restore is refused, before
`<db>` is touched, when `<db>` has views or triggers or the dump has views,
triggers, routines or events. `dumpdb mariadb` includes triggers and views but
not routines or events. Routines and events already in `<db>` are left as they
are. To restore a refused dump anyway, pipe it into the mariadb client by hand,
which is not atomic.

The database user needs `ALL` on `<db>__b2bk_restore` and `<db>__b2bk_old`,
which it creates and drops, and `ALTER`, `DROP`, `CREATE` and `INSERT` on
`<db>` for the swap. `restoredb` checks the first two before it reads the dump.
In a grant, escape the underscores, which are wildcards there:

```sql
GRANT ALL ON `app\_\_b2bk\_restore`.* TO 'app'@'%';
GRANT ALL ON `app\_\_b2bk\_old`.* TO 'app'@'%';
```

| Flag | Targets | |
| --- | --- | --- |
| `--snapshot` | all | Default `latest`, as described above |
| `--tag` | all | The source to restore, when a backup has several, or a script backup's dump |
| `--host` | all | The host the snapshot was taken on, instead of `spec.resticHost` or the namespace: for a snapshot from before the host changed. A named `--snapshot` from another host is refused without it |
| `--database` | `--to-database` | The database to restore into, instead of the source's |
| `--size` | `--to-new-pvc` | Override the new claim's size |
| `--yes` | `--in-place`, `--to-database` | Skip the typed confirmation. The other two never ask |
| `--path` | `--to`, `--to-new-pvc`, `--in-place` | A file or directory in the snapshot, absolute (`/data/app/config.php`) or relative to the source path (`app/config.php`). Matched literally, so `[`, `*` and `?` in a name are not patterns. Only that is restored, to the same place it would go in a full restore |
| `--include`, `--exclude` | `--to-new-pvc`, `--in-place` | Passed through to `restic restore`, relative to the source path. `restic dump` cannot filter, so `--to` refuses them rather than ignoring them |
| `--delete` | `--to-new-pvc`, `--in-place` | Remove files under the restored path that are not in the snapshot. Nothing outside the restored path, or outside `--path` and `--include`, is touched. Meaningless for a download, so `--to` refuses it |
| `--dry-run` | all, but see below | Report without restoring |

`--to-database` restores exactly one file, the dump, so it has nothing to select
within. `--path`, `--include`, `--exclude` and `--delete` are accepted there and
then ignored; do not read a successful run as having honored them.

`--dry-run` is not uniformly free of side effects:

| Target | What `--dry-run` does |
| --- | --- |
| `--to` | Lists the snapshot with `restic ls` instead of downloading it. Nothing is written |
| `--to-new-pvc` | **Still creates the claim**, then runs `restic restore --dry-run --verbose=2` into it. The claim is left behind |
| `--in-place` | Skips both the typed confirmation and the schedule suspension, then runs `restic restore --dry-run --verbose=2`, which lists every file it would write and delete |
| `--to-database` | Skips the typed confirmation and passes `--dry-run` to `restoredb`. For postgres that only prints the command. For mariadb it restores into `<db>__b2bk_restore`, checks the trailer and the staged objects, prints the `RENAME TABLE` it would run and drops the staging database again. `<db>` is not changed |

## Maintenance

### `unlock <name>` — `writes`

**The usual 3am fix.** A backup killed mid-run leaves a lock, and every later run
fails waiting on it. Removes only what restic considers stale.

| Flag | |
| --- | --- |
| `--remove-all` | **`DESTROYS`** Every lock, stale or not. Only safe when nothing is running, so it is refused while any Job of a ScheduledBackup on the repository, its b2bk Jobs or the Repository's own is running. Confirms first |
| `--yes` | Skip the confirmation |

### `check <name>` — `reads`

Verifies repository structure. Metadata only by default, which is fast.

| Flag | |
| --- | --- |
| `--read-data-subset` | Also re-read a share of the data (`10%`, `1/5`). The only way to catch bit rot, at the cost of downloading that share |

### `prune <name>` — `DESTROYS`

Applies the ScheduledBackup's own retention policy, so a manual prune does what
the scheduled one does: it forgets only snapshots carrying one of the backup's
source tags, on its host, so another backup writing into the same repository
keeps its own retention. Backups already prune themselves after each run; this
is for reclaiming space after a retention change. On B2 the pruned packs are
only hidden, and the bucket's lifecycle rule deletes them later, 30 days unless
a rule says otherwise.

| Flag | |
| --- | --- |
| `--dry-run` | What would be removed. Skips the confirmation |
| `--yes` | Skip the confirmation |

The retention policy comes from the ScheduledBackup, so a backup with no
`spec.retention` has nothing to forget and is refused, and so is a `spec.script`
backup, which has no source tags to scope forget to.

### `stats <name>` — `reads`

restic's own view of repository size. The usage in `status` is the size of the
files in B2, which differs because of deduplication, compression, and packs
restic has pruned that B2 has only hidden so far.

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

`reinit` is how you skip the wait. It sets the annotation
`b2backup.mcswain.dev/reinit-at` to the current time, and the operator deletes
its init Job, clears `status.initialized`, records the time in
`status.lastReinitTime` and starts over at once. A fix, usually a
`passwordSecretRef` holding the wrong password for an adopted repository, is
retried immediately rather than up to five minutes from now. An annotation no
later than `status.lastReinitTime` is ignored, so it is safe to leave in Git.

b2bk never writes a Repository's status: what status records decides what the
operator may delete, so only the operator writes it, and `b2bk-operator` has no
write access to it.

Does not touch repository contents: the init Job probes with `restic cat config`
first, and `restic init` refuses a repository that already holds a config rather
than re-keying it.

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

Any copy of the old password, such as the `passwordSecretRef` seed and its
source in Git or SOPS, is now stale, and the command says so. Replace it with
the new password. Until the seed holds it again, the operator keeps the
credentials Secret behind its `protect-password` finalizer, since that Secret
is the only copy. `b2bk` adds the finalizer in the same write as the new
password, so the Secret is never unprotected while it waits for the
operator's next pass.

## Escape hatch

The pod is deleted when you exit. The app's data is not mounted unless you ask,
and the shared restic cache is replaced with scratch space, so none of this can
interfere with a running backup.

### `shell <name>` — `writes` — alias `sh`

A pod from the backup image with `restic`, `ts` and `dumpdb` on PATH
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

Prints all five keys of the credentials Secret. `RESTIC_PASSWORD` and
`AWS_SECRET_ACCESS_KEY` are replaced with `***` unless `--show-password` is
passed; `RESTIC_REPOSITORY` holds no secret and is printed as it is, along with
the key ID and region. The "redacted" notice goes to stderr, so stdout is still
a valid script, but it exports `***`, so only the `--show-password` form is
worth evaluating.

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

### `parity <generated.yaml> <helmrelease.yaml | directory | cronjob.yaml>` — `reads`

Confirms that migrating an app does not change what gets backed up, or where.
Needs nothing on PATH. Only `--from-secret` shells out, to `sops`. The second
argument takes the same directory `migrate` does, or a manifest holding the
original CronJob itself, such as `helm template` output or a CronJob read from
the cluster.

Both sides are compared as the CronJob each one runs. The generated side is the
CronJob the operator builds, with `backup.BuildCronJob` and the CRD's defaults,
so a ScheduledBackup the operator would refuse is an error here too. The
original side is rendered from the HelmRelease's values the way app-template
4.x renders them, by a renderer of its own that does not share `migrate`'s
reading of the values, and is checked against CronJobs the real chart rendered.
Anything that renderer does not model, such as a Helm template in a value, a
chart-created claim, `dependsOn`, a key it does not know, `spec.valuesFrom`,
`spec.postRenderers` or a chart other than `app-template`, is an error rather
than a guess.

It compares the rendered script and the command around it, the schedule, the
time zone, suspend and the concurrency policy, the image, the claims and
scratch space mounted (name, path, read-only flag, `subPath`), whether every
file on them can be read, the working directory, the snapshot host, the
environment, `envFrom`, the database Secret, resources, security contexts,
affinity, node selector, tolerations, priority class, pod labels and
annotations, every other container, and whatever else is left in the pod and
container spec. It compares the namespace and the key the Repository opens the repository
with, and, given the original `RESTIC_REPOSITORY`, the Repository's bucket and
path. Both scripts are compared without the preamble
and postamble around them, the hand-written `exec > >(ts ...)` and `set -eu` on
one side and the operator's on the other.

Some differences are the operator's by design and are normalized away: its
restic cache claim, and a cache volume on the original side mounted where restic
keeps its cache (`--cache-dir`, then `RESTIC_CACHE_DIR`, then the image's
`/cache`) when the script backs nothing up from there; the database Secret's
mount path, as long as every dump on the generated side reads its credentials
where a Secret is mounted (`DATABASE SECRET UNREAD` otherwise); the
Repository's Secret in place of `restic-envs` and `restic-b2`; the pod's restart
policy, service account token and service links; and, when
the original sets none, the operator's own security contexts.

| Flag | |
| --- | --- |
| `--repository` | The original `s3:` URL, as it appears in `RESTIC_REPOSITORY` |
| `--from-secret` | A SOPS-encrypted Secret to read it from. Its `metadata.namespace` must be the HelmRelease's. Its other keys are compared as part of the original's environment |

A forget's `--group-by` is compared on its own, as `GROUPING DIFFERS`, against
what the original's wrapper would have added: `RESTIC_GROUP_BY` from the
container's `env`, from `restic-envs` given `--from-secret`, or else the image's
own `tags`.

The script comparison ignores the quoting around `--exclude` patterns and the
`--retry-lock` and `--secret-mount` flags, which the operator renders and the
hand-written script does not. It also takes out of `restic forget` the
`--host="$RESTIC_HOST"`, `--group-by=tags` and `--tag` flags the operator adds, when the tags are
exactly the backup's own, and notes the change as `FORGET SCOPED`: restic
already defaults `--host` to `$RESTIC_HOST`, so only the tags change what forget
considers. A `CNPG_SECRET_PATH` pointing at wherever the database Secret is
mounted compares equal on both sides.

A cnpg dump through `backup-cnpg.sh` and one through `dumpdb cnpg` compare equal
as commands, with `--dbname` and `--database` read as the same thing, but the
change is then reported on its own line: `backup-cnpg.sh` writes plain SQL that
the image's restic wrapper stores as `<host>`, while `dumpdb cnpg` writes
pg_dump's custom format stored as `<host>.dmp`. `b2bk restore --to-database`
restores both. Everything else has to match.

**Schedules are compared by cadence, not verbatim.** Migration hands a
hand-jittered expression back to the operator as a shorthand, which deliberately
moves the minute. Both expressions are expanded over 364 days — exactly 52
weeks, so no weekday is over-counted and monthly and yearly schedules are still
distinguishable — and checked for firing equally often with the same spacing, so
"hourly became daily" fails while a re-jittered minute does not. The move is
printed with the time of day in the schedule's time zone, such as
`RESCHEDULED: "44 0 * * *" (daily at 00:44 America/Chicago) -> "7 15 * * *" (daily at 15:07 America/Chicago)`.

| Output | Exit | |
| --- | --- | --- |
| `IDENTICAL` | 0 | Nothing moved |
| notes + `EQUIVALENT` | 0 | Only the notes below apply |
| `RESCHEDULED` | note | The time moved; the cadence did not |
| `FORGET SCOPED` | note | forget now expires only the backup's own tags |
| `JOB SETTINGS CHANGE` | note | `backoffLimit`, `startingDeadlineSeconds`, the Job history limits or `ttlSecondsAfterFinished` changed. app-template retries a failed run 6 times and skips a run 30 seconds late; the operator does neither |
| `IMAGE CHANGES` | note | `ghcr.io/gabe565/restic`, directly or through a mirror, becomes the operator's image, which is built on it |
| `AFFINITY ADDED` | note | The original sets none and the operator adds only a preference |
| `POD LABELS CHANGE` | note | app-template's own `app.kubernetes.io/controller`, `name` or `instance` label is gone |
| `TIMEZONE UNSET` | note | The original CronJob sets no `timeZone`; it is compared as UTC, the kube-controller-manager's zone |
| `REPOSITORY UNCHECKED` | note | No `--repository` or `--from-secret` was given, so the bucket and path were not compared; or the generated file holds no Repository, so neither they nor its password source were |
| `RESTIC-ENVS UNCHECKED` | note | Without `--from-secret`, keys in `restic-envs` besides `RESTIC_REPOSITORY` and `RESTIC_PASSWORD` cannot be compared |
| `INIT JOB REPLACED` | note | The release's `restic init` Job; the Repository initializes or adopts the repository itself |
| `SCRIPT DIFFERS` | 1 | Followed by both scripts |
| `COMMAND DIFFERS` | 1 | The original does not run `sh -c <script>` |
| `DUMP FORMAT CHANGES` | 1 | A cnpg dump moves from `backup-cnpg.sh` to `dumpdb cnpg` |
| `CADENCE DIFFERS` | 1 | It no longer runs as often |
| `TIMEZONE DIFFERS` | 1 | |
| `VOLUME DIFFERS` | 1 | Another claim, path, read-only flag or scratch space |
| `READ ACCESS DIFFERS` | 1 | The original could read every file on its claims, as root with `DAC_OVERRIDE`, and the generated backup cannot |
| `WORKING DIR DIFFERS` | 1 | |
| `RESTIC HOST DIFFERS` | 1 | Snapshots would start a new series under another host |
| `ENV DIFFERS` | 1 | Another extra variable, or another `TZ` |
| `GROUPING DIFFERS` | 1 | The original's forget grouped by something other than tags: its own `--group-by`, or `RESTIC_GROUP_BY`, which the image's wrapper adds and the image sets to `tags`. The operator's forget always passes `--group-by=tags` |
| `ENVFROM DIFFERS` | 1 | The original reads a Secret or ConfigMap other than `restic-envs` and `restic-b2` |
| `DATABASE SECRET DIFFERS` | 1 | |
| `DATABASE SECRET UNREAD` | 1 | A dump in the generated backup reads its credentials from a directory where no Secret is mounted: `dumpdb`'s `--secret-mount`, by default `/postgresql-app` or `/mariadb`, or `backup-cnpg.sh`'s `CNPG_SECRET_PATH` |
| `REPOSITORY DIFFERS` | 1 | The Repository is not at the original bucket and path |
| `PASSWORD DIFFERS` | 1 | The Repository would open the repository with another key than the original did: `restic-envs`/`RESTIC_PASSWORD`, or a `RESTIC_PASSWORD*` or `RESTIC_REPOSITORY_FILE` variable |
| `NAMESPACE DIFFERS` | 1 | The ScheduledBackup or Repository is not in the namespace the original runs in |
| `SUSPEND DIFFERS`, `CONCURRENCY DIFFERS` | 1 | |
| `JOB SETTINGS DIFFER` | 1 | `activeDeadlineSeconds`, `parallelism` or `completions` |
| `IMAGE DIFFERS` | 1 | Any other image change |
| `RESOURCES DIFFER`, `SECURITY CONTEXT DIFFERS` | 1 | |
| `AFFINITY DIFFERS`, `NODE SELECTOR DIFFERS`, `TOLERATIONS DIFFER`, `PRIORITY CLASS DIFFERS` | 1 | The pod would be scheduled elsewhere, or not at all |
| `POD LABELS DIFFER`, `POD ANNOTATIONS DIFFER` | 1 | A label or annotation the original sets is gone |
| `CONTAINERS DIFFER` | 1 | The original pod runs other containers or init containers |
| `CONTAINER DIFFERS`, `POD SPEC DIFFERS` | 1 | Anything else in the container or pod spec |
| `CONTROLLER DROPPED` | 1 | The HelmRelease runs another controller that is not a `restic init` Job |
| `error: ...` | 1 | Either side could not be read or rendered, or the operator would refuse the generated backup |

These are lines, not verdicts: a run prints every one that applies. The notes
can precede a difference on a run that still exits 1. `IDENTICAL` and
`EQUIVALENT` print only when no difference was found.

### `migrate <resources/restic directory | helmrelease.yaml>` — `reads`

Converts an app's hand-written backup, an app-template HelmRelease with a
`restic` CronJob controller, into operator resources printed to stdout. Nothing
is applied and nothing in the GitOps repository is modified.

Given a directory it reads `helmrelease.yaml`, or `helm.yaml`; given a file it
reads that, such as a second HelmRelease named `restic.yaml` beside an app.

The namespace is the one the CronJob lands in when Flux applies the file: the
HelmRelease's own `spec.targetNamespace`, else the `targetNamespace` of the Flux
Kustomization whose `spec.path` holds the file (found in the file's directory or
one above it, up to the repository root), else the outermost
`kustomization.yaml` `namespace` between that path and the file, else
`metadata.namespace`. Two Flux Kustomizations for the same path with different
`targetNamespace`s are refused. The ScheduledBackup is named
after the HelmRelease and backs up into a Repository named `restic`, which reads
its password from `restic-envs`. A release that reads its repository from any
other Secret is refused, so every HelmRelease in a namespace reads the same
`restic-envs`, and therefore the same repository, and two of them come out
sharing one Repository rather than two releases collapsing into one.

The Repository adopts the existing restic repository where it is. It is in
Prefix mode with the bucket and path from the old `RESTIC_REPOSITORY`, or in
Bucket mode when the repository sits at a bucket root, and it reads the
password from `restic-envs`/`RESTIC_PASSWORD`. That URL exists only in the
encrypted Secret, so pass it:

| Flag | |
| --- | --- |
| `--repository` | The `s3:` URL, as it appears in `RESTIC_REPOSITORY` |
| `--from-secret` | A SOPS-encrypted Secret to decrypt with `sops` and read `RESTIC_REPOSITORY` from, such as `<app>-restic.yaml` in the secrets repository. Its `metadata.namespace` must be the HelmRelease's. Its other `RESTIC_*` keys are carried into `spec.env` |

Both the `s3:https://host/bucket/path` form and the scheme-less
`s3:host/bucket/path` form are accepted, and the bucket name must be one a
Repository accepts. A URL carrying credentials or a query is refused, and the
error shows it with those parts replaced by `***`.

`--from-secret` delegates decryption to the `sops` binary rather than linking
the sops library, which would add every KMS backend it supports (61MB on a 93MB
binary) to read one field once per app. A native `b2:` URL is refused: the
operator only uses the S3-compatible API.

What maps across:

| HelmRelease | ScheduledBackup |
| --- | --- |
| `restic backup --tag=T <path>...` | A `files` source with those `paths`, in one snapshot, when every path is absolute and on a mounted claim or `emptyDir` |
| `restic backup --tag=T --stdin-from-command -- backup-cnpg.sh [--dbname=D]` | A `cnpg` source, which dumps with `dumpdb cnpg`; see `parity` for what that changes |
| `restic backup ... -- dumpdb <engine>` | A source of that engine |
| `restic forget --prune --keep-...` | `spec.retention`, which the operator scopes to the backup's own tags |
| Every `existingClaim` mounted in the restic container | `spec.volumes`, with the mount path and read-only flag app-template gives each, the working directory's claim first. Like app-template, `globalMounts` and the container's `advancedMounts` both apply |
| An `emptyDir` mounted in the restic container | `spec.scratch`, with its `sizeLimit` |
| The volume restic keeps its cache on: mounted where `--cache-dir`, then `RESTIC_CACHE_DIR`, then the image's `/cache` points, and not backed up by the script | Nothing, or `spec.cache.storageClass` for a claim the chart created. The operator mounts a cache claim of its own at `/cache`, and `RESTIC_CACHE_DIR` is dropped. A volume anywhere else is data, whatever its name |
| The container's `workingDir` | `spec.workingDir`, when it is not the default the operator would use |
| The one Secret mounted | `spec.database.secretName`; a `<cluster>-app` or `<cluster>-superuser` name also sets `cnpgCluster` |
| `affinity`, `nodeSelector`, `tolerations`, `priorityClassName` and `labels`, from the controller's `pod` or else `defaultPodOptions` | `spec.affinity`, `spec.nodeSelector`, `spec.tolerations`, `spec.priorityClassName` and `spec.podLabels`, verbatim |
| Container `resources`, CronJob `timeZone`, `concurrencyPolicy` and `suspend` | The same fields. No `resources` becomes `resources: {}`, so the operator's default limits do not apply. No `timeZone` becomes `UTC`: app-template then sets none, and the CronJob runs in the kube-controller-manager's zone, which is UTC unless its container sets `TZ`. The operator sets the container's `TZ` from `timeZone`, so `parity` reports a `TZ` that differed from it |
| CronJob `successfulJobsHistory` and `failedJobsHistory` | `spec.successfulJobsHistoryLimit` and `spec.failedJobsHistoryLimit` |
| An image from `ghcr.io/gabe565/restic`, directly or through a mirror | Nothing: the operator's image is built on it. Any other image becomes `spec.image`, and must meet the image contract in the README |
| `envFrom` of `restic-envs` and `restic-b2` | The Repository's Secret, with `restic-envs` as the password's source. Given `--from-secret`, every other `RESTIC_*` key in `restic-envs` goes into `spec.env`; without it, the output warns that those keys went unchecked |
| `DB_HOST`, `DB_DATABASE` and `DB_USERNAME` beside a mariadb dump | `spec.database.host`, `name` and `user`, and not `spec.env`, where the operator refuses them |
| Plain `env` values other than `TZ`, `RESTIC_HOST`, `RESTIC_CACHE_DIR` and `CNPG_SECRET_PATH` | `spec.env` |
| A `job` controller whose one container only runs `restic init` (or `restic init \|\| true`) | Nothing. The Repository initializes or adopts the repository itself, and the output notes the drop |

A script any of whose lines cannot become sources, such as a backup with two
tags or its own `--stdin-filename`, a relative path, a line the shell would
rewrite (an escape, a variable, an unquoted glob, a redirection), or a shell
command that is not restic, is kept verbatim as `spec.script` instead, minus the preamble and `restic cache
--cleanup` the operator adds itself. If it calls `backup-cnpg.sh`,
`CNPG_SECRET_PATH` is pointed at the operator's database mount. b2bk's tag
filtering does not work on a script backup.

The script is read from the container's `command` and `args` together, as
app-template joins them. They must come to `sh -c <script>`, which is how the
operator runs its own; another shell, arguments after the script, or no
`command` at all is refused.

`migrate` builds the CronJob the operator would for what it generated, through
the operator's own validation, and refuses to print a ScheduledBackup the
operator would reject.

`spec.values` is read strictly: a key `migrate` does not carry over is refused,
not dropped. So are `spec.valuesFrom`, `spec.postRenderers`, a chart other than
`app-template`, a Helm template (`{{ ... }}`) in a value, labels or annotations
on the CronJob, pod annotations, `defaultPodOptionsStrategy`, `backoffLimit`,
`startingDeadlineSeconds`, `activeDeadlineSeconds`, a `ttlSecondsAfterFinished`
other than the operator's 3600, any `envFrom` other than `restic-envs` and
`restic-b2`, and, in `restic-envs`, a key that is not `RESTIC_*`, one that would
point restic at another repository or key (`RESTIC_REPOSITORY_FILE`,
`RESTIC_PASSWORD_FILE`, `RESTIC_PASSWORD_COMMAND`), or a credential, since
`spec.env` is plaintext.

A number in `env` is carried only when app-template would print it as written.
Helm reads values as float64, so `1000000` reaches the container as `1e+06`;
such a value is refused, to be quoted in the HelmRelease.

`migrate` refuses a HelmRelease whose `restic` controller is disabled or is not
a `cronjob`, runs init containers or any container but `restic`, or that runs
any other controller than that `restic init` Job, rather than drop them.

A Secret is mounted only for a database dump, at the operator's
`/var/run/secrets/b2backup/database`. So `migrate` refuses a mounted Secret no
dump reads, and a script kept verbatim that runs `dumpdb` without
`--secret-mount=/var/run/secrets/b2backup/database` or runs
`backup-mariadb.sh`, since either would look for its credentials elsewhere.

`migrate` refuses what a ScheduledBackup cannot mount: data at `/cache`, where
the operator mounts restic's cache, more than one Secret, more
than one `emptyDir`, any other volume type, a `subPath` or `subPathExpr`, or
`mountPropagation`.

The Repository it writes has no `lifecycle` block and the default
`deletionPolicy: Retain`. A bucket rule on a parent prefix, such as `k8s/`,
covers the path, and the operator adds no rule of its own.

Once the operator reports the Repository ready, `restic-envs` only needs
`RESTIC_PASSWORD`, and the app no longer reads `restic-b2`. Do not remove
`RESTIC_PASSWORD`: it is the only off-cluster copy of the key that decrypts
these backups.

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
# lands in ./recovered/data/app/config.php when the source is /data
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
b2bk migrate apps/kosync/resources/restic \
  --from-secret ../home-cluster-secrets/kosync-restic.yaml > generated.yaml
b2bk validate generated.yaml
b2bk parity generated.yaml apps/kosync/resources/restic \
  --from-secret ../home-cluster-secrets/kosync-restic.yaml
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
  it only to people who should hold that key. It patches Repositories and
  ScheduledBackups (`suspend`, `reinit`, `backup`), which RBAC cannot narrow to
  those fields, so it can also change `deletionPolicy`. It cannot write
  Repository status, which decides what the operator deletes.

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
