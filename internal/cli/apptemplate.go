package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

// ErrAppTemplateUnmodeled means a HelmRelease uses something the in-process
// app-template renderer does not model, so it cannot say what the chart renders.
var ErrAppTemplateUnmodeled = errors.New("not modelled by parity's app-template renderer")

const (
	controllerTypeCronJob = "cronjob"
	controllerTypeJob     = "job"

	keyName             = "name"
	keyContainers       = "containers"
	keyResources        = "resources"
	keySecurityContext  = "securityContext"
	keyDNSPolicy        = "dnsPolicy"
	keyHostPath         = "hostPath"
	keyConfigMap        = "configMap"
	keyBackoffLimit     = "backoffLimit"
	keyStartingDeadline = "startingDeadlineSeconds"
	keyActiveDeadline   = "activeDeadlineSeconds"
	keyTTLAfterFinished = "ttlSecondsAfterFinished"
	keyEnabled          = "enabled"
	keyParallelism      = "parallelism"
	keyRestartPolicy    = "restartPolicy"
	keyPath             = "path"
	keyInitContainers   = "initContainers"
)

// appTemplateRelease is what bjw-s app-template 4.x renders for a HelmRelease:
// the restic controller's CronJob, and a summary of every other controller.
type appTemplateRelease struct {
	cronJob *batchv1.CronJob
	others  []appTemplateController
}

// appTemplateController is a controller other than restic, and the command
// lines its containers run.
type appTemplateController struct {
	name     string
	kind     string
	commands [][]string
}

func unmodeled(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrAppTemplateUnmodeled, fmt.Sprintf(format, args...))
}

// renderAppTemplateFile renders a HelmRelease file the way app-template would.
// It reads the values itself rather than through migrate's reading of them, so
// that parity compares the migration against what the chart really runs.
func renderAppTemplateFile(path, namespace string) (*appTemplateRelease, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Values map[string]any `json:"values"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc, func(d *json.Decoder) *json.Decoder {
		d.UseNumber()
		return d
	}); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return renderAppTemplate(doc.Spec.Values, doc.Metadata.Name, namespace)
}

type appTemplateRenderer struct {
	values     map[string]any
	release    string
	namespace  string
	strategy   string
	defaultPod map[string]any
}

var (
	appTemplateValueKeys      = []string{"controllers", "persistence", "defaultPodOptions", "defaultPodOptionsStrategy"}
	appTemplateControllerKeys = []string{keyEnabled, "type", "cronjob", "job", "pod", keyContainers, keyInitContainers,
		"labels", "annotations"}
	appTemplateCronJobKeys = []string{"schedule", "timeZone", "concurrencyPolicy", "suspend", keyStartingDeadline,
		"successfulJobsHistory", "failedJobsHistory", keyBackoffLimit, keyTTLAfterFinished,
		keyActiveDeadline, keyParallelism}
	appTemplatePodKeys = []string{"enableServiceLinks", "automountServiceAccountToken", "priorityClassName",
		"runtimeClassName", "schedulerName", keySecurityContext, "hostname", "hostIPC", "hostNetwork", "hostPID",
		"hostUsers", "shareProcessNamespace", keyDNSPolicy, "dnsConfig", "hostAliases", "imagePullSecrets",
		"terminationGracePeriodSeconds", keyResources, "resourceClaims", keyRestartPolicy, "nodeSelector", "affinity",
		"topologySpreadConstraints", "tolerations", "schedulingGates", "labels", "annotations"}
	appTemplateContainerKeys = []string{keyEnabled, "image", "command", "args", "workingDir", keySecurityContext,
		"lifecycle", "terminationMessagePath", "terminationMessagePolicy", "env", "envFrom", "ports", keyResources,
		keyRestartPolicy, "stdin", "tty"}
	appTemplatePersistenceKeys = []string{keyEnabled, "type", "existingClaim", keyName, "defaultMode", "items",
		"medium", "sizeLimit", keyHostPath, "hostPathType", "server", keyPath, "volumeSpec", "globalMounts",
		"advancedMounts", "storageClass", "accessMode", "size", "retain"}
	appTemplateMountKeys = []string{keyPath, "subPath", "subPathExpr", "readOnly", "mountPropagation"}
)

func renderAppTemplate(values map[string]any, release, namespace string) (*appTemplateRelease, error) {
	if err := checkKeys("values", values, appTemplateValueKeys); err != nil {
		return nil, err
	}
	if err := refuseTemplates("values", values); err != nil {
		return nil, err
	}
	r := &appTemplateRenderer{values: values, release: release, namespace: namespace, strategy: "overwrite"}
	if s, ok := values["defaultPodOptionsStrategy"].(string); ok {
		r.strategy = s
	}
	if r.strategy != "overwrite" && r.strategy != "merge" {
		return nil, unmodeled("defaultPodOptionsStrategy %v", values["defaultPodOptionsStrategy"])
	}
	defaultPod, err := mapAt(values, "defaultPodOptions")
	if err != nil {
		return nil, err
	}
	if err := checkKeys("defaultPodOptions", defaultPod, appTemplatePodKeys); err != nil {
		return nil, err
	}
	r.defaultPod = defaultPod

	controllers, err := mapAt(values, "controllers")
	if err != nil {
		return nil, err
	}
	out := &appTemplateRelease{}
	for _, name := range sortedKeys(controllers) {
		c, ok := controllers[name].(map[string]any)
		if !ok {
			continue
		}
		if err := checkKeys("controllers."+name, c, appTemplateControllerKeys); err != nil {
			return nil, err
		}
		if enabled, set := c[keyEnabled]; set && enabled != true {
			continue
		}
		kind, _ := c["type"].(string)
		if kind == "" {
			kind = "deployment"
		}
		if name != resticController {
			other, err := r.summarize(name, kind, c)
			if err != nil {
				return nil, err
			}
			out.others = append(out.others, other)
			continue
		}
		if kind != controllerTypeCronJob {
			return nil, fmt.Errorf("%w: controllers.restic is a %s, not a cronjob", ErrNoResticController, kind)
		}
		cj, err := r.cronJob(c)
		if err != nil {
			return nil, err
		}
		out.cronJob = cj
	}
	if out.cronJob == nil {
		return nil, ErrNoResticController
	}
	return out, nil
}

func (r *appTemplateRenderer) summarize(name, kind string, c map[string]any) (appTemplateController, error) {
	out := appTemplateController{name: name, kind: kind}
	for _, field := range []string{keyInitContainers, keyContainers} {
		containers, err := mapAt(c, field)
		if err != nil {
			return out, err
		}
		for _, id := range sortedKeys(containers) {
			container, ok := containers[id].(map[string]any)
			if !ok {
				continue
			}
			if enabled, set := container[keyEnabled]; set && enabled != true {
				continue
			}
			argv, err := r.argv(fmt.Sprintf("controllers.%s.%s.%s", name, field, id), container)
			if err != nil {
				return out, err
			}
			out.commands = append(out.commands, argv)
		}
	}
	return out, nil
}

func (r *appTemplateRenderer) cronJob(c map[string]any) (*batchv1.CronJob, error) {
	settings, err := mapAt(c, "cronjob")
	if err != nil {
		return nil, err
	}
	if err := checkKeys("controllers.restic.cronjob", settings, appTemplateCronJobKeys); err != nil {
		return nil, err
	}
	schedule, _ := settings["schedule"].(string)
	if schedule == "" {
		return nil, fmt.Errorf("controllers.restic.cronjob.schedule is not set")
	}

	cj := &batchv1.CronJob{}
	cj.APIVersion, cj.Kind = "batch/v1", "CronJob"
	cj.Name, cj.Namespace = r.release, r.namespace
	cj.Labels = map[string]string{labelController: resticController}
	if err := mergeStrings(cj.Labels, c["labels"], "controllers.restic.labels"); err != nil {
		return nil, err
	}
	if annotations, ok := c["annotations"].(map[string]any); ok && len(annotations) > 0 {
		cj.Annotations = map[string]string{}
		if err := mergeStrings(cj.Annotations, annotations, "controllers.restic.annotations"); err != nil {
			return nil, err
		}
	}

	spec := &cj.Spec
	spec.Schedule = schedule
	if tz, _ := settings["timeZone"].(string); tz != "" {
		spec.TimeZone = ptr.To(tz)
	}
	spec.Suspend = ptr.To(settings["suspend"] == true)
	spec.ConcurrencyPolicy = batchv1.ForbidConcurrent
	if p, _ := settings["concurrencyPolicy"].(string); p != "" {
		spec.ConcurrencyPolicy = batchv1.ConcurrencyPolicy(p)
	}
	ints := map[string]int64{}
	for _, field := range []string{keyStartingDeadline, "successfulJobsHistory", "failedJobsHistory",
		keyBackoffLimit, keyTTLAfterFinished, keyActiveDeadline, keyParallelism} {
		v, set := settings[field]
		if !set || v == nil {
			continue
		}
		n, err := integer(v)
		if err != nil {
			return nil, fmt.Errorf("controllers.restic.cronjob.%s: %w", field, err)
		}
		ints[field] = n
	}
	spec.StartingDeadlineSeconds = ptr.To(withDefault(ints, keyStartingDeadline, 30))
	spec.SuccessfulJobsHistoryLimit = ptr.To(int32(withDefault(ints, "successfulJobsHistory", 1)))
	spec.FailedJobsHistoryLimit = ptr.To(int32(withDefault(ints, "failedJobsHistory", 1)))
	job := &spec.JobTemplate.Spec
	if n := ints[keyActiveDeadline]; n != 0 {
		job.ActiveDeadlineSeconds = ptr.To(n)
	}
	if n := ints[keyTTLAfterFinished]; n != 0 {
		job.TTLSecondsAfterFinished = ptr.To(int32(n))
	}
	if n := ints[keyParallelism]; n != 0 {
		job.Parallelism = ptr.To(int32(n))
	}
	job.BackoffLimit = ptr.To(int32(withDefault(ints, keyBackoffLimit, 6)))

	template, err := r.podTemplate(c)
	if err != nil {
		return nil, err
	}
	job.Template = template
	return cj, nil
}

func withDefault(values map[string]int64, key string, def int64) int64 {
	if v, ok := values[key]; ok {
		return v
	}
	return def
}

func (r *appTemplateRenderer) podTemplate(c map[string]any) (corev1.PodTemplateSpec, error) {
	var out corev1.PodTemplateSpec
	pod, err := mapAt(c, "pod")
	if err != nil {
		return out, err
	}
	if err := checkKeys("controllers.restic.pod", pod, appTemplatePodKeys); err != nil {
		return out, err
	}
	pod = maps.Clone(pod)
	if empty(pod[keyRestartPolicy]) {
		pod[keyRestartPolicy] = string(corev1.RestartPolicyNever)
	}
	option := func(name string) any { return r.podOption(pod, name) }

	out.Labels = map[string]string{
		labelController: resticController, labelName: r.release, labelInstance: r.release,
	}
	if err := mergeStrings(out.Labels, option("labels"), "pod labels"); err != nil {
		return out, err
	}
	if annotations, ok := option("annotations").(map[string]any); ok && len(annotations) > 0 {
		out.Annotations = map[string]string{}
		if err := mergeStrings(out.Annotations, annotations, "pod annotations"); err != nil {
			return out, err
		}
	}

	spec := map[string]any{
		"enableServiceLinks":           orDefault(option("enableServiceLinks"), false),
		"serviceAccountName":           "default",
		"automountServiceAccountToken": orDefault(option("automountServiceAccountToken"), true),
		"hostIPC":                      orDefault(option("hostIPC"), false),
		"hostNetwork":                  orDefault(option("hostNetwork"), false),
		"hostPID":                      orDefault(option("hostPID"), false),
		keyDNSPolicy:                   string(corev1.DNSClusterFirst),
	}
	if option("hostNetwork") == true {
		spec[keyDNSPolicy] = string(corev1.DNSClusterFirstWithHostNet)
	}
	for _, name := range []string{"priorityClassName", "runtimeClassName", "schedulerName", keySecurityContext,
		"hostname", "hostUsers", "shareProcessNamespace", keyDNSPolicy, "dnsConfig", "hostAliases",
		"imagePullSecrets", "terminationGracePeriodSeconds", keyResources, "resourceClaims", keyRestartPolicy,
		"nodeSelector", "affinity", "topologySpreadConstraints", "tolerations", "schedulingGates"} {
		if v := option(name); v != nil {
			spec[name] = v
		}
	}

	for _, field := range []string{keyInitContainers, keyContainers} {
		containers, err := r.containers(c, field)
		if err != nil {
			return out, err
		}
		if len(containers) > 0 {
			spec[field] = containers
		}
	}
	volumes, err := r.volumes()
	if err != nil {
		return out, err
	}
	if len(volumes) > 0 {
		spec["volumes"] = volumes
	}

	if err := strictDecode(spec, &out.Spec); err != nil {
		return out, fmt.Errorf("the restic controller's pod: %w", err)
	}
	return out, nil
}

// podOption is app-template's pod.getOption: the controller's pod value wins
// over defaultPodOptions, a map is merged into it under the merge strategy, and
// an empty value that is not a bool counts as unset.
func (r *appTemplateRenderer) podOption(pod map[string]any, name string) any {
	var value any
	if v, ok := r.defaultPod[name]; ok {
		if _, isBool := v.(bool); isBool || !empty(v) {
			value = v
		}
	}
	if v, ok := pod[name]; ok {
		_, isBool := v.(bool)
		m, isMap := v.(map[string]any)
		switch {
		case isBool:
			value = v
		case isMap && r.strategy == "merge":
			base, _ := value.(map[string]any)
			value = mergeOverwrite(base, m)
		case isMap:
			value = v
		case !empty(v):
			value = v
		}
	}
	if _, isBool := value.(bool); !isBool && empty(value) {
		return nil
	}
	return value
}

func mergeOverwrite(dst, src map[string]any) map[string]any {
	out := maps.Clone(dst)
	if out == nil {
		out = map[string]any{}
	}
	for k, v := range src {
		sub, isMap := v.(map[string]any)
		existing, wasMap := out[k].(map[string]any)
		if isMap && wasMap {
			out[k] = mergeOverwrite(existing, sub)
			continue
		}
		out[k] = v
	}
	return out
}

func orDefault(v, def any) any {
	if v == nil {
		return def
	}
	return v
}

func (r *appTemplateRenderer) containers(c map[string]any, field string) ([]any, error) {
	containers, err := mapAt(c, field)
	if err != nil {
		return nil, err
	}
	var out []any
	for _, id := range sortedKeys(containers) {
		container, ok := containers[id].(map[string]any)
		if !ok {
			continue
		}
		where := fmt.Sprintf("controllers.restic.%s.%s", field, id)
		if err := checkKeys(where, container, appTemplateContainerKeys); err != nil {
			return nil, err
		}
		if enabled, set := container[keyEnabled]; set && enabled != true {
			continue
		}
		rendered, err := r.container(where, id, container)
		if err != nil {
			return nil, err
		}
		out = append(out, rendered)
	}
	return out, nil
}

func (r *appTemplateRenderer) container(where, id string, c map[string]any) (map[string]any, error) {
	out := map[string]any{keyName: id}
	image, err := imageOf(where, c["image"])
	if err != nil {
		return nil, err
	}
	out["image"] = image
	if m, ok := c["image"].(map[string]any); ok {
		if p, _ := m["pullPolicy"].(string); p != "" {
			out["imagePullPolicy"] = p
		}
	}
	for _, field := range []string{"command", "args"} {
		list, err := stringList(where+"."+field, c[field])
		if err != nil {
			return nil, err
		}
		if len(list) > 0 {
			out[field] = list
		}
	}
	for _, field := range []string{"workingDir", keySecurityContext, "lifecycle", "terminationMessagePath",
		"terminationMessagePolicy", "ports", keyResources, keyRestartPolicy, "stdin", "tty"} {
		if v, ok := c[field]; ok && !empty(v) {
			out[field] = v
		}
	}
	env, err := renderEnv(where+".env", c["env"])
	if err != nil {
		return nil, err
	}
	if len(env) > 0 {
		out["env"] = env
	}
	envFrom, err := renderEnvFrom(where+".envFrom", c["envFrom"])
	if err != nil {
		return nil, err
	}
	if len(envFrom) > 0 {
		out["envFrom"] = envFrom
	}
	mounts, err := r.volumeMounts(id)
	if err != nil {
		return nil, err
	}
	if len(mounts) > 0 {
		out["volumeMounts"] = mounts
	}
	return out, nil
}

func (r *appTemplateRenderer) argv(where string, c map[string]any) ([]string, error) {
	command, err := stringList(where+".command", c["command"])
	if err != nil {
		return nil, err
	}
	args, err := stringList(where+".args", c["args"])
	if err != nil {
		return nil, err
	}
	return append(command, args...), nil
}

func imageOf(where string, v any) (string, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return "", unmodeled("%s.image is not a map of repository, tag and digest", where)
	}
	if err := checkKeys(where+".image", m, []string{"repository", "tag", "digest", "pullPolicy"}); err != nil {
		return "", err
	}
	repository, _ := m["repository"].(string)
	if repository == "" {
		return "", fmt.Errorf("%s.image.repository is not set", where)
	}
	image := repository
	if tag := scalarString(m["tag"]); tag != "" {
		image += ":" + tag
	}
	if digest := scalarString(m["digest"]); digest != "" {
		image += "@" + digest
	}
	return image, nil
}

func stringList(where string, v any) ([]string, error) {
	switch value := v.(type) {
	case nil:
		return nil, nil
	case string:
		if value == "" {
			return nil, nil
		}
		return []string{value}, nil
	case []any:
		out := make([]string, 0, len(value))
		for _, item := range value {
			s, ok := item.(string)
			if !ok {
				return nil, unmodeled("%s holds %v, which is not a string", where, item)
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, unmodeled("%s is neither a string nor a list", where)
}

// renderEnv is app-template's env field: a map is rendered in name order, a
// plain value as a string the way Helm prints it, and a map value as valueFrom.
func renderEnv(where string, v any) ([]any, error) {
	switch value := v.(type) {
	case nil:
		return nil, nil
	case []any:
		out := make([]any, 0, len(value))
		for _, item := range value {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, unmodeled("%s holds %v, which is not a map", where, item)
			}
			rendered, err := renderEnvVar(where, m)
			if err != nil {
				return nil, err
			}
			out = append(out, rendered)
		}
		return out, nil
	case map[string]any:
		out := make([]any, 0, len(value))
		for _, name := range sortedKeys(value) {
			item := map[string]any{keyName: name}
			if m, ok := value[name].(map[string]any); ok {
				if _, set := m["dependsOn"]; set {
					return nil, unmodeled("%s.%s.dependsOn", where, name)
				}
				maps.Copy(item, m)
			} else {
				item["value"] = value[name]
			}
			rendered, err := renderEnvVar(where, item)
			if err != nil {
				return nil, err
			}
			out = append(out, rendered)
		}
		return out, nil
	}
	return nil, unmodeled("%s is neither a map nor a list", where)
}

func renderEnvVar(where string, item map[string]any) (map[string]any, error) {
	name, _ := item[keyName].(string)
	if name == "" {
		return nil, fmt.Errorf("%s holds a variable with no name", where)
	}
	if v, ok := item["value"]; ok {
		s, err := helmString(v)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", where, name, err)
		}
		return map[string]any{keyName: name, "value": s}, nil
	}
	if v, ok := item["valueFrom"]; ok {
		return map[string]any{keyName: name, "valueFrom": v}, nil
	}
	return map[string]any{keyName: name, "valueFrom": withoutKey(item, keyName)}, nil
}

func withoutKey(m map[string]any, key string) map[string]any {
	out := maps.Clone(m)
	delete(out, key)
	return out
}

// helmString is how Helm's toString prints a value from the chart's values:
// numbers are float64 there, so 1000000 prints as 1e+06.
func helmString(v any) (string, error) {
	switch value := v.(type) {
	case string:
		return value, nil
	case bool:
		return strconv.FormatBool(value), nil
	case json.Number:
		f, err := value.Float64()
		if err != nil {
			return "", err
		}
		return fmt.Sprint(f), nil
	}
	return "", unmodeled("value %v is not a string, number or bool", v)
}

func scalarString(v any) string {
	if v == nil {
		return ""
	}
	s, err := helmString(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return s
}

func renderEnvFrom(where string, v any) ([]any, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, unmodeled("%s is not a list", where)
	}
	out := make([]any, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, unmodeled("%s holds %v, which is not a map", where, item)
		}
		if err := checkKeys(where, m, []string{"secret", "secretRef", keyConfigMap, "configMapRef", "prefix"}); err != nil {
			return nil, err
		}
		rendered := map[string]any{}
		for _, ref := range []struct{ short, long string }{{"secret", "secretRef"}, {keyConfigMap, "configMapRef"}} {
			if name, ok := m[ref.short].(string); ok {
				rendered[ref.long] = map[string]any{keyName: name}
			}
			if r, ok := m[ref.long].(map[string]any); ok {
				if err := checkKeys(where+"."+ref.long, r, []string{keyName, "optional"}); err != nil {
					return nil, err
				}
				target := map[string]any{keyName: r[keyName]}
				if r["optional"] == true {
					target["optional"] = true
				}
				rendered[ref.long] = target
			}
		}
		if len(rendered) != 1 {
			return nil, unmodeled("%s holds %v, which names no single Secret or ConfigMap", where, m)
		}
		if p, _ := m["prefix"].(string); p != "" {
			rendered["prefix"] = p
		}
		out = append(out, rendered)
	}
	return out, nil
}

func (r *appTemplateRenderer) persistence() (map[string]map[string]any, error) {
	all, err := mapAt(r.values, "persistence")
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	for _, id := range sortedKeys(all) {
		p, ok := all[id].(map[string]any)
		if !ok {
			return nil, unmodeled("persistence.%s is not a map", id)
		}
		if err := checkKeys("persistence."+id, p, appTemplatePersistenceKeys); err != nil {
			return nil, err
		}
		if enabled, set := p[keyEnabled]; set && enabled != true {
			continue
		}
		out[id] = p
	}
	return out, nil
}

// volumeMounts is app-template's volumeMounts field. Like the chart, a mount
// without a path reuses the path of the one before it in the same item.
func (r *appTemplateRenderer) volumeMounts(container string) ([]any, error) {
	items, err := r.persistence()
	if err != nil {
		return nil, err
	}
	var out []any
	for _, id := range sortedKeys(items) {
		p := items[id]
		mountPath := "/" + id
		if p["type"] == keyHostPath {
			mountPath, _ = p[keyHostPath].(string)
		}
		_, hasGlobal := p["globalMounts"]
		_, hasAdvanced := p["advancedMounts"]
		if empty(p["globalMounts"]) && empty(p["advancedMounts"]) {
			out = append(out, map[string]any{keyName: id, "mountPath": mountPath})
			continue
		}
		var mounts []any
		if hasGlobal {
			list, ok := p["globalMounts"].([]any)
			if !ok && p["globalMounts"] != nil {
				return nil, unmodeled("persistence.%s.globalMounts is not a list", id)
			}
			mounts = append(mounts, list...)
		}
		if hasAdvanced {
			advanced, ok := p["advancedMounts"].(map[string]any)
			if !ok {
				return nil, unmodeled("persistence.%s.advancedMounts is not a map", id)
			}
			if byController, ok := advanced[resticController].(map[string]any); ok {
				list, ok := byController[container].([]any)
				if !ok && byController[container] != nil {
					return nil, unmodeled("persistence.%s.advancedMounts.restic.%s is not a list", id, container)
				}
				mounts = append(mounts, list...)
			}
		}
		for _, item := range mounts {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, unmodeled("persistence.%s holds the mount %v, which is not a map", id, item)
			}
			if err := checkKeys("persistence."+id+" mount", m, appTemplateMountKeys); err != nil {
				return nil, err
			}
			if path, _ := m[keyPath].(string); path != "" {
				mountPath = path
			}
			mount := map[string]any{keyName: id, "mountPath": mountPath}
			for _, field := range []string{"subPath", "subPathExpr", "readOnly", "mountPropagation"} {
				if v := m[field]; !empty(v) {
					mount[field] = v
				}
			}
			out = append(out, mount)
		}
	}
	return out, nil
}

// volumes is app-template's volumes field for the restic controller.
func (r *appTemplateRenderer) volumes() ([]any, error) {
	items, err := r.persistence()
	if err != nil {
		return nil, err
	}
	var out []any
	for _, id := range sortedKeys(items) {
		p := items[id]
		hasGlobal := !empty(p["globalMounts"])
		hasAdvanced := !empty(p["advancedMounts"])
		var forController any
		if advanced, ok := p["advancedMounts"].(map[string]any); ok {
			forController = advanced[resticController]
		}
		if !hasGlobal && hasAdvanced && empty(forController) {
			continue
		}
		volume, err := volumeOf(id, p)
		if err != nil {
			return nil, err
		}
		out = append(out, volume)
	}
	return out, nil
}

func volumeOf(id string, p map[string]any) (map[string]any, error) {
	volume := map[string]any{keyName: id}
	kind, _ := p["type"].(string)
	copyFields := func(target map[string]any, fields ...string) {
		for _, f := range fields {
			if v := p[f]; !empty(v) {
				target[f] = v
			}
		}
	}
	switch kind {
	case "", volumeClaim:
		claim, _ := p["existingClaim"].(string)
		if claim == "" {
			return nil, unmodeled("persistence.%s is a claim the chart creates", id)
		}
		volume[volumeClaim] = map[string]any{"claimName": claim}
	case keyConfigMap:
		source := map[string]any{keyName: p[keyName]}
		copyFields(source, "defaultMode", "items")
		volume[keyConfigMap] = source
	case volumeSecret:
		source := map[string]any{"secretName": p[keyName]}
		copyFields(source, "defaultMode", "items")
		volume[volumeSecret] = source
	case volumeEmptyDir:
		source := map[string]any{}
		copyFields(source, "medium", "sizeLimit")
		volume[volumeEmptyDir] = source
	case keyHostPath:
		source := map[string]any{keyPath: p[keyHostPath]}
		if t := p["hostPathType"]; !empty(t) {
			source["type"] = t
		}
		volume[keyHostPath] = source
	case "nfs":
		volume["nfs"] = map[string]any{"server": p["server"], keyPath: p[keyPath]}
	case "custom":
		spec, ok := p["volumeSpec"].(map[string]any)
		if !ok {
			return nil, unmodeled("persistence.%s has no volumeSpec", id)
		}
		volume = maps.Clone(spec)
		volume[keyName] = id
	default:
		return nil, unmodeled("persistence.%s has type %q", id, kind)
	}
	return volume, nil
}

func mapAt(m map[string]any, key string) (map[string]any, error) {
	switch v := m[key].(type) {
	case nil:
		return map[string]any{}, nil
	case map[string]any:
		return v, nil
	}
	return nil, unmodeled("%s is not a map", key)
}

func checkKeys(where string, m map[string]any, allowed []string) error {
	var unknown []string
	for k := range m {
		if !slices.Contains(allowed, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return unmodeled("%s sets %s", where, strings.Join(unknown, ", "))
	}
	return nil
}

// refuseTemplates refuses a Go template anywhere in the values: app-template
// runs most strings through tpl, which this renderer does not.
func refuseTemplates(where string, v any) error {
	switch value := v.(type) {
	case string:
		if strings.Contains(value, "{{") {
			return unmodeled("%s holds a Helm template, which the chart would expand", where)
		}
	case map[string]any:
		for _, k := range sortedKeys(value) {
			if err := refuseTemplates(where+"."+k, value[k]); err != nil {
				return err
			}
		}
	case []any:
		for i, item := range value {
			if err := refuseTemplates(fmt.Sprintf("%s[%d]", where, i), item); err != nil {
				return err
			}
		}
	}
	return nil
}

func mergeStrings(dst map[string]string, v any, where string) error {
	if v == nil {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return unmodeled("%s is not a map", where)
	}
	for k, value := range m {
		s, err := helmString(value)
		if err != nil {
			return fmt.Errorf("%s.%s: %w", where, k, err)
		}
		dst[k] = s
	}
	return nil
}

func integer(v any) (int64, error) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, fmt.Errorf("%v is not a number", v)
	}
	return n.Int64()
}

// empty is sprig's empty: nil, false, zero, and an empty string, map or list.
func empty(v any) bool {
	switch value := v.(type) {
	case nil:
		return true
	case bool:
		return !value
	case string:
		return value == ""
	case json.Number:
		f, err := value.Float64()
		return err == nil && f == 0
	case map[string]any:
		return len(value) == 0
	case []any:
		return len(value) == 0
	}
	return false
}

func strictDecode(in any, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(out)
}
