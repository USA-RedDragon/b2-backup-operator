package controller

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CacheOptions returns the manager's cache options. Jobs and CronJobs are
// cached only when they carry the operator's managed-by label, so the cached
// client never sees a Job that b2bk or anyone else created. Read those through
// an uncached reader.
func CacheOptions() cache.Options {
	managedByOperator := cache.ByObject{
		Label: labels.SelectorFromSet(labels.Set{labelManagedBy: managedByValue}),
	}
	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&batchv1.Job{}:     managedByOperator,
			&batchv1.CronJob{}: managedByOperator,
		},
	}
}

// ClientCacheOptions returns the kinds the manager's client reads without a
// cache. A cached client builds an informer over every object of the type,
// not just the handful read by name. Caching Secrets holds every Helm release
// and pull secret in the cluster in memory, which is what was OOM killing the
// manager.
func ClientCacheOptions() *client.CacheOptions {
	return &client.CacheOptions{
		DisableFor: []client.Object{
			&corev1.Secret{},
			&corev1.PersistentVolumeClaim{},
		},
	}
}
