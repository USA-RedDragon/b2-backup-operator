package cli

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const appTemplateTestdata = "testdata/apptemplate"

var appTemplateCases = []string{"home", "edge", "merge"}

func appTemplateValues(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(appTemplateTestdata, name+".values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(raw, &values, func(d *json.Decoder) *json.Decoder {
		d.UseNumber()
		return d
	}); err != nil {
		t.Fatal(err)
	}
	return values
}

// TestAppTemplateRendererMatchesTheChart checks the renderer against CronJobs
// the real chart rendered. Each <case>.cronjob.yaml is the CronJob from
//
//	helm template restic app-template-4.6.2.tgz -n app -f <case>.values.yaml
//
// TestAppTemplateGoldensMatchHelm re-renders them when the chart is at hand.
func TestAppTemplateRendererMatchesTheChart(t *testing.T) {
	t.Parallel()
	for _, name := range appTemplateCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want, err := readCronJob(filepath.Join(appTemplateTestdata, name+".cronjob.yaml"))
			if err != nil || want == nil {
				t.Fatalf("golden CronJob: %v", err)
			}
			got, err := renderAppTemplate(appTemplateValues(t, name), resticController, "app")
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if a, b := canonicalJSON(want.Spec), canonicalJSON(got.cronJob.Spec); a != b {
				t.Errorf("rendered CronJob differs from the chart's\nchart:    %s\nrendered: %s", a, b)
			}
		})
	}
}

func TestAppTemplateRendererSummarizesOtherControllers(t *testing.T) {
	t.Parallel()
	got, err := renderAppTemplate(appTemplateValues(t, "home"), resticController, "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.others) != 1 || got.others[0].name != "init" || !isResticInit(got.others[0]) {
		t.Errorf("others = %+v, want the init Job recognized as restic init", got.others)
	}
}

// TestAppTemplateGoldensMatchHelm renders the testdata with the real chart.
// Point B2BK_APP_TEMPLATE_CHART at the app-template 4.6.2 chart archive, as
// pulled with helm pull oci://ghcr.io/bjw-s-labs/helm/app-template --version 4.6.2.
func TestAppTemplateGoldensMatchHelm(t *testing.T) {
	chart := os.Getenv("B2BK_APP_TEMPLATE_CHART")
	if chart == "" {
		t.Skip("B2BK_APP_TEMPLATE_CHART is not set")
	}
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not on PATH")
	}
	for _, name := range appTemplateCases {
		out, err := exec.Command("helm", "template", resticController, chart, "-n", "app",
			"-f", filepath.Join(appTemplateTestdata, name+".values.yaml")).Output()
		if err != nil {
			t.Fatalf("%s: helm template: %v", name, err)
		}
		var doc string
		for d := range strings.SplitSeq(string(out), "\n---\n") {
			if strings.Contains(d, "\nkind: CronJob\n") {
				doc = d
			}
		}
		rendered := filepath.Join(t.TempDir(), "cronjob.yaml")
		if err := os.WriteFile(rendered, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		fromHelm, err := readCronJob(rendered)
		if err != nil || fromHelm == nil {
			t.Fatalf("%s: no CronJob in helm's output: %v", name, err)
		}
		golden, err := readCronJob(filepath.Join(appTemplateTestdata, name+".cronjob.yaml"))
		if err != nil || golden == nil {
			t.Fatalf("%s: golden: %v", name, err)
		}
		if canonicalJSON(fromHelm.Spec) != canonicalJSON(golden.Spec) {
			t.Errorf("%s.cronjob.yaml is stale; re-render it with helm template", name)
		}
	}
}

func TestAppTemplateRendererRefusesWhatItDoesNotModel(t *testing.T) {
	t.Parallel()
	tests := map[string]func(map[string]any){
		"a Helm template": func(v map[string]any) {
			container(v)["workingDir"] = "{{ .Release.Name }}"
		},
		"an unknown container field": func(v map[string]any) {
			container(v)["probes"] = map[string]any{}
		},
		"a chart-created claim": func(v map[string]any) {
			v["persistence"].(map[string]any)["new"] = map[string]any{"size": "1Gi", "accessMode": "ReadWriteOnce"}
		},
		"ordered env": func(v map[string]any) {
			container(v)["env"].(map[string]any)["TZ"] = map[string]any{"value": "UTC", "dependsOn": "RESTIC_HOST"}
		},
		"chart-managed secrets": func(v map[string]any) {
			v["secrets"] = map[string]any{}
		},
		"a default container option": func(v map[string]any) {
			v["controllers"].(map[string]any)[resticController].(map[string]any)["defaultContainerOptions"] = map[string]any{}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			values := appTemplateValues(t, "home")
			mutate(values)
			if _, err := renderAppTemplate(values, resticController, "app"); !errors.Is(err, ErrAppTemplateUnmodeled) {
				t.Errorf("err = %v, want ErrAppTemplateUnmodeled", err)
			}
		})
	}
}

func container(values map[string]any) map[string]any {
	controllers := values["controllers"].(map[string]any)
	return controllers[resticController].(map[string]any)["containers"].(map[string]any)[resticController].(map[string]any)
}
