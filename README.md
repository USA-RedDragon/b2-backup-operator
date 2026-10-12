# b2-backup-operator

[![Release](https://github.com/USA-RedDragon/b2-backup-operator/actions/workflows/release.yaml/badge.svg)](https://github.com/USA-RedDragon/b2-backup-operator/actions/workflows/release.yaml) [![go.mod version](https://img.shields.io/github/go-mod/go-version/USA-RedDragon/b2-backup-operator.svg)](https://github.com/USA-RedDragon/b2-backup-operator) [![License](https://badgen.net/github/license/USA-RedDragon/b2-backup-operator)](https://github.com/USA-RedDragon/b2-backup-operator/blob/main/LICENSE) [![Release](https://img.shields.io/github/release/USA-RedDragon/b2-backup-operator.svg)](https://github.com/USA-RedDragon/b2-backup-operator/releases/)

A Kubernetes operator that backs up apps with [restic](https://restic.net) into [Backblaze B2](https://www.backblaze.com/cloud-storage) through B2's S3-compatible API. Each app gets two short custom resources: a Repository that provisions or adopts the restic repository, its bucket, its own scoped application key and its lifecycle rule, and a ScheduledBackup that renders the CronJob that backs up into it. `b2bk`, a CLI, kubectl plugin and full-screen dashboard, runs backups on demand, explains why one is not running, and browses and restores snapshots.

![The b2bk dashboard: five backups in mixed states, one backup's run history, a running backup's log with its progress bar, database and file snapshots with the file pane, a restore into a new claim with its dry run, doctor on a failing backup, and the help overlay](docs/demo/tui.gif)

The dashboard: every backup in the namespace, its logs and progress, snapshots and their files, and a restore wizard.

![b2bk in a terminal: get -A lists every repository and backup, status names the backup it picked, backup --follow streams a run, then snapshots, doctor on a failing backup, and a restore into a new claim](docs/demo/cli.gif)

The CLI: the same operations as plain-text commands, for scripts and for kubectl.

## What it does

- **Two layouts**: a bucket per repository, or a path in a shared bucket. See [Repository](docs/operator.md#repository).
- **Adopts existing repositories** in place, and refuses a path that holds files but no restic repository. See [The restic password](docs/operator.md#the-restic-password).
- **Scoped keys**: every repository gets its own B2 application key, restricted to its bucket and path, with no `deleteFiles`. See [The application key](docs/operator.md#the-application-key).
- **Keeps old file versions** for a window set by a bucket lifecycle rule, never shortening one someone else set. See [Old file versions](docs/operator.md#old-file-versions).
- **Schedules with jitter**: `@daily` and friends spread across the period and avoid daylight saving gaps. See [Schedules](docs/operator.md#schedules).
- **Sources**: `files`, `cnpg` and `mariadb`, with per-source retention, or a `script` for anything else. See [ScheduledBackup](docs/operator.md#scheduledbackup).
- **Restores** to a local directory, a new claim, in place over the source claims, or back into the database. See [`restore`](docs/b2bk.md#restore-name--destroys).
- **Safety guards**: conflict detection between repositories and tags, a finalizer that keeps the only copy of a password, and deletion that never widens its scope. See [Deletion](docs/operator.md#deletion) and [Conflicts](docs/operator.md#conflicts).
- **Migrates** hand-written restic CronJobs. See [docs/migrating.md](docs/migrating.md).

## Quick start

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

The operator runs in `b2-backup-operator-system`. Give it a B2 application key with the [capabilities it needs](docs/operator.md#b2-credentials):

```sh
kubectl -n b2-backup-operator-system create secret generic b2-credentials \
  --from-literal=B2_APPLICATION_KEY_ID=<keyID> \
  --from-literal=B2_APPLICATION_KEY=<applicationKey>
```

Then give an app a Repository and a ScheduledBackup:

```yaml
apiVersion: b2backup.mcswain.dev/v1
kind: Repository
metadata:
  name: restic
  namespace: myapp-prod
spec:
  mode: Prefix
  bucket: acme-restic
---
apiVersion: b2backup.mcswain.dev/v1
kind: ScheduledBackup
metadata:
  name: restic
  namespace: myapp-prod
spec:
  repositoryRef: {name: restic}
  schedule: "@daily"
  sources:
    - {type: files, tag: files, paths: [app]}
  retention: {daily: 30, monthly: 12}
  volumes:
    - {existingClaim: myapp-prod-storage, mountPath: /data}
```

The repository lives at `acme-restic/myapp-prod/restic`, and backs up `/data/app` once a day. Check on it with `b2bk`:

```sh
b2bk doctor -n myapp-prod        # or: kubectl b2bk doctor -n myapp-prod
b2bk -n myapp-prod               # the dashboard
```

## Installing b2bk

Download the archive for your platform from the [GitHub releases](https://github.com/USA-RedDragon/b2-backup-operator/releases), or build it from source:

```sh
go install github.com/USA-RedDragon/b2-backup-operator/cmd/kubectl-b2bk@latest
ln -s "$(go env GOPATH)/bin/kubectl-b2bk" "$(go env GOPATH)/bin/b2bk"
```

kubectl runs any `kubectl-b2bk` on PATH as `kubectl b2bk`, so install the binary under both names to use it both ways. For shell completion in bash or zsh, add `source <(b2bk completion zsh)` (or `bash`) to your shell's startup file; `b2bk completion --help` covers fish and PowerShell.

`b2bk` runs as you, not as the operator. Apply `config/rbac/b2bk_user_role.yaml` and bind `b2bk-viewer` or `b2bk-operator`; see [Permissions](docs/b2bk.md#permissions).

## Documentation

- [docs/operator.md](docs/operator.md): the Repository and ScheduledBackup resources, operator flags, B2 credentials, the backup image contract and the optional monitoring manifests.
- [docs/b2bk.md](docs/b2bk.md): every `b2bk` command and the dashboard.
- [docs/migrating.md](docs/migrating.md): moving hand-written restic CronJobs to the operator.
- [docs/development.md](docs/development.md): building, testing, linting and recording the demos.

## License

b2-backup-operator is licensed under the [MIT License](LICENSE).
