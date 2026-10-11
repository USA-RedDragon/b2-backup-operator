// Package controller holds the Repository and ScheduledBackup reconcilers.
package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/borgbase"
	"github.com/USA-RedDragon/b2-backup-operator/internal/secrets"
)

const (
	// FinalizerName guards the BorgBase repository against deletion of the Repository.
	FinalizerName = "b2backup.mcswain.dev/finalizer"

	// KeyResticRepository is the Secret key holding the restic URL.
	KeyResticRepository = "RESTIC_REPOSITORY"
	// KeyResticPassword is the Secret key holding the restic password.
	KeyResticPassword = "RESTIC_PASSWORD"

	initJobSuffix = "-init"

	initCleanupDelay = 10 * time.Second

	initRetryDelay = 5 * time.Minute

	maxConcurrentReconciles = 4
)

// RepositoryReconciler reconciles Repository objects against the BorgBase API.
type RepositoryReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	NewAPI func(token string) borgbase.API

	DefaultTokenSecret types.NamespacedName
	DefaultTokenKey    string

	BackupImage string

	APIReader client.Reader

	Endpoint string
}

// +kubebuilder:rbac:groups=b2backup.mcswain.dev,resources=repositories,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=b2backup.mcswain.dev,resources=repositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=b2backup.mcswain.dev,resources=repositories/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete

// Reconcile brings the BorgBase repository and its credentials Secret in line with the spec.
func (r *RepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var repo b2backupv1.Repository
	if err := r.Get(ctx, req.NamespacedName, &repo); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !repo.DeletionTimestamp.IsZero() {
		statusBase := repo.DeepCopy()
		if err := r.finalize(ctx, &repo); err != nil {
			if statusErr := r.patchStatus(ctx, &repo, statusBase); statusErr != nil {
				logger.Error(statusErr, "Failed to update status after a failed finalize")
			}
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&repo, FinalizerName) {
		patch := client.MergeFrom(repo.DeepCopy())
		controllerutil.AddFinalizer(&repo, FinalizerName)
		if err := r.Patch(ctx, &repo, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	statusBase := repo.DeepCopy()

	if repo.Spec.Suspend {
		logger.V(1).Info("Reconciliation suspended")
		r.setCondition(&repo, metav1.Condition{
			Type:    b2backupv1.RepositoryConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "Suspended",
			Message: "reconciliation is paused by spec.suspend",
		})
		return ctrl.Result{}, r.patchStatus(ctx, &repo, statusBase)
	}

	api, err := r.apiFor(ctx, &repo)
	if err != nil {
		r.setCondition(&repo, metav1.Condition{
			Type:    b2backupv1.RepositoryConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "APITokenUnavailable",
			Message: err.Error(),
		})
		r.Recorder.Eventf(&repo, nil, corev1.EventTypeWarning, "APITokenUnavailable", "Reconcile", "%s", err.Error())
		if statusErr := r.patchStatus(ctx, &repo, statusBase); statusErr != nil {
			logger.Error(statusErr, "Failed to update status after an API token failure")
		}
		return ctrl.Result{}, err
	}

	result, err := r.reconcile(ctx, &repo, api)
	if err != nil {
		meta := metav1.Condition{
			Type:    b2backupv1.RepositoryConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "ReconcileFailed",
			Message: err.Error(),
		}
		r.setCondition(&repo, meta)
		r.Recorder.Eventf(&repo, nil, corev1.EventTypeWarning, "ReconcileFailed", "Reconcile", "%s", err.Error())
		if statusErr := r.patchStatus(ctx, &repo, statusBase); statusErr != nil {
			logger.Error(statusErr, "Failed to update status after a failed reconcile")
		}
		return ctrl.Result{}, err
	}

	if err := r.patchStatus(ctx, &repo, statusBase); err != nil {
		return ctrl.Result{}, err
	}
	return result, nil
}

func (r *RepositoryReconciler) reconcile(
	ctx context.Context, repo *b2backupv1.Repository, api borgbase.API,
) (ctrl.Result, error) {
	known := repo.Status.SecretName != "" || repo.Status.Initialized

	remote, err := r.resolveRepo(ctx, repo, api)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !remote.IsRestic() {
		return ctrl.Result{}, fmt.Errorf(
			"%w: repository %s has format %q", borgbase.ErrNotRestic, remote.ID, remote.Format)
	}

	if other, err := r.repositoryConflict(ctx, repo, remote.ID); err != nil {
		return ctrl.Result{}, err
	} else if other != "" {
		r.setCondition(repo, metav1.Condition{
			Type:   b2backupv1.RepositoryConditionReady,
			Status: metav1.ConditionFalse,
			Reason: "RepositoryConflict",
			Message: fmt.Sprintf(
				"BorgBase repository %s is already managed by Repository %s; "+
					"point one of them elsewhere, or delete this one", remote.ID, other),
		})

		return ctrl.Result{RequeueAfter: r.interval(repo)}, nil
	}

	if remote, err = r.reconcileSettings(ctx, repo, remote, api); err != nil {
		return ctrl.Result{}, err
	}

	repo.Status.RepositoryID = remote.ID
	repo.Status.Server = remote.Host()
	repo.Status.CurrentUsage = formatUsage(remote.CurrentUsage)
	if remote.QuotaEnabled {
		repo.Status.Quota = formatQuota(remote.Quota)
	} else {
		repo.Status.Quota = ""
	}

	if err := r.reconcileSecret(ctx, repo, remote, known); err != nil {
		return ctrl.Result{}, err
	}

	justInitialized := false
	if !repo.Status.Initialized {
		requeue, err := r.reconcileInitJob(ctx, repo)
		if err != nil {
			return ctrl.Result{}, err
		}
		if requeue > 0 {
			r.setCondition(repo, metav1.Condition{
				Type:    b2backupv1.RepositoryConditionReady,
				Status:  metav1.ConditionFalse,
				Reason:  "Initializing",
				Message: "waiting for restic init to complete",
			})
			return ctrl.Result{RequeueAfter: requeue}, nil
		}
		justInitialized = true
	} else if err := r.cleanupInitJob(ctx, repo); err != nil {
		return ctrl.Result{}, err
	}

	r.setCondition(repo, metav1.Condition{
		Type:    b2backupv1.RepositoryConditionInitialized,
		Status:  metav1.ConditionTrue,
		Reason:  "Initialized",
		Message: "repository is initialized",
	})
	r.setCondition(repo, metav1.Condition{
		Type:    b2backupv1.RepositoryConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  "Ready",
		Message: "repository is ready",
	})

	if justInitialized {
		return ctrl.Result{RequeueAfter: initCleanupDelay}, nil
	}

	return ctrl.Result{RequeueAfter: r.interval(repo)}, nil
}

func (r *RepositoryReconciler) resolveRepo(
	ctx context.Context, repo *b2backupv1.Repository, api borgbase.API,
) (*borgbase.Repo, error) {
	if id := repo.Spec.ExistingRepositoryID; id != "" {
		if recorded := repo.Status.RepositoryID; recorded != "" && recorded != id {
			return nil, fmt.Errorf(
				"spec.existingRepositoryID is %s but this resource already manages repository %s; "+
					"delete and recreate the Repository if the change is intended", id, recorded)
		}
		remote, err := api.Get(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("adopting repository %s: %w", id, err)
		}
		repo.Status.Adopted = true
		return remote, nil
	}

	if id := repo.Status.RepositoryID; id != "" {
		remote, err := api.Get(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("looking up repository %s: %w", id, err)
		}
		return remote, nil
	}

	name := repo.RepositoryName()
	remote, err := api.FindByName(ctx, name)
	switch {
	case err == nil:
		repo.Status.Adopted = true
		r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "RepositoryAdopted", "Adopt",
			"Adopted the existing BorgBase repository %s named %s", remote.ID, name)
		return remote, nil
	case !errors.Is(err, borgbase.ErrNotFound):
		return nil, err
	}

	remote, err = api.Add(ctx, borgbase.AddOptions{
		Name:       name,
		Region:     repo.Spec.Region,
		Quota:      quotaMBPtr(repo.Spec.QuotaGiB),
		AlertDays:  int64Ptr(repo.Spec.AlertDays),
		AppendOnly: repo.Spec.AppendOnly,
	})
	if err != nil {
		return nil, fmt.Errorf("creating repository %q: %w", name, err)
	}
	r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "RepositoryCreated", "Create",
		"Created BorgBase repository %s (%s)", remote.ID, name)
	if err := r.recordRepositoryID(ctx, repo, remote.ID); err != nil {
		return nil, err
	}
	return remote, nil
}

func (r *RepositoryReconciler) recordRepositoryID(
	ctx context.Context, repo *b2backupv1.Repository, id string,
) error {
	base := repo.DeepCopy()
	repo.Status.RepositoryID = id
	if err := r.Status().Patch(ctx, repo, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("recording new repository %s on %s/%s: %w", id, repo.Namespace, repo.Name, err)
	}
	return nil
}

func (r *RepositoryReconciler) repositoryConflict(
	ctx context.Context, repo *b2backupv1.Repository, id string,
) (string, error) {
	if id == "" {
		return "", nil
	}

	var list b2backupv1.RepositoryList
	if err := r.List(ctx, &list); err != nil {
		return "", fmt.Errorf("listing repositories: %w", err)
	}

	for i := range list.Items {
		other := &list.Items[i]
		if !other.DeletionTimestamp.IsZero() ||
			(other.Namespace == repo.Namespace && other.Name == repo.Name) {
			continue
		}
		if managedID(other) != id {
			continue
		}
		if olderRepository(other, repo) {
			return other.Namespace + "/" + other.Name, nil
		}
	}
	return "", nil
}

func managedID(repo *b2backupv1.Repository) string {
	if repo.Status.RepositoryID != "" {
		return repo.Status.RepositoryID
	}
	return repo.Spec.ExistingRepositoryID
}

func olderRepository(a, b *b2backupv1.Repository) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	return a.Name < b.Name
}

func (r *RepositoryReconciler) reconcileSettings(
	ctx context.Context, repo *b2backupv1.Repository, remote *borgbase.Repo, api borgbase.API,
) (*borgbase.Repo, error) {
	var opts borgbase.EditOptions

	switch {
	case repo.Spec.QuotaGiB != nil:
		q := quotaMB(*repo.Spec.QuotaGiB)
		if !remote.QuotaEnabled || q != remote.Quota {
			opts.QuotaEnabled = ptr.To(true)
			opts.Quota = ptr.To(q)
		}
	case remote.QuotaEnabled:
		opts.QuotaEnabled = ptr.To(false)
		opts.Quota = ptr.To(remote.Quota)
	}
	if days := int64Ptr(repo.Spec.AlertDays); days != nil && *days != remote.AlertDays {
		opts.AlertDays = days
	}
	if repo.Spec.AppendOnly != remote.AppendOnly {
		opts.AppendOnly = ptr.To(repo.Spec.AppendOnly)
	}

	if opts.IsZero() {
		return remote, nil
	}

	updated, err := api.Edit(ctx, remote.ID, opts)
	if err != nil {
		return nil, fmt.Errorf("updating settings on repository %s: %w", remote.ID, err)
	}
	r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "SettingsUpdated", "Edit",
		"Updated settings on BorgBase repository %s", remote.ID)
	return updated, nil
}

// reconcileSecret writes the credentials Secret.
//
// The restic password is the encryption key for every snapshot in the repository,
// so once one exists anywhere it is reused verbatim and never regenerated:
// rotating it would render every existing backup unreadable.
//
// known says whether this resource has already written credentials, and so whether
// a password must already exist somewhere even though this Secret lacks one.
func (r *RepositoryReconciler) reconcileSecret(
	ctx context.Context, repo *b2backupv1.Repository, remote *borgbase.Repo, known bool,
) error {
	url, err := remote.ResticURL()
	if err != nil {
		return err
	}

	name := repo.SecretName()
	var existing corev1.Secret
	err = r.Get(ctx, types.NamespacedName{Namespace: repo.Namespace, Name: name}, &existing)
	switch {
	case err == nil:
	case apierrors.IsNotFound(err):
		existing = corev1.Secret{}
	default:
		return err
	}

	password := string(existing.Data[KeyResticPassword])
	generated := false

	if password == "" && repo.Spec.PasswordSecretRef != nil {
		password, err = r.readSecretKey(ctx, repo.Namespace, repo.Spec.PasswordSecretRef)
		if err != nil {
			return fmt.Errorf("reading passwordSecretRef: %w", err)
		}
	}

	if password == "" {
		if repo.Status.Adopted || known || remote.CurrentUsage > 0 {
			return fmt.Errorf(
				"repository %s has already been provisioned but no password is available; "+
					"restore Secret %s or set spec.passwordSecretRef to the existing RESTIC_PASSWORD",
				remote.ID, name)
		}
		if password, err = secrets.GeneratePassword(); err != nil {
			return err
		}
		generated = true
	}

	desired := &corev1.Secret{
		Name:      name,
		Namespace: repo.Namespace,
		Labels: map[string]string{
			labelManagedBy: managedByValue,
		},
		Type: corev1.SecretTypeOpaque,

		Data: map[string][]byte{
			KeyResticRepository: []byte(url),
			KeyResticPassword:   []byte(password),
		},
	}

	if repo.Spec.DeletionPolicy == b2backupv1.DeletionPolicyDelete {
		if err := controllerutil.SetControllerReference(repo, desired, r.Scheme); err != nil {
			return err
		}
	}

	op, err := r.createOrPatch(ctx, desired, &existing)
	if err != nil {
		return err
	}
	if generated {
		r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "PasswordGenerated", "Generate",
			"Generated a new restic password in Secret %s; back it up, it cannot be recovered", name)
	}
	if op != "" {
		log.FromContext(ctx).Info("Reconciled credentials Secret", "secret", name, "operation", op)
	}

	repo.Status.SecretName = name
	return nil
}

func (r *RepositoryReconciler) createOrPatch(
	ctx context.Context, desired *corev1.Secret, existing *corev1.Secret,
) (string, error) {
	if existing.Name == "" {
		if err := r.Create(ctx, desired); err != nil {
			return "", fmt.Errorf("creating secret %s: %w", desired.Name, err)
		}
		return "created", nil
	}

	if existing.Labels[labelManagedBy] != managedByValue {
		return "", fmt.Errorf(
			"secret %s/%s already exists and is not managed by this operator; "+
				"set spec.secretName to a name the operator owns",
			existing.Namespace, existing.Name)
	}

	updated := existing.DeepCopy()
	if updated.Labels == nil {
		updated.Labels = map[string]string{}
	}
	maps.Copy(updated.Labels, desired.Labels)
	if updated.Data == nil {
		updated.Data = map[string][]byte{}
	}
	maps.Copy(updated.Data, desired.Data)
	syncControllerRef(updated, desired)

	if equality.Semantic.DeepEqual(existing, updated) {
		return "", nil
	}
	if err := r.Update(ctx, updated); err != nil {
		return "", fmt.Errorf("updating secret %s: %w", desired.Name, err)
	}
	return "updated", nil
}

func syncControllerRef(obj *corev1.Secret, desired *corev1.Secret) {
	want := metav1.GetControllerOf(desired)
	have := metav1.GetControllerOf(obj)

	switch {
	case want == nil && have == nil:
		return
	case want != nil && have != nil && have.UID == want.UID && have.Name == want.Name:
		return
	}

	refs := slices.DeleteFunc(slices.Clone(obj.OwnerReferences), func(o metav1.OwnerReference) bool {
		return o.Controller != nil && *o.Controller
	})
	if want != nil {
		refs = append(refs, *want)
	}
	obj.OwnerReferences = refs
}

func (r *RepositoryReconciler) reconcileInitJob(
	ctx context.Context, repo *b2backupv1.Repository,
) (time.Duration, error) {
	name := repo.Name + initJobSuffix
	key := types.NamespacedName{Namespace: repo.Namespace, Name: name}

	var job batchv1.Job
	err := r.Get(ctx, key, &job)
	switch {
	case err == nil:
		if job.Status.Succeeded > 0 {
			recorded, err := r.initializationRecorded(ctx, repo)
			if err != nil {
				return 0, err
			}
			repo.Status.Initialized = true
			if !recorded {
				r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "Initialized", "Initialize",
					"restic init completed successfully")
			}
			return 0, nil
		}
		if failedAt := jobFailedAt(&job); failedAt != nil {
			r.setInitializing(repo, "InitFailed", fmt.Sprintf(
				"restic init failed in Job %s; on an adopted repository this usually means "+
					"passwordSecretRef holds the wrong password. Retrying in %s", name, initRetryDelay))
			if wait := initRetryDelay - time.Since(*failedAt); wait > 0 {
				return wait, nil
			}
			r.Recorder.Eventf(repo, nil, corev1.EventTypeWarning, "InitFailed", "Initialize",
				"restic init failed in Job %s; if this repository was adopted, check that "+
					"passwordSecretRef holds its original password. Retrying", name)
			if err := r.deleteJob(ctx, &job); err != nil {
				return 0, err
			}
			return initRetryDelay, nil
		}
		r.setInitializing(repo, "Initializing", fmt.Sprintf("waiting for init Job %s to complete", name))
		return 15 * time.Second, nil

	case !apierrors.IsNotFound(err):
		return 0, err
	}

	job = r.buildInitJob(repo, name)
	if err := controllerutil.SetControllerReference(repo, &job, r.Scheme); err != nil {
		return 0, err
	}
	if err := r.Create(ctx, &job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return 15 * time.Second, nil
		}
		return 0, fmt.Errorf("creating init job: %w", err)
	}
	r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "Initializing", "Initialize", "Started init job %s", name)
	r.setInitializing(repo, "Initializing", fmt.Sprintf("started init Job %s", name))
	return 15 * time.Second, nil
}

func (r *RepositoryReconciler) setInitializing(repo *b2backupv1.Repository, reason, message string) {
	r.setCondition(repo, metav1.Condition{
		Type:    b2backupv1.RepositoryConditionInitialized,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	})
}

func (r *RepositoryReconciler) buildInitJob(repo *b2backupv1.Repository, name string) batchv1.Job {
	return batchv1.Job{
		Name:      name,
		Namespace: repo.Namespace,
		Labels: map[string]string{
			"app.kubernetes.io/name":      repo.Name,
			"app.kubernetes.io/component": "init",
			labelManagedBy:                managedByValue,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(int32(3)),
			TTLSecondsAfterFinished: ptr.To(int32(3600)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: ptr.To(false),
					SecurityContext: &corev1.PodSecurityContext{
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:  "init",
						Image: r.BackupImage,

						Command: []string{"sh", "-c", "restic cat config >/dev/null 2>&1 || restic init"},
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{
								Name: repo.SecretName(),
							},
						}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},

						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("10m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
					}},
				},
			},
		},
	}
}

func (r *RepositoryReconciler) deleteJob(ctx context.Context, job *batchv1.Job) error {
	policy := metav1.DeletePropagationBackground
	if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &policy}); err != nil &&
		!apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting init job %s: %w", job.Name, err)
	}
	return nil
}

func (r *RepositoryReconciler) initializationRecorded(
	ctx context.Context, repo *b2backupv1.Repository,
) (bool, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var live b2backupv1.Repository
	key := types.NamespacedName{Namespace: repo.Namespace, Name: repo.Name}
	if err := reader.Get(ctx, key, &live); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return live.Status.Initialized, nil
}

func (r *RepositoryReconciler) cleanupInitJob(
	ctx context.Context, repo *b2backupv1.Repository,
) error {
	var job batchv1.Job
	key := types.NamespacedName{Namespace: repo.Namespace, Name: repo.Name + initJobSuffix}
	if err := r.Get(ctx, key, &job); err != nil {
		return client.IgnoreNotFound(err)
	}
	return r.deleteJob(ctx, &job)
}

func jobFailedAt(job *batchv1.Job) *time.Time {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			t := c.LastTransitionTime.Time
			return &t
		}
	}
	return nil
}

func (r *RepositoryReconciler) finalize(ctx context.Context, repo *b2backupv1.Repository) error {
	if !controllerutil.ContainsFinalizer(repo, FinalizerName) {
		return nil
	}

	switch {
	case repo.Spec.DeletionPolicy == b2backupv1.DeletionPolicyDelete && repo.Status.RepositoryID != "":
		api, err := r.apiFor(ctx, repo)
		if err != nil {
			r.setCondition(repo, metav1.Condition{
				Type:    b2backupv1.RepositoryConditionReady,
				Status:  metav1.ConditionFalse,
				Reason:  "APITokenUnavailable",
				Message: err.Error(),
			})
			r.Recorder.Eventf(repo, nil, corev1.EventTypeWarning, "APITokenUnavailable", "Delete", "%s", err.Error())
			return err
		}
		if err := r.deleteRemote(ctx, repo, api); err != nil {
			return err
		}

	case repo.Status.RepositoryID != "":
		r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "RepositoryRetained", "Retain",
			"Retained BorgBase repository %s; delete it manually if it is no longer needed",
			repo.Status.RepositoryID)
	}

	patch := client.MergeFrom(repo.DeepCopy())
	controllerutil.RemoveFinalizer(repo, FinalizerName)
	return r.Patch(ctx, repo, patch)
}

// deleteRemote deletes the BorgBase repository, confirming by lookup rather than
// by reading the error text when the mutation reports a failure.
func (r *RepositoryReconciler) deleteRemote(
	ctx context.Context, repo *b2backupv1.Repository, api borgbase.API,
) error {
	id := repo.Status.RepositoryID

	deleteErr := api.Delete(ctx, id)
	if deleteErr == nil {
		r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "RepositoryDeleted", "Delete",
			"Deleted BorgBase repository %s and all its snapshots", id)
		return nil
	}

	gone, probeErr := repositoryGone(ctx, api, id)
	switch {
	case gone:
		r.Recorder.Eventf(repo, nil, corev1.EventTypeWarning, "RepositoryDeleteUnconfirmed", "Delete",
			"BorgBase rejected the delete of repository %s but no longer has it, so it is "+
				"treated as deleted; its snapshots may remain: %s", id, deleteErr.Error())
		return nil
	case repo.Annotations[b2backupv1.AnnotationForceFinalize] == "true":
		r.Recorder.Eventf(repo, nil, corev1.EventTypeWarning, "RepositoryOrphaned", "Delete",
			"BorgBase repository %s could not be deleted and %s is set, so it is being left "+
				"behind; delete it manually, it is still billed: %s",
			id, b2backupv1.AnnotationForceFinalize, deleteErr.Error())
		return nil
	}

	err := deleteErr
	if probeErr != nil {
		err = fmt.Errorf("%w (checking whether it still exists: %w)", deleteErr, probeErr)
	}
	r.Recorder.Eventf(repo, nil, corev1.EventTypeWarning, "DeleteFailed", "Delete", "%s", err.Error())
	return fmt.Errorf("deleting repository %s: %w", id, err)
}

func repositoryGone(ctx context.Context, api borgbase.API, id string) (bool, error) {
	switch _, err := api.Get(ctx, id); {
	case err == nil:
		return false, nil
	case errors.Is(err, borgbase.ErrNotFound):
		return true, nil
	default:
		return false, err
	}
}

func (r *RepositoryReconciler) apiFor(
	ctx context.Context, repo *b2backupv1.Repository,
) (borgbase.API, error) {
	ref := repo.Spec.APITokenSecretRef
	namespace, selector := repo.Namespace, ref
	if ref == nil {
		namespace = r.DefaultTokenSecret.Namespace
		selector = &corev1.SecretKeySelector{
			Name: r.DefaultTokenSecret.Name,
			Key:  r.DefaultTokenKey,
		}
	}

	token, err := r.readSecretKey(ctx, namespace, selector)
	if err != nil {
		return nil, fmt.Errorf("reading BorgBase API token: %w", err)
	}
	newAPI := r.NewAPI
	if newAPI == nil {
		newAPI = func(t string) borgbase.API {
			c := borgbase.NewClient(t)
			if r.Endpoint != "" {
				c.Endpoint = r.Endpoint
			}
			return c
		}
	}
	return newAPI(token), nil
}

func (r *RepositoryReconciler) readSecretKey(
	ctx context.Context, namespace string, ref *corev1.SecretKeySelector,
) (string, error) {
	if ref == nil || ref.Name == "" {
		return "", fmt.Errorf("no secret reference configured")
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, &secret); err != nil {
		return "", err
	}
	value, ok := secret.Data[ref.Key]
	if !ok || len(value) == 0 {
		return "", fmt.Errorf("secret %s/%s has no key %q", namespace, ref.Name, ref.Key)
	}
	return string(value), nil
}

func (r *RepositoryReconciler) interval(repo *b2backupv1.Repository) time.Duration {
	if repo.Spec.Interval != nil && repo.Spec.Interval.Duration > 0 {
		return repo.Spec.Interval.Duration
	}
	return time.Hour
}

func (r *RepositoryReconciler) setCondition(repo *b2backupv1.Repository, cond metav1.Condition) {
	cond.ObservedGeneration = repo.Generation
	apimeta.SetStatusCondition(&repo.Status.Conditions, cond)
}

func (r *RepositoryReconciler) patchStatus(
	ctx context.Context, repo *b2backupv1.Repository, base *b2backupv1.Repository,
) error {
	repo.Status.ObservedGeneration = repo.Generation
	if equality.Semantic.DeepEqual(repo.Status, base.Status) {
		return nil
	}
	return r.Status().Patch(ctx, repo, client.MergeFrom(base))
}

// SetupWithManager registers the controller with the manager.
func (r *RepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("repository-controller")
	}
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(crcontroller.Options{MaxConcurrentReconciles: maxConcurrentReconciles}).
		For(&b2backupv1.Repository{}, builder.WithPredicates(ignoreOwnStatusWrites())).
		Owns(&batchv1.Job{}).
		Named("repository").
		Complete(r)
}
