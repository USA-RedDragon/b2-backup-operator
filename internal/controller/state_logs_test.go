package controller_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/b2"
	"github.com/USA-RedDragon/b2-backup-operator/internal/b2/b2fake"
	"github.com/USA-RedDragon/b2-backup-operator/internal/controller"
)

const (
	logNS       = "media"
	logRepo     = "media"
	logBucket   = "acme-media-backups"
	logCredsNS  = "b2backup-system"
	logCreds    = "b2-credentials"
	logMasterID = "0055aa000000000000000000aa"
	logMaster   = "K005masterkeysecret"
	logPath     = "restic/media"
)

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (s *logSink) add(prefix, args string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, prefix+" "+args)
}

func (s *logSink) all() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.lines, "\n")
}

func TestRepositoryStateChangesAreLogged(t *testing.T) {
	t.Parallel()
	bucketKey := `"bucket"="` + logBucket + `"`
	cases := []struct {
		mode b2backupv1.StorageMode
		msg  string
		keys []string
	}{
		{mode: b2backupv1.StorageModeBucket, msg: "Created bucket",
			keys: []string{bucketKey, `"bucketID"=`, `"daysFromHidingToDeleting"=30`}},
		{mode: b2backupv1.StorageModeBucket, msg: "Created application key", keys: []string{`"keyID"=`, bucketKey}},
		{mode: b2backupv1.StorageModeBucket, msg: "Generated restic password",
			keys: []string{`"secret"="` + logRepo + `-credentials"`}},
		{mode: b2backupv1.StorageModeBucket, msg: "Started init Job", keys: []string{`"job"="` + logRepo + `-init"`}},
		{mode: b2backupv1.StorageModeBucket, msg: "Opened restic repository",
			keys: []string{`"job"="` + logRepo + `-init"`}},
		{mode: b2backupv1.StorageModeBucket, msg: "Retained repository data and deleted its application key",
			keys: []string{bucketKey, `"keyID"=`, `"secret"="` + logRepo + `-credentials"`, `"credentials"="kept`}},
		{mode: b2backupv1.StorageModePrefix, msg: "Updated lifecycle rule",
			keys: []string{bucketKey, `"prefix"="` + logPath + `/"`, `"daysFromHidingToDeleting"=30`}},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode)+"/"+tc.msg, func(t *testing.T) {
			t.Parallel()
			sink, secret := provisionAndRetain(t, tc.mode)
			logs := sink.all()
			line := ""
			for l := range strings.Lines(logs) {
				if strings.Contains(l, `"msg"="`+tc.msg+`"`) {
					line = l
				}
			}
			if line == "" {
				t.Fatalf("no %q log line in:\n%s", tc.msg, logs)
			}
			for _, k := range tc.keys {
				if !strings.Contains(line, k) {
					t.Errorf("%q log line lacks %s: %s", tc.msg, k, line)
				}
			}
			for _, key := range []string{controller.KeyResticPassword, controller.KeySecretAccessKey} {
				if v := string(secret.Data[key]); v == "" || strings.Contains(logs, v) {
					t.Errorf("%s is empty or in the logs", key)
				}
			}
			if strings.Contains(logs, logMaster) {
				t.Error("the master application key is in the logs")
			}
		})
	}
}

func provisionAndRetain(t *testing.T, mode b2backupv1.StorageMode) (*logSink, *corev1.Secret) {
	t.Helper()
	sink := &logSink{}
	ctx := log.IntoContext(context.Background(), funcr.New(sink.add, funcr.Options{}))

	f := b2fake.New(logMasterID, logMaster)
	path := ""
	if mode == b2backupv1.StorageModePrefix {
		f.AddBucket(logBucket)
		path = logPath
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := b2backupv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	repo := &b2backupv1.Repository{
		Namespace: logNS, Name: logRepo, UID: "uid-media-repo",
		Spec: b2backupv1.RepositorySpec{
			Mode: mode, Bucket: logBucket, Path: path, DeletionPolicy: b2backupv1.DeletionPolicyRetain,
		},
	}
	creds := &corev1.Secret{
		Namespace: logCredsNS, Name: logCreds,
		Data: map[string][]byte{
			b2backupv1.CredentialsKeyID: []byte(logMasterID),
			b2backupv1.CredentialsKey:   []byte(logMaster),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repo, creds).
		WithStatusSubresource(&b2backupv1.Repository{}, &batchv1.Job{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetUID() == "" {
					obj.SetUID(uuid.NewUUID())
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
	r := &controller.RepositoryReconciler{
		Client: c, APIReader: c, Scheme: scheme, Recorder: events.NewFakeRecorder(100),
		NewAPI: func(keyID, key string) b2.API {
			cl := b2.NewClient(keyID, key)
			cl.AuthURL = srv.URL
			cl.Sleep = func(context.Context, time.Duration) error { return nil }
			return cl
		},
		DefaultCredentialsSecret: types.NamespacedName{Namespace: logCredsNS, Name: logCreds},
		BackupImage:              "ghcr.io/usa-reddragon/restic:test",
	}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(repo)}
	reconcile := func(n int) {
		for range n {
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
		}
	}

	reconcile(3)
	var job batchv1.Job
	if err := c.Get(ctx, types.NamespacedName{Namespace: logNS, Name: logRepo + "-init"}, &job); err != nil {
		t.Fatalf("no init Job: %v\n%s", err, sink.all())
	}
	job.Status.Succeeded = 1
	if err := c.Status().Update(ctx, &job); err != nil {
		t.Fatal(err)
	}
	reconcile(2)

	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: logNS, Name: logRepo + "-credentials"}, &secret); err != nil {
		t.Fatal(err)
	}
	var current b2backupv1.Repository
	if err := c.Get(ctx, req.NamespacedName, &current); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, &current); err != nil {
		t.Fatal(err)
	}
	reconcile(1)
	return sink, &secret
}

func TestScheduledBackupStateChangesAreLogged(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		trigger bool
		msg     string
		key     string
	}{
		{name: "the CronJob", msg: "Created CronJob", key: `"cronJob"="` + lsName + `-backup"`},
		{name: "a triggered backup", trigger: true, msg: "Started triggered backup Job", key: `"job"="` + lsName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sb := &b2backupv1.ScheduledBackup{
				Namespace: lsNS, Name: lsName, UID: lsSBUID,
				Spec: b2backupv1.ScheduledBackupSpec{
					RepositoryRef: corev1.LocalObjectReference{Name: lsRepo},
					Schedule:      "@hourly",
					Sources:       []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG}},
				},
			}
			if tc.trigger {
				sb.Annotations = map[string]string{b2backupv1.AnnotationTriggerAt: time.Now().UTC().Format(time.RFC3339)}
			}
			repo := &b2backupv1.Repository{
				Namespace: lsNS, Name: lsRepo,
				Status: b2backupv1.RepositoryStatus{Initialized: true, BucketID: "b1", BucketName: "acme"},
			}
			r, _ := lastSuccessfulHarness(t, sb, repo)
			sink := &logSink{}
			ctx := log.IntoContext(context.Background(), funcr.New(sink.add, funcr.Options{}))
			for range 2 {
				if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sb)}); err != nil {
					t.Fatal(err)
				}
			}
			logs := sink.all()
			found := false
			for l := range strings.Lines(logs) {
				found = found || strings.Contains(l, `"msg"="`+tc.msg+`"`) && strings.Contains(l, tc.key)
			}
			if !found {
				t.Errorf("no %q log line with %s in:\n%s", tc.msg, tc.key, logs)
			}
		})
	}
}
