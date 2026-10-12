#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$root"

cluster=b2bk-demo
bin=$root/bin
export KUBECONFIG=$bin/demo-$cluster.kubeconfig
KIND=${KIND:-$bin/kind}
KIND_NODE_IMAGE=${KIND_NODE_IMAGE:-kindest/node:v1.37.0}
manager_image=example.com/b2-backup-operator:demo
stub_image=example.com/b2-stub:demo
cnpg_url=https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v1.30.1/cnpg-1.30.1.yaml
cnpg_sha256=37237f145d8138256ea25ae830f87759255665ff08f8d552fdd8224a5ec032fb
apps=(notes wiki photos git bookmarks)

say() { printf '\033[1m==> %s\033[0m\n' "$*" >&2; }
die() { printf 'demo: %s\n' "$*" >&2; exit 1; }

guard() {
	[ -f "$KUBECONFIG" ] || die "$KUBECONFIG does not exist; run make demo-env"
	local context server want
	context=$(kubectl config current-context)
	[ "$context" = "kind-$cluster" ] || die "refusing: $KUBECONFIG has context $context, not kind-$cluster"
	server=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
	want=$("$KIND" get kubeconfig --name "$cluster" | kubectl --kubeconfig=/dev/stdin config view -o jsonpath="{.clusters[?(@.name=='kind-$cluster')].cluster.server}")
	[ -n "$want" ] && [ "$server" = "$want" ] || die "refusing: $KUBECONFIG points at $server, not kind's $want"
}

b2bk() { "$bin/b2bk" "$@"; }

cluster_up() {
	if "$KIND" get clusters 2>/dev/null | grep -qxF "$cluster"; then
		say "kind cluster $cluster exists"
	else
		say "creating kind cluster $cluster"
		"$KIND" create cluster --name "$cluster" --image "$KIND_NODE_IMAGE" --kubeconfig "$KUBECONFIG"
	fi
	"$KIND" get kubeconfig --name "$cluster" >"$KUBECONFIG.tmp"
	mv -f "$KUBECONFIG.tmp" "$KUBECONFIG"
	guard
}

images() {
	say "building images"
	make docker-build IMG="$manager_image"
	docker build -q -f test/e2e/stub/Dockerfile -t "$stub_image" .
	"$KIND" load docker-image --name "$cluster" "$manager_image" "$stub_image"
	go build -o "$bin/b2bk" ./cmd/kubectl-b2bk
}

deploy() {
	say "deploying the operator, the B2 stub and the S3 server"
	kubectl create namespace b2-backup-operator-system --dry-run=client -o yaml | kubectl apply -f -
	kubectl label --overwrite namespace b2-backup-operator-system pod-security.kubernetes.io/enforce=restricted
	make install deploy IMG="$manager_image"
	kubectl apply -f hack/demo/storage.yaml
	kubectl -n b2-backup-operator-system patch deployment b2-backup-operator-controller-manager --type=json -p \
		'[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--b2-auth-url=http://b2.b2-demo.svc:8080"}]'
	local manifest
	manifest=$(mktemp -p "$bin" cnpg.XXXXXX.yaml)
	curl -fsSL -o "$manifest" "$cnpg_url"
	echo "$cnpg_sha256  $manifest" | sha256sum -c --quiet || die "CloudNativePG manifest checksum mismatch"
	kubectl apply --server-side -f "$manifest" >/dev/null
	rm -f "$manifest"
	kubectl -n b2-demo rollout status deployment/b2 --timeout=5m
	kubectl -n b2-backup-operator-system rollout status deployment/b2-backup-operator-controller-manager --timeout=5m
	kubectl -n cnpg-system rollout status deployment/cnpg-controller-manager --timeout=5m
}

apply_apps() {
	say "creating the demo apps"
	local hour
	hour=$(( (10#$(TZ=America/Chicago date +%H) + 12) % 24 ))
	local i
	for i in $(seq 30); do
		sed "s/__HOUR__/$hour/" hack/demo/apps.yaml | kubectl apply -f - >/dev/null && break
		[ "$i" -lt 30 ] || die "could not create the demo apps"
		sleep 5
	done
	for app in "${apps[@]}"; do
		kubectl -n "$app" rollout status "deployment/$app" --timeout=5m
	done
	kubectl -n wiki wait cluster.postgresql.cnpg.io/postgresql --for=condition=Ready --timeout=10m
	for app in "${apps[@]}"; do
		kubectl -n "$app" wait repository/restic --for=condition=Ready --timeout=5m
		kubectl -n "$app" wait "scheduledbackup/$app" --for=condition=Ready --timeout=5m
	done
}

run() {
	local app=$1
	shift
	kubectl -n "$app" exec "deploy/$app" -- sh -c "$*"
}

write_round() {
	local n=$1
	run notes "mkdir -p /data/notes/journal /data/notes/projects /data/attachments
		printf '# Week %s\n\n- planned the garden beds\n- fixed the bike brakes\n' $n >/data/notes/journal/week-$n.md
		{ printf '# Reading list\n\n'; seq 1 $((n * 3)) | sed 's/^/- book /'; } >/data/notes/reading-list.md
		printf '# Home network\n\nRevision %s\n' $n >/data/notes/projects/home-network.md
		printf '# Recipes\n\n- bread\n- pasta\n- curry\n' >/data/notes/projects/recipes.md
		head -c $((n * 40000)) /dev/urandom >/data/attachments/scan-00$n.pdf"
	run wiki "mkdir -p /data/uploads/2026/09 /data/uploads/2026/10 /data/uploads/avatars
		head -c 120000 /dev/urandom >/data/uploads/2026/09/diagram-$n.png
		head -c 80000 /dev/urandom >/data/uploads/2026/10/screenshot-$n.png
		head -c 9000 /dev/urandom >/data/uploads/avatars/user-$n.png"
	kubectl -n wiki exec postgresql-1 -c postgres -- psql -q -d wiki -c \
		"set client_min_messages = warning; set role wiki;
		 create table if not exists pages (id serial primary key, title text, body text, updated timestamptz default now());
		 insert into pages (title, body) select 'Page ' || g, repeat('wiki text ', 200) from generate_series(1, $((n * 25))) g;" >/dev/null
	run photos "mkdir -p /data/library/2026/summer /data/library/2026/autumn
		for i in \$(seq 1 $((n * 4))); do f=/data/library/2026/summer/IMG_\$((1000 + i)).jpg; [ -f \$f ] || head -c 300000 /dev/urandom >\$f; done
		head -c 250000 /dev/urandom >/data/library/2026/autumn/IMG_20$n.jpg"
	run git "mkdir -p /data/repositories/infra.git/objects /data/repositories/website.git/objects /data/repositories/dotfiles.git/objects
		for r in infra website dotfiles; do head -c 30000 /dev/urandom >/data/repositories/\$r.git/objects/pack-$n.pack; printf 'ref: refs/heads/main\n' >/data/repositories/\$r.git/HEAD; done"
	run bookmarks "mkdir -p /data/export
		seq 1 $((n * 30)) | sed 's|^|https://example.org/article/|' >/data/export/bookmarks.txt
		printf '{\"version\": %s}\n' $n >/data/export/settings.json"
}

backup_round() {
	local pids=() app
	for app in "$@"; do
		b2bk backup -n "$app" "$app" --wait >/dev/null &
		pids+=($!)
	done
	local failed=0 pid
	for pid in "${pids[@]}"; do
		wait "$pid" || failed=1
	done
	return "$failed"
}

seed() {
	if kubectl -n b2-demo get configmap demo-seeded >/dev/null 2>&1; then
		say "demo data already seeded"
		return
	fi
	local n
	for n in 1 2 3; do
		say "seeding round $n"
		write_round "$n"
		backup_round "${apps[@]}"
		sleep 5
	done
	say "moving the git data so its next backup fails"
	run git "mv /data/repositories /data/repos"
	if backup_round git; then
		die "the git backup was meant to fail"
	fi
	say "suspending bookmarks"
	kubectl -n bookmarks patch scheduledbackup/bookmarks --type=merge -p '{"spec":{"suspend":true}}'
	say "measuring repository usage"
	for app in "${apps[@]}"; do
		kubectl -n "$app" patch repository/restic --subresource=status --type=json \
			-p '[{"op":"remove","path":"/status/usageTime"}]'
	done
	kubectl -n b2-backup-operator-system rollout restart deployment/b2-backup-operator-controller-manager
	kubectl -n b2-backup-operator-system rollout status deployment/b2-backup-operator-controller-manager --timeout=5m
	kubectl -n b2-demo create configmap demo-seeded --from-literal=at="$(date -u +%FT%TZ)"
}

stage() {
	guard
	say "staging the recording"
	local app
	for app in "${apps[@]}"; do
		kubectl -n "$app" get pvc -o name | { grep -e '-restore' || true; } | xargs -r kubectl -n "$app" delete
	done
	if ! kubectl -n photos get jobs -o jsonpath='{range .items[*]}{.status.active}{"\n"}{end}' | grep -q 1; then
		run photos "rm -rf /data/library/2026/raw && mkdir -p /data/library/2026/raw && cd /data/library/2026/raw && for i in \$(seq 1 40); do head -c 30000000 /dev/urandom >DSC_\$((4100 + i)).NEF; done"
		b2bk backup -n photos photos >/dev/null
	fi
	local i
	for i in $(seq 60); do
		[ -n "$(kubectl -n photos get pods -l batch.kubernetes.io/job-name --field-selector=status.phase=Running -o name)" ] && return
		sleep 2
	done
	die "the photos backup did not start"
}

env_up() {
	cluster_up
	images
	deploy
	apply_apps
	seed
	say "demo environment ready: KUBECONFIG=$KUBECONFIG"
}

clean() {
	if "$KIND" get clusters 2>/dev/null | grep -qxF "$cluster"; then
		"$KIND" delete cluster --name "$cluster" --kubeconfig "$KUBECONFIG"
	fi
	rm -f "$KUBECONFIG" "$KUBECONFIG.tmp"
	docker image rm "$manager_image" "$stub_image" >/dev/null 2>&1 || true
}

case ${1:-} in
up) env_up ;;
stage) stage ;;
guard) guard ;;
clean) clean ;;
*) die "usage: $0 up|stage|guard|clean" ;;
esac
