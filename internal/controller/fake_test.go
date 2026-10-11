package controller

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/b2"
	"github.com/USA-RedDragon/b2-backup-operator/internal/b2/b2fake"
)

const (
	testNS     = "myapp-prod"
	resticName = "restic"
	seedSecret = "restic-envs"
	credsNS    = "b2backup-system"
	credsName  = "b2-credentials"

	masterKeyID = "0055aa000000000000000000aa"
	masterKey   = "K005masterkeysecret"

	sharedBucket = "acme-shared"
	ownBucket    = "acme-myapp"

	credsSecretName = resticName + "-credentials"
	teamSecret      = "team-b2"
	missingSecret   = "gone"
	myappPath       = "restic/myapp"

	wantInitPoll = 15 * time.Second
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := b2backupv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newFakeB2(t *testing.T) *b2fake.Server {
	t.Helper()
	f := b2fake.New(masterKeyID, masterKey)
	f.Logf = t.Logf
	return f
}

func newHarness(t *testing.T, f *b2fake.Server, objs ...client.Object) (*RepositoryReconciler, client.Client) {
	t.Helper()
	return newHarnessWithInterceptor(t, f, interceptor.Funcs{}, objs...)
}

func newHarnessWithInterceptor(
	t *testing.T, f *b2fake.Server, funcs interceptor.Funcs, objs ...client.Object,
) (*RepositoryReconciler, client.Client) {
	t.Helper()
	scheme := testScheme(t)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	all := append([]client.Object{&corev1.Secret{
		Name: credsName, Namespace: credsNS,
		Data: map[string][]byte{
			b2backupv1.CredentialsKeyID: []byte(masterKeyID),
			b2backupv1.CredentialsKey:   []byte(masterKey),
		},
	}}, objs...)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(all...).
		WithStatusSubresource(&b2backupv1.Repository{}).
		WithInterceptorFuncs(funcs).
		Build()

	return &RepositoryReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(100),
		NewAPI: func(keyID, key string) b2.API {
			cl := b2.NewClient(keyID, key)
			cl.AuthURL = srv.URL
			cl.Sleep = func(context.Context, time.Duration) error { return nil }
			return cl
		},
		DefaultCredentialsSecret: types.NamespacedName{Namespace: credsNS, Name: credsName},
		BackupImage:              "ghcr.io/usa-reddragon/b2-backup-operator-restic:test",
	}, c
}
