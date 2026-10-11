package controller

import (
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// ignoreOwnStatusWrites drops update events that changed nothing but status.
//
// A status write re-triggers the controller, and that pass reads an informer
// cache that has not caught up with the write, so it re-observes work it has
// already done and duplicates the events reporting it.
//
// Deletions and finalizer changes are let through so this cannot strand an
// object mid-deletion, and watchAnnotations are let through because annotations
// do not advance the generation: a request carried by one would otherwise be
// filtered out and never acted on.
func ignoreOwnStatusWrites(watchAnnotations ...string) predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return true
			}

			if !e.ObjectNew.GetDeletionTimestamp().IsZero() {
				return true
			}
			if !equality.Semantic.DeepEqual(e.ObjectOld.GetFinalizers(), e.ObjectNew.GetFinalizers()) {
				return true
			}
			for _, key := range watchAnnotations {
				if e.ObjectOld.GetAnnotations()[key] != e.ObjectNew.GetAnnotations()[key] {
					return true
				}
			}

			return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration()
		},
	}
}
