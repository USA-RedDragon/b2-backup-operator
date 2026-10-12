package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/kube"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
	"github.com/USA-RedDragon/b2-backup-operator/internal/tui"
)

const (
	watchSyncTimeout = 30 * time.Second
	logTailLines     = 5
)

// addTUIFlags registers the TUI's flags on cmd.
func addTUIFlags(cmd *cobra.Command) {
	cmd.Flags().BoolP("all-namespaces", "A", false,
		"Start on every namespace instead of the current one (config: tui.allNamespaces, env: "+
			tui.EnvAllNamespaces+")")
	cmd.Flags().Bool("mouse", true,
		"Capture the mouse for clicks and the wheel; m toggles it inside (config: tui.mouse, env: "+tui.EnvMouse+")")
}

func tuiOptionsFrom(cmd *cobra.Command) (tui.Options, error) {
	var flags tui.Flags
	if cmd.Flags().Changed("all-namespaces") {
		v, _ := cmd.Flags().GetBool("all-namespaces")
		flags.AllNamespaces = &v
	}
	if cmd.Flags().Changed("mouse") {
		v, _ := cmd.Flags().GetBool("mouse")
		flags.Mouse = &v
	}
	return tui.LoadOptions(flags, os.Getenv)
}

func runTUI(cmd *cobra.Command, f *Factory) error {
	opts, err := tuiOptionsFrom(cmd)
	if err != nil {
		return err
	}
	if f.tuiHook != nil {
		return f.tuiHook(cmd.Context(), opts)
	}
	klog.SetLogger(logr.Discard())
	ctrllog.SetLogger(logr.Discard())
	return tui.Run(cmd.Context(), &tuiBackend{f: f}, opts, f.Streams.In, f.Streams.Out)
}

func newTUICommand(f *Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tui",
		Short:   "Open the full-screen dashboard, as a bare b2bk does on a terminal",
		GroupID: GroupInspect,
		Long: `Open the full-screen dashboard of every backup in the namespace, or in
every namespace with -A.

A bare b2bk opens it too when both ends are a terminal; piped, redirected or
with TERM=dumb it prints help instead. Settings come from flags, then the
B2BK_ALL_NAMESPACES and B2BK_MOUSE environment variables, then
~/.config/b2bk/config.yaml (tui.allNamespaces, tui.mouse), then the defaults.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !f.canDrawTUI() {
				return tui.ErrNoTerminal
			}
			return runTUI(cmd, f)
		},
	}
	addTUIFlags(cmd)
	return cmd
}

// tuiBackend gives the TUI b2bk's own clients and commands.
type tuiBackend struct {
	f *Factory
}

var _ tui.Backend = (*tuiBackend)(nil)

func (b *tuiBackend) Context() string {
	if name := *b.f.ConfigFlags.Context; name != "" {
		return name
	}
	raw, err := b.f.ConfigFlags.ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return ""
	}
	return raw.CurrentContext
}

func (b *tuiBackend) Namespace() (string, error) { return b.f.Namespace() }

func (b *tuiBackend) Reader() (client.Reader, error) { return b.f.Client() }

func (b *tuiBackend) Clientset() (kubernetes.Interface, error) { return b.f.Clientset() }

// on derives a Factory for one action in namespace, writing to w.
func (b *tuiBackend) on(namespace string, w io.Writer, confirmWith func(what, name string) error) *Factory {
	return b.f.derive(namespace, genericiooptions.IOStreams{In: &bytes.Buffer{}, Out: w, ErrOut: w}, confirmWith)
}

func (b *tuiBackend) backup(ctx context.Context, f *Factory, name string) (*b2backupv1.ScheduledBackup, error) {
	c, err := f.Client()
	if err != nil {
		return nil, err
	}
	ns, err := f.Namespace()
	if err != nil {
		return nil, err
	}
	target, err := Resolve(ctx, c, ns, "sb/"+name)
	if err != nil {
		return nil, err
	}
	return target.ScheduledBackup, nil
}

func watchObject(kind tui.Kind) client.Object {
	switch kind {
	case tui.KindScheduledBackup:
		return &b2backupv1.ScheduledBackup{}
	case tui.KindRepository:
		return &b2backupv1.Repository{}
	case tui.KindCronJob:
		return &batchv1.CronJob{}
	case tui.KindJob:
		return &batchv1.Job{}
	}
	return &corev1.Pod{}
}

// Watch starts one informer per kind in namespace, or in every namespace.
// CronJobs, Jobs and pods are narrowed to the operator's and b2bk's own.
func (b *tuiBackend) Watch(ctx context.Context, namespace string, kinds []tui.Kind, changed func()) (
	client.Reader, error,
) {
	cfg, err := b.f.RESTConfig()
	if err != nil {
		return nil, err
	}
	ours, err := labels.NewRequirement(labelManagedBy, selection.In, []string{operatorManagedBy, runner.ManagedByValue})
	if err != nil {
		return nil, err
	}
	byOurs := cache.ByObject{Label: labels.NewSelector().Add(*ours)}
	opts := cache.Options{
		Scheme: b.f.Scheme(),
		ByObject: map[client.Object]cache.ByObject{
			&batchv1.CronJob{}: {Label: labels.SelectorFromSet(labels.Set{labelManagedBy: operatorManagedBy})},
			&batchv1.Job{}:     byOurs,
			&corev1.Pod{}:      byOurs,
		},
	}
	if namespace != "" {
		opts.DefaultNamespaces = map[string]cache.Config{namespace: {}}
	}
	c, err := cache.New(cfg, opts)
	if err != nil {
		return nil, err
	}
	handler := toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { changed() },
		UpdateFunc: func(any, any) { changed() },
		DeleteFunc: func(any) { changed() },
	}
	for _, kind := range kinds {
		inf, err := c.GetInformer(ctx, watchObject(kind))
		if err != nil {
			return nil, err
		}
		if _, err := inf.AddEventHandler(handler); err != nil {
			return nil, err
		}
	}
	go func() { _ = c.Start(ctx) }()
	syncCtx, cancel := context.WithTimeout(ctx, watchSyncTimeout)
	defer cancel()
	if !c.WaitForCacheSync(syncCtx) {
		return nil, fmt.Errorf("the watches on %v did not sync within %s", kinds, watchSyncTimeout)
	}
	return c, nil
}

func (b *tuiBackend) Operator(ctx context.Context) tui.OperatorInfo {
	c, err := b.f.Client()
	if err != nil {
		return tui.OperatorInfo{}
	}
	var deployments appsv1.DeploymentList
	if err := c.List(ctx, &deployments, operatorDeploymentLabels); err != nil || len(deployments.Items) == 0 {
		return tui.OperatorInfo{}
	}
	d := &deployments.Items[0]
	info := tui.OperatorInfo{Found: true, Ready: d.Status.AvailableReplicas > 0}
	for _, ctr := range d.Spec.Template.Spec.Containers {
		if ctr.Name == "manager" {
			if i := strings.LastIndex(ctr.Image, ":"); i >= 0 && !strings.Contains(ctr.Image[i:], "/") {
				info.Version = strings.SplitN(ctr.Image[i+1:], "@", 2)[0]
			}
		}
	}
	return info
}

func (b *tuiBackend) BackupNow(ctx context.Context, namespace, name string) (string, error) {
	var out bytes.Buffer
	f := b.on(namespace, &out, nil)
	sb, err := b.backup(ctx, f, name)
	if err != nil {
		return "", err
	}
	c, err := f.Client()
	if err != nil {
		return "", err
	}
	cs, err := f.Clientset()
	if err != nil {
		return "", err
	}
	err = runBackupNow(ctx, c, cs, &out, sb, false, false, defaultWaitTimeout)
	return out.String(), err
}

func (b *tuiBackend) SetSuspend(ctx context.Context, namespace, name string, suspend bool) (string, error) {
	var out bytes.Buffer
	c, err := b.f.Client()
	if err != nil {
		return "", err
	}
	past := "resumed"
	if suspend {
		past = "suspended"
	}
	err = runSetSuspend(ctx, c, &out, namespace, "sb/"+name, suspend, past)
	return out.String(), err
}

func (b *tuiBackend) Cancel(ctx context.Context, namespace, name string) (string, error) {
	var out bytes.Buffer
	f := b.on(namespace, &out, nil)
	sb, err := b.backup(ctx, f, name)
	if err != nil {
		return "", err
	}
	c, err := f.Client()
	if err != nil {
		return "", err
	}
	err = runCancel(ctx, c, &out, sb)
	return out.String(), err
}

func (b *tuiBackend) Doctor(ctx context.Context, namespace, name string) ([]tui.Check, error) {
	c, err := b.f.Client()
	if err != nil {
		return nil, err
	}
	reports, err := collectReports(ctx, c, namespace, "sb/"+name)
	if err != nil {
		return nil, err
	}
	var checks []tui.Check
	for _, r := range reports {
		for _, chk := range r.checks {
			checks = append(checks, tui.Check{
				Subject: r.subject, Level: tui.Level(chk.level), Summary: chk.summary, Detail: chk.detail,
				Next: nextSteps(chk),
			})
		}
	}
	return checks, nil
}

// nextSteps reads the b2bk commands a doctor check suggests as TUI steps.
func nextSteps(chk check) []tui.Step {
	var steps []tui.Step
	add := func(s tui.Step) {
		if !slices.Contains(steps, s) {
			steps = append(steps, s)
		}
	}
	for _, line := range chk.detail {
		switch {
		case strings.Contains(line, "b2bk logs sb/"):
			add(tui.StepLogs)
			if chk.level == levelFail {
				add(tui.StepBackup)
			}
		case strings.Contains(line, "b2bk backup sb/"):
			add(tui.StepBackup)
		case strings.Contains(line, "Clear spec.suspend"):
			add(tui.StepResume)
		}
	}
	return steps
}

func (b *tuiBackend) job(ctx context.Context, namespace, name string) (*batchv1.Job, error) {
	c, err := b.f.Client()
	if err != nil {
		return nil, err
	}
	var job batchv1.Job
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &job); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: job/%s is gone; Jobs are removed an hour after they finish", ErrNoRuns, name)
		}
		return nil, err
	}
	return &job, nil
}

// Logs streams a Job's logs as b2bk logs does, with timestamps. A Job still
// starting is waited for, as b2bk backup --follow does.
func (b *tuiBackend) Logs(ctx context.Context, namespace, name string, follow bool, w io.Writer) error {
	c, err := b.f.Client()
	if err != nil {
		return err
	}
	cs, err := b.f.Clientset()
	if err != nil {
		return err
	}
	job, err := b.job(ctx, namespace, name)
	if err != nil {
		return err
	}
	if follow && jobIsRunning(job) {
		if job, err = waitForJobPod(ctx, c, client.ObjectKeyFromObject(job), defaultWaitTimeout); err != nil {
			return err
		}
	}
	return streamJobLogs(ctx, c, cs, w, job, corev1.PodLogOptions{Follow: follow, Timestamps: true})
}

func (b *tuiBackend) LastLogLine(ctx context.Context, namespace, name string) string {
	c, err := b.f.Client()
	if err != nil {
		return ""
	}
	cs, err := b.f.Clientset()
	if err != nil {
		return ""
	}
	job, err := b.job(ctx, namespace, name)
	if err != nil {
		return ""
	}
	tail := int64(logTailLines)
	var out bytes.Buffer
	if err := streamJobLogs(ctx, c, cs, &out, job, corev1.PodLogOptions{TailLines: &tail}); err != nil {
		return ""
	}
	last := ""
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" && !strings.HasPrefix(line, "# pod/") {
			last = line
		}
	}
	return last
}

func (b *tuiBackend) Check(ctx context.Context, namespace, arg, readData string, w io.Writer) error {
	f := b.on(namespace, w, nil)
	return runRestic(ctx, f, arg, "check",
		func(*b2backupv1.ScheduledBackup, *b2backupv1.Repository) ([]string, error) {
			argv := []string{"check"}
			if readData != "" {
				argv = append(argv, "--read-data-subset="+readData)
			}
			return resticCommand(argv...), nil
		},
		runner.Options{})
}

// sessionPod is the pod the TUI keeps for one backup while it browses.
type sessionPod struct {
	name   string
	pod    restorePod
	cancel context.CancelFunc
	done   chan error
}

func (p *sessionPod) Name() string { return p.name }

func (p *sessionPod) Exec(ctx context.Context, argv []string, stdout io.Writer) error {
	return p.exec(ctx, argv, stdout)
}

// exec runs argv in the pod, with what it writes on stderr in the error.
func (p *sessionPod) exec(ctx context.Context, argv []string, stdout io.Writer) error {
	rp, ok := p.pod.(runnerPod)
	if !ok {
		return p.pod.exec(ctx, argv, stdout)
	}
	var stderr bytes.Buffer
	err := rp.run.Exec(ctx, rp.pod, kube.ExecOptions{Command: argv, Stdout: stdout, Stderr: &stderr})
	if err != nil {
		if msg := lastLines(stderr.String(), 3); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
	}
	return err
}

func lastLines(s string, n int) string {
	var keep []string
	for line := range strings.SplitSeq(strings.TrimSpace(s), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			keep = append(keep, line)
		}
	}
	return strings.Join(keep[max(0, len(keep)-n):], "; ")
}

func (p *sessionPod) Close() {
	p.cancel()
	<-p.done
}

// OpenPod starts the idle restic pod b2bk snapshots and b2bk restore --to
// use, and keeps it until Close; leaving removes its Job.
func (b *tuiBackend) OpenPod(ctx context.Context, namespace, name string, progress io.Writer) (tui.Pod, error) {
	f := b.on(namespace, progress, nil)
	sb, err := b.backup(ctx, f, name)
	if err != nil {
		return nil, err
	}
	podCtx, cancel := context.WithCancel(ctx)
	ready := make(chan *sessionPod, 1)
	done := make(chan error, 1)
	go func() {
		err := f.attachPod(podCtx, runner.ForBackup(sb), runner.Options{Purpose: tui.PodPurpose},
			func(pod restorePod, job string) error {
				podName := job
				if rp, ok := pod.(runnerPod); ok {
					podName = rp.pod.Name
				}
				ready <- &sessionPod{name: podName, pod: pod, cancel: cancel, done: done}
				<-podCtx.Done()
				return nil
			})
		done <- err
	}()
	select {
	case p := <-ready:
		return p, nil
	case err := <-done:
		cancel()
		if err == nil {
			err = context.Canceled
		}
		return nil, err
	}
}

func asSessionPod(p tui.Pod) (*sessionPod, error) {
	sp, ok := p.(*sessionPod)
	if !ok {
		return nil, fmt.Errorf("not a pod b2bk started: %T", p)
	}
	return sp, nil
}

func (b *tuiBackend) Snapshots(ctx context.Context, p tui.Pod, namespace, name string) ([]tui.Snapshot, error) {
	pod, err := asSessionPod(p)
	if err != nil {
		return nil, err
	}
	sb, err := b.backup(ctx, b.on(namespace, io.Discard, nil), name)
	if err != nil {
		return nil, err
	}
	argv, err := snapshotsCommand(sb, &snapshotsOptions{asJSON: true})
	if errors.Is(err, errUnknownSourceTags) {
		argv, err = snapshotsCommand(sb, &snapshotsOptions{asJSON: true, allTags: true})
	}
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := pod.exec(ctx, argv, &out); err != nil {
		return nil, fmt.Errorf("listing snapshots: %w", err)
	}
	snaps, err := parseSnapshots(out.Bytes())
	if err != nil {
		return nil, err
	}
	list := make([]tui.Snapshot, 0, len(snaps))
	for _, s := range snaps {
		snap := tui.Snapshot{ID: s.ID, ShortID: s.short(), Time: s.Time, Paths: s.Paths, Hostname: s.Hostname,
			Tags: s.Tags}
		if s.Summary != nil {
			snap.Size, snap.HasSize = s.Summary.TotalBytesProcessed, true
		}
		list = append(list, snap)
	}
	return list, nil
}

type lsEntry struct {
	StructType string      `json:"struct_type"`
	Type       string      `json:"type"`
	Path       string      `json:"path"`
	Size       uint64      `json:"size"`
	Mode       os.FileMode `json:"mode"`
	UID        uint32      `json:"uid"`
	MTime      time.Time   `json:"mtime"`
}

func (b *tuiBackend) Files(ctx context.Context, p tui.Pod, snapshot string) ([]tui.Node, error) {
	pod, err := asSessionPod(p)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := pod.exec(ctx, resticCommand("ls", jsonFlag, snapshot), &out); err != nil {
		return nil, fmt.Errorf("listing snapshot %s: %w", snapshot, err)
	}
	var nodes []tui.Node
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var e lsEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Path == "" || (e.StructType != "" && e.StructType != "node") {
			continue
		}
		nodes = append(nodes, tui.Node{Path: e.Path, Type: e.Type, Size: e.Size, Mode: e.Mode, UID: e.UID,
			MTime: e.MTime})
	}
	return nodes, sc.Err()
}

func (b *tuiBackend) Diff(ctx context.Context, p tui.Pod, from, to string, w io.Writer) error {
	pod, err := asSessionPod(p)
	if err != nil {
		return err
	}
	return pod.exec(ctx, resticCommand("diff", from, to), w)
}

// Download runs b2bk restore --to's download through the session pod: the
// files under a path of a files source, or a database dump as one file.
func (b *tuiBackend) Download(ctx context.Context, p tui.Pod, req tui.DownloadRequest, w io.Writer) error {
	pod, err := asSessionPod(p)
	if err != nil {
		return err
	}
	f := b.on(req.Namespace, w, nil)
	sb, err := b.backup(ctx, f, req.Backup)
	if err != nil {
		return err
	}
	if err := requireRepositoryOwnsItsSecret(ctx, f, sb); err != nil {
		return err
	}
	dest, err := filepath.Abs(req.Dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	o := &restoreOptions{snapshot: req.Snapshot, tag: req.Tag, path: req.Path, toDir: dest}
	src, err := o.filesSource(sb)
	if err == nil {
		return o.download(ctx, pod, src, dest, w, w)
	}
	if !errors.Is(err, ErrNoFilesSource) || req.Path == "" {
		return err
	}
	if req.Folder {
		return fmt.Errorf("%w; a folder of it downloads with b2bk exec sb/%s -- restic dump", err, req.Backup)
	}
	step := restoreStep{snapPath: path.Dir(req.Path), sub: "/" + path.Base(req.Path), file: true}
	root := filepath.Join(dest, filepath.FromSlash(step.snapPath))
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	if err := downloadFile(ctx, pod, resticSnapshot{ID: req.Snapshot}, step, root); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "downloaded %s into %s\n", req.Path, filepath.Join(root, path.Base(req.Path)))
	return err
}

// RestoreTargets runs the checks b2bk restore makes before it starts a pod, for
// each target, and says why each that fails cannot be used.
func (b *tuiBackend) RestoreTargets(ctx context.Context, namespace, name, tag string) map[tui.Target]string {
	why := map[tui.Target]string{}
	f := b.on(namespace, io.Discard, nil)
	sb, err := b.backup(ctx, f, name)
	if err != nil {
		for _, t := range []tui.Target{tui.TargetNewClaim, tui.TargetDownload, tui.TargetInPlace, tui.TargetDatabase} {
			why[t] = errText(err)
		}
		return why
	}
	c, err := f.Client()
	if err != nil {
		return why
	}
	o := &restoreOptions{tag: tag}
	own := requireRepositoryOwnsItsSecret(ctx, f, sb)
	current := requireCurrentBackup(ctx, c, sb)
	if current == nil {
		if hold := sb.Annotations[b2backupv1.AnnotationRestoreLock]; hold != "" {
			current = fmt.Errorf("%w: held by %s", ErrRestoreRunning, hold)
		} else {
			current = refuseActive(ctx, c, sb, "")
		}
	}

	src, filesErr := o.filesSource(sb)
	isDB := filesErr != nil && databaseSourceTag(sb) != "" && tag == databaseSourceTag(sb)
	switch {
	case isDB:
		dump := "this is a database dump; download it with D in snapshots"
		why[tui.TargetNewClaim], why[tui.TargetDownload], why[tui.TargetInPlace] = dump, dump, dump
	case filesErr != nil:
		why[tui.TargetNewClaim], why[tui.TargetDownload], why[tui.TargetInPlace] =
			errText(filesErr), errText(filesErr), errText(filesErr)
	default:
		if _, err := sourceVolume(sb, src); err != nil {
			why[tui.TargetNewClaim] = errText(err)
		}
		for _, p := range src.paths {
			if backup.MountFor(sb, p) == nil {
				why[tui.TargetInPlace] = fmt.Sprintf("%s is on none of the backup's claims", p)
			}
		}
	}
	for _, t := range []tui.Target{tui.TargetNewClaim, tui.TargetDownload} {
		if why[t] == "" && own != nil {
			why[t] = errText(own)
		}
	}

	switch _, dbErr := o.databaseSource(sb); {
	case sb.Spec.Database == nil:
		why[tui.TargetDatabase] = "this backup has no database"
	case !isDB && filesErr == nil:
		why[tui.TargetDatabase] = "needs a db snapshot; switch the tag to " + orNone(databaseSourceTag(sb)) +
			" with tab in snapshots"
	case dbErr != nil:
		why[tui.TargetDatabase] = errText(dbErr)
	}
	for _, t := range []tui.Target{tui.TargetInPlace, tui.TargetDatabase} {
		if why[t] == "" && current != nil {
			why[t] = errText(current)
		}
	}
	return why
}

func errText(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}

// Restore runs b2bk restore with the options the wizard chose. It writes
// nothing it would not, and confirms an overwriting restore only when
// req.Confirm is the name b2bk restore asks to be typed.
func (b *tuiBackend) Restore(ctx context.Context, req tui.RestoreRequest, w io.Writer) error {
	f := b.on(req.Namespace, w, func(what, name string) error {
		if req.Confirm != name {
			return fmt.Errorf("%w: %s was not changed; type %s to confirm", ErrNotConfirmed, what, name)
		}
		return nil
	})
	sb, err := b.backup(ctx, f, req.Backup)
	if err != nil {
		return err
	}
	c, err := f.Client()
	if err != nil {
		return err
	}
	o := &restoreOptions{snapshot: req.Snapshot, tag: req.Tag, path: req.Path, delete: req.Delete, dryRun: req.DryRun}
	if o.snapshot == "" {
		o.snapshot = latest
	}
	switch req.Target {
	case tui.TargetNewClaim:
		if req.DryRun {
			o.toDir, o.delete = ".", false
			break
		}
		o.toNewPVC, o.size = req.Claim, req.Size
	case tui.TargetDownload:
		o.toDir, o.delete = req.Dir, false
	case tui.TargetInPlace:
		o.inPlace = true
	case tui.TargetDatabase:
		o.toDatabase = true
	}
	return runRestore(ctx, f, c, sb, o)
}
