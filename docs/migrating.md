# Migrating hand-written restic CronJobs

`b2bk migrate` turns an app-template HelmRelease with a `restic` CronJob controller into a Repository that adopts the existing repository in place, plus a ScheduledBackup. `b2bk parity` then checks the result against the HelmRelease. Neither changes anything; see [b2bk.md](b2bk.md) for every flag.

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

After cutover, see [After a migration](b2bk.md#after-a-migration) for the two changes that look like faults but are not.
