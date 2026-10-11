package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

const (
	flagYes                  = "--yes"
	reasonRepositoryConflict = "RepositoryConflict"
	targetInPlace            = "in place"
	notRendered              = "has not rendered"
	inATagConflict           = "tag conflict"
	renderedFromKey          = "b2backup.mcswain.dev/rendered-from"
	specHashKey              = "b2backup.mcswain.dev/spec-hash"
	currentGenRepo           = 2
	currentGenBackup         = 3
)

type currentFixture struct {
	sb   *b2backupv1.ScheduledBackup
	repo *b2backupv1.Repository
	cj   *batchv1.CronJob
}

func currentBackup(t *testing.T, mutate ...func(*b2backupv1.ScheduledBackup)) currentFixture {
	t.Helper()
	sb := newBackup(testBackupName, testRepoName)
	sb.Generation = currentGenBackup
	sb.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG}}
	sb.Spec.Retention = &b2backupv1.Retention{Daily: ptr.To(int32(7))}
	for _, m := range mutate {
		m(sb)
	}
	sb.Status.ObservedGeneration = currentGenBackup
	sb.Status.Conditions = []metav1.Condition{{
		Type: b2backupv1.ScheduledBackupConditionReady, Status: metav1.ConditionTrue, Reason: reasonReady,
		ObservedGeneration: currentGenBackup,
	}}
	repo := newRepo(testNS)
	repo.Generation = currentGenRepo
	scope, err := json.Marshal(backup.ScopeOf(sb))
	if err != nil {
		t.Fatal(err)
	}
	cj := &batchv1.CronJob{
		Namespace: testNS, Name: backup.CronJobName(sb), UID: testCronJobUID,
		Labels: map[string]string{labelManagedBy: operatorManagedBy},
		Annotations: map[string]string{
			specHashKey:     "0123abcd",
			renderedFromKey: "scheduledbackup=3,repository=2",
		},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: b2backupv1.GroupVersion.String(), Kind: kindScheduledBackup, Name: sb.Name, UID: sb.UID,
			Controller: ptr.To(true),
		}},
		Spec: batchv1.CronJobSpec{Schedule: testSchedule},
	}
	cj.Spec.JobTemplate.Annotations = map[string]string{"b2backup.mcswain.dev/forget-scope": string(scope)}
	return currentFixture{sb: sb, repo: repo, cj: cj}
}

func (fx currentFixture) objects(extra ...client.Object) []client.Object {
	return append([]client.Object{fx.sb, fx.repo, fx.cj}, extra...)
}

func checkStalenessError(t *testing.T, err error, wants string) {
	t.Helper()
	if err == nil {
		t.Fatal("the command reached a Job with the offline factory")
	}
	for _, phrase := range []string{notRendered, inATagConflict} {
		if got := strings.Contains(err.Error(), phrase); got != (phrase == wants) {
			t.Errorf("err = %v, want %q: %t", err, phrase, phrase == wants)
		}
	}
}

func stalenessCases() []struct {
	name  string
	edit  func(*currentFixture) []client.Object
	wants string
} {
	return []struct {
		name  string
		edit  func(*currentFixture) []client.Object
		wants string
	}{
		{"current", func(*currentFixture) []client.Object { return nil }, ""},
		{"a spec the operator has not seen", func(fx *currentFixture) []client.Object {
			fx.sb.Generation = currentGenBackup + 1
			return nil
		}, notRendered},
		{"not Ready", func(fx *currentFixture) []client.Object {
			fx.sb.Status.Conditions[0].Status = metav1.ConditionFalse
			fx.sb.Status.Conditions[0].Reason = "ReconcileFailed"
			return nil
		}, notRendered},
		{"Ready from an older generation", func(fx *currentFixture) []client.Object {
			fx.sb.Status.Conditions[0].ObservedGeneration = currentGenBackup - 1
			return nil
		}, notRendered},
		{"a Repository changed since the CronJob was rendered", func(fx *currentFixture) []client.Object {
			fx.repo.Generation = currentGenRepo + 1
			return nil
		}, notRendered},
		{"a CronJob from an operator that does not record its sources", func(fx *currentFixture) []client.Object {
			delete(fx.cj.Annotations, renderedFromKey)
			return nil
		}, notRendered},
		{"a CronJob the backup does not own", func(fx *currentFixture) []client.Object {
			fx.cj.OwnerReferences = nil
			return nil
		}, notRendered},
		{"a CronJob forgetting under other tags", func(fx *currentFixture) []client.Object {
			fx.cj.Spec.JobTemplate.Annotations["b2backup.mcswain.dev/forget-scope"] =
				`{"repository":"` + testRepoName + `","host":"` + testNS + `","tags":["old"]}`
			return nil
		}, notRendered},
		{"a sibling edited into a conflict the status does not show yet", func(fx *currentFixture) []client.Object {
			other := newBackup(otherBackup, testRepoName)
			other.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG}}
			return []client.Object{other}
		}, inATagConflict},
	}
}

func TestPruneRefusesToActOnStaleStatus(t *testing.T) {
	t.Parallel()
	for _, tt := range stalenessCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fx := currentBackup(t)
			extra := tt.edit(&fx)
			f := factoryWithClient(t, newClient(t, fx.objects(extra...)...))
			cmd := newPruneCommand(f)
			cmd.SetArgs([]string{subjectBackup, flagYes})
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			checkStalenessError(t, cmd.Execute(), tt.wants)
		})
	}
}

func TestOverwritingRestoresRefuseToActOnStaleStatus(t *testing.T) {
	t.Parallel()
	targets := []struct {
		name    string
		mutate  func(*b2backupv1.ScheduledBackup)
		restore func(context.Context, *Factory, client.Client, *b2backupv1.ScheduledBackup) error
	}{
		{targetInPlace, withVolume, func(ctx context.Context, f *Factory, c client.Client, sb *b2backupv1.ScheduledBackup) error {
			return restoreInPlace(ctx, f, c, sb, &restoreOptions{snapshot: latest, yes: true})
		}},
		{"to the database", withDatabase, func(ctx context.Context, f *Factory, c client.Client, sb *b2backupv1.ScheduledBackup) error {
			return restoreToDatabase(ctx, f, c, sb, &restoreOptions{snapshot: latest, yes: true, toDatabase: true})
		}},
	}
	for _, target := range targets {
		for _, tt := range stalenessCases() {
			t.Run(target.name+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				fx := currentBackup(t, target.mutate)
				extra := tt.edit(&fx)
				c := newClient(t, fx.objects(extra...)...)
				checkStalenessError(t, target.restore(context.Background(), factoryWithClient(t, c), c, fx.sb), tt.wants)
			})
		}
	}
}

func TestARepositoryWithSeveralBackupsNeedsOneNamed(t *testing.T) {
	t.Parallel()
	fx := currentBackup(t)
	other := newBackup(otherBackup, testRepoName)
	c := newClient(t, fx.objects(other)...)
	cmd := newPruneCommand(factoryWithClient(t, c))
	cmd.SetArgs([]string{subjectRepo, flagYes})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "other") || !strings.Contains(err.Error(), testBackupName) {
		t.Errorf("err = %v, want both ScheduledBackups named", err)
	}
	if err != nil && !strings.Contains(err.Error(), "name one") {
		t.Errorf("err = %v, want it to ask for one", err)
	}
}

func TestRestoreOfARepositoryWithSeveralBackupsNeedsOneNamed(t *testing.T) {
	t.Parallel()
	fx := currentBackup(t, withVolume)
	other := newBackup(otherBackup, testRepoName)
	cmd := newRestoreCommand(factoryWithClient(t, newClient(t, fx.objects(other)...)))
	cmd.SetArgs([]string{subjectRepo, "--in-place", flagYes})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); !errors.Is(err, ErrSeveralBackupsForRepository) {
		t.Errorf("err = %v, want the restore refused until one ScheduledBackup is named", err)
	}
}

func TestPruneRefusesARepositoryThatIsNotItsOwn(t *testing.T) {
	t.Parallel()
	fx := currentBackup(t)
	fx.repo.Status.Conditions = []metav1.Condition{{
		Type: b2backupv1.RepositoryConditionReady, Status: metav1.ConditionFalse, Reason: reasonRepositoryConflict,
		Message: "another Repository names this Secret",
	}}
	f := factoryWithClient(t, newClient(t, fx.objects()...))
	cmd := newPruneCommand(f)
	cmd.SetArgs([]string{subjectBackup, flagYes})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); !errors.Is(err, ErrRepositoryNotUsable) {
		t.Errorf("err = %v, want prune refused while the Repository is not its own", err)
	}
}
