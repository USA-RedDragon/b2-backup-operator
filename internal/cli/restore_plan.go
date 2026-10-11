package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

const (
	latest   = "latest"
	jsonFlag = "--json"
	fileNode = "file"
)

type resticSnapshot struct {
	ID       string    `json:"id"`
	ShortID  string    `json:"short_id"`
	Time     time.Time `json:"time"`
	Paths    []string  `json:"paths"`
	Hostname string    `json:"hostname"`
	Tags     []string  `json:"tags"`
}

func (s resticSnapshot) short() string {
	if s.ShortID != "" {
		return s.ShortID
	}
	return s.ID[:min(len(s.ID), 8)]
}

// parseSnapshots reads restic snapshots --json, flat or grouped.
func parseSnapshots(data []byte) ([]resticSnapshot, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("reading restic snapshots --json: %w", err)
	}
	var out []resticSnapshot
	for _, item := range raw {
		var group struct {
			GroupKey  json.RawMessage  `json:"group_key"`
			Snapshots []resticSnapshot `json:"snapshots"`
		}
		if err := json.Unmarshal(item, &group); err == nil && group.GroupKey != nil {
			out = append(out, group.Snapshots...)
			continue
		}
		var snap resticSnapshot
		if err := json.Unmarshal(item, &snap); err != nil {
			return nil, fmt.Errorf("reading restic snapshots --json: %w", err)
		}
		out = append(out, snap)
	}
	return out, nil
}

// restoreSource is what one restore reads: the snapshots of one tag, on one host,
// of exactly these paths. paths is empty for a database dump. exclude is what
// the source's backups leave out, which a restore must not delete; database is
// the database a dump goes back into, when the source names one.
type restoreSource struct {
	tag      string
	host     string
	paths    []string
	exclude  []string
	database string
}

func (o *restoreOptions) wantsLatest() bool {
	return o.snapshot == "" || o.snapshot == latest
}

func (o *restoreOptions) snapshotsQuery(src restoreSource) []string {
	argv := []string{"snapshots", jsonFlag, "--group-by="}
	if !o.wantsLatest() {
		return append(argv, o.snapshot)
	}
	argv = append(argv, "--tag="+src.tag, "--host="+src.host)
	for _, p := range src.paths {
		argv = append(argv, "--path="+p)
	}
	return argv
}

func under(p, dir string) bool {
	return p == dir || dir == "/" || strings.HasPrefix(p, dir+"/")
}

// chooseSnapshot picks the snapshot to restore from what snapshotsQuery listed.
// The latest one must carry the source's tag and host and exactly its paths; a
// named one must carry the tag, and every one of its paths must lie under one of
// the source's. A database dump is one stdin file, so it must have one path.
func (o *restoreOptions) chooseSnapshot(snaps []resticSnapshot, src restoreSource) (resticSnapshot, error) {
	database := len(src.paths) == 0
	fits := func(s resticSnapshot) error {
		if !slices.Contains(s.Tags, src.tag) {
			return fmt.Errorf("%w: snapshot %s is tagged %v, not %s", ErrWrongSnapshot, s.short(), s.Tags, src.tag)
		}
		if database {
			if len(s.Paths) != 1 {
				return fmt.Errorf("%w: snapshot %s holds %v, not one database dump", ErrWrongSnapshot, s.short(), s.Paths)
			}
			return nil
		}
		for _, p := range s.Paths {
			if !slices.ContainsFunc(src.paths, func(dir string) bool { return under(p, dir) }) {
				return fmt.Errorf("%w: snapshot %s holds %s, which is not under %s",
					ErrWrongSnapshot, s.short(), p, strings.Join(src.paths, ", "))
			}
		}
		return nil
	}

	if !o.wantsLatest() {
		if len(snaps) != 1 {
			return resticSnapshot{}, fmt.Errorf("snapshot %q matched %d snapshots", o.snapshot, len(snaps))
		}
		return snaps[0], fits(snaps[0])
	}

	want := slices.Sorted(slices.Values(src.paths))
	var best *resticSnapshot
	for i := range snaps {
		s := &snaps[i]
		if fits(*s) != nil || s.Hostname != src.host {
			continue
		}
		if !database && !slices.Equal(slices.Sorted(slices.Values(s.Paths)), want) {
			continue
		}
		if best == nil || s.Time.After(best.Time) {
			best = s
		}
	}
	if best != nil && database {
		if err := oneDumpSeries(snaps, src, fits); err != nil {
			return resticSnapshot{}, err
		}
	}
	if best == nil {
		what := "of " + strings.Join(src.paths, ", ")
		if database {
			what = "holding a database dump"
		}
		return resticSnapshot{}, fmt.Errorf("no snapshot tagged %s on host %s %s; list them with "+
			"b2bk snapshots and pass --snapshot", src.tag, src.host, what)
	}
	return *best, nil
}

// oneDumpSeries refuses a latest dump when the tag holds dumps of more than one
// file. A dump moving from <host> to <host>.dmp, as backup-cnpg.sh dumps did
// when they became dumpdb cnpg ones, is one series.
func oneDumpSeries(snaps []resticSnapshot, src restoreSource, fits func(resticSnapshot) error) error {
	var series []string
	for _, s := range snaps {
		if fits(s) != nil || s.Hostname != src.host {
			continue
		}
		name := strings.TrimSuffix(strings.TrimSuffix(s.Paths[0], ".dmp"), ".sql")
		if !slices.Contains(series, name) {
			series = append(series, name)
		}
	}
	if len(series) > 1 {
		return fmt.Errorf("%w: tag %s holds dumps of %s; pass a --tag only one of them carries, or --snapshot",
			ErrWrongSnapshot, src.tag, strings.Join(series, ", "))
	}
	return nil
}

// restoreStep restores one path of a snapshot: the snapshot subfolder, and
// within it the --path selection, if any, as an include relative to it. A file
// is restored from its directory, since restic cannot use a file as the
// subfolder; file is then set.
type restoreStep struct {
	snapPath string
	sub      string
	file     bool
}

func (o *restoreOptions) steps(snap resticSnapshot, types map[string]string) ([]restoreStep, error) {
	paths := slices.Sorted(slices.Values(snap.Paths))
	if o.path == "" {
		steps := make([]restoreStep, 0, len(paths))
		for _, p := range paths {
			steps = append(steps, stepFor(p, "", types))
		}
		return steps, nil
	}
	want := o.path
	if !path.IsAbs(want) {
		if len(paths) != 1 {
			return nil, fmt.Errorf("--path %s is relative, but snapshot %s holds %s; give an absolute path",
				want, snap.short(), strings.Join(paths, ", "))
		}
		want = path.Join(paths[0], want)
	}
	want = path.Clean(want)
	for _, p := range paths {
		if !under(want, p) {
			continue
		}
		sub := ""
		if want != p {
			sub = "/" + strings.TrimPrefix(strings.TrimPrefix(want, p), "/")
		}
		return []restoreStep{stepFor(p, sub, types)}, nil
	}
	return nil, fmt.Errorf("--path %s is not in snapshot %s, which holds %s",
		o.path, snap.short(), strings.Join(paths, ", "))
}

func stepFor(snapPath, sub string, types map[string]string) restoreStep {
	if sub == "" && types[snapPath] == fileNode {
		return restoreStep{snapPath: path.Dir(snapPath), sub: "/" + path.Base(snapPath), file: true}
	}
	return restoreStep{snapPath: snapPath, sub: sub}
}

var globMeta = strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`)

// literalPattern makes a path into a restic pattern matching only that path.
func literalPattern(p string) string { return globMeta.Replace(p) }

// sourceExcludes rewrites the source's backup excludes for a restore rooted at
// snapPath. A pattern without a leading slash matches anywhere and is kept; an
// absolute one is made relative to snapPath, or dropped when outside it.
func sourceExcludes(exclude []string, snapPath string) []string {
	var out []string
	for _, p := range exclude {
		if !strings.HasPrefix(p, "/") {
			out = append(out, p)
			continue
		}
		if under(p, snapPath) && p != snapPath {
			out = append(out, "/"+strings.TrimPrefix(strings.TrimPrefix(p, snapPath), "/"))
		}
	}
	return out
}

// restoreArgs builds restic restore for one step. restic refuses includes and
// excludes together, so the source's excludes, which keep --delete off what the
// backup never saved, are passed only when nothing is included; with an include,
// --delete is refused when the source excludes anything.
func (o *restoreOptions) restoreArgs(
	snap resticSnapshot, step restoreStep, src restoreSource, target string,
) ([]string, error) {
	argv := []string{"restore", snap.ID + ":" + step.snapPath, "--target=" + target}
	var includes []string
	if step.sub != "" {
		includes = append(includes, literalPattern(step.sub))
	}
	includes = append(includes, o.include...)
	excludes := sourceExcludes(src.exclude, step.snapPath)
	switch {
	case len(includes) > 0 && len(o.exclude) > 0:
		return nil, fmt.Errorf("--exclude cannot be combined with --path or --include: restic restore takes " +
			"one or the other")
	case len(includes) > 0 && len(excludes) > 0 && o.delete:
		return nil, fmt.Errorf("--delete with --path or --include would remove what the backup excludes (%s) "+
			"under the restored path, since restic restore cannot take both; drop --delete, or restore the "+
			"whole source", strings.Join(excludes, ", "))
	case len(includes) > 0:
		excludes = nil
	}
	for _, p := range includes {
		argv = append(argv, "--include="+p)
	}
	for _, p := range append(excludes, o.exclude...) {
		argv = append(argv, "--exclude="+p)
	}
	if o.delete {
		argv = append(argv, "--delete")
	}
	if o.dryRun {
		argv = append(argv, "--dry-run", "--verbose=2")
	}
	return argv, nil
}

func dumpArgs(snap resticSnapshot, step restoreStep) []string {
	sub := step.sub
	if sub == "" {
		sub = "/"
	}
	return []string{"dump", "--archive=tar", snap.ID + ":" + step.snapPath, sub}
}

func listArgs(snap resticSnapshot, step restoreStep) []string {
	argv := []string{"ls", "--long", "--recursive", snap.ID + ":" + step.snapPath}
	if step.sub != "" {
		argv = append(argv, step.sub)
	}
	return argv
}

// filesSource returns the files source to restore: the one tagged --tag, or the
// only one there is.
func (o *restoreOptions) filesSource(sb *b2backupv1.ScheduledBackup) (restoreSource, error) {
	var tags []string
	var chosen *b2backupv1.BackupSource
	for i := range sb.Spec.Sources {
		src := &sb.Spec.Sources[i]
		if src.Type != b2backupv1.SourceTypeFiles {
			continue
		}
		tags = append(tags, src.EffectiveTag())
		if o.tag == "" || src.EffectiveTag() == o.tag {
			chosen = src
		}
	}
	switch {
	case len(tags) == 0:
		return restoreSource{}, fmt.Errorf("%w: scheduledbackup/%s backs up no files sources; download a script "+
			"backup's files with b2bk exec %s -- restic dump", ErrNoFilesSource, sb.Name, sb.Name)
	case o.tag == "" && len(tags) > 1:
		return restoreSource{}, fmt.Errorf("%w: scheduledbackup/%s has files sources tagged %s; pass --tag",
			ErrNoFilesSource, sb.Name, strings.Join(tags, ", "))
	case chosen == nil:
		return restoreSource{}, fmt.Errorf("%w: scheduledbackup/%s has no files source tagged %s, only %s",
			ErrNoFilesSource, sb.Name, o.tag, strings.Join(tags, ", "))
	}
	return restoreSource{
		tag:     chosen.EffectiveTag(),
		host:    sb.EffectiveResticHost(),
		paths:   slices.Sorted(slices.Values(chosen.ResolvedPaths(sb.EffectiveWorkingDir()))),
		exclude: chosen.Exclude,
	}, nil
}

// sourceClaims returns the claims a volume restore of this backup can write to.
func sourceClaims(sb *b2backupv1.ScheduledBackup) []string {
	var claims []string
	for _, src := range sb.Spec.Sources {
		if src.Type != b2backupv1.SourceTypeFiles {
			continue
		}
		for _, p := range src.ResolvedPaths(sb.EffectiveWorkingDir()) {
			if v := backup.MountFor(sb, p); v != nil && !slices.Contains(claims, "pvc/"+v.ExistingClaim) {
				claims = append(claims, "pvc/"+v.ExistingClaim)
			}
		}
	}
	return claims
}

// restorePod is a running pod a restore executes restic in.
type restorePod interface {
	exec(ctx context.Context, argv []string, stdout io.Writer) error
}

// resolve picks the snapshot to restore, and lists what kind of node each of its
// paths is.
func (o *restoreOptions) resolve(
	ctx context.Context, pod restorePod, src restoreSource,
) (resticSnapshot, map[string]string, error) {
	var out bytes.Buffer
	if err := pod.exec(ctx, resticCommand(o.snapshotsQuery(src)...), &out); err != nil {
		return resticSnapshot{}, nil, fmt.Errorf("listing snapshots: %w", err)
	}
	snaps, err := parseSnapshots(out.Bytes())
	if err != nil {
		return resticSnapshot{}, nil, err
	}
	snap, err := o.chooseSnapshot(snaps, src)
	if err != nil {
		return snap, nil, err
	}
	var listing bytes.Buffer
	argv := append([]string{"ls", jsonFlag, snap.ID}, snap.Paths...)
	if err := pod.exec(ctx, resticCommand(argv...), &listing); err != nil {
		return snap, nil, fmt.Errorf("listing snapshot %s: %w", snap.short(), err)
	}
	types := map[string]string{}
	for _, p := range snap.Paths {
		types[p] = nodeType(listing.Bytes(), p)
	}
	return snap, types, nil
}

// volumeTarget maps a path in the snapshot to where it is restored in the pod.
type volumeTarget func(snapPath string) (string, error)

// volumePlan is a resolved volume restore: the snapshot, what to restore from it,
// and where each part goes in the pod.
type volumePlan struct {
	snap    resticSnapshot
	steps   []restoreStep
	targets []string
}

func (o *restoreOptions) planVolumes(
	ctx context.Context, pod restorePod, src restoreSource, target volumeTarget,
) (volumePlan, error) {
	snap, types, err := o.resolve(ctx, pod, src)
	if err != nil {
		return volumePlan{}, err
	}
	steps, err := o.steps(snap, types)
	if err != nil {
		return volumePlan{}, err
	}
	plan := volumePlan{snap: snap, steps: steps, targets: make([]string, len(steps))}
	for i, step := range steps {
		if plan.targets[i], err = target(step.snapPath); err != nil {
			return volumePlan{}, err
		}
		if _, err := o.restoreArgs(snap, step, src, plan.targets[i]); err != nil {
			return volumePlan{}, err
		}
	}
	return plan, nil
}

func (o *restoreOptions) runVolumes(
	ctx context.Context, pod restorePod, src restoreSource, plan volumePlan, out io.Writer,
) error {
	for i, step := range plan.steps {
		p := newPrinter(out)
		p.printf("restoring %s:%s%s into %s\n", plan.snap.short(), step.snapPath, step.sub, plan.targets[i])
		if err := p.Err(); err != nil {
			return err
		}
		argv, err := o.restoreArgs(plan.snap, step, src, plan.targets[i])
		if err != nil {
			return err
		}
		if err := pod.exec(ctx, resticCommand(argv...), out); err != nil {
			return err
		}
	}
	return nil
}

func (o *restoreOptions) restoreVolumes(
	ctx context.Context, pod restorePod, src restoreSource, target volumeTarget, out io.Writer,
) error {
	plan, err := o.planVolumes(ctx, pod, src, target)
	if err != nil {
		return err
	}
	return o.runVolumes(ctx, pod, src, plan, out)
}

func inPlaceTarget(sb *b2backupv1.ScheduledBackup) volumeTarget {
	return func(p string) (string, error) {
		if backup.MountFor(sb, p) == nil {
			return "", fmt.Errorf("%s is on none of scheduledbackup/%s's claims, so it cannot be restored in place",
				p, sb.Name)
		}
		return p, nil
	}
}

func newClaimTarget(sb *b2backupv1.ScheduledBackup, claim *b2backupv1.VolumeSpec, mountPath string) volumeTarget {
	return func(p string) (string, error) {
		v := backup.MountFor(sb, p)
		if v == nil || v.ExistingClaim != claim.ExistingClaim || v.EffectiveMountPath() != claim.EffectiveMountPath() {
			return "", fmt.Errorf("%s is not on pvc/%s, the claim the new one copies; restore it with --tag "+
				"or --path one claim at a time", p, claim.ExistingClaim)
		}
		return path.Join(mountPath, strings.TrimPrefix(p, claim.EffectiveMountPath())), nil
	}
}

// sourceVolume returns the one claim the source's paths are on.
func sourceVolume(sb *b2backupv1.ScheduledBackup, src restoreSource) (*b2backupv1.VolumeSpec, error) {
	var found *b2backupv1.VolumeSpec
	for _, p := range src.paths {
		v := backup.MountFor(sb, p)
		switch {
		case v == nil:
			return nil, fmt.Errorf("%w: %s is on none of scheduledbackup/%s's claims", ErrNoSourceVolume, p, sb.Name)
		case found != nil && found.ExistingClaim != v.ExistingClaim:
			return nil, fmt.Errorf("the source tagged %s spans pvc/%s and pvc/%s; --to-new-pvc copies one claim, "+
				"so restore --in-place or --to DIR", src.tag, found.ExistingClaim, v.ExistingClaim)
		}
		found = v
	}
	return found, nil
}

func (o *restoreOptions) databaseSource(sb *b2backupv1.ScheduledBackup) (restoreSource, error) {
	src := restoreSource{tag: o.tag, host: sb.EffectiveResticHost(), database: o.database}
	for i := range sb.Spec.Sources {
		s := &sb.Spec.Sources[i]
		if s.Type == b2backupv1.SourceTypeFiles || (o.tag != "" && s.EffectiveTag() != o.tag) {
			continue
		}
		src.tag = s.EffectiveTag()
		if src.database == "" {
			src.database = s.Database
		}
		return src, nil
	}
	if o.tag == "" {
		return restoreSource{}, fmt.Errorf("%w: scheduledbackup/%s has no database source; pass --tag with the "+
			"tag its script gives the dump", ErrNoDatabase, sb.Name)
	}
	if slices.Contains(backup.SourceTags(sb.Spec.Sources), o.tag) {
		return restoreSource{}, fmt.Errorf("%w: the source tagged %s is not a database source", ErrNoDatabase, o.tag)
	}
	return src, nil
}

func restoreDatabaseCommand(sb *b2backupv1.ScheduledBackup, snap resticSnapshot, database string, dryRun bool) []string {
	pipeline := fmt.Sprintf("restic dump %s %s | restoredb %s --secret-mount=%s",
		shellQuote(snap.ID), shellQuote(snap.Paths[0]), string(sb.Spec.Database.Engine), b2backupv1.DBSecretMountPath)
	if database != "" {
		pipeline += " --database=" + shellQuote(database)
	}
	if dryRun {
		pipeline += " --dry-run"
	}
	guarded := fmt.Sprintf(
		"set -o pipefail\ncommand -v restoredb >/dev/null 2>&1 || { echo %s >&2; exit 1; }\n%s",
		shellQuote("this backup image has no restoredb; re-pull ghcr.io/usa-reddragon/b2-backup-operator-restic "+
			"or unpin spec.image"), pipeline)
	return []string{"sh", "-c", guarded}
}

func databaseSourceTag(sb *b2backupv1.ScheduledBackup) string {
	for i := range sb.Spec.Sources {
		switch sb.Spec.Sources[i].Type {
		case b2backupv1.SourceTypeCNPG, b2backupv1.SourceTypeMariaDB:
			return sb.Spec.Sources[i].EffectiveTag()
		}
	}
	return ""
}

type lsNode struct {
	Type string `json:"type"`
	Path string `json:"path"`
}

// nodeType reads restic ls --json output for the type of the node at sub.
func nodeType(data []byte, sub string) string {
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		var n lsNode
		if json.Unmarshal(line, &n) == nil && n.Path == sub && n.Type != "" {
			return n.Type
		}
	}
	return ""
}
