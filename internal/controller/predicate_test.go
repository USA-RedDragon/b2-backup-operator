package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

func repoWith(mutate func(*b2backupv1.Repository)) *b2backupv1.Repository {
	r := &b2backupv1.Repository{
		Name: resticName, Namespace: testNS, Generation: 1,
	}
	mutate(r)
	return r
}

func TestIgnoreOwnStatusWrites(t *testing.T) {
	now := metav1.Now()
	tests := []struct {
		name     string
		old, new *b2backupv1.Repository
		want     bool
	}{
		{
			name: "status-only change is filtered",
			old:  repoWith(func(r *b2backupv1.Repository) {}),
			new:  repoWith(func(r *b2backupv1.Repository) { r.Status.Initialized = true }),
			want: false,
		},
		{
			name: "spec change passes",
			old:  repoWith(func(r *b2backupv1.Repository) {}),
			new:  repoWith(func(r *b2backupv1.Repository) { r.Generation = 2 }),
			want: true,
		},
		{
			name: "deletion always passes",
			old:  repoWith(func(r *b2backupv1.Repository) {}),
			new: repoWith(func(r *b2backupv1.Repository) {
				r.DeletionTimestamp = &now
				r.Finalizers = []string{FinalizerName}
			}),
			want: true,
		},
		{
			name: "finalizer change passes",
			old:  repoWith(func(r *b2backupv1.Repository) {}),
			new:  repoWith(func(r *b2backupv1.Repository) { r.Finalizers = []string{FinalizerName} }),
			want: true,
		},
	}

	p := ignoreOwnStatusWrites()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.Update(event.UpdateEvent{
				ObjectOld: client.Object(tt.old),
				ObjectNew: client.Object(tt.new),
			})
			if got != tt.want {
				t.Errorf("Update() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIgnoreOwnStatusWritesPassesCreateAndDelete(t *testing.T) {
	p := ignoreOwnStatusWrites()
	obj := repoWith(func(r *b2backupv1.Repository) {})
	if !p.Create(event.CreateEvent{Object: obj}) {
		t.Error("create events must not be filtered")
	}
	if !p.Delete(event.DeleteEvent{Object: obj}) {
		t.Error("delete events must not be filtered")
	}
}

func TestIgnoreOwnStatusWritesPassesWatchedAnnotations(t *testing.T) {
	annotated := func(v string) *b2backupv1.Repository {
		return repoWith(func(r *b2backupv1.Repository) {
			if v != "" {
				r.Annotations = map[string]string{b2backupv1.AnnotationTriggerAt: v}
			}
		})
	}

	tests := []struct {
		name     string
		old, new *b2backupv1.Repository
		watched  []string
		want     bool
	}{
		{
			name:    "watched annotation added passes",
			old:     annotated(""),
			new:     annotated("2026-09-03T14:12:00Z"),
			watched: []string{b2backupv1.AnnotationTriggerAt},
			want:    true,
		},
		{
			name:    "watched annotation changed passes",
			old:     annotated("2026-09-03T14:12:00Z"),
			new:     annotated("2026-09-03T15:30:00Z"),
			watched: []string{b2backupv1.AnnotationTriggerAt},
			want:    true,
		},
		{
			name:    "unchanged annotation is still filtered",
			old:     annotated("2026-09-03T14:12:00Z"),
			new:     annotated("2026-09-03T14:12:00Z"),
			watched: []string{b2backupv1.AnnotationTriggerAt},
			want:    false,
		},
		{
			name:    "unwatched annotation is filtered",
			old:     annotated(""),
			new:     annotated("2026-09-03T14:12:00Z"),
			watched: nil,
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := ignoreOwnStatusWrites(tt.watched...)
			got := p.Update(event.UpdateEvent{
				ObjectOld: client.Object(tt.old),
				ObjectNew: client.Object(tt.new),
			})
			if got != tt.want {
				t.Errorf("Update() = %t, want %t", got, tt.want)
			}
		})
	}
}
