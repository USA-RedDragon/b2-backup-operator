package backup

import (
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestAManualJobRecordsItsForgetScope(t *testing.T) {
	t.Parallel()
	sb := testBackup(nil)
	job, err := BuildManualJob(sb, testRepo(), testConfig(), time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ScopeFrom(job.Annotations)
	if !ok || got.Repository != containerName || got.Host != sb.EffectiveResticHost() ||
		len(got.Tags) != 1 || got.Tags[0] != "db" {
		t.Errorf("manual Job scope = %+v (%t), want the backup's own", got, ok)
	}
}

func TestFindConflictWithRunningJobs(t *testing.T) {
	t.Parallel()
	sb := testBackup(nil)
	sb.UID = "uid-self"
	own := &batchv1.CronJob{Namespace: testNS, Name: CronJobName(sb), UID: "uid-self-cj"}
	scopeDB := ScopeOf(sb).encode()
	tests := []struct {
		name     string
		job      batchv1.Job
		conflict bool
	}{
		{"its own Job under an old scope", batchv1.Job{
			Name: "own", Annotations: map[string]string{AnnotationForgetScope: `{"repository":"` + containerName +
				`","host":"` + sb.EffectiveResticHost() + `","tags":["db"]}`},
			OwnerReferences: []metav1.OwnerReference{{Kind: "CronJob", Name: own.Name, UID: own.UID, Controller: ptr.To(true)}},
		}, false},
		{"an ownerless Job sharing the tag", batchv1.Job{
			Name: "stray", Annotations: map[string]string{AnnotationForgetScope: scopeDB},
		}, true},
		{"an ownerless Job with no recorded scope", batchv1.Job{Name: "old"}, true},
		{"a finished Job sharing the tag", batchv1.Job{
			Name: "done", Annotations: map[string]string{AnnotationForgetScope: scopeDB},
			Status: batchv1.JobStatus{CompletionTime: ptr.To(metav1.Now())},
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := FindConflict(sb, nil, []batchv1.CronJob{*own}, []batchv1.Job{tt.job})
			if (got != nil) != tt.conflict {
				t.Errorf("FindConflict() = %+v, want conflict %t", got, tt.conflict)
			}
		})
	}
}
