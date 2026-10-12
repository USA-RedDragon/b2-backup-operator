package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

func readyRepo(ns string) *b2backupv1.Repository {
	r := newRepo(ns)
	r.CreationTimestamp = metav1.NewTime(time.Now().Add(-3 * time.Hour))
	r.Spec.Mode = b2backupv1.StorageModePrefix
	r.Spec.Bucket = testBucket
	r.Spec.Path = testPath
	r.Status = b2backupv1.RepositoryStatus{
		BucketName:       testBucket,
		BucketID:         "bucket0001",
		Path:             testPath,
		Endpoint:         testEndpoint,
		ApplicationKeyID: testKeyID,
		Initialized:      true,
		CurrentUsage:     testUsage,
		SecretName:       testRepoName + "-credentials",
		Conditions: []metav1.Condition{
			{Type: b2backupv1.RepositoryConditionReady, Status: metav1.ConditionTrue, Reason: reasonReady},
		},
	}
	return r
}

func readyBackup(name, repoRef string) *b2backupv1.ScheduledBackup {
	b := newBackup(name, repoRef)
	b.CreationTimestamp = metav1.NewTime(time.Now().Add(-3 * time.Hour))
	b.Spec.TimeZone = testTimeZone
	last := metav1.NewTime(time.Now().Add(-90 * time.Minute))
	b.Status = b2backupv1.ScheduledBackupStatus{
		EffectiveSchedule:  testSchedule,
		LastSuccessfulTime: &last,
		Conditions: []metav1.Condition{
			{Type: b2backupv1.ScheduledBackupConditionReady, Status: metav1.ConditionTrue, Reason: reasonReady},
		},
	}
	return b
}

func getOutput(t *testing.T, c client.Client, ns string, all bool, output string, args ...string) string {
	t.Helper()
	kinds, err := kindsToList(args)
	if err != nil {
		t.Fatalf("kindsToList: %v", err)
	}
	var buf bytes.Buffer
	if err := runGet(context.Background(), c, &buf, ns, all, output, kinds, ""); err != nil {
		t.Fatalf("runGet: %v", err)
	}
	return buf.String()
}

func TestGetTable(t *testing.T) {
	c := newClient(t, readyRepo(testNS), readyBackup(testBackupName, testRepoName))

	out := getOutput(t, c, "prod", false, OutputTable)

	for _, want := range []string{
		"NAME", "BUCKET", "PATH", "READY", "INITIALIZED", "USAGE",
		testRepoName, testBucket, testPath, statusTrue, testUsage,
		"REPOSITORY", "SCHEDULE", "LAST BACKUP", "SUSPENDED", "ACTIVE",
		testBackupName, testSchedule, "ago", "3h",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}

	if strings.Contains(out, "NAMESPACE") {
		t.Errorf("unexpected NAMESPACE column without -A:\n%s", out)
	}
}

func TestGetWideAddsColumns(t *testing.T) {
	c := newClient(t, readyRepo(testNS), readyBackup(testBackupName, testRepoName))

	out := getOutput(t, c, "prod", false, OutputWide)
	for _, want := range []string{
		"MODE", "Prefix", "KEY ID", testKeyID, "SECRET", "store-credentials",
		"TIMEZONE", testTimeZone,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in wide output:\n%s", want, out)
		}
	}
}

func TestGetAllNamespaces(t *testing.T) {
	c := newClient(t, readyRepo(testNS), readyRepo("staging"))

	out := getOutput(t, c, "", true, OutputTable, "repositories")
	if !strings.Contains(out, "NAMESPACE") {
		t.Errorf("expected NAMESPACE column with -A:\n%s", out)
	}
	for _, ns := range []string{"prod", "staging"} {
		if !strings.Contains(out, ns) {
			t.Errorf("expected namespace %q in output:\n%s", ns, out)
		}
	}
}

func TestGetNarrowsByKind(t *testing.T) {
	c := newClient(t, readyRepo(testNS), readyBackup(testBackupName, testRepoName))

	if out := getOutput(t, c, "prod", false, OutputTable, "repositories"); strings.Contains(out, testBackupName) {
		t.Errorf("`get repositories` listed a backup:\n%s", out)
	}
	if out := getOutput(t, c, "prod", false, OutputTable, "backups"); strings.Contains(out, "REPO ID") {
		t.Errorf("`get backups` listed a repository:\n%s", out)
	}
}

func TestGetEmpty(t *testing.T) {
	out := getOutput(t, newClient(t), "prod", false, OutputTable)
	if !strings.Contains(out, `No resources found in namespace "prod".`) {
		t.Errorf("unexpected empty output: %q", out)
	}
	if out := getOutput(t, newClient(t), "", true, OutputTable); !strings.Contains(out, "in any namespace") {
		t.Errorf("unexpected empty -A output: %q", out)
	}
}

func TestGetMachineOutput(t *testing.T) {
	c := newClient(t, readyRepo(testNS))

	jsonOut := getOutput(t, c, "prod", false, OutputJSON, "repositories")
	for _, want := range []string{`"kind": "RepositoryList"`, `"apiVersion"`, `"applicationKeyID": "` + testKeyID + `"`} {
		if !strings.Contains(jsonOut, want) {
			t.Errorf("expected %q in JSON output:\n%s", want, jsonOut)
		}
	}

	yamlOut := getOutput(t, c, "prod", false, OutputYAML, "repositories")
	if !strings.Contains(yamlOut, "kind: RepositoryList") {
		t.Errorf("expected kind in YAML output:\n%s", yamlOut)
	}

	nameOut := getOutput(t, c, "prod", false, OutputName, "repositories")
	if want := "repository.b2backup.mcswain.dev/store\n"; nameOut != want {
		t.Errorf("name output = %q, want %q", nameOut, want)
	}
}

func TestGetRejectsBadInput(t *testing.T) {
	if err := ValidateOutput("toml"); err == nil {
		t.Error("expected an error for an unsupported output format")
	}
	if _, err := kindsToList([]string{"cronjobs"}); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("expected ErrUnknownKind, got %v", err)
	}
}

type parsedList struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Items      []struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	} `json:"items"`
}

func parseStrict(t *testing.T, format, out string) parsedList {
	t.Helper()
	var got parsedList
	var err error
	if format == OutputJSON {
		err = json.Unmarshal([]byte(out), &got)
	} else {
		err = yaml.Unmarshal([]byte(out), &got)
	}
	if err != nil {
		t.Fatalf("%s output is not a single valid document: %v\n%s", format, err, out)
	}
	return got
}

func TestGetMachineOutputIsOneDocumentPerKind(t *testing.T) {
	c := newClient(t, readyRepo(testNS), readyBackup(testBackupName, testRepoName))

	for _, format := range []string{OutputJSON, OutputYAML} {
		got := parseStrict(t, format, getOutput(t, c, testNS, false, format, "repositories"))
		if got.Kind != "RepositoryList" {
			t.Errorf("%s: one kind must stay a typed list, got %q", format, got.Kind)
		}
		if len(got.Items) != 1 || got.Items[0].Metadata.Name != testRepoName {
			t.Errorf("%s: unexpected items %+v", format, got.Items)
		}
	}
}

func TestGetMachineOutputWrapsBothKindsInAList(t *testing.T) {
	c := newClient(t, readyRepo(testNS), readyBackup(testBackupName, testRepoName))

	for _, format := range []string{OutputJSON, OutputYAML} {
		got := parseStrict(t, format, getOutput(t, c, testNS, false, format))
		if got.Kind != "List" || got.APIVersion != "v1" {
			t.Errorf("%s: expected a v1.List, got %s/%s", format, got.APIVersion, got.Kind)
		}
		if len(got.Items) != 2 {
			t.Fatalf("%s: expected both resources, got %+v", format, got.Items)
		}
		want := map[string]string{kindRepository: testRepoName, kindScheduledBackup: testBackupName}
		for _, item := range got.Items {
			if want[item.Kind] != item.Metadata.Name {
				t.Errorf("%s: unexpected item %s/%s", format, item.Kind, item.Metadata.Name)
			}
			delete(want, item.Kind)
		}
		if len(want) != 0 {
			t.Errorf("%s: missing %v", format, want)
		}
	}
}

func TestKindsToListMatchesResolveAliases(t *testing.T) {
	for _, arg := range []string{"repo", "Repositories", "REPO", "repos"} {
		kinds, err := kindsToList([]string{arg})
		if err != nil || len(kinds) != 1 || kinds[0] != TargetRepository {
			t.Errorf("kindsToList(%q) = %v, %v", arg, kinds, err)
		}
	}
	for _, arg := range []string{"sb", "Backups", "scheduledbackup"} {
		kinds, err := kindsToList([]string{arg})
		if err != nil || len(kinds) != 1 || kinds[0] != TargetScheduledBackup {
			t.Errorf("kindsToList(%q) = %v, %v", arg, kinds, err)
		}
	}
	for _, arg := range knownPrefixes() {
		if _, err := kindsToList([]string{arg}); err != nil {
			t.Errorf("kindsToList rejects the completion candidate %q: %v", arg, err)
		}
	}
}
