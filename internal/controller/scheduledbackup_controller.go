package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"slices"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

const (
	repositoryRefField = ".spec.repositoryRef.name"

	annotationSpecHash = "b2backup.mcswain.dev/spec-hash"

	backupRequeueInterval = 5 * time.Minute
)

// ScheduledBackupReconciler reconciles ScheduledBackup objects.
type ScheduledBackupReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	Config backup.Config
}

// +kubebuilder:rbac:groups=b2backup.mcswain.dev,resources=scheduledbackups,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=b2backup.mcswain.dev,resources=scheduledbackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=b2backup.mcswain.dev,resources=scheduledbackups/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;create;delete

// Reconcile keeps the backup's CronJob, cache claim and run history up to date.
func (r *ScheduledBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sb b2backupv1.ScheduledBackup
	if err := r.Get(ctx, req.NamespacedName, &sb); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !sb.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	statusBase := sb.DeepCopy()

	result, err := r.reconcile(ctx, &sb)
	if err != nil {
		r.setCondition(&sb, metav1.Condition{
			Type:    b2backupv1.ScheduledBackupConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "ReconcileFailed",
			Message: err.Error(),
		})
		r.Recorder.Eventf(&sb, nil, corev1.EventTypeWarning, "ReconcileFailed", "Reconcile", "%s", err.Error())
		if statusErr := r.patchStatus(ctx, &sb, statusBase); statusErr != nil {
			log.FromContext(ctx).Error(statusErr, "Failed to update status after a failed reconcile")
		}
		return ctrl.Result{}, err
	}

	if err := r.patchStatus(ctx, &sb, statusBase); err != nil {
		return ctrl.Result{}, err
	}
	return result, nil
}

func (r *ScheduledBackupReconciler) reconcile(
	ctx context.Context, sb *b2backupv1.ScheduledBackup,
) (ctrl.Result, error) {
	var repo b2backupv1.Repository
	key := types.NamespacedName{Namespace: sb.Namespace, Name: sb.Spec.RepositoryRef.Name}
	if err := r.Get(ctx, key, &repo); err != nil {
		if apierrors.IsNotFound(err) {
			r.setCondition(sb, metav1.Condition{
				Type:    b2backupv1.ScheduledBackupConditionReady,
				Status:  metav1.ConditionFalse,
				Reason:  "RepositoryNotFound",
				Message: fmt.Sprintf("Repository %q does not exist", sb.Spec.RepositoryRef.Name),
			})

			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !repo.Status.Initialized {
		r.setCondition(sb, metav1.Condition{
			Type:    b2backupv1.ScheduledBackupConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "RepositoryNotReady",
			Message: fmt.Sprintf("Repository %q is not initialized yet", repo.Name),
		})
		return ctrl.Result{}, nil
	}

	if other, tag, err := r.tagConflict(ctx, sb); err != nil {
		return ctrl.Result{}, err
	} else if other != "" {
		r.setCondition(sb, metav1.Condition{
			Type:   b2backupv1.ScheduledBackupConditionReady,
			Status: metav1.ConditionFalse,
			Reason: "TagConflict",
			Message: fmt.Sprintf("ScheduledBackup %s also backs up into Repository %s under tag %q; the image groups "+
				"forget by tags, so each would expire the other's snapshots. Give this one tags of its own",
				other, repo.Name, tag),
		})
		return ctrl.Result{RequeueAfter: backupRequeueInterval}, r.suspendOwnCronJob(ctx, sb)
	}

	if err := r.reconcileCache(ctx, sb); err != nil {
		return ctrl.Result{}, err
	}

	desired, err := backup.BuildCronJob(sb, &repo, r.Config)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := controllerutil.SetControllerReference(sb, desired, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	hash, err := cronJobHash(desired)
	if err != nil {
		return ctrl.Result{}, err
	}
	if desired.Annotations == nil {
		desired.Annotations = map[string]string{}
	}
	desired.Annotations[annotationSpecHash] = hash

	var cronJobUID types.UID

	var current batchv1.CronJob
	err = r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, &current)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Create(ctx, desired); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return ctrl.Result{}, fmt.Errorf(
					"CronJob %s already exists but is not managed by this operator; "+
						"remove it or rename the ScheduledBackup", desired.Name)
			}
			return ctrl.Result{}, fmt.Errorf("creating cronjob: %w", err)
		}
		r.Recorder.Eventf(sb, nil, corev1.EventTypeNormal, "CronJobCreated", "Create",
			"Created CronJob %s on schedule %s", desired.Name, desired.Spec.Schedule)
		cronJobUID = desired.UID
	case err != nil:
		return ctrl.Result{}, err
	case !metav1.IsControlledBy(&current, sb) &&
		(current.Labels[labelManagedBy] != managedByValue || metav1.GetControllerOf(&current) != nil):
		r.setCondition(sb, metav1.Condition{
			Type:   b2backupv1.ScheduledBackupConditionReady,
			Status: metav1.ConditionFalse,
			Reason: "CronJobNotManaged",
			Message: fmt.Sprintf("CronJob %s already exists, is not labelled %s=%s and is not owned by "+
				"this ScheduledBackup, so it was left alone. Delete it or rename the ScheduledBackup",
				desired.Name, labelManagedBy, managedByValue),
		})
		return ctrl.Result{RequeueAfter: backupRequeueInterval}, nil
	default:
		if !metav1.IsControlledBy(&current, sb) || cronJobDrifted(&current, desired) {
			updated := current.DeepCopy()
			updated.Spec = desired.Spec
			updated.Labels = desired.Labels
			updated.OwnerReferences = desired.OwnerReferences
			if updated.Annotations == nil {
				updated.Annotations = map[string]string{}
			}
			updated.Annotations[annotationSpecHash] = hash
			if err := r.Update(ctx, updated); err != nil {
				return ctrl.Result{}, fmt.Errorf("updating cronjob: %w", err)
			}
		}
		sb.Status.LastScheduleTime = current.Status.LastScheduleTime
		sb.Status.LastSuccessfulTime = current.Status.LastSuccessfulTime
		sb.Status.Active = int32(len(current.Status.Active))
		cronJobUID = current.UID
	}

	sb.Status.EffectiveSchedule = desired.Spec.Schedule

	r.setCondition(sb, metav1.Condition{
		Type:    b2backupv1.ScheduledBackupConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  "Ready",
		Message: fmt.Sprintf("Backing up on schedule %s", desired.Spec.Schedule),
	})

	runs, err := r.observedRuns(ctx, sb, cronJobUID)
	if err != nil {
		return ctrl.Result{}, err
	}
	sb.Status.History = mergeHistory(sb.Status.History, runs)

	if err := r.reconcileTrigger(ctx, sb, &repo, runs); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: backupRequeueInterval}, nil
}

// tagConflict finds an older ScheduledBackup writing into the same Repository
// under one of this one's source tags.
func (r *ScheduledBackupReconciler) tagConflict(
	ctx context.Context, sb *b2backupv1.ScheduledBackup,
) (string, string, error) {
	tags := backup.SourceTags(sb.Spec.Sources)
	if len(tags) == 0 {
		return "", "", nil
	}
	var list b2backupv1.ScheduledBackupList
	if err := r.List(ctx, &list, client.InNamespace(sb.Namespace)); err != nil {
		return "", "", err
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.Name == sb.Name || other.Spec.RepositoryRef.Name != sb.Spec.RepositoryRef.Name ||
			!other.DeletionTimestamp.IsZero() || !olderBackup(other, sb) {
			continue
		}
		for _, tag := range backup.SourceTags(other.Spec.Sources) {
			if slices.Contains(tags, tag) {
				return other.Name, tag, nil
			}
		}
	}
	return "", "", nil
}

func olderBackup(a, b *b2backupv1.ScheduledBackup) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
}

func (r *ScheduledBackupReconciler) suspendOwnCronJob(ctx context.Context, sb *b2backupv1.ScheduledBackup) error {
	var cj batchv1.CronJob
	err := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: backup.CronJobName(sb)}, &cj)
	if apierrors.IsNotFound(err) || (err == nil && (!metav1.IsControlledBy(&cj, sb) ||
		(cj.Spec.Suspend != nil && *cj.Spec.Suspend))) {
		return nil
	}
	if err != nil {
		return err
	}
	patch := client.MergeFrom(cj.DeepCopy())
	cj.Spec.Suspend = ptr.To(true)
	return r.Patch(ctx, &cj, patch)
}

func cronJobDrifted(current, desired *batchv1.CronJob) bool {
	return current.Annotations[annotationSpecHash] != desired.Annotations[annotationSpecHash] ||
		!equality.Semantic.DeepDerivative(desired.Labels, current.Labels) ||
		!equality.Semantic.DeepDerivative(desired.Spec, current.Spec)
}

func cronJobHash(cj *batchv1.CronJob) (string, error) {
	raw, err := json.Marshal(struct {
		Spec   batchv1.CronJobSpec     `json:"spec"`
		Labels map[string]string       `json:"labels"`
		Owners []metav1.OwnerReference `json:"owners"`
	}{cj.Spec, cj.Labels, cj.OwnerReferences})
	if err != nil {
		return "", err
	}
	sum := fnv.New64a()
	if _, err := sum.Write(raw); err != nil {
		return "", err
	}
	return strconv.FormatUint(sum.Sum64(), 16), nil
}

func (r *ScheduledBackupReconciler) reconcileCache(
	ctx context.Context, sb *b2backupv1.ScheduledBackup,
) error {
	desired, err := backup.BuildCachePVC(sb, r.Config)
	if err != nil {
		return err
	}
	if desired == nil {
		return r.deleteCache(ctx, sb)
	}
	if err := controllerutil.SetControllerReference(sb, desired, r.Scheme); err != nil {
		return err
	}

	var current corev1.PersistentVolumeClaim
	err = r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, &current)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating cache volume: %w", err)
		}
		return nil
	}
	return err
}

func (r *ScheduledBackupReconciler) deleteCache(
	ctx context.Context, sb *b2backupv1.ScheduledBackup,
) error {
	var pvc corev1.PersistentVolumeClaim
	key := types.NamespacedName{Namespace: sb.Namespace, Name: backup.CacheName(sb)}
	if err := r.Get(ctx, key, &pvc); err != nil {
		return client.IgnoreNotFound(err)
	}
	if owner := metav1.GetControllerOf(&pvc); owner == nil || owner.UID != sb.UID {
		return nil
	}
	if err := r.Delete(ctx, &pvc); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting cache volume: %w", err)
	}
	return nil
}

func (r *ScheduledBackupReconciler) patchStatus(
	ctx context.Context, sb *b2backupv1.ScheduledBackup, base *b2backupv1.ScheduledBackup,
) error {
	sb.Status.ObservedGeneration = sb.Generation
	if equality.Semantic.DeepEqual(sb.Status, base.Status) {
		return nil
	}
	return r.Status().Patch(ctx, sb, client.MergeFrom(base))
}

func (r *ScheduledBackupReconciler) setCondition(
	sb *b2backupv1.ScheduledBackup, cond metav1.Condition,
) {
	cond.ObservedGeneration = sb.Generation
	apimeta.SetStatusCondition(&sb.Status.Conditions, cond)
}

// SetupWithManager registers the controller with the manager.
func (r *ScheduledBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("scheduledbackup-controller")
	}

	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(), &b2backupv1.ScheduledBackup{}, repositoryRefField,
		func(o client.Object) []string {
			sb, ok := o.(*b2backupv1.ScheduledBackup)
			if !ok || sb.Spec.RepositoryRef.Name == "" {
				return nil
			}
			return []string{sb.Spec.RepositoryRef.Name}
		},
	); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(crcontroller.Options{MaxConcurrentReconciles: maxConcurrentReconciles}).
		For(&b2backupv1.ScheduledBackup{}, builder.WithPredicates(ignoreOwnStatusWrites(b2backupv1.AnnotationTriggerAt))).
		Owns(&batchv1.CronJob{}).
		Owns(&batchv1.Job{}).
		Watches(
			&b2backupv1.Repository{},
			handler.EnqueueRequestsFromMapFunc(r.backupsForRepository),
		).
		Named("scheduledbackup").
		Complete(r)
}

func (r *ScheduledBackupReconciler) backupsForRepository(
	ctx context.Context, obj client.Object,
) []reconcile.Request {
	var list b2backupv1.ScheduledBackupList
	if err := r.List(ctx, &list,
		client.InNamespace(obj.GetNamespace()),
		client.MatchingFieldsSelector{
			Selector: fields.OneTermEqualSelector(repositoryRefField, obj.GetName()),
		},
	); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list backups for repository", "repository", obj.GetName())
		return nil
	}

	requests := make([]reconcile.Request, 0, len(list.Items))
	for _, sb := range list.Items {
		requests = append(requests, reconcile.Request{
			Namespace: sb.Namespace, Name: sb.Name,
		})
	}
	return requests
}
