package cli_test

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli"
)

const sharedName = "stage1"

var hintCommand = regexp.MustCompile("b2bk (?:exec|logs|status|resume|wait|cancel|backup|doctor) ([^ `\n,;]+)")

func TestHintsNameABackupThatSharesItsRepositorysName(t *testing.T) {
	t.Parallel()
	sb := pvcScheduledBackup()
	sb.Name = sharedName
	sb.Spec.RepositoryRef.Name = sharedName
	sb.Spec.Volumes, sb.Spec.Sources = nil, []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG}}
	sb.Annotations = map[string]string{b2backupv1.AnnotationRestorePartial: `["pvc/app-data:/"]`}
	start := metav1.NewTime(time.Now().Add(-time.Hour))
	sb.Status.History = []b2backupv1.BackupRun{{
		JobName: sharedName + "-1", Trigger: b2backupv1.BackupTriggerScheduled,
		Result: b2backupv1.BackupRunFailed, StartTime: &start,
	}}
	repo := &b2backupv1.Repository{Namespace: pvcNS, Name: sharedName}
	c := pvcClient(t, sb, repo, &corev1.Secret{Namespace: pvcNS, Name: repo.SecretName()})
	current := &corev1.Secret{Namespace: pvcNS, Name: repo.SecretName()}
	temp := &corev1.Secret{Namespace: pvcNS, Name: sharedName + "-b2bk-newkey-abcde"}
	conflict := apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, current.Name, errors.New("changed"))

	var doctor bytes.Buffer
	_ = cli.Doctor(context.Background(), c, &doctor, pvcNS, "sb/"+sharedName)
	rejected, _ := cli.RotateWriteFailure(sb, current, temp, conflict)
	unknown, _ := cli.RotateWriteFailure(sb, current, temp, errors.New("connection reset"))

	cases := []struct {
		name string
		text string
	}{
		{name: "rotate-password, done", text: strings.Join(cli.KeyCommands(sb), "\n")},
		{name: "rotate-password, the write was rejected", text: rejected.Error()},
		{name: "rotate-password, the write may have gone through", text: unknown.Error()},
		{name: cmdDoctor, text: doctor.String()},
		{name: "a partial restore hold", text: backup.RestoreHold(sb)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hints := hintCommand.FindAllStringSubmatch(tc.text, -1)
			if len(hints) == 0 {
				t.Fatalf("no b2bk hint in %q", tc.text)
			}
			for _, h := range hints {
				if _, err := cli.Resolve(context.Background(), c, pvcNS, h[1]); err != nil {
					t.Errorf("hint %q does not resolve: %v", h[0], err)
				}
			}
		})
	}
}
