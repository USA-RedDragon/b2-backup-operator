// Package runner creates one-off Jobs modelled on a backup's CronJob, so ad-hoc restic
// commands run with the same image, mounts and credentials as a real backup.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/kube"
)

const (
	// ManagedByValue marks the Jobs this package creates.
	ManagedByValue = "b2bk"

	// LabelPurpose records on a Job what it was started for, such as restore.
	LabelPurpose = "b2backup.mcswain.dev/b2bk-purpose"

	// LabelScheduledBackup records on a Job and its pod the ScheduledBackup it
	// was built from, so a pod that outlives its deleted Job can still be told
	// apart.
	LabelScheduledBackup = "b2backup.mcswain.dev/scheduledbackup"

	// LabelRepository records on a Job and its pod the Repository it was
	// built from when no ScheduledBackup uses that Repository.
	LabelRepository = "b2backup.mcswain.dev/repository"

	labelManagedBy = "app.kubernetes.io/managed-by"
	containerName  = "restic"
	cacheVolume    = "cache"
	dbVolume       = "db-credentials"

	ttlSeconds int32 = 600

	maxJobName = 52

	interactiveDeadline = 12 * time.Hour

	pollInterval = 500 * time.Millisecond

	podStartProgress = 15 * time.Second

	jobNameLabel = "batch.kubernetes.io/job-name"
)

// DefaultPodStartTimeout is how long a one-off Job's pod has to start running.
const DefaultPodStartTimeout = 10 * time.Minute

var (
	// ErrNoCronJob means the backup has no CronJob to model a Job on.
	ErrNoCronJob = errors.New("no CronJob for this backup")
	// ErrFailed means the Job ran and failed.
	ErrFailed = errors.New("command failed")
	// ErrNoPod means the Job never produced a pod.
	ErrNoPod = errors.New("no pod was created")

	// ErrMissingDependency means a Secret or claim the Job mounts is missing.
	ErrMissingDependency = errors.New("the backup references something that does not exist")
)

// Runner creates and drives one-off Jobs modelled on a backup's CronJob.
type Runner struct {
	Client     client.Client
	Clientset  kubernetes.Interface
	RESTConfig *rest.Config

	PodStartTimeout time.Duration

	Progress io.Writer
}

// Options are the settings for a one-off Job.
type Options struct {
	Command []string

	Purpose string

	MountData bool

	MountCache bool

	MountDatabase bool

	Image string

	TTY bool

	Keep bool

	ExtraVolumes []corev1.Volume
	ExtraMounts  []corev1.VolumeMount

	// Restore adds the capabilities restic restore needs to put files back as
	// root with their owners, modes and times: RestoreCapabilities.
	Restore bool
}

// RestoreCapabilities are what restic restore needs, as root, to recreate files
// owned by other users: chown them, write into their directories, and set their
// modes, setgid bits and times. All are allowed by the baseline Pod Security
// Standard; the restricted one allows none, and rejects the pod.
var RestoreCapabilities = []corev1.Capability{"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID"}

var idleCommand = []string{"sh", "-c", `trap "exit 0" TERM INT; sleep infinity & wait`}

// Subject is what a one-off Job is built for: a ScheduledBackup, whose
// CronJob it copies, or else a Repository no ScheduledBackup uses.
type Subject struct {
	Backup     *b2backupv1.ScheduledBackup
	Repository *b2backupv1.Repository
}

// ForBackup is the Subject of a ScheduledBackup.
func ForBackup(sb *b2backupv1.ScheduledBackup) Subject { return Subject{Backup: sb} }

func (s Subject) object() client.Object {
	if s.Backup != nil {
		return s.Backup
	}
	return s.Repository
}

// Build derives a one-off Job from the backup's CronJob. It does not create it.
func (r *Runner) Build(
	ctx context.Context, sb *b2backupv1.ScheduledBackup, opts Options,
) (*batchv1.Job, error) {
	return r.BuildFor(ctx, ForBackup(sb), opts)
}

// BuildFor derives a one-off Job for s: from the CronJob of its ScheduledBackup,
// or for a bare Repository from backup.RepositoryJobSpec. It does not create it.
func (r *Runner) BuildFor(ctx context.Context, s Subject, opts Options) (*batchv1.Job, error) {
	var spec batchv1.JobSpec
	labels := map[string]string{labelManagedBy: ManagedByValue}
	if sb := s.Backup; sb != nil {
		var cj batchv1.CronJob
		key := types.NamespacedName{Namespace: sb.Namespace, Name: backup.CronJobName(sb)}
		if err := r.Client.Get(ctx, key, &cj); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("%w: expected %s; run `b2bk doctor sb/%s`",
					ErrNoCronJob, key.Name, sb.Name)
			}
			return nil, err
		}
		spec = *cj.Spec.JobTemplate.Spec.DeepCopy()
		labels[LabelScheduledBackup] = sb.Name
	} else {
		spec = backup.RepositoryJobSpec(s.Repository, opts.Image)
		labels[LabelRepository] = s.Repository.Name
	}
	owner := s.object()
	spec.TTLSecondsAfterFinished = ptr.To(ttlSeconds)
	spec.BackoffLimit = ptr.To(int32(0))
	spec.Template.Spec.RestartPolicy = corev1.RestartPolicyNever
	if opts.TTY && !opts.Keep {
		spec.ActiveDeadlineSeconds = ptr.To(int64(interactiveDeadline.Seconds()))
	}

	container := findContainer(&spec.Template.Spec)
	if container == nil {
		return nil, fmt.Errorf("the Job for %s has no %q container", owner.GetName(), containerName)
	}

	if opts.TTY {
		container.Command = idleCommand
		container.Stdin, container.TTY = true, true
	} else {
		container.Command = opts.Command
	}
	container.Args = nil
	if opts.Image != "" {
		container.Image = opts.Image
	}

	if !opts.MountCache {
		useEphemeralCache(&spec.Template.Spec)
	}
	if opts.MountData {
		mountDataWritable(&spec.Template.Spec, container)
	} else {
		dropData(&spec.Template.Spec, container)
	}
	if !opts.MountDatabase {
		spec.Template.Spec.Volumes = filterVolumes(spec.Template.Spec.Volumes, dbVolume)
		container.VolumeMounts = filterMounts(container.VolumeMounts, dbVolume)
	}

	if opts.Restore {
		addCapabilities(container, RestoreCapabilities)
	}

	spec.Template.Spec.Volumes = append(spec.Template.Spec.Volumes, opts.ExtraVolumes...)
	container.VolumeMounts = append(container.VolumeMounts, opts.ExtraMounts...)

	if opts.Purpose != "" {
		labels[LabelPurpose] = opts.Purpose
	}

	if v, ok := spec.Template.Labels["mariadb-client"]; ok {
		labels["mariadb-client"] = v
	}
	spec.Template.Labels = labels

	job := &batchv1.Job{
		Name:      jobName(owner.GetName(), opts.Purpose),
		Namespace: owner.GetNamespace(),
		Labels:    labels,
		Spec:      spec,
	}
	if err := controllerutil.SetOwnerReference(owner, job, r.Client.Scheme()); err != nil {
		return nil, err
	}
	return job, nil
}

func jobName(name, purpose string) string {
	if purpose == "" {
		purpose = "run"
	}
	suffix := fmt.Sprintf("-b2bk-%s-%s", purpose, rand.String(5))
	if limit := max(0, maxJobName-len(suffix)); len(name) > limit {
		name = name[:limit]
	}
	return name + suffix
}

func findContainer(pod *corev1.PodSpec) *corev1.Container {
	for i := range pod.Containers {
		if pod.Containers[i].Name == containerName {
			return &pod.Containers[i]
		}
	}
	if len(pod.Containers) == 1 {
		return &pod.Containers[0]
	}
	return nil
}

func useEphemeralCache(pod *corev1.PodSpec) {
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == cacheVolume {
			pod.Volumes[i].PersistentVolumeClaim = nil
			pod.Volumes[i].EmptyDir = &corev1.EmptyDirVolumeSource{}
		}
	}
}

func mountDataWritable(pod *corev1.PodSpec, container *corev1.Container) {
	for i := range pod.Volumes {
		if isDataVolume(pod.Volumes[i].Name) && pod.Volumes[i].PersistentVolumeClaim != nil {
			pod.Volumes[i].PersistentVolumeClaim.ReadOnly = false
		}
	}
	for i := range container.VolumeMounts {
		if isDataVolume(container.VolumeMounts[i].Name) {
			container.VolumeMounts[i].ReadOnly = false
		}
	}
}

func dropData(pod *corev1.PodSpec, container *corev1.Container) {
	pod.Volumes = slices.DeleteFunc(pod.Volumes, func(v corev1.Volume) bool { return isDataVolume(v.Name) })
	container.VolumeMounts = slices.DeleteFunc(container.VolumeMounts,
		func(m corev1.VolumeMount) bool { return isDataVolume(m.Name) })
	if pod.Affinity != nil && pod.Affinity.PodAffinity != nil {
		pod.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution = nil
		if pod.Affinity.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution == nil {
			pod.Affinity.PodAffinity = nil
		}
	}
}

func addCapabilities(container *corev1.Container, add []corev1.Capability) {
	if container.SecurityContext == nil {
		container.SecurityContext = &corev1.SecurityContext{}
	}
	if container.SecurityContext.Capabilities == nil {
		container.SecurityContext.Capabilities = &corev1.Capabilities{}
	}
	caps := container.SecurityContext.Capabilities
	for _, c := range add {
		if !slices.Contains(caps.Add, c) {
			caps.Add = append(caps.Add, c)
		}
	}
}

func isDataVolume(name string) bool {
	return strings.HasPrefix(name, backup.DataVolumePrefix)
}

func filterVolumes(in []corev1.Volume, name string) []corev1.Volume {
	out := make([]corev1.Volume, 0, len(in))
	for _, v := range in {
		if v.Name != name {
			out = append(out, v)
		}
	}
	return out
}

func filterMounts(in []corev1.VolumeMount, name string) []corev1.VolumeMount {
	out := make([]corev1.VolumeMount, 0, len(in))
	for _, m := range in {
		if m.Name != name {
			out = append(out, m)
		}
	}
	return out
}

// Cleanup deletes a Job and its pods.
func (r *Runner) Cleanup(job *batchv1.Job) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 30*time.Second)
	defer cancel()

	err := r.Client.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting job/%s: %w", job.Name, err)
	}
	return nil
}

// WaitForPod waits for the Job's pod to start running.
func (r *Runner) WaitForPod(ctx context.Context, job *batchv1.Job) (*corev1.Pod, error) {
	budget := r.PodStartTimeout
	if budget <= 0 {
		budget = DefaultPodStartTimeout
	}

	var found *corev1.Pod
	start := time.Now()
	reported := start
	every := min(podStartProgress, budget/4)
	binding := ""

	err := wait.PollUntilContextTimeout(ctx, pollInterval, budget, true,
		func(ctx context.Context) (bool, error) {
			binding = ""
			pods, err := r.jobPods(ctx, job)
			if err != nil {
				return false, err
			}
			for i := range pods {
				pod := &pods[i]
				if pod.Status.Phase != corev1.PodPending {
					found = pod
					return true, nil
				}

				msg := blockedReason(pod)
				if msg == "" {
					continue
				}
				if unschedulable(pod) && strings.Contains(msg, unboundClaimsMessage) {
					unbound, err := r.unboundClaims(ctx, pod)
					if err != nil {
						return false, err
					}
					binding = "waiting for the scheduler to retry now that the claims are bound"
					if len(unbound) > 0 {
						binding = "waiting for " + strings.Join(unbound, ", ") + " to bind"
					}
					continue
				}
				return false, fmt.Errorf("%w: %s", ErrFailed, msg)
			}
			if r.Progress != nil && time.Since(reported) >= every {
				reported = time.Now()
				if pending := describePending(pods); pending != "" {
					if binding != "" {
						pending += "; " + binding
					}
					_, _ = fmt.Fprintf(r.Progress, "still waiting: %s (%s of %s)\n",
						pending, time.Since(start).Round(time.Second), budget)
				}
			}
			return false, nil
		})
	if err != nil {
		pending := r.pendingPod(ctx, job)
		if pending != "" && binding != "" {
			pending += "; " + binding
		}
		switch {
		case pending == "":
			return nil, err
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("%w: %s after %s (raise --pod-start-timeout); describe it for the reason",
				err, pending, budget)
		default:
			return nil, fmt.Errorf("%w: pod %s; describe it for the reason", err, pending)
		}
	}
	if found == nil {
		return nil, ErrNoPod
	}
	return found, nil
}

func (r *Runner) jobPods(ctx context.Context, job *batchv1.Job) ([]corev1.Pod, error) {
	var pods corev1.PodList
	err := r.Client.List(ctx, &pods,
		client.InNamespace(job.Namespace),
		client.MatchingLabels{jobNameLabel: job.Name},
	)
	return pods.Items, err
}

func (r *Runner) pendingPod(ctx context.Context, job *batchv1.Job) string {
	pods, err := r.jobPods(ctx, job)
	if err != nil {
		return ""
	}
	return describePending(pods)
}

func describePending(pods []corev1.Pod) string {
	if len(pods) == 0 {
		return ""
	}

	pod := &pods[0]
	detail := string(pod.Status.Phase)
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil {
			detail = cs.State.Waiting.Reason
			break
		}
	}
	return fmt.Sprintf("%s is %s", pod.Name, detail)
}

// Preflight checks that the Secrets and claims the Job mounts exist.
func (r *Runner) Preflight(ctx context.Context, job *batchv1.Job) error {
	pod := &job.Spec.Template.Spec

	for _, v := range pod.Volumes {
		switch {
		case v.Secret != nil:
			if err := r.mustExist(ctx, job.Namespace, v.Secret.SecretName, &corev1.Secret{}, "Secret"); err != nil {
				return err
			}
		case v.PersistentVolumeClaim != nil:
			name := v.PersistentVolumeClaim.ClaimName
			if err := r.mustExist(ctx, job.Namespace, name, &corev1.PersistentVolumeClaim{}, "PersistentVolumeClaim"); err != nil {
				return err
			}
		}
	}

	for i := range pod.Containers {
		for _, from := range pod.Containers[i].EnvFrom {
			if from.SecretRef == nil || ptr.Deref(from.SecretRef.Optional, false) {
				continue
			}
			if err := r.mustExist(ctx, job.Namespace, from.SecretRef.Name, &corev1.Secret{}, "Secret"); err != nil {
				return err
			}
		}
		for _, env := range pod.Containers[i].Env {
			if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
				continue
			}
			if err := r.mustHaveKey(ctx, job.Namespace, env.ValueFrom.SecretKeyRef); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Runner) mustHaveKey(ctx context.Context, namespace string, sel *corev1.SecretKeySelector) error {
	if ptr.Deref(sel.Optional, false) {
		return nil
	}

	var secret corev1.Secret
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: sel.Name}, &secret)
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: Secret %q does not exist in namespace %q",
			ErrMissingDependency, sel.Name, namespace)
	}
	if err != nil {
		return err
	}
	if _, ok := secret.Data[sel.Key]; !ok {
		return fmt.Errorf("%w: Secret %q in namespace %q has no key %q",
			ErrMissingDependency, sel.Name, namespace, sel.Key)
	}
	return nil
}

func (r *Runner) mustExist(ctx context.Context, namespace, name string, into client.Object, kind string) error {
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, into)
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: %s %q does not exist in namespace %q",
			ErrMissingDependency, kind, name, namespace)
	}
	return err
}

// unboundClaimsMessage is what the scheduler reports while a pod's claim with
// Immediate binding has no volume yet, as when b2bk has just created it. It
// may still say so for a moment after the claim binds, until the scheduler
// retries. Any other reason the pod cannot be scheduled is final.
const unboundClaimsMessage = "unbound immediate PersistentVolumeClaims"

func unschedulable(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse &&
			cond.Reason == corev1.PodReasonUnschedulable {
			return true
		}
	}
	return false
}

// unboundClaims names the claims the pod mounts that exist but are not Bound
// yet. A missing claim is an error: nothing will bind it.
func (r *Runner) unboundClaims(ctx context.Context, pod *corev1.Pod) ([]string, error) {
	var unbound []string
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		name := v.PersistentVolumeClaim.ClaimName
		var pvc corev1.PersistentVolumeClaim
		err := r.Client.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: name}, &pvc)
		switch {
		case apierrors.IsNotFound(err):
			return nil, fmt.Errorf("%w: PersistentVolumeClaim %q does not exist in namespace %q",
				ErrMissingDependency, name, pod.Namespace)
		case err != nil:
			return nil, err
		case pvc.Status.Phase != corev1.ClaimBound:
			unbound = append(unbound, "pvc/"+name)
		}
	}
	return unbound, nil
}

func blockedReason(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting == nil {
			continue
		}
		switch cs.State.Waiting.Reason {
		case "ErrImagePull", "ImagePullBackOff", "CreateContainerConfigError", "InvalidImageName":
			return cs.State.Waiting.Reason + ": " + cs.State.Waiting.Message
		}
	}

	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse &&
			cond.Reason == corev1.PodReasonUnschedulable {
			return "Unschedulable: " + cond.Message
		}
	}
	return ""
}

// StreamLogs follows the pod's logs into w.
func (r *Runner) StreamLogs(ctx context.Context, pod *corev1.Pod, w io.Writer) error {
	stream, err := r.Clientset.CoreV1().Pods(pod.Namespace).
		GetLogs(pod.Name, &corev1.PodLogOptions{Follow: true}).Stream(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()

	_, err = io.Copy(w, stream)
	return err
}

// Wait blocks until the Job succeeds or fails, returning ErrFailed on failure.
func (r *Runner) Wait(ctx context.Context, job *batchv1.Job, timeout time.Duration) error {
	key := client.ObjectKeyFromObject(job)

	var final batchv1.Job
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			if err := r.Client.Get(ctx, key, &final); err != nil {
				return false, client.IgnoreNotFound(err)
			}
			return final.Status.Succeeded > 0 || final.Status.Failed > 0, nil
		})
	if err != nil {
		return err
	}
	if final.Status.Failed > 0 {
		return fmt.Errorf("%w: job/%s", ErrFailed, job.Name)
	}
	return nil
}

// Run builds a Job, runs it to completion and streams its logs to w.
func (r *Runner) Run(
	ctx context.Context,
	s Subject,
	opts Options,
	w io.Writer,
	timeout time.Duration,
) error {
	job, err := r.BuildFor(ctx, s, opts)
	if err != nil {
		return err
	}
	if err := r.Preflight(ctx, job); err != nil {
		return err
	}
	if err := r.Client.Create(ctx, job); err != nil {
		return fmt.Errorf("creating job/%s: %w", job.Name, err)
	}
	if !opts.Keep {
		defer func() { _ = r.Cleanup(job) }()
	}

	pod, err := r.WaitForPod(ctx, job)
	if err != nil {
		return err
	}
	streamErr := r.StreamLogs(ctx, pod, w)
	if err := r.Wait(ctx, job, timeout); err != nil {
		return err
	}
	return streamErr
}

// Attach starts an idle Job and hands its pod to fn, cleaning up afterwards.
func (r *Runner) Attach(
	ctx context.Context,
	s Subject,
	opts Options,
	timeout time.Duration,
	fn func(pod *corev1.Pod) error,
) error {
	opts.TTY = true

	job, err := r.BuildFor(ctx, s, opts)
	if err != nil {
		return err
	}
	if err := r.Preflight(ctx, job); err != nil {
		return err
	}
	if err := r.Client.Create(ctx, job); err != nil {
		return fmt.Errorf("creating job/%s: %w", job.Name, err)
	}
	if !opts.Keep {
		defer func() { _ = r.Cleanup(job) }()
	}

	pod, err := r.WaitForPod(ctx, job)
	if err != nil {
		return err
	}
	return fn(pod)
}

// ContainerName is the name of the restic container.
const ContainerName = containerName

// Exec runs a command in the pod's restic container.
func (r *Runner) Exec(ctx context.Context, pod *corev1.Pod, opts kube.ExecOptions) error {
	opts.Namespace = pod.Namespace
	opts.Pod = pod.Name
	if opts.Container == "" {
		opts.Container = containerName
	}

	return kube.Exec(ctx, r.RESTConfig, r.Clientset, opts)
}
