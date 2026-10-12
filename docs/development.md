# Development

```sh
make manifests generate        # regenerate CRDs, RBAC and deepcopy
make test                      # unit tests, no cluster needed
make test-crd                  # CRD validation and Event timestamps against a real API server (envtest)
make test-e2e                  # full deploy against a throwaway Kind cluster
make lint                      # golangci-lint with the logcheck plugin
make build-installer IMG=...   # render dist/install.yaml
```

The recordings in [docs/demo](demo) are [VHS](https://github.com/charmbracelet/vhs) tapes. `make demo` records them again against a throwaway Kind cluster, `b2bk-demo`, through its own kubeconfig in `bin/`: it runs the operator, the B2 API stub from the e2e tests and an S3 server, seeds a few apps with backups, and renders `docs/demo/*.gif`. The Makefile fetches VHS and ttyd into `bin/`; Docker, ffmpeg and Chrome or Chromium must be installed. `make demo-clean` deletes the cluster and its images.

The e2e suite creates its Kind cluster, `b2-backup-operator-test-e2e` (`KIND_CLUSTER`), from the node image in `KIND_NODE_IMAGE`, with kind at `KIND_VERSION`. Both are pinned in the Makefile, and `make demo` uses the same node image. Override one on the command line to test against another Kubernetes version, such as `make test-e2e KIND_NODE_IMAGE=kindest/node:<version>`. The suite never uses the current kube context: it talks to the cluster only through `bin/e2e-<cluster>.kubeconfig`. `make cleanup-test-e2e` deletes the cluster.

`make lint` installs golangci-lint at `GOLANGCI_LINT_VERSION` into `bin/` on first use, then builds a custom binary from `.custom-gcl.yml` with the Kubernetes logcheck plugin compiled in. logcheck enforces the logging conventions in [AGENTS.md](../AGENTS.md#logging). The Makefile and `.custom-gcl.yml` both pin the golangci-lint version; keep them equal. `make lint-config` checks `.golangci.yml`.

Most of the validation lives in CEL rules on the CRDs, which no Go test can reach: a rule that fails to compile makes every CRD install fail, and one that never fires is invisible. `make test-crd` runs them against a real API server.

Pushes to `main` publish `beta` images. A `v*` tag publishes the manager image to `ghcr.io/usa-reddragon/b2-backup-operator`, the rendered manifests to `ghcr.io/usa-reddragon/b2-backup-operator-manifests`, and the `b2bk` archives and checksums to the GitHub release.
