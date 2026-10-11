package backup

import (
	"encoding/json"
	"fmt"
	"slices"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

const (
	// AnnotationForgetScope records on a backup Job, and on the CronJob template
	// it comes from, the Repository, host and tags its forget reaches, as JSON.
	// A Job keeps the scope it started with after its ScheduledBackup changes.
	AnnotationForgetScope = "b2backup.mcswain.dev/forget-scope"
)

// ForgetScope is what a backup's forget can reach.
type ForgetScope struct {
	Repository string   `json:"repository"`
	Host       string   `json:"host"`
	Tags       []string `json:"tags,omitempty"`
	Script     bool     `json:"script,omitempty"`
}

// ScopeOf returns the forget scope the ScheduledBackup's spec renders.
func ScopeOf(sb *b2backupv1.ScheduledBackup) ForgetScope {
	return ForgetScope{
		Repository: sb.Spec.RepositoryRef.Name,
		Host:       sb.EffectiveResticHost(),
		Tags:       SourceTags(sb.Spec.Sources),
		Script:     sb.Spec.Script != "",
	}
}

func (s ForgetScope) encode() string {
	raw, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(raw)
}

// ScopeFrom reads AnnotationForgetScope from an object's annotations.
func ScopeFrom(annotations map[string]string) (ForgetScope, bool) {
	raw, ok := annotations[AnnotationForgetScope]
	if !ok {
		return ForgetScope{}, false
	}
	var s ForgetScope
	if err := json.Unmarshal([]byte(raw), &s); err != nil || s.Repository == "" {
		return ForgetScope{}, false
	}
	return s, true
}

func overlap(own, other ForgetScope) string {
	switch {
	case own.Repository != other.Repository:
		return ""
	case own.Script:
		return "any tag or host, which this script's restic commands may forget"
	case other.Script:
		return "any tag or host, which its script's restic commands may forget"
	case own.Host != other.Host:
		return ""
	}
	for _, tag := range other.Tags {
		if slices.Contains(own.Tags, tag) {
			return fmt.Sprintf("%q", tag)
		}
	}
	return ""
}

const unknownScope = "a scope it does not record, since an older operator rendered it and no ScheduledBackup owns it"

// Conflict is something else whose forget would reach a backup's snapshots, or
// the other way round.
type Conflict struct {
	With  string
	Under string

	// CronJob is set when the other side is a CronJob no live ScheduledBackup
	// controls: nothing else would ever suspend it.
	CronJob *batchv1.CronJob
}

// FindConflict looks for anything that writes into the backup's Repository with
// a forget that would reach its snapshots, or the other way round: another
// ScheduledBackup's spec, a CronJob that is not suspended and was rendered from
// another spec or outlived its ScheduledBackup, and a backup Job still running
// under the scope it started with. backups, cronJobs and jobs are everything of
// the kind in the namespace; the operator's own CronJobs and Jobs carry
// AnnotationForgetScope, and one that does not and that no live ScheduledBackup
// owns has an unknown scope, which conflicts with everything.
func FindConflict(
	sb *b2backupv1.ScheduledBackup,
	backups []b2backupv1.ScheduledBackup,
	cronJobs []batchv1.CronJob,
	jobs []batchv1.Job,
) *Conflict {
	own := ScopeOf(sb)
	live := map[types.UID]bool{}
	for i := range backups {
		other := &backups[i]
		if other.Name == sb.Name || !other.DeletionTimestamp.IsZero() {
			continue
		}
		live[other.UID] = true
		if under := overlap(own, ScopeOf(other)); under != "" {
			return &Conflict{With: "ScheduledBackup " + other.Name, Under: under}
		}
	}

	ownCronJobs := map[types.UID]bool{}
	liveCronJobs := map[types.UID]bool{}
	for i := range cronJobs {
		cj := &cronJobs[i]
		owner := metav1.GetControllerOf(cj)
		if cj.Name == CronJobName(sb) {
			ownCronJobs[cj.UID] = true
			continue
		}
		ownedByLive := owner != nil && live[owner.UID]
		if ownedByLive {
			liveCronJobs[cj.UID] = true
		}
		if cj.Spec.Suspend != nil && *cj.Spec.Suspend {
			continue
		}
		scope, ok := ScopeFrom(cj.Spec.JobTemplate.Annotations)
		under := ""
		switch {
		case ok:
			under = overlap(own, scope)
		case !ownedByLive:
			under = unknownScope
		}
		if under == "" {
			continue
		}
		c := &Conflict{With: "CronJob " + cj.Name, Under: under}
		if !ownedByLive {
			c.CronJob = cj
		}
		return c
	}

	for i := range jobs {
		job := &jobs[i]
		owner := metav1.GetControllerOf(job)
		if JobFinished(job) || (owner != nil && (owner.UID == sb.UID || ownCronJobs[owner.UID])) {
			continue
		}
		scope, ok := ScopeFrom(job.Annotations)
		under := ""
		switch {
		case ok:
			under = overlap(own, scope)
		case owner == nil || (!live[owner.UID] && !liveCronJobs[owner.UID]):
			under = unknownScope
		}
		if under != "" {
			return &Conflict{With: "running Job " + job.Name, Under: under}
		}
	}
	return nil
}

// JobFinished reports whether a Job has completed or failed.
func JobFinished(job *batchv1.Job) bool {
	if job.Status.CompletionTime != nil {
		return true
	}
	for _, cond := range job.Status.Conditions {
		if (cond.Type == batchv1.JobComplete || cond.Type == batchv1.JobFailed) && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
