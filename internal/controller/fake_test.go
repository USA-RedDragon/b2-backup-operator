package controller

import (
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
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
	resticPrefix    = "restic/"
	myappPrefix     = myappPath + "/"
	otherBucket     = "acme-other"
	notesFile       = "notes.txt"
	relativeDir     = "data"

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

	raw := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(all...).
		WithStatusSubresource(&b2backupv1.Repository{}, &batchv1.Job{}).
		WithInterceptorFuncs(interceptor.Funcs{Create: assignUID}).
		Build()
	c := interceptor.NewClient(managerCacheView(t, raw, scheme), funcs)

	return &RepositoryReconciler{
		Client:    c,
		APIReader: raw,
		Scheme:    scheme,
		Recorder:  events.NewFakeRecorder(100),
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

// managerCacheView makes a client read Jobs and CronJobs the way the manager's
// cached client does with CacheOptions: an object the cache's label selector
// leaves out is not found and not listed.
func managerCacheView(t *testing.T, c client.WithWatch, scheme *runtime.Scheme) client.WithWatch {
	t.Helper()
	selectors := map[schema.GroupVersionKind]labels.Selector{}
	for obj, by := range CacheOptions().ByObject {
		gvk, err := apiutil.GVKForObject(obj, scheme)
		if err != nil {
			t.Fatal(err)
		}
		selectors[gvk] = by.Label
	}
	selectorFor := func(obj runtime.Object) labels.Selector {
		gvk, err := apiutil.GVKForObject(obj, scheme)
		if err != nil {
			return nil
		}
		gvk.Kind = strings.TrimSuffix(gvk.Kind, "List")
		return selectors[gvk]
	}
	return interceptor.NewClient(c, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if sel := selectorFor(obj); sel != nil && !sel.Matches(labels.Set(obj.GetLabels())) {
				gvk, _ := apiutil.GVKForObject(obj, scheme)
				return apierrors.NewNotFound(schema.GroupResource{Group: gvk.Group, Resource: gvk.Kind}, key.Name)
			}
			return nil
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			sel := selectorFor(list)
			if sel == nil {
				return nil
			}
			items, err := apimeta.ExtractList(list)
			if err != nil {
				return err
			}
			items = slices.DeleteFunc(items, func(o runtime.Object) bool {
				accessor, err := apimeta.Accessor(o)
				return err != nil || !sel.Matches(labels.Set(accessor.GetLabels()))
			})
			return apimeta.SetList(list, items)
		},
	})
}

// assignUID gives a created object a UID, as the API server does and the fake
// client does not.
func assignUID(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
	if obj.GetUID() == "" {
		obj.SetUID(uuid.NewUUID())
	}
	return c.Create(ctx, obj, opts...)
}
