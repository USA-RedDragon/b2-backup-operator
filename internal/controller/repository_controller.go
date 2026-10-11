// Package controller holds the Repository and ScheduledBackup reconcilers.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
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
	"github.com/USA-RedDragon/b2-backup-operator/internal/b2"
	"github.com/USA-RedDragon/b2-backup-operator/internal/secrets"
)

const (
	// FinalizerName guards the B2 data against deletion of the Repository.
	FinalizerName = "b2backup.mcswain.dev/finalizer"

	// KeyResticRepository is the Secret key holding the restic URL.
	KeyResticRepository = "RESTIC_REPOSITORY"
	// KeyResticPassword is the Secret key holding the restic password.
	KeyResticPassword = "RESTIC_PASSWORD"
	// KeyAccessKeyID is the Secret key holding the B2 application key ID restic uses over S3.
	KeyAccessKeyID = "AWS_ACCESS_KEY_ID"
	// KeySecretAccessKey is the Secret key holding the B2 application key restic uses over S3.
	KeySecretAccessKey = "AWS_SECRET_ACCESS_KEY"
	// KeyDefaultRegion is the Secret key holding the S3 region of the bucket.
	KeyDefaultRegion = "AWS_DEFAULT_REGION"

	initJobSuffix = "-init"

	initCleanupDelay = 10 * time.Second

	initRetryDelay = 5 * time.Minute

	maxConcurrentReconciles = 4

	usageInterval = 6 * time.Hour
	usageMaxPages = 20

	deleteBudget       = 5000
	deleteRequeueDelay = time.Second

	keyNameMaxLength = 100
)

// SecretKeys are the keys the operator writes to every credentials Secret.
var SecretKeys = []string{
	KeyResticRepository, KeyResticPassword, KeyAccessKeyID, KeySecretAccessKey, KeyDefaultRegion,
}

// ResticKeyCapabilities is what the per-repository application key may do. restic's
// S3 backend never names a version when it deletes, so B2 only hides the file and
// writeFiles is enough; without deleteFiles the key cannot destroy a version, and a
// hidden one stays recoverable until the lifecycle rule deletes it.
var ResticKeyCapabilities = []string{"listBuckets", "listFiles", "readFiles", "writeFiles"}

// RepositoryReconciler reconciles Repository objects against the B2 API.
type RepositoryReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	NewAPI func(keyID, key string) b2.API

	DefaultCredentialsSecret types.NamespacedName

	BackupImage string

	APIReader client.Reader

	AuthURL string

	Now func() time.Time

	apiMu    sync.Mutex
	apiCache map[string]b2.API
}

// +kubebuilder:rbac:groups=b2backup.mcswain.dev,resources=repositories,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=b2backup.mcswain.dev,resources=repositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=b2backup.mcswain.dev,resources=repositories/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete

// Reconcile brings the B2 bucket, application key and credentials Secret in line with the spec.
func (r *RepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var repo b2backupv1.Repository
	if err := r.Get(ctx, req.NamespacedName, &repo); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !repo.DeletionTimestamp.IsZero() {
		statusBase := repo.DeepCopy()
		result, err := r.finalize(ctx, &repo)
		if err != nil {
			if statusErr := r.patchStatus(ctx, &repo, statusBase); statusErr != nil {
				logger.Error(statusErr, "Failed to update status after a failed finalize")
			}
		}
		return result, err
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

	if other, err := r.repositoryConflict(ctx, &repo); err != nil {
		return ctrl.Result{}, err
	} else if other != "" {
		r.setCondition(&repo, metav1.Condition{
			Type:   b2backupv1.RepositoryConditionReady,
			Status: metav1.ConditionFalse,
			Reason: "RepositoryConflict",
			Message: fmt.Sprintf(
				"bucket %s path %s overlaps Repository %s, which claimed it first; "+
					"point one of them elsewhere, or delete this one",
				repo.Spec.Bucket, displayPath(repo.EffectivePath()), other),
		})
		return ctrl.Result{RequeueAfter: r.interval(&repo)}, r.patchStatus(ctx, &repo, statusBase)
	}

	api, err := r.apiFor(ctx, &repo)
	if err != nil {
		r.setCondition(&repo, metav1.Condition{
			Type:    b2backupv1.RepositoryConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "CredentialsUnavailable",
			Message: err.Error(),
		})
		r.Recorder.Eventf(&repo, nil, corev1.EventTypeWarning, "CredentialsUnavailable", "Reconcile", "%s", err.Error())
		if statusErr := r.patchStatus(ctx, &repo, statusBase); statusErr != nil {
			logger.Error(statusErr, "Failed to update status after a credentials failure")
		}
		return ctrl.Result{}, err
	}

	result, err := r.reconcile(ctx, &repo, api)
	if err != nil {
		r.setCondition(&repo, metav1.Condition{
			Type:    b2backupv1.RepositoryConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  reasonFor(err),
			Message: err.Error(),
		})
		r.Recorder.Eventf(&repo, nil, corev1.EventTypeWarning, reasonFor(err), "Reconcile", "%s", err.Error())
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

type reasonError struct {
	reason string
	err    error
}

func (e *reasonError) Error() string { return e.err.Error() }
func (e *reasonError) Unwrap() error { return e.err }

func withReason(reason string, err error) error {
	return &reasonError{reason: reason, err: err}
}

func reasonFor(err error) string {
	if re, ok := errors.AsType[*reasonError](err); ok {
		return re.reason
	}
	return "ReconcileFailed"
}

func (r *RepositoryReconciler) reconcile(
	ctx context.Context, repo *b2backupv1.Repository, api b2.API,
) (ctrl.Result, error) {
	known := repo.Status.SecretName != "" || repo.Status.Initialized || repo.Status.ApplicationKeyID != ""

	auth, err := api.Authorize(ctx)
	if err != nil {
		return ctrl.Result{}, withReason("CredentialsRejected", fmt.Errorf("authorizing with B2: %w", err))
	}
	region, err := auth.S3Region()
	if err != nil {
		return ctrl.Result{}, err
	}

	path := repo.EffectivePath()
	if repo.Status.BucketID != "" && repo.Status.Path != path {
		return ctrl.Result{}, fmt.Errorf(
			"this resource already manages path %s but now resolves to %s; "+
				"delete and recreate the Repository if the move is intended",
			displayPath(repo.Status.Path), displayPath(path))
	}

	bucket, err := r.resolveBucket(ctx, repo, api)
	if err != nil {
		return ctrl.Result{}, err
	}
	repo.Status.BucketName = bucket.BucketName
	repo.Status.BucketID = bucket.BucketID
	repo.Status.Path = path
	repo.Status.Endpoint = auth.S3APIURL

	dataExists := false
	if !repo.Status.Initialized {
		exists, err := b2.FileExists(ctx, api, bucket.BucketID, joinPath(path, "config"))
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("checking for an existing restic repository: %w", err)
		}
		if exists && !repo.Status.Adopted {
			repo.Status.Adopted = true
			r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "RepositoryAdopted", "Adopt",
				"Found an existing restic repository at %s/%s; adopting it", bucket.BucketName, displayPath(path))
		}
		if dataExists, err = b2.AnyVersionUnder(ctx, api, bucket.BucketID, filePrefix(path)); err != nil {
			return ctrl.Result{}, fmt.Errorf("checking for existing data: %w", err)
		}
	}

	if err := r.reconcileLifecycle(ctx, repo, api, bucket); err != nil {
		return ctrl.Result{}, err
	}

	creds := credentials{
		url:    resticURL(auth.S3APIURL, bucket.BucketName, path),
		region: region,
	}
	if err := r.reconcileSecret(ctx, repo, api, bucket, creds, known || dataExists); err != nil {
		return ctrl.Result{}, err
	}

	r.reconcileUsage(ctx, repo, api)

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

func (r *RepositoryReconciler) resolveBucket(
	ctx context.Context, repo *b2backupv1.Repository, api b2.API,
) (*b2.Bucket, error) {
	name := repo.Spec.Bucket
	bucket, err := api.FindBucket(ctx, name)
	switch {
	case err == nil:
		if recorded := repo.Status.BucketID; recorded != "" && recorded != bucket.BucketID {
			return nil, withReason("BucketReplaced", fmt.Errorf(
				"bucket %s now has ID %s but this resource recorded %s; it was deleted and "+
					"recreated, so the repository it held is gone. Delete and recreate the Repository",
				name, bucket.BucketID, recorded))
		}
		return bucket, nil
	case !errors.Is(err, b2.ErrNotFound):
		return nil, fmt.Errorf("looking up bucket %s: %w", name, err)
	}

	if repo.Spec.Mode != b2backupv1.StorageModeBucket {
		return nil, withReason("BucketNotFound", fmt.Errorf(
			"bucket %s does not exist in this B2 account; Prefix mode never creates buckets", name))
	}
	if repo.Status.BucketID != "" {
		return nil, withReason("BucketNotFound", fmt.Errorf(
			"bucket %s (%s) recorded on this resource no longer exists; refusing to create an "+
				"empty one in its place", name, repo.Status.BucketID))
	}

	var rules []b2.LifecycleRule
	if repo.ManagesLifecycle() {
		rules = []b2.LifecycleRule{wantedRule(repo)}
	}
	bucket, err = api.CreateBucket(ctx, name, rules)
	if errors.Is(err, b2.ErrDuplicateBucketName) {
		return nil, withReason("BucketNameTaken", fmt.Errorf(
			"bucket name %s is taken by another B2 account; bucket names are global", name))
	}
	if err != nil {
		return nil, fmt.Errorf("creating bucket %s: %w", name, err)
	}
	r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "BucketCreated", "Create",
		"Created private bucket %s (%s)", name, bucket.BucketID)
	return bucket, nil
}

func wantedRule(repo *b2backupv1.Repository) b2.LifecycleRule {
	return b2.LifecycleRule{
		FileNamePrefix:           filePrefix(repo.EffectivePath()),
		DaysFromHidingToDeleting: ptr.To(int(repo.DaysFromHidingToDeleting())),
	}
}

func (r *RepositoryReconciler) reconcileLifecycle(
	ctx context.Context, repo *b2backupv1.Repository, api b2.API, bucket *b2.Bucket,
) error {
	prefix := filePrefix(repo.EffectivePath())

	if hiding := b2.HidingRules(bucket.LifecycleRules, prefix); len(hiding) > 0 {
		msg := fmt.Sprintf("bucket %s has lifecycle rules hiding files under %q after some days, "+
			"which will hide restic data that snapshots still need: %q",
			bucket.BucketName, prefix, hiding)
		r.Recorder.Eventf(repo, nil, corev1.EventTypeWarning, "LifecycleHidesData", "Lifecycle", "%s", msg)
		r.setCondition(repo, metav1.Condition{
			Type: b2backupv1.RepositoryConditionLifecycleRule, Status: metav1.ConditionFalse,
			Reason: "HidesLiveFiles", Message: msg,
		})
		return nil
	}

	if !repo.ManagesLifecycle() {
		r.setCondition(repo, metav1.Condition{
			Type: b2backupv1.RepositoryConditionLifecycleRule, Status: metav1.ConditionTrue,
			Reason: "NotManaged", Message: "spec.lifecycle.manage is false; the bucket's rules are left alone",
		})
		return nil
	}

	for attempt := 0; ; attempt++ {
		rules, outcome, at := b2.MergeLifecycleRule(bucket.LifecycleRules, wantedRule(repo))
		switch outcome {
		case b2.RuleCovered:
			r.setCondition(repo, metav1.Condition{
				Type: b2backupv1.RepositoryConditionLifecycleRule, Status: metav1.ConditionTrue,
				Reason: "Covered", Message: fmt.Sprintf(
					"the bucket's rule for %q already deletes hidden files at least as soon", at),
			})
			return nil
		case b2.RuleUnchanged:
			r.setCondition(repo, metav1.Condition{
				Type: b2backupv1.RepositoryConditionLifecycleRule, Status: metav1.ConditionTrue,
				Reason: "Managed", Message: fmt.Sprintf("hidden files under %q are deleted after %d day(s)",
					prefix, repo.DaysFromHidingToDeleting()),
			})
			return nil
		}

		updated, err := api.UpdateLifecycleRules(ctx, bucket, rules)
		if errors.Is(err, b2.ErrConflict) && attempt == 0 {
			if bucket, err = api.FindBucket(ctx, bucket.BucketName); err != nil {
				return fmt.Errorf("re-reading bucket %s after a conflicting update: %w", repo.Spec.Bucket, err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("updating lifecycle rules on bucket %s: %w", bucket.BucketName, err)
		}
		*bucket = *updated
		r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "LifecycleRuleUpdated", "Lifecycle",
			"Set bucket %s to delete hidden files under %q after %d day(s)",
			bucket.BucketName, prefix, repo.DaysFromHidingToDeleting())
	}
}

type credentials struct {
	url    string
	region string
}

// reconcileSecret writes the credentials Secret, creating a scoped application key
// when there is no usable one.
//
// The restic password is the encryption key for every snapshot in the repository,
// so once one exists anywhere it is reused verbatim and never regenerated:
// rotating it would render every existing backup unreadable.
//
// mustExist says whether a password must already exist somewhere: this resource
// has written credentials before, or the path already holds data.
func (r *RepositoryReconciler) reconcileSecret(
	ctx context.Context, repo *b2backupv1.Repository, api b2.API, bucket *b2.Bucket,
	creds credentials, mustExist bool,
) error {
	name := repo.SecretName()
	var existing corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: repo.Namespace, Name: name}, &existing)
	switch {
	case err == nil:
		if existing.Labels[labelManagedBy] != managedByValue {
			return withReason("SecretNotManaged", fmt.Errorf(
				"secret %s/%s already exists and is not managed by this operator; "+
					"set spec.secretName to a name the operator owns",
				existing.Namespace, existing.Name))
		}
	case apierrors.IsNotFound(err):
		existing = corev1.Secret{}
	default:
		return err
	}

	password, generated, err := r.resolvePassword(ctx, repo, &existing, mustExist || repo.Status.Adopted)
	if err != nil {
		return err
	}

	keys, err := api.ListKeys(ctx)
	if err != nil {
		return fmt.Errorf("listing application keys: %w", err)
	}

	keyID := string(existing.Data[KeyAccessKeyID])
	keySecret := string(existing.Data[KeySecretAccessKey])
	usable := keyID != "" && keySecret != "" && keyID == repo.Status.ApplicationKeyID &&
		slices.ContainsFunc(keys, func(k b2.Key) bool {
			return k.ApplicationKeyID == keyID && keyMatches(k, bucket.BucketID, repo.EffectivePath())
		})

	var created *b2.Key
	if !usable {
		created, err = api.CreateKey(ctx, b2.CreateKeyRequest{
			KeyName:      keyName(repo),
			Capabilities: ResticKeyCapabilities,
			BucketID:     bucket.BucketID,
			NamePrefix:   filePrefix(repo.EffectivePath()),
		})
		if err != nil {
			return fmt.Errorf("creating an application key: %w", err)
		}
		keyID, keySecret = created.ApplicationKeyID, created.ApplicationKey
	}

	desired := &corev1.Secret{
		Name:      name,
		Namespace: repo.Namespace,
		Labels: map[string]string{
			labelManagedBy: managedByValue,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			KeyResticRepository: []byte(creds.url),
			KeyResticPassword:   []byte(password),
			KeyAccessKeyID:      []byte(keyID),
			KeySecretAccessKey:  []byte(keySecret),
			KeyDefaultRegion:    []byte(creds.region),
		},
	}
	if repo.Spec.DeletionPolicy == b2backupv1.DeletionPolicyDelete {
		if err := controllerutil.SetControllerReference(repo, desired, r.Scheme); err != nil {
			return err
		}
	}

	op, err := r.createOrPatch(ctx, desired, &existing)
	if err != nil {
		if created != nil {
			if delErr := api.DeleteKey(ctx, created.ApplicationKeyID); delErr != nil {
				err = fmt.Errorf("%w (and deleting the unsaved application key %s: %w)",
					err, created.ApplicationKeyID, delErr)
			}
		}
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

	if created != nil {
		if err := r.recordKey(ctx, repo, created.ApplicationKeyID); err != nil {
			return err
		}
		r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "KeyCreated", "Create",
			"Created application key %s restricted to %s/%s", created.ApplicationKeyID,
			bucket.BucketName, displayPath(repo.EffectivePath()))
	}

	return r.deleteStaleKeys(ctx, repo, api, keys)
}

func (r *RepositoryReconciler) resolvePassword(
	ctx context.Context, repo *b2backupv1.Repository, existing *corev1.Secret, mustExist bool,
) (string, bool, error) {
	password := string(existing.Data[KeyResticPassword])
	if password == "" && repo.Spec.PasswordSecretRef != nil {
		var err error
		if password, err = r.readSecretKey(ctx, repo.Namespace, repo.Spec.PasswordSecretRef); err != nil {
			return "", false, withReason("PasswordUnavailable", fmt.Errorf("reading passwordSecretRef: %w", err))
		}
	}
	if password != "" {
		return password, false, nil
	}
	if mustExist {
		return "", false, withReason("PasswordUnavailable", fmt.Errorf(
			"%s/%s already holds a repository or data, or this resource has provisioned one, "+
				"but no password is available; restore Secret %s or set spec.passwordSecretRef to "+
				"the existing RESTIC_PASSWORD", repo.Spec.Bucket, displayPath(repo.EffectivePath()),
			repo.SecretName()))
	}
	password, err := secrets.GeneratePassword()
	if err != nil {
		return "", false, err
	}
	return password, true, nil
}

func (r *RepositoryReconciler) recordKey(ctx context.Context, repo *b2backupv1.Repository, keyID string) error {
	obj := repo.DeepCopy()
	base := obj.DeepCopy()
	obj.Status.ApplicationKeyID = keyID
	if err := r.Status().Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("recording application key %s on %s/%s: %w", keyID, repo.Namespace, repo.Name, err)
	}
	repo.Status.ApplicationKeyID = keyID
	return nil
}

func (r *RepositoryReconciler) deleteStaleKeys(
	ctx context.Context, repo *b2backupv1.Repository, api b2.API, keys []b2.Key,
) error {
	name := keyName(repo)
	for _, k := range keys {
		if k.KeyName != name || k.ApplicationKeyID == repo.Status.ApplicationKeyID {
			continue
		}
		if err := api.DeleteKey(ctx, k.ApplicationKeyID); err != nil && !errors.Is(err, b2.ErrNotFound) {
			return fmt.Errorf("deleting superseded application key %s: %w", k.ApplicationKeyID, err)
		}
		r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "KeyDeleted", "Delete",
			"Deleted superseded application key %s", k.ApplicationKeyID)
	}
	return nil
}

func keyMatches(k b2.Key, bucketID, path string) bool {
	inBucket := k.BucketID == bucketID || (k.BucketID == "" && slices.Equal(k.BucketIDs, []string{bucketID}))
	caps := slices.Sorted(slices.Values(k.Capabilities))
	want := slices.Sorted(slices.Values(ResticKeyCapabilities))
	return inBucket && k.NamePrefix == filePrefix(path) && slices.Equal(caps, want)
}

func keyName(repo *b2backupv1.Repository) string {
	id := strings.ReplaceAll(string(repo.UID), "-", "")
	if id == "" {
		sum := sha256.Sum256([]byte(repo.Namespace + "/" + repo.Name))
		id = hex.EncodeToString(sum[:])
	}
	suffix := "-" + id[:min(len(id), 12)]
	base := sanitizeKeyName("b2bk-" + repo.Namespace + "-" + repo.Name)
	if len(base)+len(suffix) > keyNameMaxLength {
		base = base[:keyNameMaxLength-len(suffix)]
	}
	return base + suffix
}

func sanitizeKeyName(s string) string {
	return strings.Map(func(c rune) rune {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
			return c
		default:
			return '-'
		}
	}, s)
}

func (r *RepositoryReconciler) reconcileUsage(ctx context.Context, repo *b2backupv1.Repository, api b2.API) {
	now := r.now()
	if !repo.Status.Initialized {
		return
	}
	if t := repo.Status.UsageTime; t != nil && now.Sub(t.Time) < usageInterval {
		return
	}
	total, truncated, err := b2.Usage(ctx, api, repo.Status.BucketID, filePrefix(repo.Status.Path), usageMaxPages)
	if err != nil {
		log.FromContext(ctx).Error(err, "Failed to measure repository usage")
		return
	}
	repo.Status.UsageTime = &metav1.Time{Time: now}
	if truncated {
		repo.Status.CurrentUsage = ""
		return
	}
	repo.Status.CurrentUsage = formatBytes(total)
}

func (r *RepositoryReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *RepositoryReconciler) repositoryConflict(
	ctx context.Context, repo *b2backupv1.Repository,
) (string, error) {
	var list b2backupv1.RepositoryList
	if err := r.List(ctx, &list); err != nil {
		return "", fmt.Errorf("listing repositories: %w", err)
	}

	bucket, path := managedLocation(repo)
	for i := range list.Items {
		other := &list.Items[i]
		if !other.DeletionTimestamp.IsZero() ||
			(other.Namespace == repo.Namespace && other.Name == repo.Name) {
			continue
		}
		otherBucket, otherPath := managedLocation(other)
		if otherBucket != bucket || !pathsOverlap(path, otherPath) {
			continue
		}
		if olderRepository(other, repo) {
			return other.Namespace + "/" + other.Name, nil
		}
	}
	return "", nil
}

func managedLocation(repo *b2backupv1.Repository) (string, string) {
	if repo.Status.BucketID != "" {
		return repo.Status.BucketName, repo.Status.Path
	}
	return repo.Spec.Bucket, repo.EffectivePath()
}

func pathsOverlap(a, b string) bool {
	return a == "" || b == "" || a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
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

func (r *RepositoryReconciler) createOrPatch(
	ctx context.Context, desired *corev1.Secret, existing *corev1.Secret,
) (string, error) {
	if existing.Name == "" {
		if err := r.Create(ctx, desired); err != nil {
			return "", fmt.Errorf("creating secret %s: %w", desired.Name, err)
		}
		return "created", nil
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

func (r *RepositoryReconciler) finalize(ctx context.Context, repo *b2backupv1.Repository) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(repo, FinalizerName) {
		return ctrl.Result{}, nil
	}

	switch {
	case repo.Spec.DeletionPolicy == b2backupv1.DeletionPolicyDelete && repo.Status.BucketID != "":
		api, err := r.apiFor(ctx, repo)
		if err != nil {
			if r.forceFinalize(repo, err) {
				break
			}
			r.setCondition(repo, metav1.Condition{
				Type:    b2backupv1.RepositoryConditionReady,
				Status:  metav1.ConditionFalse,
				Reason:  "CredentialsUnavailable",
				Message: err.Error(),
			})
			r.Recorder.Eventf(repo, nil, corev1.EventTypeWarning, "CredentialsUnavailable", "Delete", "%s", err.Error())
			return ctrl.Result{}, err
		}
		done, err := r.deleteRemote(ctx, repo, api)
		if err != nil {
			if r.forceFinalize(repo, err) {
				break
			}
			r.Recorder.Eventf(repo, nil, corev1.EventTypeWarning, "DeleteFailed", "Delete", "%s", err.Error())
			return ctrl.Result{}, err
		}
		if !done {
			return ctrl.Result{RequeueAfter: deleteRequeueDelay}, nil
		}

	case repo.Status.BucketID != "":
		r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "RepositoryRetained", "Retain",
			"Retained %s/%s and application key %s; delete them manually if they are no longer needed",
			repo.Status.BucketName, displayPath(repo.Status.Path), repo.Status.ApplicationKeyID)
	}

	patch := client.MergeFrom(repo.DeepCopy())
	controllerutil.RemoveFinalizer(repo, FinalizerName)
	return ctrl.Result{}, r.Patch(ctx, repo, patch)
}

func (r *RepositoryReconciler) forceFinalize(repo *b2backupv1.Repository, err error) bool {
	if repo.Annotations[b2backupv1.AnnotationForceFinalize] != "true" {
		return false
	}
	r.Recorder.Eventf(repo, nil, corev1.EventTypeWarning, "RepositoryOrphaned", "Delete",
		"Could not delete %s/%s or application key %s, and %s is set, so they are being left "+
			"behind; delete them manually, they are still billed: %s",
		repo.Status.BucketName, displayPath(repo.Status.Path), repo.Status.ApplicationKeyID,
		b2backupv1.AnnotationForceFinalize, err.Error())
	return true
}

func (r *RepositoryReconciler) deleteRemote(
	ctx context.Context, repo *b2backupv1.Repository, api b2.API,
) (bool, error) {
	keys, err := api.ListKeys(ctx)
	if err != nil {
		return false, fmt.Errorf("listing application keys: %w", err)
	}
	name := keyName(repo)
	for _, k := range keys {
		if k.ApplicationKeyID != repo.Status.ApplicationKeyID && k.KeyName != name {
			continue
		}
		if err := api.DeleteKey(ctx, k.ApplicationKeyID); err != nil && !errors.Is(err, b2.ErrNotFound) {
			return false, fmt.Errorf("deleting application key %s: %w", k.ApplicationKeyID, err)
		}
	}

	bucketID, path := repo.Status.BucketID, repo.Status.Path
	prefix := filePrefix(path)
	deleted, done, err := b2.DeleteVersionsUnder(ctx, api, bucketID, prefix, deleteBudget)
	switch {
	case errors.Is(err, b2.ErrNotFound) && repo.Spec.Mode == b2backupv1.StorageModeBucket:
		r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "RepositoryDeleted", "Delete",
			"Bucket %s is already gone", repo.Status.BucketName)
		return true, nil
	case err != nil:
		return false, fmt.Errorf("deleting files under %s/%s: %w", repo.Status.BucketName, displayPath(path), err)
	case !done:
		log.FromContext(ctx).Info("Deleted file versions, more remain", "deleted", deleted)
		return false, nil
	}

	bucket, err := api.FindBucket(ctx, repo.Status.BucketName)
	if errors.Is(err, b2.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("re-reading bucket %s: %w", repo.Status.BucketName, err)
	}

	if repo.Spec.Mode == b2backupv1.StorageModeBucket {
		remaining, err := b2.AnyVersionUnder(ctx, api, bucketID, "")
		if err != nil {
			return false, err
		}
		if !remaining {
			if err := api.DeleteBucket(ctx, bucketID); err != nil && !errors.Is(err, b2.ErrNotFound) {
				return false, fmt.Errorf("deleting bucket %s: %w", repo.Status.BucketName, err)
			}
			r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "RepositoryDeleted", "Delete",
				"Deleted application key %s, every file under %s and bucket %s",
				repo.Status.ApplicationKeyID, displayPath(path), repo.Status.BucketName)
			return true, nil
		}
		r.Recorder.Eventf(repo, nil, corev1.EventTypeWarning, "BucketRetained", "Delete",
			"Bucket %s holds files outside %s, so it was left in place", repo.Status.BucketName, displayPath(path))
	}

	if rules, removed := b2.RemoveLifecycleRule(bucket.LifecycleRules, prefix); removed {
		if _, err := api.UpdateLifecycleRules(ctx, bucket, rules); err != nil {
			return false, fmt.Errorf("removing the lifecycle rule for %q: %w", prefix, err)
		}
	}
	r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "RepositoryDeleted", "Delete",
		"Deleted application key %s and every file under %s/%s",
		repo.Status.ApplicationKeyID, repo.Status.BucketName, displayPath(path))
	return true, nil
}

func (r *RepositoryReconciler) apiFor(ctx context.Context, repo *b2backupv1.Repository) (b2.API, error) {
	namespace, name := r.DefaultCredentialsSecret.Namespace, r.DefaultCredentialsSecret.Name
	if ref := repo.Spec.CredentialsSecretRef; ref != nil {
		namespace, name = repo.Namespace, ref.Name
	}

	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &secret); err != nil {
		return nil, fmt.Errorf("reading B2 credentials: %w", err)
	}
	keyID := string(secret.Data[b2backupv1.CredentialsKeyID])
	key := string(secret.Data[b2backupv1.CredentialsKey])
	if keyID == "" || key == "" {
		return nil, fmt.Errorf("reading B2 credentials: secret %s/%s needs both %s and %s",
			namespace, name, b2backupv1.CredentialsKeyID, b2backupv1.CredentialsKey)
	}

	sum := sha256.Sum256([]byte(keyID + "\x00" + key))
	cacheKey := hex.EncodeToString(sum[:])

	r.apiMu.Lock()
	defer r.apiMu.Unlock()
	if api, ok := r.apiCache[cacheKey]; ok {
		return api, nil
	}
	var api b2.API
	if r.NewAPI != nil {
		api = r.NewAPI(keyID, key)
	} else {
		c := b2.NewClient(keyID, key)
		if r.AuthURL != "" {
			c.AuthURL = r.AuthURL
		}
		api = c
	}
	if r.apiCache == nil {
		r.apiCache = map[string]b2.API{}
	}
	r.apiCache[cacheKey] = api
	return api, nil
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
