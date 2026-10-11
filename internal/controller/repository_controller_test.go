package controller

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/b2"
	"github.com/USA-RedDragon/b2-backup-operator/internal/b2/b2fake"
)

const mediaPrefix = "media/"

func bucketRepo(mutate func(*b2backupv1.Repository)) *b2backupv1.Repository {
	repo := &b2backupv1.Repository{
		Name: resticName, Namespace: testNS, UID: "0b1c2d3e-0000-4000-8000-000000000001",
		Spec: b2backupv1.RepositorySpec{
			Mode:           b2backupv1.StorageModeBucket,
			Bucket:         ownBucket,
			DeletionPolicy: b2backupv1.DeletionPolicyRetain,
		},
	}
	if mutate != nil {
		mutate(repo)
	}
	return repo
}

func prefixRepo(mutate func(*b2backupv1.Repository)) *b2backupv1.Repository {
	return bucketRepo(func(r *b2backupv1.Repository) {
		r.Spec.Mode = b2backupv1.StorageModePrefix
		r.Spec.Bucket = sharedBucket
		r.Spec.Path = myappPath
		if mutate != nil {
			mutate(r)
		}
	})
}

func seedSecretFixture() *corev1.Secret {
	return &corev1.Secret{
		Name: seedSecret, Namespace: testNS,
		Data: map[string][]byte{KeyResticPassword: []byte("existing-password-from-sops")},
	}
}

func usePasswordSeed(r *b2backupv1.Repository) {
	r.Spec.PasswordSecretRef = &corev1.SecretKeySelector{Name: seedSecret, Key: KeyResticPassword}
}

func reconcileN(t *testing.T, r *RepositoryReconciler, n int) {
	t.Helper()
	for i := range n {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{Namespace: testNS, Name: resticName}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
}

func reconcileOnce(t *testing.T, r *RepositoryReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{Namespace: testNS, Name: resticName})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func reconcileErr(r *RepositoryReconciler) error {
	_, err := r.Reconcile(context.Background(), ctrl.Request{Namespace: testNS, Name: resticName})
	return err
}

func getSecret(t *testing.T, c client.Client, name string) *corev1.Secret {
	t.Helper()
	var s corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, &s); err != nil {
		t.Fatalf("getting secret %s: %v", name, err)
	}
	return &s
}

func getSecretErr(c client.Client) error {
	return c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: credsSecretName}, &corev1.Secret{})
}

func getRepo(t *testing.T, c client.Client) *b2backupv1.Repository {
	t.Helper()
	var repo b2backupv1.Repository
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: resticName}, &repo); err != nil {
		t.Fatal(err)
	}
	return &repo
}

func condition(t *testing.T, c client.Client, condType string) *metav1.Condition {
	t.Helper()
	return apimeta.FindStatusCondition(getRepo(t, c).Status.Conditions, condType)
}

func initJob(t *testing.T, c client.Client) *batchv1.Job {
	t.Helper()
	var job batchv1.Job
	key := types.NamespacedName{Namespace: testNS, Name: resticName + initJobSuffix}
	if err := c.Get(context.Background(), key, &job); err != nil {
		t.Fatalf("getting the init job: %v", err)
	}
	return &job
}

func succeedInitJob(t *testing.T, c client.Client) {
	t.Helper()
	job := initJob(t, c)
	job.Status.Succeeded = 1
	if err := c.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

func failInitJob(t *testing.T, c client.Client, at time.Time) {
	t.Helper()
	job := initJob(t, c)
	job.Status.Conditions = []batchv1.JobCondition{{
		Type:               batchv1.JobFailed,
		Status:             corev1.ConditionTrue,
		Reason:             "BackoffLimitExceeded",
		LastTransitionTime: metav1.NewTime(at),
	}}
	if err := c.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

func recordedEvents(t *testing.T, r *RepositoryReconciler) []string {
	t.Helper()
	rec, ok := r.Recorder.(*events.FakeRecorder)
	if !ok {
		t.Fatalf("recorder is %T, want *events.FakeRecorder", r.Recorder)
	}
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func hasEvent(recorded []string, reason string) bool {
	return slices.ContainsFunc(recorded, func(e string) bool { return strings.Contains(e, " "+reason+" ") })
}

func loseSecret(t *testing.T, c client.Client) {
	t.Helper()
	secret := getSecret(t, c, credsSecretName)
	secret.Finalizers = nil
	if err := c.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
}

func ownKeys(f *b2fake.Server) []b2.Key {
	return slices.DeleteFunc(f.Keys(), func(k b2.Key) bool { return !strings.HasPrefix(k.KeyName, "b2bk-") })
}

func TestBucketModeCreatesABucketAKeyAndTheSecret(t *testing.T) {
	f := newFakeB2(t)
	r, c := newHarness(t, f, bucketRepo(nil))
	reconcileN(t, r, 2)

	bucket := f.Bucket(ownBucket)
	if bucket == nil {
		t.Fatal("no bucket was created")
	}
	if bucket.BucketType != b2.BucketTypePrivate {
		t.Errorf("bucket type = %q, want allPrivate", bucket.BucketType)
	}
	if len(bucket.LifecycleRules) != 1 || bucket.LifecycleRules[0].FileNamePrefix != "" ||
		*bucket.LifecycleRules[0].DaysFromHidingToDeleting != 30 {
		t.Errorf("lifecycle rules = %+v, want one bucket-wide rule deleting hidden files after 30 days",
			bucket.LifecycleRules)
	}

	keys := ownKeys(f)
	if len(keys) != 1 {
		t.Fatalf("keys = %+v, want exactly one", keys)
	}
	key := keys[0]
	if key.BucketID != bucket.BucketID || key.NamePrefix != "" {
		t.Errorf("key restricted to %q/%q, want the bucket and no prefix", key.BucketID, key.NamePrefix)
	}
	if !slices.Equal(slices.Sorted(slices.Values(key.Capabilities)), slices.Sorted(slices.Values(ResticKeyCapabilities))) {
		t.Errorf("key capabilities = %v, want %v", key.Capabilities, ResticKeyCapabilities)
	}
	if slices.Contains(key.Capabilities, "deleteFiles") {
		t.Error("the restic key can delete file versions; restic only ever hides them")
	}

	secret := getSecret(t, c, credsSecretName)
	want := map[string]string{
		KeyResticRepository: "s3:https://s3.us-east-005.backblazeb2.com/" + ownBucket,
		KeyAccessKeyID:      key.ApplicationKeyID,
		KeySecretAccessKey:  f.KeySecret(key.ApplicationKeyID),
		KeyDefaultRegion:    "us-east-005",
	}
	for k, v := range want {
		if got := string(secret.Data[k]); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if len(secret.Data[KeyResticPassword]) < 32 {
		t.Error("no usable password was generated")
	}
	if secret.Labels[labelManagedBy] != managedByValue {
		t.Error("the Secret is not labelled as managed")
	}

	st := getRepo(t, c).Status
	if st.BucketName != ownBucket || st.BucketID != bucket.BucketID || st.Path != "" ||
		st.Endpoint != "https://s3.us-east-005.backblazeb2.com" || st.ApplicationKeyID != key.ApplicationKeyID ||
		st.Adopted || st.SecretName != credsSecretName {
		t.Errorf("status = %+v", st)
	}

	job := initJob(t, c)
	container := job.Spec.Template.Spec.Containers[0]
	if got := container.Command[len(container.Command)-1]; got != "restic cat config >/dev/null 2>&1 || restic init" {
		t.Errorf("init command = %q", got)
	}
	if container.EnvFrom[0].SecretRef.Name != credsSecretName {
		t.Errorf("init job reads %s", container.EnvFrom[0].SecretRef.Name)
	}
	if automount := job.Spec.Template.Spec.AutomountServiceAccountToken; automount == nil || *automount {
		t.Error("init job should not mount a service account token")
	}
}

func TestPasswordIsGeneratedOnceAndNeverRotated(t *testing.T) {
	f := newFakeB2(t)
	r, c := newHarness(t, f, bucketRepo(nil))

	reconcileN(t, r, 1)
	first := string(getSecret(t, c, credsSecretName).Data[KeyResticPassword])
	reconcileN(t, r, 5)
	if after := string(getSecret(t, c, credsSecretName).Data[KeyResticPassword]); after != first {
		t.Errorf("password changed across reconciles")
	}
	if n := f.CountCalls("b2_create_key"); n != 1 {
		t.Errorf("created %d keys over six passes, want 1", n)
	}
}

func TestBucketModeAdoptsAnExistingBucketByName(t *testing.T) {
	f := newFakeB2(t)
	existing := f.AddBucket(ownBucket)
	r, c := newHarness(t, f, bucketRepo(nil))
	reconcileN(t, r, 1)

	if f.CountCalls("b2_create_bucket") != 0 {
		t.Error("a bucket that already exists was created again")
	}
	if got := getRepo(t, c).Status.BucketID; got != existing.BucketID {
		t.Errorf("BucketID = %q, want the existing %q", got, existing.BucketID)
	}
}

func TestBucketNameTakenElsewhereIsReported(t *testing.T) {
	f := newFakeB2(t)
	f.Fail("b2_create_bucket", b2fake.Failure{Status: 400, Code: b2.CodeDuplicateBucketName, Message: "taken"})
	r, c := newHarness(t, f, bucketRepo(nil))

	if err := reconcileErr(r); err == nil {
		t.Fatal("reconcile succeeded with no bucket")
	}
	if cond := condition(t, c, b2backupv1.RepositoryConditionReady); cond == nil || cond.Reason != "BucketNameTaken" {
		t.Errorf("Ready = %+v, want BucketNameTaken", cond)
	}
	if len(ownKeys(f)) != 0 {
		t.Error("a key was created without a bucket")
	}
}

func TestPrefixModeNeverCreatesABucket(t *testing.T) {
	f := newFakeB2(t)
	r, c := newHarness(t, f, prefixRepo(nil))

	if err := reconcileErr(r); err == nil {
		t.Fatal("reconcile succeeded without the shared bucket")
	}
	if f.CountCalls("b2_create_bucket") != 0 {
		t.Error("Prefix mode created a bucket")
	}
	if cond := condition(t, c, b2backupv1.RepositoryConditionReady); cond == nil || cond.Reason != "BucketNotFound" {
		t.Errorf("Ready = %+v, want BucketNotFound", cond)
	}
}

func TestPrefixModeScopesTheKeyAndDefaultsThePath(t *testing.T) {
	f := newFakeB2(t)
	bucket := f.AddBucket(sharedBucket)
	r, c := newHarness(t, f, prefixRepo(func(r *b2backupv1.Repository) { r.Spec.Path = "" }))
	reconcileN(t, r, 1)

	keys := ownKeys(f)
	if len(keys) != 1 || keys[0].BucketID != bucket.BucketID || keys[0].NamePrefix != testNS+"/"+resticName+"/" {
		t.Fatalf("keys = %+v, want one restricted to %s/%s/", keys, testNS, resticName)
	}
	want := "s3:https://s3.us-east-005.backblazeb2.com/" + sharedBucket + "/" + testNS + "/" + resticName
	if got := string(getSecret(t, c, credsSecretName).Data[KeyResticRepository]); got != want {
		t.Errorf("RESTIC_REPOSITORY = %q, want %q", got, want)
	}
	if got := getRepo(t, c).Status.Path; got != testNS+"/"+resticName {
		t.Errorf("status.path = %q", got)
	}
}

func TestAdoptsAnExistingRepositoryWithTheSeededPassword(t *testing.T) {
	f := newFakeB2(t)
	bucket := f.AddBucket(sharedBucket)
	f.PutFile(bucket.BucketID, "restic/myapp/config", 155)
	f.PutFile(bucket.BucketID, "restic/myapp/data/00/00aa", 4096)

	r, c := newHarness(t, f, prefixRepo(usePasswordSeed), seedSecretFixture())
	reconcileN(t, r, 2)

	secret := getSecret(t, c, credsSecretName)
	if got := string(secret.Data[KeyResticPassword]); got != "existing-password-from-sops" {
		t.Errorf("adopted password = %q, want the seeded one", got)
	}
	if got := string(secret.Data[KeyResticRepository]); got !=
		"s3:https://s3.us-east-005.backblazeb2.com/"+sharedBucket+"/restic/myapp" {
		t.Errorf("RESTIC_REPOSITORY = %q", got)
	}
	if !getRepo(t, c).Status.Adopted {
		t.Error("the repository is not reported as adopted")
	}
	if !hasEvent(recordedEvents(t, r), "RepositoryAdopted") {
		t.Error("no RepositoryAdopted event")
	}
	if got := f.FileNames(bucket.BucketID); len(got) != 2 {
		t.Errorf("adoption touched the data: %v", got)
	}
	if seed := getSecret(t, c, seedSecret); string(seed.Data[KeyResticPassword]) != "existing-password-from-sops" ||
		len(seed.Data) != 1 {
		t.Error("the operator modified the seed secret")
	}
}

func TestNeverGeneratesAPasswordForAnExistingRepository(t *testing.T) {
	f := newFakeB2(t)
	bucket := f.AddBucket(sharedBucket)
	f.PutFile(bucket.BucketID, "restic/myapp/config", 155)

	r, c := newHarness(t, f, prefixRepo(nil))
	err := reconcileErr(r)
	if err == nil || !strings.Contains(err.Error(), "passwordSecretRef") {
		t.Fatalf("err = %v, want a refusal naming passwordSecretRef", err)
	}
	if cond := condition(t, c, b2backupv1.RepositoryConditionReady); cond.Reason != "PasswordUnavailable" {
		t.Errorf("Ready reason = %q, want PasswordUnavailable", cond.Reason)
	}
	if !apierrors.IsNotFound(getSecretErr(c)) {
		t.Error("a Secret was written with an invented password")
	}
	if f.CountCalls("b2_create_key") != 0 {
		t.Error("a key was created for a repository that cannot be opened")
	}
}

func TestForeignDataStopsEverything(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		repo  *b2backupv1.Repository
		files []string
		hide  string
	}{
		{"a hidden file under the path", prefixRepo(nil), []string{"restic/myapp/keys/abc"}, "restic/myapp/keys/abc"},
		{"a parent of other repositories", prefixRepo(func(r *b2backupv1.Repository) {
			r.Spec.Path = resticName
			usePasswordSeed(r)
		}), []string{"restic/app1/config", "restic/app1/data/00/aa", "restic/app2/config"}, ""},
		{"a shared bucket adopted in Bucket mode", bucketRepo(func(r *b2backupv1.Repository) {
			r.Spec.Bucket = sharedBucket
			usePasswordSeed(r)
		}), []string{"restic/app1/config", "restic/app2/config"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeB2(t)
			bucket := f.AddBucket(sharedBucket)
			for _, name := range tt.files {
				f.PutFile(bucket.BucketID, name, 1)
			}
			if tt.hide != "" {
				f.HideFile(bucket.BucketID, tt.hide)
			}
			r, c := newHarness(t, f, tt.repo, seedSecretFixture())
			reconcileN(t, r, 2)

			if cond := condition(t, c, b2backupv1.RepositoryConditionReady); cond.Reason != reasonForeign {
				t.Errorf("Ready = %+v, want %s", cond, reasonForeign)
			}
			if f.CountCalls("b2_create_key") != 0 || f.CountCalls("b2_update_bucket") != 0 {
				t.Error("a key or lifecycle rule was created for a path holding someone else's files")
			}
			if !apierrors.IsNotFound(getSecretErr(c)) {
				t.Error("a credentials Secret was written")
			}
			var job batchv1.Job
			err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: resticName + initJobSuffix}, &job)
			if !apierrors.IsNotFound(err) {
				t.Error("an init Job was created")
			}
			if st := getRepo(t, c).Status; st.BucketID != "" || st.Adopted {
				t.Errorf("status = %+v, want the location left unrecorded", st)
			}
		})
	}
}

func TestASiblingPrefixIsNotMistakenForData(t *testing.T) {
	f := newFakeB2(t)
	bucket := f.AddBucket(sharedBucket)
	f.PutFile(bucket.BucketID, "restic/myapp2/config", 155)
	f.PutFile(bucket.BucketID, "restic/myapplication", 1)

	r, c := newHarness(t, f, prefixRepo(nil))
	reconcileN(t, r, 1)
	if getRepo(t, c).Status.Adopted {
		t.Error("a sibling repository was adopted")
	}
}

func TestNeverInventsAPasswordOnceProvisioned(t *testing.T) {
	f := newFakeB2(t)
	r, c := newHarness(t, f, bucketRepo(nil))
	reconcileN(t, r, 1)

	loseSecret(t, c)
	if err := reconcileErr(r); err == nil {
		t.Fatal("a fresh password was generated after the Secret was lost")
	}
	if !apierrors.IsNotFound(getSecretErr(c)) {
		t.Error("a Secret with a new password was written")
	}
}

func TestALostSecretGetsANewKeyAndTheOldOneIsDeleted(t *testing.T) {
	f := newFakeB2(t)
	f.AddBucket(sharedBucket)
	r, c := newHarness(t, f, prefixRepo(usePasswordSeed), seedSecretFixture())
	now := time.Now()
	r.Now = func() time.Time { return now }
	reconcileN(t, r, 1)
	oldKey := getRepo(t, c).Status.ApplicationKeyID
	succeedInitJob(t, c)

	loseSecret(t, c)
	reconcileN(t, r, 1)

	newKey := getRepo(t, c).Status.ApplicationKeyID
	if newKey == "" || newKey == oldKey {
		t.Fatalf("key %q after losing the Secret, want a new one replacing %q", newKey, oldKey)
	}
	if len(ownKeys(f)) != 2 {
		t.Error("the old key was deleted in the same pass, cutting off a backup still running with it")
	}
	now = now.Add(staleKeyGrace)
	reconcileN(t, r, 1)
	keys := ownKeys(f)
	if len(keys) != 1 || keys[0].ApplicationKeyID != newKey {
		t.Errorf("keys = %+v, want only the new one", keys)
	}
	secret := getSecret(t, c, credsSecretName)
	if string(secret.Data[KeyAccessKeyID]) != newKey || string(secret.Data[KeySecretAccessKey]) != f.KeySecret(newKey) {
		t.Error("the Secret does not hold the new key")
	}
}

func TestARevokedKeyIsReplaced(t *testing.T) {
	f := newFakeB2(t)
	r, c := newHarness(t, f, bucketRepo(nil))
	reconcileN(t, r, 1)
	oldKey := getRepo(t, c).Status.ApplicationKeyID
	password := string(getSecret(t, c, credsSecretName).Data[KeyResticPassword])

	f.DeleteKeyDirectly(oldKey)
	reconcileN(t, r, 1)

	secret := getSecret(t, c, credsSecretName)
	newKey := getRepo(t, c).Status.ApplicationKeyID
	if newKey == oldKey || string(secret.Data[KeyAccessKeyID]) != newKey {
		t.Errorf("key not replaced: status %q, secret %q, old %q", newKey, secret.Data[KeyAccessKeyID], oldKey)
	}
	if string(secret.Data[KeyResticPassword]) != password {
		t.Error("replacing the key changed the password")
	}
}

func TestStaleKeysOfThisRepositoryAreDeletedAndOthersAreNot(t *testing.T) {
	f := newFakeB2(t)
	repo := bucketRepo(nil)
	stale := f.AddKey(keyName(repo), "", "")
	foreign := f.AddKey("b2bk-otherns-restic-0123456789ab", "", "")
	handmade := f.AddKey("my-laptop", "", "")

	r, c := newHarness(t, f, repo)
	now := time.Now()
	r.Now = func() time.Time { return now }
	reconcileN(t, r, 1)
	succeedInitJob(t, c)
	now = now.Add(staleKeyGrace)
	reconcileN(t, r, 1)

	ids := make([]string, 0, len(f.Keys()))
	for _, k := range f.Keys() {
		ids = append(ids, k.ApplicationKeyID)
	}
	if slices.Contains(ids, stale) {
		t.Error("a stale key carrying this Repository's name survived")
	}
	if !slices.Contains(ids, foreign) || !slices.Contains(ids, handmade) {
		t.Errorf("a key belonging to something else was deleted: %v", ids)
	}
}

func TestRefusesToWriteAnUnmanagedSecretBeforeCreatingAKey(t *testing.T) {
	f := newFakeB2(t)
	foreign := &corev1.Secret{
		Name: credsSecretName, Namespace: testNS,
		Data: map[string][]byte{"unmanaged": []byte(relativeDir)},
	}
	r, c := newHarness(t, f, bucketRepo(nil), foreign)
	if err := reconcileErr(r); err == nil {
		t.Fatal("reconcile wrote over a Secret it does not manage")
	}
	if got := getSecret(t, c, credsSecretName); string(got.Data["unmanaged"]) != relativeDir || len(got.Data) != 1 {
		t.Error("the unmanaged Secret was modified")
	}
	if f.CountCalls("b2_create_key") != 0 {
		t.Error("a key was created that could never be saved")
	}
	if cond := condition(t, c, b2backupv1.RepositoryConditionReady); cond.Reason != "SecretNotManaged" {
		t.Errorf("Ready reason = %q, want SecretNotManaged", cond.Reason)
	}
}

func TestOwnerReferenceFollowsDeletionPolicy(t *testing.T) {
	f := newFakeB2(t)
	r, c := newHarness(t, f, bucketRepo(nil))
	reconcileN(t, r, 1)
	if owner := metav1.GetControllerOf(getSecret(t, c, credsSecretName)); owner != nil {
		t.Error("Retain must not own the Secret; it holds the only copy of the encryption key")
	}

	repo := getRepo(t, c)
	repo.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
	if err := c.Update(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, 1)
	if owner := metav1.GetControllerOf(getSecret(t, c, credsSecretName)); owner == nil || owner.Name != resticName {
		t.Error("Delete should own the Secret")
	}
}

func TestLifecycleRuleIsMergedWithTheBucketsOtherRules(t *testing.T) {
	f := newFakeB2(t)
	seven, thirty := 7, 30
	f.AddBucket(sharedBucket,
		b2.LifecycleRule{FileNamePrefix: mediaPrefix, DaysFromHidingToDeleting: &seven},
		b2.LifecycleRule{FileNamePrefix: "restic/other/", DaysFromHidingToDeleting: &thirty},
	)
	r, c := newHarness(t, f, prefixRepo(nil))
	reconcileN(t, r, 1)

	rules := f.Bucket(sharedBucket).LifecycleRules
	if len(rules) != 3 {
		t.Fatalf("rules = %+v, want the two existing ones plus this repository's", rules)
	}
	byPrefix := map[string]int{}
	for _, rule := range rules {
		byPrefix[rule.FileNamePrefix] = *rule.DaysFromHidingToDeleting
	}
	if byPrefix[mediaPrefix] != 7 || byPrefix["restic/other/"] != 30 || byPrefix[myappPrefix] != 30 {
		t.Errorf("rules = %v", byPrefix)
	}
	if cond := condition(t, c, b2backupv1.RepositoryConditionLifecycleRule); cond == nil ||
		cond.Status != metav1.ConditionTrue || cond.Reason != reasonManaged {
		t.Errorf("LifecycleRule = %+v", cond)
	}

	reconcileN(t, r, 2)
	if n := f.CountCalls("b2_update_bucket"); n != 1 {
		t.Errorf("updated the bucket %d times, want once", n)
	}
}

func TestLifecycleRuleOnAParentPrefixCoversTheRepository(t *testing.T) {
	f := newFakeB2(t)
	one := 1
	f.AddBucket(sharedBucket, b2.LifecycleRule{FileNamePrefix: resticPrefix, DaysFromHidingToDeleting: &one})
	r, c := newHarness(t, f, prefixRepo(nil))
	reconcileN(t, r, 1)

	if f.CountCalls("b2_update_bucket") != 0 {
		t.Error("a rule was added under a parent rule that already covers it")
	}
	cond := condition(t, c, b2backupv1.RepositoryConditionLifecycleRule)
	if cond.Reason != reasonCovered || !strings.Contains(cond.Message, `"`+resticPrefix+`"`) ||
		!strings.Contains(cond.Message, "after 1 day(s)") {
		t.Errorf("LifecycleRule = %+v, want %s naming the rule and its days", cond, reasonCovered)
	}
}

func TestLifecycleRuleUpdateRetriesAfterAConflict(t *testing.T) {
	f := newFakeB2(t)
	f.AddBucket(sharedBucket)
	f.Fail("b2_update_bucket", b2fake.Failure{Status: 409, Code: b2.CodeConflict})
	r, _ := newHarness(t, f, prefixRepo(nil))
	reconcileN(t, r, 1)

	if rules := f.Bucket(sharedBucket).LifecycleRules; len(rules) != 1 {
		t.Errorf("rules = %+v, want the rule added on the retry", rules)
	}
}

func TestLifecycleManagementCanBeTurnedOff(t *testing.T) {
	f := newFakeB2(t)
	f.AddBucket(sharedBucket)
	r, c := newHarness(t, f, prefixRepo(func(r *b2backupv1.Repository) {
		r.Spec.Lifecycle = &b2backupv1.LifecycleSpec{Manage: ptr.To(false)}
	}))
	reconcileN(t, r, 1)
	if f.CountCalls("b2_update_bucket") != 0 {
		t.Error("the bucket was changed with lifecycle management off")
	}
	if cond := condition(t, c, b2backupv1.RepositoryConditionLifecycleRule); cond.Reason != "NotManaged" {
		t.Errorf("LifecycleRule reason = %q", cond.Reason)
	}
}

func TestARuleThatHidesLiveFilesIsReported(t *testing.T) {
	f := newFakeB2(t)
	thirty := 30
	f.AddBucket(sharedBucket, b2.LifecycleRule{FileNamePrefix: "", DaysFromUploadingToHiding: &thirty})
	r, c := newHarness(t, f, prefixRepo(nil))
	reconcileN(t, r, 1)

	if cond := condition(t, c, b2backupv1.RepositoryConditionLifecycleRule); cond == nil ||
		cond.Status != metav1.ConditionFalse || cond.Reason != "HidesLiveFiles" {
		t.Errorf("LifecycleRule = %+v, want False HidesLiveFiles", cond)
	}
	succeedInitJob(t, c)
	reconcileN(t, r, 1)
	if cond := condition(t, c, b2backupv1.RepositoryConditionReady); cond.Status != metav1.ConditionFalse ||
		cond.Reason != "LifecycleHidesData" {
		t.Errorf("Ready = %+v, want False while a rule will hide live packs", cond)
	}
	if !hasEvent(recordedEvents(t, r), "LifecycleHidesData") {
		t.Error("no LifecycleHidesData warning")
	}
}

func TestCreatedBucketCarriesNoRuleWhenUnmanaged(t *testing.T) {
	f := newFakeB2(t)
	r, _ := newHarness(t, f, bucketRepo(func(r *b2backupv1.Repository) {
		r.Spec.Lifecycle = &b2backupv1.LifecycleSpec{Manage: ptr.To(false)}
	}))
	reconcileN(t, r, 1)
	if rules := f.Bucket(ownBucket).LifecycleRules; len(rules) != 0 {
		t.Errorf("rules = %+v, want none", rules)
	}
}

func TestSucceededInitJobIsRemovedOnTheNextPass(t *testing.T) {
	f := newFakeB2(t)
	r, c := newHarness(t, f, bucketRepo(nil))
	reconcileN(t, r, 1)
	succeedInitJob(t, c)

	reconcileN(t, r, 1)
	if !getRepo(t, c).Status.Initialized {
		t.Fatal("repository should be marked initialized")
	}
	key := types.NamespacedName{Namespace: testNS, Name: resticName + initJobSuffix}
	if err := c.Get(context.Background(), key, &batchv1.Job{}); err != nil {
		t.Errorf("init job should survive the pass that records success: %v", err)
	}
	reconcileN(t, r, 1)
	if err := c.Get(context.Background(), key, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Errorf("init job should have been deleted, got %v", err)
	}
	reconcileN(t, r, 3)
	if err := c.Get(context.Background(), key, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Error("a second init job was created after initialization completed")
	}
}

func TestFailedInitJobBacksOffAndIsThenReplaced(t *testing.T) {
	f := newFakeB2(t)
	r, c := newHarness(t, f, bucketRepo(nil))
	reconcileN(t, r, 1)

	failInitJob(t, c, time.Now())
	res := reconcileOnce(t, r)
	if res.RequeueAfter <= 0 || res.RequeueAfter > initRetryDelay {
		t.Errorf("RequeueAfter = %v, want the remainder of the %v backoff", res.RequeueAfter, initRetryDelay)
	}
	cond := condition(t, c, b2backupv1.RepositoryConditionInitialized)
	if cond == nil || cond.Reason != "InitFailed" || !strings.Contains(cond.Message, "passwordSecretRef") {
		t.Fatalf("Initialized = %+v, want InitFailed naming passwordSecretRef", cond)
	}

	failInitJob(t, c, time.Now().Add(-initRetryDelay-time.Minute))
	if res := reconcileOnce(t, r); res.RequeueAfter != initRetryDelay {
		t.Errorf("RequeueAfter = %v, want %v once the backoff has elapsed", res.RequeueAfter, initRetryDelay)
	}
	key := types.NamespacedName{Namespace: testNS, Name: resticName + initJobSuffix}
	if err := c.Get(context.Background(), key, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Errorf("the failed init Job was not deleted: %v", err)
	}
	if res := reconcileOnce(t, r); res.RequeueAfter != wantInitPoll {
		t.Errorf("RequeueAfter = %v, want %v while the replacement Job runs", res.RequeueAfter, wantInitPoll)
	}
}

func TestSpecIntervalSetsTheRequeueDelay(t *testing.T) {
	f := newFakeB2(t)
	r, c := newHarness(t, f, bucketRepo(func(r *b2backupv1.Repository) {
		r.Spec.Interval = &metav1.Duration{Duration: 7 * time.Minute}
	}))
	reconcileN(t, r, 1)
	succeedInitJob(t, c)
	reconcileN(t, r, 2)
	if res := reconcileOnce(t, r); res.RequeueAfter != 7*time.Minute {
		t.Errorf("RequeueAfter = %v, want spec.interval (7m)", res.RequeueAfter)
	}
}

func TestUsageIsMeasuredAtMostEverySixHours(t *testing.T) {
	f := newFakeB2(t)
	bucket := f.AddBucket(sharedBucket)
	f.PutFile(bucket.BucketID, "restic/myapp/config", 1024)
	f.PutFile(bucket.BucketID, "restic/myapp/data/00/aa", 3<<20)
	f.PutFile(bucket.BucketID, "restic/other/data/00/bb", 1<<30)

	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	r, c := newHarness(t, f, prefixRepo(usePasswordSeed), seedSecretFixture())
	r.Now = func() time.Time { return now }
	reconcileN(t, r, 1)
	if getRepo(t, c).Status.CurrentUsage != "" {
		t.Error("usage was measured before the repository was initialized")
	}
	succeedInitJob(t, c)
	reconcileN(t, r, 2)

	st := getRepo(t, c).Status
	if st.CurrentUsage != "3MiB" || st.UsageTime == nil {
		t.Errorf("usage = %q at %v, want 3MiB, the files under this path only", st.CurrentUsage, st.UsageTime)
	}

	before := f.CountCalls("b2_list_file_names")
	now = now.Add(time.Hour)
	reconcileN(t, r, 2)
	if f.CountCalls("b2_list_file_names") != before {
		t.Error("usage was measured again within six hours")
	}
	now = now.Add(6 * time.Hour)
	reconcileN(t, r, 1)
	if f.CountCalls("b2_list_file_names") == before {
		t.Error("usage was not refreshed after six hours")
	}
}

func TestSuspendSkipsReconcile(t *testing.T) {
	f := newFakeB2(t)
	r, c := newHarness(t, f, bucketRepo(func(r *b2backupv1.Repository) { r.Spec.Suspend = true }))
	reconcileN(t, r, 3)

	if len(f.Calls()) != 0 {
		t.Errorf("a suspended repository called B2: %v", f.Calls())
	}
	if cond := condition(t, c, b2backupv1.RepositoryConditionReady); cond == nil || cond.Reason != "Suspended" {
		t.Errorf("Ready = %+v, want Suspended", cond)
	}
}

func TestMissingCredentialsAreReported(t *testing.T) {
	f := newFakeB2(t)
	r, c := newHarness(t, f, bucketRepo(func(r *b2backupv1.Repository) {
		r.Spec.CredentialsSecretRef = &corev1.LocalObjectReference{Name: missingSecret}
	}))
	if err := reconcileErr(r); err == nil {
		t.Fatal("reconcile succeeded without credentials")
	}
	if cond := condition(t, c, b2backupv1.RepositoryConditionReady); cond.Reason != "CredentialsUnavailable" {
		t.Errorf("Ready reason = %q", cond.Reason)
	}
}

func TestCredentialsSecretRefOverridesTheOperatorsKey(t *testing.T) {
	f := newFakeB2(t)
	other := f.AddKey("team-key", "", "")
	override := &corev1.Secret{
		Name: teamSecret, Namespace: testNS,
		Data: map[string][]byte{
			b2backupv1.CredentialsKeyID: []byte(other),
			b2backupv1.CredentialsKey:   []byte(f.KeySecret(other)),
		},
	}
	r, _ := newHarness(t, f, bucketRepo(func(r *b2backupv1.Repository) {
		r.Spec.CredentialsSecretRef = &corev1.LocalObjectReference{Name: teamSecret}
	}), override)
	r.DefaultCredentialsSecret.Name = missingSecret
	reconcileN(t, r, 1)
	if f.Bucket(ownBucket) == nil {
		t.Error("the override credentials were not used")
	}
}

func TestRejectedCredentialsAreReported(t *testing.T) {
	f := newFakeB2(t)
	wrong := &corev1.Secret{
		Name: "wrong", Namespace: testNS,
		Data: map[string][]byte{
			b2backupv1.CredentialsKeyID: []byte(masterKeyID),
			b2backupv1.CredentialsKey:   []byte("not-the-key"),
		},
	}
	r, c := newHarness(t, f, bucketRepo(func(r *b2backupv1.Repository) {
		r.Spec.CredentialsSecretRef = &corev1.LocalObjectReference{Name: "wrong"}
	}), wrong)
	if err := reconcileErr(r); err == nil {
		t.Fatal("reconcile succeeded with a rejected key")
	}
	if cond := condition(t, c, b2backupv1.RepositoryConditionReady); cond.Reason != "CredentialsRejected" {
		t.Errorf("Ready reason = %q, want CredentialsRejected", cond.Reason)
	}
}

func TestARecreatedBucketIsRefused(t *testing.T) {
	f := newFakeB2(t)
	f.AddBucket(ownBucket)
	r, c := newHarness(t, f, bucketRepo(func(r *b2backupv1.Repository) {
		r.Status = b2backupv1.RepositoryStatus{BucketID: "bucket9999", BucketName: ownBucket}
	}))
	if err := reconcileErr(r); err == nil {
		t.Fatal("reconcile accepted a bucket with a different ID")
	}
	if cond := condition(t, c, b2backupv1.RepositoryConditionReady); cond.Reason != "BucketReplaced" {
		t.Errorf("Ready reason = %q, want BucketReplaced", cond.Reason)
	}
}

func TestAVanishedBucketIsNotRecreated(t *testing.T) {
	f := newFakeB2(t)
	r, _ := newHarness(t, f, bucketRepo(func(r *b2backupv1.Repository) {
		r.Status = b2backupv1.RepositoryStatus{BucketID: "bucket9999", BucketName: ownBucket}
	}))
	if err := reconcileErr(r); err == nil {
		t.Fatal("reconcile succeeded without the recorded bucket")
	}
	if f.CountCalls("b2_create_bucket") != 0 {
		t.Error("an empty bucket was created in place of the recorded one")
	}
}

func TestKeyNameIsValidBoundedAndUnique(t *testing.T) {
	long := bucketRepo(func(r *b2backupv1.Repository) {
		r.Namespace = strings.Repeat("n", 63)
		r.Name = strings.Repeat("a.b", 60)
	})
	name := keyName(long)
	if len(name) > keyNameMaxLength {
		t.Errorf("key name is %d characters, B2 allows %d", len(name), keyNameMaxLength)
	}
	if strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-") != "" {
		t.Errorf("key name %q has characters B2 rejects", name)
	}
	other := long.DeepCopy()
	other.UID = "ffffffff-0000-4000-8000-000000000001"
	if keyName(other) == name {
		t.Error("two Repositories share a key name, so one could delete the other's key")
	}
}

func TestRepositoryConflicts(t *testing.T) {
	tests := []struct {
		name         string
		incumbent    func(*b2backupv1.Repository)
		wantConflict bool
	}{
		{"same path", func(r *b2backupv1.Repository) {}, true},
		{"parent path", func(r *b2backupv1.Repository) { r.Spec.Path = strings.Split(myappPath, "/")[0] }, true},
		{"child path", func(r *b2backupv1.Repository) { r.Spec.Path = "restic/myapp/sub" }, true},
		{"the whole bucket", func(r *b2backupv1.Repository) {
			r.Spec.Mode = b2backupv1.StorageModeBucket
			r.Spec.Path = ""
		}, true},
		{"recorded in status", func(r *b2backupv1.Repository) {
			r.Spec.Path = "elsewhere"
			r.Status = b2backupv1.RepositoryStatus{BucketID: "b", BucketName: sharedBucket, Path: myappPath}
		}, true},
		{"sibling path", func(r *b2backupv1.Repository) { r.Spec.Path = "restic/myapp2" }, false},
		{"another bucket", func(r *b2backupv1.Repository) { r.Spec.Bucket = otherBucket }, false},
		{"being deleted", func(r *b2backupv1.Repository) {
			r.DeletionTimestamp = ptr.To(metav1.Now())
			r.Finalizers = []string{FinalizerName}
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeB2(t)
			f.AddBucket(sharedBucket)
			incumbent := prefixRepo(func(r *b2backupv1.Repository) {
				r.Name, r.Namespace, r.UID = "incumbent", "other-ns", "uid-incumbent"
				r.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
				tt.incumbent(r)
			})
			newcomer := prefixRepo(func(r *b2backupv1.Repository) {
				r.CreationTimestamp = metav1.NewTime(time.Now())
				r.Spec.Interval = &metav1.Duration{Duration: 3 * time.Minute}
			})
			r, c := newHarness(t, f, incumbent, newcomer)
			res := reconcileOnce(t, r)

			cond := condition(t, c, b2backupv1.RepositoryConditionReady)
			conflicted := cond != nil && cond.Reason == reasonConflict
			if conflicted != tt.wantConflict {
				t.Fatalf("conflict = %t (%+v), want %t", conflicted, cond, tt.wantConflict)
			}
			if !tt.wantConflict {
				return
			}
			if !strings.Contains(cond.Message, "other-ns/incumbent") {
				t.Errorf("message %q does not name the incumbent", cond.Message)
			}
			if res.RequeueAfter != 3*time.Minute {
				t.Errorf("RequeueAfter = %v, want spec.interval so the conflict is rechecked", res.RequeueAfter)
			}
			if len(f.Calls()) != 0 {
				t.Errorf("the loser of a conflict called B2: %v", f.Calls())
			}
			if !apierrors.IsNotFound(getSecretErr(c)) {
				t.Error("the loser of a conflict wrote credentials")
			}
		})
	}
}

func TestTheOlderRepositoryKeepsThePath(t *testing.T) {
	f := newFakeB2(t)
	f.AddBucket(sharedBucket)
	newer := prefixRepo(func(r *b2backupv1.Repository) {
		r.Name, r.UID = "newer", "uid-newer"
		r.CreationTimestamp = metav1.NewTime(time.Now())
	})
	older := prefixRepo(func(r *b2backupv1.Repository) {
		r.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	})
	r, c := newHarness(t, f, newer, older)
	reconcileN(t, r, 1)
	if cond := condition(t, c, b2backupv1.RepositoryConditionReady); cond.Reason == reasonConflict {
		t.Error("the older Repository lost the conflict")
	}
}

// provision reconciles a Repository until its init Job has succeeded, as one
// that created or opened its repository has.
func provision(t *testing.T, r *RepositoryReconciler, c client.Client) {
	t.Helper()
	reconcileN(t, r, 1)
	succeedInitJob(t, c)
	reconcileN(t, r, 1)
}

func provisionForDeletion(t *testing.T, r *RepositoryReconciler, c client.Client) *b2backupv1.Repository {
	t.Helper()
	provision(t, r, c)
	repo := getRepo(t, c)
	if repo.Status.BucketID == "" {
		t.Fatal("nothing was provisioned")
	}
	if err := c.Delete(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	return getRepo(t, c)
}

func reconcileUntilGone(t *testing.T, r *RepositoryReconciler, c client.Client) {
	t.Helper()
	for range 10 {
		reconcileN(t, r, 1)
		err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: resticName},
			&b2backupv1.Repository{})
		if apierrors.IsNotFound(err) {
			return
		}
	}
	t.Fatal("the Repository was never released")
}

func TestDeleteInPrefixModeRemovesOnlyItsOwnPrefix(t *testing.T) {
	f := newFakeB2(t)
	one, seven := 1, 7
	bucket := f.AddBucket(sharedBucket,
		b2.LifecycleRule{FileNamePrefix: mediaPrefix, DaysFromHidingToDeleting: &seven},
		b2.LifecycleRule{FileNamePrefix: "restic/myapp2/", DaysFromHidingToDeleting: &one},
	)
	foreignKey := f.AddKey("b2bk-other-restic-0123456789ab", bucket.BucketID, "restic/myapp2/")

	r, c := newHarness(t, f, prefixRepo(func(r *b2backupv1.Repository) {
		r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
		usePasswordSeed(r)
	}), seedSecretFixture())

	f.PutFile(bucket.BucketID, "restic/myapp/config", 1)
	f.PutFile(bucket.BucketID, "restic/myapp/data/00/aa", 1)
	f.HideFile(bucket.BucketID, "restic/myapp/data/00/aa")
	f.StartLargeFile(bucket.BucketID, "restic/myapp/data/11/big")
	f.PutFile(bucket.BucketID, "restic/myapp2/config", 1)
	f.PutFile(bucket.BucketID, "restic/myapplication", 1)
	f.PutFile(bucket.BucketID, "restic/myapp", 1)
	f.PutFile(bucket.BucketID, "media/photo.jpg", 1)
	f.StartLargeFile(bucket.BucketID, "restic/myapp2/data/22/big")

	provisionForDeletion(t, r, c)
	reconcileUntilGone(t, r, c)

	want := []string{"restic/myapp2/config", "restic/myapplication", "restic/myapp", "media/photo.jpg",
		"restic/myapp2/data/22/big"}
	if got := f.FileNames(bucket.BucketID); !slices.Equal(got, want) {
		t.Errorf("remaining files = %v, want everything outside restic/myapp/ untouched: %v", got, want)
	}
	if f.Bucket(sharedBucket) == nil {
		t.Fatal("Prefix mode deleted the shared bucket")
	}
	rules := f.Bucket(sharedBucket).LifecycleRules
	prefixes := make([]string, 0, len(rules))
	for _, rule := range rules {
		prefixes = append(prefixes, rule.FileNamePrefix)
	}
	if !slices.Equal(prefixes, []string{mediaPrefix, "restic/myapp2/"}) {
		t.Errorf("lifecycle rules = %v, want only this repository's rule removed", prefixes)
	}
	keys := ownKeys(f)
	if len(keys) != 1 || keys[0].ApplicationKeyID != foreignKey {
		t.Errorf("keys = %+v, want only the other repository's key left", keys)
	}
	if !hasEvent(recordedEvents(t, r), "RepositoryDeleted") {
		t.Error("no RepositoryDeleted event")
	}
}

func TestDeleteInBucketModeDeletesTheBucket(t *testing.T) {
	f := newFakeB2(t)
	r, c := newHarness(t, f, bucketRepo(func(r *b2backupv1.Repository) {
		r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
	}))
	provisionForDeletion(t, r, c)
	bucket := f.Bucket(ownBucket)
	f.PutFile(bucket.BucketID, "config", 1)
	f.PutFile(bucket.BucketID, "data/00/aa", 1)

	reconcileUntilGone(t, r, c)
	if f.Bucket(ownBucket) != nil {
		t.Error("the bucket survived")
	}
	if len(ownKeys(f)) != 0 {
		t.Error("the key survived")
	}
}

func TestDeleteInBucketModeKeepsABucketHoldingOtherFiles(t *testing.T) {
	f := newFakeB2(t)
	r, c := newHarness(t, f, bucketRepo(func(r *b2backupv1.Repository) {
		r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
		r.Spec.Path = "repo"
	}))
	provisionForDeletion(t, r, c)
	bucket := f.Bucket(ownBucket)
	f.PutFile(bucket.BucketID, "repo/config", 1)
	f.PutFile(bucket.BucketID, notesFile, 1)

	reconcileUntilGone(t, r, c)
	if f.Bucket(ownBucket) == nil {
		t.Fatal("a bucket holding files outside the repository was deleted")
	}
	if got := f.FileNames(bucket.BucketID); !slices.Equal(got, []string{notesFile}) {
		t.Errorf("remaining files = %v, want only notes.txt", got)
	}
	if !hasEvent(recordedEvents(t, r), "BucketRetained") {
		t.Error("no BucketRetained warning")
	}
}

func TestDeleteWithoutCredentialsKeepsTheFinalizer(t *testing.T) {
	f := newFakeB2(t)
	r, c := newHarness(t, f, bucketRepo(func(r *b2backupv1.Repository) {
		r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
	}))
	provisionForDeletion(t, r, c)
	r.DefaultCredentialsSecret.Name = missingSecret

	for range 3 {
		if err := reconcileErr(r); err == nil {
			t.Fatal("finalize succeeded without credentials")
		}
	}
	if !slices.Contains(getRepo(t, c).Finalizers, FinalizerName) {
		t.Error("the finalizer was released with the data still in B2")
	}
}

func TestAFailedDeleteKeepsTheFinalizerUntilForced(t *testing.T) {
	f := newFakeB2(t)
	r, c := newHarness(t, f, bucketRepo(func(r *b2backupv1.Repository) {
		r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
	}))
	provisionForDeletion(t, r, c)
	f.Fail("b2_list_file_versions", b2fake.Failure{Status: 401, Code: b2.CodeUnauthorized, Times: 100})

	if err := reconcileErr(r); err == nil {
		t.Fatal("finalize succeeded with B2 refusing")
	}
	if !slices.Contains(getRepo(t, c).Finalizers, FinalizerName) {
		t.Fatal("the finalizer was released after a failed delete")
	}
	if !hasEvent(recordedEvents(t, r), "DeleteFailed") {
		t.Error("no DeleteFailed event")
	}

	repo := getRepo(t, c)
	repo.Annotations = map[string]string{b2backupv1.AnnotationForceFinalize: "true"}
	if err := c.Update(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	reconcileUntilGone(t, r, c)
	if !hasEvent(recordedEvents(t, r), "RepositoryOrphaned") {
		t.Error("no RepositoryOrphaned event")
	}
}

func TestTheLoserOfAConflictNeverDeletesTheWinnersData(t *testing.T) {
	f := newFakeB2(t)
	bucket := f.AddBucket(sharedBucket)
	f.PutFile(bucket.BucketID, "restic/myapp/config", 1)
	incumbent := prefixRepo(func(r *b2backupv1.Repository) {
		r.Name, r.UID = "incumbent", "uid-incumbent"
		r.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
		r.Status = b2backupv1.RepositoryStatus{BucketID: bucket.BucketID, BucketName: sharedBucket, Path: myappPath}
	})
	loser := prefixRepo(func(r *b2backupv1.Repository) {
		r.CreationTimestamp = metav1.NewTime(time.Now())
		r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
	})
	r, c := newHarness(t, f, incumbent, loser)
	reconcileN(t, r, 2)

	repo := getRepo(t, c)
	if err := c.Delete(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	reconcileUntilGone(t, r, c)
	if got := f.FileNames(bucket.BucketID); len(got) != 1 {
		t.Errorf("deleting the loser destroyed the incumbent's data: %v", got)
	}
}

func TestARepositoryThatRecordedItsPathWinsAConflict(t *testing.T) {
	f := newFakeB2(t)
	bucket := f.AddBucket(sharedBucket)
	tie := metav1.NewTime(time.Now().Truncate(time.Second))
	recorded := prefixRepo(func(r *b2backupv1.Repository) {
		r.CreationTimestamp = tie
		r.Status = b2backupv1.RepositoryStatus{BucketID: bucket.BucketID, BucketName: sharedBucket, Path: myappPath}
	})
	rival := prefixRepo(func(r *b2backupv1.Repository) {
		r.Name, r.UID = "aaa", "uid-aaa"
		r.CreationTimestamp = tie
	})
	r, c := newHarness(t, f, recorded, rival)
	reconcileN(t, r, 1)
	if cond := condition(t, c, b2backupv1.RepositoryConditionReady); cond.Reason == reasonConflict {
		t.Error("a Repository already holding the path lost it to a newcomer that sorts first by name")
	}
}

func TestAPathBeingDeletedBlocksANewcomer(t *testing.T) {
	f := newFakeB2(t)
	bucket := f.AddBucket(sharedBucket)
	f.PutFile(bucket.BucketID, myappPath+"/config", 1)
	leaving := prefixRepo(func(r *b2backupv1.Repository) {
		r.Name, r.UID = "leaving", "uid-leaving"
		r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
		r.DeletionTimestamp = ptr.To(metav1.Now())
		r.Finalizers = []string{FinalizerName}
		r.Status = b2backupv1.RepositoryStatus{BucketID: bucket.BucketID, BucketName: sharedBucket, Path: myappPath}
	})
	r, c := newHarness(t, f, leaving, prefixRepo(usePasswordSeed), seedSecretFixture())
	reconcileN(t, r, 1)

	cond := condition(t, c, b2backupv1.RepositoryConditionReady)
	if cond.Reason != reasonConflict || !strings.Contains(cond.Message, "which is deleting it") {
		t.Errorf("Ready = %+v, want a conflict naming the deletion", cond)
	}
	if getRepo(t, c).Status.Adopted {
		t.Error("a path was adopted while another Repository was deleting it")
	}
}

func TestDeleteLeavesDataAnotherRepositoryStillUses(t *testing.T) {
	f := newFakeB2(t)
	bucket := f.AddBucket(sharedBucket)
	f.PutFile(bucket.BucketID, myappPath+"/config", 1)
	other := prefixRepo(func(r *b2backupv1.Repository) {
		r.Name, r.UID = "other", "uid-other"
		r.Status = b2backupv1.RepositoryStatus{BucketID: bucket.BucketID, BucketName: sharedBucket, Path: myappPath}
	})
	leaving := prefixRepo(func(r *b2backupv1.Repository) {
		r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
		r.DeletionTimestamp = ptr.To(metav1.Now())
		r.Finalizers = []string{FinalizerName}
		r.Status = b2backupv1.RepositoryStatus{
			BucketID: bucket.BucketID, BucketName: sharedBucket, Path: myappPath, ApplicationKeyID: "key-gone",
			Initialized: true, InitJobUID: "uid-init-job",
		}
	})
	r, c := newHarness(t, f, other, leaving)
	reconcileUntilGone(t, r, c)

	if got := f.FileNames(bucket.BucketID); len(got) != 1 {
		t.Errorf("deleting one Repository destroyed data another still uses: %v", got)
	}
	if !hasEvent(recordedEvents(t, r), "DataRetained") {
		t.Error("no DataRetained warning")
	}
}

func TestForceFinalizeKeepsTheOnlyPasswordCopy(t *testing.T) {
	f := newFakeB2(t)
	r, c := newHarness(t, f, bucketRepo(func(r *b2backupv1.Repository) {
		r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
		r.Annotations = map[string]string{b2backupv1.AnnotationForceFinalize: "true"}
	}))
	provisionForDeletion(t, r, c)
	if metav1.GetControllerOf(getSecret(t, c, credsSecretName)) == nil {
		t.Fatal("precondition: the Delete policy should own the Secret")
	}
	f.Fail("b2_list_file_versions", b2fake.Failure{Status: 401, Code: b2.CodeUnauthorized, Times: 100})

	reconcileUntilGone(t, r, c)
	if owner := metav1.GetControllerOf(getSecret(t, c, credsSecretName)); owner != nil {
		t.Error("a forced release left the Secret owned, so garbage collection deletes the only password")
	}
}

func TestADuplicateNameFromOurOwnRetriedCreateAdoptsTheBucket(t *testing.T) {
	f := newFakeB2(t)
	f.AddBucket(ownBucket)
	r, c := newHarness(t, f, bucketRepo(nil))
	f.Fail("b2_list_buckets", b2fake.Failure{Status: 400, Code: b2.CodeBadBucketID, Message: "not found"})
	reconcileN(t, r, 1)
	if f.CountCalls("b2_create_bucket") != 1 {
		t.Fatal("precondition: the create should have run and hit the duplicate name")
	}
	if getRepo(t, c).Status.BucketID != f.Bucket(ownBucket).BucketID {
		t.Error("the bucket this account already holds was not picked up")
	}
}

func TestLifecycleUpdatesAreBounded(t *testing.T) {
	f := newFakeB2(t)
	f.AddBucket(sharedBucket)
	f.Fail("b2_update_bucket", b2fake.Failure{Status: 409, Code: b2.CodeConflict, Times: 10})
	r, _ := newHarness(t, f, prefixRepo(nil))
	if err := reconcileErr(r); err == nil {
		t.Fatal("reconcile succeeded with every update conflicting")
	}
	if n := f.CountCalls("b2_update_bucket"); n != maxLifecycleUpdates {
		t.Errorf("updated %d times, want at most %d", n, maxLifecycleUpdates)
	}
}

func TestTheRepositoryItsOwnInitCreatedIsNotAdopted(t *testing.T) {
	f := newFakeB2(t)
	r, c := newHarness(t, f, bucketRepo(nil))
	reconcileN(t, r, 1)
	f.PutFile(f.Bucket(ownBucket).BucketID, "config", 1)
	succeedInitJob(t, c)
	reconcileN(t, r, 2)
	repo := getRepo(t, c)
	if !repo.Status.Initialized {
		t.Fatal("precondition: initialized")
	}
	if repo.Status.Adopted || hasEvent(recordedEvents(t, r), "RepositoryAdopted") {
		t.Error("the config this Repository's own restic init wrote was taken for an existing repository")
	}
}
