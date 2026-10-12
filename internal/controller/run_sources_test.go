package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

const (
	tagFiles     = "files"
	sourcesMount = "/srv"
	jobA         = "restic-manual-a"

	filesSummary = `files {"backup_start":"2026-10-12T04:21:22.770054578Z","backup_end":"2026-10-12T04:21:23.540711822Z",` +
		`"files_new":1,"files_changed":1,"files_unmodified":1,"data_added":101393,"data_added_packed":100890,` +
		`"total_files_processed":3,"total_bytes_processed":400008}`
	dbSummary = `db {"backup_start":"2026-10-12T04:21:25.067656787Z","backup_end":"2026-10-12T04:21:25.805864904Z",` +
		`"files_new":1,"files_changed":0,"files_unmodified":0,"data_added":280,"data_added_packed":248,` +
		`"total_files_processed":1,"total_bytes_processed":8}`
)

func finishedBackupJob(t *testing.T, c client.Client, sb *b2backupv1.ScheduledBackup) *batchv1.Job {
	t.Helper()
	name, uid := jobA, types.UID("uid-job-a")
	start := metav1.NewTime(time.Now().Add(-time.Minute))
	job := &batchv1.Job{
		Namespace: testNS, Name: name, UID: uid,
		Labels: map[string]string{labelManagedBy: managedByValue, backup.LabelTrigger: backup.TriggerManual},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "b2backup.mcswain.dev/v1", Kind: "ScheduledBackup",
			Name: sb.Name, UID: sb.UID, Controller: ptr.To(true)}},
	}
	if err := c.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	job.Status = batchv1.JobStatus{Succeeded: 1, StartTime: &start, CompletionTime: ptr.To(metav1.Now())}
	if err := c.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	return job
}

func backupPod(t *testing.T, c client.Client, job *batchv1.Job, container, message string) {
	t.Helper()
	pod := &corev1.Pod{
		Namespace: testNS, Name: job.Name + "-pod",
		Labels: map[string]string{batchv1.ControllerUidLabel: string(job.UID), batchv1.JobNameLabel: job.Name},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job", Name: job.Name,
			UID: job.UID, Controller: ptr.To(true)}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: container, Image: "restic"}}},
	}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	pod.Status = corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{
		Name: container, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: message}},
	}}}
	if err := c.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

func sourcesBackup() *b2backupv1.ScheduledBackup {
	sb := scheduledBackup()
	sb.UID = "uid-sb"
	sb.Spec.Sources = []b2backupv1.BackupSource{
		{Type: b2backupv1.SourceTypeFiles, Paths: []string{sourcesMount}},
		{Type: b2backupv1.SourceTypeCNPG},
	}
	sb.Spec.Volumes = []b2backupv1.VolumeSpec{{ExistingClaim: "data", MountPath: sourcesMount}}
	return sb
}

func historyEntry(t *testing.T, c client.Client, job string) b2backupv1.BackupRun {
	t.Helper()
	for _, run := range loadBackup(t, c).Status.History {
		if run.JobName == job {
			return run
		}
	}
	t.Fatalf("no history entry for %s", job)
	return b2backupv1.BackupRun{}
}

func TestHistoryRecordsRunSourcesFromTheTerminationMessage(t *testing.T) {
	sb := sourcesBackup()
	r, c := backupHarness(t, initializedRepo(), sb)
	job := finishedBackupJob(t, c, sb)
	backupPod(t, c, job, backup.ContainerName, filesSummary+"\n"+dbSummary+"\n")
	reconcileBackup(t, r)

	got := historyEntry(t, c, job.Name).Sources
	if len(got) != 2 {
		t.Fatalf("sources = %+v, want files and db", got)
	}
	files := got[0]
	if files.Tag != tagFiles || files.FilesNew != 1 || files.FilesChanged != 1 || files.FilesUnmodified != 1 ||
		files.ProcessedFiles != 3 || files.ProcessedBytes != 400008 || files.AddedBytes != 101393 ||
		files.StoredBytes != 100890 {
		t.Errorf("files source = %+v", files)
	}
	if files.StartTime == nil || files.CompletionTime == nil ||
		!files.StartTime.Equal(ptr.To(metav1.NewTime(time.Date(2026, 10, 12, 4, 21, 22, 0, time.UTC)))) ||
		!files.CompletionTime.Equal(ptr.To(metav1.NewTime(time.Date(2026, 10, 12, 4, 21, 23, 0, time.UTC)))) {
		t.Errorf("files source times = %v to %v", files.StartTime, files.CompletionTime)
	}
	if got[1].Tag != "db" || got[1].AddedBytes != 280 || got[1].StoredBytes != 248 {
		t.Errorf("db source = %+v", got[1])
	}
}

func TestHistoryKeepsRunSourcesOnceThePodIsGone(t *testing.T) {
	sb := sourcesBackup()
	r, c := backupHarness(t, initializedRepo(), sb)
	job := finishedBackupJob(t, c, sb)
	backupPod(t, c, job, backup.ContainerName, filesSummary)
	reconcileBackup(t, r)

	if err := c.Delete(context.Background(), &corev1.Pod{Namespace: testNS, Name: job.Name + "-pod"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, r)

	if got := historyEntry(t, c, job.Name).Sources; len(got) != 1 || got[0].AddedBytes != 101393 {
		t.Errorf("sources after the pod went = %+v, want the recorded files source", got)
	}
}

func TestHistoryToleratesUnreadableTerminationMessages(t *testing.T) {
	long := filesSummary + "\n" + strings.Repeat("x", 5000)
	truncated := dbSummary + "\n" + filesSummary[:len(filesSummary)/2]
	for name, tc := range map[string]struct {
		container, message string
		want               []string
	}{
		"missing":           {container: backup.ContainerName, message: ""},
		"garbage":           {container: backup.ContainerName, message: "Error: something\n{not json}\n"},
		"negative":          {container: backup.ContainerName, message: strings.Replace(filesSummary, `"data_added":101393`, `"data_added":-1`, 1)},
		"field missing":     {container: backup.ContainerName, message: strings.Replace(filesSummary, `"data_added_packed":100890,`, "", 1)},
		"bad tag":           {container: backup.ContainerName, message: "fi$les" + strings.TrimPrefix(filesSummary, tagFiles)},
		"trailing data":     {container: backup.ContainerName, message: filesSummary + ` {"x":1}`},
		"too long":          {container: backup.ContainerName, message: long, want: []string{tagFiles}},
		"cut short":         {container: backup.ContainerName, message: truncated, want: []string{"db"}},
		"another container": {container: "sidecar", message: filesSummary},
		"repeated tag":      {container: backup.ContainerName, message: filesSummary + "\n" + filesSummary, want: []string{tagFiles}},
	} {
		t.Run(name, func(t *testing.T) {
			sb := sourcesBackup()
			r, c := backupHarness(t, initializedRepo(), sb)
			job := finishedBackupJob(t, c, sb)
			backupPod(t, c, job, tc.container, tc.message)
			reconcileBackup(t, r)

			run := historyEntry(t, c, job.Name)
			if run.Result != b2backupv1.BackupRunSucceeded {
				t.Errorf("result = %q, want Succeeded whatever the message says", run.Result)
			}
			tags := make([]string, 0, len(run.Sources))
			for _, src := range run.Sources {
				tags = append(tags, src.Tag)
			}
			if strings.Join(tags, ",") != strings.Join(tc.want, ",") {
				t.Errorf("sources = %v, want %v", tags, tc.want)
			}
		})
	}
}

func TestHistoryReadsOnlyThePodsOfItsOwnJobs(t *testing.T) {
	sb := sourcesBackup()
	r, c := backupHarness(t, initializedRepo(), sb)
	job := finishedBackupJob(t, c, sb)
	stranger := &batchv1.Job{Namespace: testNS, Name: "someone-elses", UID: "uid-stranger"}
	backupPod(t, c, stranger, backup.ContainerName, filesSummary)
	reconcileBackup(t, r)

	if got := historyEntry(t, c, job.Name).Sources; len(got) != 0 {
		t.Errorf("sources = %+v, want none: the only pod belongs to another Job", got)
	}
}

func TestScriptBackupRecordsNoSources(t *testing.T) {
	sb := scheduledBackup()
	sb.UID = "uid-sb"
	sb.Spec.Sources = nil
	sb.Spec.Script = "restic backup /data\n"
	r, c := backupHarness(t, initializedRepo(), sb)
	job := finishedBackupJob(t, c, sb)
	backupPod(t, c, job, backup.ContainerName, filesSummary)
	reconcileBackup(t, r)

	if got := historyEntry(t, c, job.Name).Sources; len(got) != 0 {
		t.Errorf("sources = %+v, want none for a script backup", got)
	}
}
