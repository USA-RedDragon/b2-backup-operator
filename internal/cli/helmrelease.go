package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// Errors returned when reading a HelmRelease.
var (
	ErrNoHelmRelease         = errors.New("no HelmRelease found")
	ErrNoResticController    = errors.New("no restic controller in the HelmRelease")
	ErrNoResticScript        = errors.New("no restic command in the HelmRelease")
	ErrMultipleClaims        = errors.New("more than one persistent volume claim is mounted in the restic container")
	ErrMultipleDBSecrets     = errors.New("more than one secret is mounted in the restic container")
	ErrUnsupportedMount      = errors.New("the restic container mounts a volume a ScheduledBackup cannot")
	ErrUnsupportedCommand    = errors.New("the restic container does not run sh -c <script>")
	ErrUnsupportedController = errors.New("the HelmRelease runs something a ScheduledBackup cannot")
	ErrUnsupportedValues     = errors.New("the HelmRelease sets something migrate cannot carry over")
)

const resticController = "restic"

var helmReleaseFiles = []string{"helmrelease.yaml", "helm.yaml"}

type helmRelease struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		Chart *struct {
			Spec struct {
				Chart string `json:"chart"`
			} `json:"spec"`
		} `json:"chart"`
		ChartRef *struct {
			Name string `json:"name"`
		} `json:"chartRef"`
		TargetNamespace string          `json:"targetNamespace"`
		ValuesFrom      []any           `json:"valuesFrom"`
		PostRenderers   []any           `json:"postRenderers"`
		RawValues       json.RawMessage `json:"values"`

		Values hrValues `json:"-"`
	} `json:"spec"`
}

// hrValues is every app-template value migrate reads. Values are decoded
// strictly against it, so a key it does not know is refused, not dropped.
type hrValues struct {
	DefaultPodOptions hrPodOptions             `json:"defaultPodOptions"`
	Controllers       map[string]hrController  `json:"controllers"`
	Persistence       map[string]hrPersistence `json:"persistence"`
}

type hrPodOptions struct {
	Affinity                     *corev1.Affinity           `json:"affinity"`
	SecurityContext              *corev1.PodSecurityContext `json:"securityContext"`
	NodeSelector                 map[string]string          `json:"nodeSelector"`
	Tolerations                  []corev1.Toleration        `json:"tolerations"`
	PriorityClassName            string                     `json:"priorityClassName"`
	Labels                       map[string]string          `json:"labels"`
	AutomountServiceAccountToken *bool                      `json:"automountServiceAccountToken"`
}

type hrController struct {
	Enabled     *bool             `json:"enabled"`
	Type        string            `json:"type"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	CronJob     struct {
		Schedule                string `json:"schedule"`
		ConcurrencyPolicy       string `json:"concurrencyPolicy"`
		TimeZone                string `json:"timeZone"`
		Suspend                 bool   `json:"suspend"`
		TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished"`
		SuccessfulJobsHistory   *int32 `json:"successfulJobsHistory"`
		FailedJobsHistory       *int32 `json:"failedJobsHistory"`
	} `json:"cronjob"`
	Pod            hrPodOptions           `json:"pod"`
	Containers     map[string]hrContainer `json:"containers"`
	InitContainers map[string]hrContainer `json:"initContainers"`
}

func (c hrController) enabled() bool { return c.Enabled == nil || *c.Enabled }

// enabledContainers returns the names of a container map's enabled containers.
func enabledContainers(containers map[string]hrContainer) []string {
	var out []string
	for _, name := range sortedKeys(containers) {
		if c := containers[name]; c.Enabled == nil || *c.Enabled {
			out = append(out, name)
		}
	}
	return out
}

type hrContainer struct {
	Enabled         *bool                        `json:"enabled"`
	Image           hrImage                      `json:"image"`
	Command         []string                     `json:"command"`
	Args            []string                     `json:"args"`
	WorkingDir      string                       `json:"workingDir"`
	Resources       *corev1.ResourceRequirements `json:"resources"`
	SecurityContext *corev1.SecurityContext      `json:"securityContext"`
	EnvFrom         []hrEnvFrom                  `json:"envFrom"`

	Env map[string]any `json:"env"`
}

type hrEnvFrom struct {
	Secret       string    `json:"secret"`
	SecretRef    *hrEnvRef `json:"secretRef"`
	ConfigMap    string    `json:"configMap"`
	ConfigMapRef *hrEnvRef `json:"configMapRef"`
	Prefix       string    `json:"prefix"`
}

type hrEnvRef struct {
	Name     string `json:"name"`
	Optional *bool  `json:"optional"`
}

type hrPersistence struct {
	Enabled        *bool                           `json:"enabled"`
	Type           string                          `json:"type"`
	Name           string                          `json:"name"`
	ExistingClaim  string                          `json:"existingClaim"`
	StorageClass   string                          `json:"storageClass"`
	AccessMode     string                          `json:"accessMode"`
	Size           string                          `json:"size"`
	Retain         *bool                           `json:"retain"`
	SizeLimit      string                          `json:"sizeLimit"`
	GlobalMounts   []hrMount                       `json:"globalMounts"`
	AdvancedMounts map[string]map[string][]hrMount `json:"advancedMounts"`
}

type hrMount struct {
	Path             string `json:"path"`
	ReadOnly         bool   `json:"readOnly"`
	SubPath          string `json:"subPath"`
	SubPathExpr      string `json:"subPathExpr"`
	MountPropagation string `json:"mountPropagation"`
}

const (
	volumeClaim    = "persistentVolumeClaim"
	volumeSecret   = "secret"
	volumeEmptyDir = "emptyDir"
)

type hrVolume struct {
	key      string
	kind     string
	name     string
	path     string
	readOnly bool
}

func resolveHelmRelease(arg string) (string, error) {
	info, err := os.Stat(arg)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return arg, nil
	}
	for _, name := range helmReleaseFiles {
		path := filepath.Join(arg, name)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("%w in %s: looked for %s", ErrNoHelmRelease, arg, strings.Join(helmReleaseFiles, ", "))
}

func readHelmRelease(path string) (*helmRelease, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var hr helmRelease
	if err := yaml.Unmarshal(raw, &hr); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if err := hr.decodeValues(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	namespace, err := releaseNamespace(path, hr.Metadata.Namespace, hr.Spec.TargetNamespace)
	if err != nil {
		return nil, err
	}
	hr.Metadata.Namespace = namespace
	return &hr, nil
}

// decodeValues reads spec.values strictly, after checking that nothing but
// spec.values and the app-template chart decides what the release renders.
func (hr *helmRelease) decodeValues() error {
	spec := hr.Spec
	switch {
	case len(spec.ValuesFrom) > 0:
		return fmt.Errorf("%w: spec.valuesFrom adds values migrate cannot see", ErrUnsupportedValues)
	case len(spec.PostRenderers) > 0:
		return fmt.Errorf("%w: spec.postRenderers change what the chart renders", ErrUnsupportedValues)
	case spec.Chart != nil && spec.Chart.Spec.Chart != appTemplateChart:
		return fmt.Errorf("%w: the chart is %s, not %s", ErrUnsupportedValues, spec.Chart.Spec.Chart, appTemplateChart)
	case spec.ChartRef != nil && spec.ChartRef.Name != appTemplateChart:
		return fmt.Errorf("%w: the chart is %s, not %s", ErrUnsupportedValues, spec.ChartRef.Name, appTemplateChart)
	}
	if len(spec.RawValues) == 0 {
		return nil
	}
	var generic any
	if err := json.Unmarshal(spec.RawValues, &generic); err != nil {
		return err
	}
	if where := findTemplate("values", generic); where != "" {
		return fmt.Errorf("%w: %s holds a Helm template, which app-template would expand", ErrUnsupportedValues, where)
	}
	d := json.NewDecoder(bytes.NewReader(spec.RawValues))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(&hr.Spec.Values); err != nil {
		return fmt.Errorf("%w: spec.values: %w", ErrUnsupportedValues, err)
	}
	return nil
}

// releaseNamespace is the namespace the CronJob of the HelmRelease in path
// lands in. The HelmRelease's own spec.targetNamespace wins, as that is where
// Helm installs it; then the targetNamespace of the Flux Kustomization that
// applies the file; then the outermost kustomization.yaml namespace between
// that Kustomization's path and the file; then metadata.namespace.
func releaseNamespace(path, declared, releaseTarget string) (string, error) {
	if releaseTarget != "" {
		return releaseTarget, nil
	}
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	top, target, err := fluxKustomizationFor(dir)
	if err != nil {
		return "", err
	}
	if target != "" {
		return target, nil
	}
	namespace := declared
	for d := dir; ; d = filepath.Dir(d) {
		if ns := kustomizeNamespace(d); ns != "" {
			namespace = ns
		}
		if top == "" || d == top || d == filepath.Dir(d) {
			break
		}
	}
	return namespace, nil
}

// fluxKustomizationFor finds the Flux Kustomization that applies dir: the one
// in dir or a directory above it, up to the repository root, whose spec.path
// holds dir most closely. It returns that path and its targetNamespace.
func fluxKustomizationFor(dir string) (string, string, error) {
	root := ""
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			root = d
			break
		}
		if d == filepath.Dir(d) {
			return "", "", nil
		}
	}
	var bestPath, bestTarget string
	conflict := false
	for d := dir; ; d = filepath.Dir(d) {
		files, _ := filepath.Glob(filepath.Join(d, "*.yaml"))
		for _, f := range files {
			for _, k := range fluxKustomizations(f) {
				p := filepath.Join(root, k.Spec.Path)
				if p != dir && !strings.HasPrefix(dir, p+string(filepath.Separator)) {
					continue
				}
				switch {
				case len(p) > len(bestPath):
					bestPath, bestTarget, conflict = p, k.Spec.TargetNamespace, false
				case p == bestPath && k.Spec.TargetNamespace != bestTarget:
					conflict = true
				}
			}
		}
		if d == root {
			break
		}
	}
	if conflict {
		return "", "", fmt.Errorf("%w: more than one Flux Kustomization applies %s, with different "+
			"targetNamespaces", ErrUnsupportedValues, dir)
	}
	return bestPath, bestTarget, nil
}

type fluxKustomization struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       struct {
		Path            string `json:"path"`
		TargetNamespace string `json:"targetNamespace"`
	} `json:"spec"`
}

func fluxKustomizations(file string) []fluxKustomization {
	raw, err := os.ReadFile(file)
	if err != nil || !bytes.Contains(raw, []byte("kustomize.toolkit.fluxcd.io")) {
		return nil
	}
	var out []fluxKustomization
	for doc := range strings.SplitSeq(string(raw), "\n---") {
		var k fluxKustomization
		if yaml.Unmarshal([]byte(doc), &k) != nil || k.Kind != "Kustomization" ||
			!strings.HasPrefix(k.APIVersion, "kustomize.toolkit.fluxcd.io/") {
			continue
		}
		out = append(out, k)
	}
	return out
}

func kustomizeNamespace(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml"))
	if err != nil {
		return ""
	}
	var k struct {
		Namespace string `json:"namespace"`
	}
	if yaml.Unmarshal(raw, &k) != nil {
		return ""
	}
	return k.Namespace
}

func (hr *helmRelease) backupController() (hrController, hrContainer, error) {
	controller, ok := hr.Spec.Values.Controllers[resticController]
	if !ok {
		return hrController{}, hrContainer{}, ErrNoResticController
	}
	switch {
	case !controller.enabled():
		return hrController{}, hrContainer{}, fmt.Errorf("%w: the restic controller is disabled, "+
			"so the HelmRelease backs nothing up", ErrUnsupportedController)
	case controller.Type != controllerTypeCronJob:
		return hrController{}, hrContainer{}, fmt.Errorf("%w: the restic controller is a %s, not a cronjob",
			ErrUnsupportedController, cmpOr(controller.Type, "deployment"))
	}
	if names := enabledContainers(controller.InitContainers); len(names) > 0 {
		return hrController{}, hrContainer{}, fmt.Errorf("%w: the restic controller runs init containers %s "+
			"before the backup", ErrUnsupportedController, strings.Join(names, ", "))
	}
	if names := enabledContainers(controller.Containers); !slices.Equal(names, []string{resticController}) {
		if !slices.Contains(names, resticController) {
			return hrController{}, hrContainer{}, ErrNoResticScript
		}
		return hrController{}, hrContainer{}, fmt.Errorf("%w: the restic controller runs containers %s, "+
			"and a ScheduledBackup runs one", ErrUnsupportedController, strings.Join(names, ", "))
	}
	return controller, controller.Containers[resticController], nil
}

// otherControllers checks every enabled controller but restic. The only one a
// migration may drop is a Job that runs restic init, which the Repository does
// itself; it returns a note for each.
func (hr *helmRelease) otherControllers() ([]string, error) {
	var notes []string
	for _, name := range sortedKeys(hr.Spec.Values.Controllers) {
		c := hr.Spec.Values.Controllers[name]
		if name == resticController || !c.enabled() {
			continue
		}
		if !c.runsResticInit() {
			return nil, fmt.Errorf("%w: controller %s is not a Job that only runs restic init, "+
				"and the migration would drop it", ErrUnsupportedController, name)
		}
		notes = append(notes, fmt.Sprintf("%s: this Job ran restic init after install. It is not migrated: "+
			"the Repository initializes a new repository or adopts the existing one itself.", name))
	}
	return notes, nil
}

func (c hrController) runsResticInit() bool {
	names := enabledContainers(c.Containers)
	if c.Type != controllerTypeJob || len(enabledContainers(c.InitContainers)) > 0 || len(names) != 1 {
		return false
	}
	script, err := c.Containers[names[0]].script()
	if err != nil {
		return false
	}
	script = strings.Join(strings.Fields(script), " ")
	return script == "restic init" || script == "restic init || true"
}

// podOptions is the restic pod's options: the controller's pod value of each
// wins over defaultPodOptions, as under app-template's default overwrite
// strategy, where a map wins even when empty and a list or string only when not.
func (hr *helmRelease) podOptions() hrPodOptions {
	def := hr.Spec.Values.DefaultPodOptions
	pod := hr.Spec.Values.Controllers[resticController].Pod
	out := def
	if pod.Affinity != nil {
		out.Affinity = pod.Affinity
	}
	if pod.SecurityContext != nil {
		out.SecurityContext = pod.SecurityContext
	}
	if pod.NodeSelector != nil {
		out.NodeSelector = pod.NodeSelector
	}
	if len(pod.Tolerations) > 0 {
		out.Tolerations = pod.Tolerations
	}
	if pod.PriorityClassName != "" {
		out.PriorityClassName = pod.PriorityClassName
	}
	if pod.Labels != nil {
		out.Labels = pod.Labels
	}
	if pod.AutomountServiceAccountToken != nil {
		out.AutomountServiceAccountToken = pod.AutomountServiceAccountToken
	}
	return out
}

func (hr *helmRelease) affinity() *corev1.Affinity { return hr.podOptions().Affinity }

func (hr *helmRelease) podSecurityContext() *corev1.PodSecurityContext {
	return hr.podOptions().SecurityContext
}

type hrImage struct {
	Repository string `json:"repository"`
	Tag        any    `json:"tag"`
	Digest     string `json:"digest"`
	PullPolicy string `json:"pullPolicy"`
}

// cacheDir is where restic keeps its cache in the container: --cache-dir, then
// RESTIC_CACHE_DIR, then the default of the image the operator's is built on.
// It is empty when that cannot be told.
func (c hrContainer) cacheDir(script string) string {
	if m := cacheDirFlag.FindStringSubmatch(script); m != nil {
		return pathpkg.Clean(strings.Trim(m[1], `'"`))
	}
	if dir := c.env(envResticCacheDir); dir != "" {
		return pathpkg.Clean(dir)
	}
	if isUpstreamImage(c.Image.Repository) || isOperatorImage(c.Image.Repository+":") {
		return operatorCacheDir
	}
	return ""
}

var scriptShells = []string{"sh", "/bin/sh"}

// script returns the shell script the container runs: app-template appends
// args to command, and the operator runs its script as sh -c, so that is the
// only shape read.
func (c hrContainer) script() (string, error) {
	if len(c.Command) == 0 {
		return "", fmt.Errorf("%w: it sets no command, so the image's entrypoint runs", ErrUnsupportedCommand)
	}
	argv := append(slices.Clone(c.Command), c.Args...)
	if len(argv) != 3 || !slices.Contains(scriptShells, argv[0]) || argv[1] != "-c" {
		return "", fmt.Errorf("%w: it runs %q", ErrUnsupportedCommand, argv)
	}
	return argv[2], nil
}

// checkNumbers refuses a number in env whose YAML text app-template would
// print differently: Helm reads values as float64, so 1000000 reaches the
// container as 1e+06, and carrying either over would surprise someone.
func (c hrContainer) checkNumbers() error {
	for _, name := range sortedKeys(c.Env) {
		n, ok := c.Env[name].(json.Number)
		if !ok {
			continue
		}
		if rendered, err := helmString(n); err != nil || rendered != n.String() {
			return fmt.Errorf("%w: env %s is %s, which app-template renders as %s; quote it in the HelmRelease "+
				"so the value is the one meant", ErrUnsupportedValues, name, n, rendered)
		}
	}
	return nil
}

func (c hrContainer) env(name string) string {
	v, ok := c.Env[name]
	if !ok {
		return ""
	}
	switch value := v.(type) {
	case string:
		return value
	case json.Number, bool:
		s, err := helmString(value)
		if err != nil {
			return ""
		}
		return s
	default:
		return ""
	}
}

// volumes returns what the restic container mounts. Like app-template, it
// applies globalMounts and the container's advancedMounts together, and a
// mount without a path reuses the path of the one before it.
func (hr *helmRelease) volumes() ([]hrVolume, error) {
	var out []hrVolume
	for _, key := range sortedKeys(hr.Spec.Values.Persistence) {
		p := hr.Spec.Values.Persistence[key]
		if p.Enabled != nil && !*p.Enabled {
			continue
		}
		kind, name := p.Type, p.Name
		if p.ExistingClaim != "" && (kind == "" || kind == volumeClaim) {
			kind, name = volumeClaim, p.ExistingClaim
		}

		mounts := []hrMount{{}}
		if len(p.GlobalMounts) > 0 || len(p.AdvancedMounts) > 0 {
			mounts = append(slices.Clone(p.GlobalMounts), p.AdvancedMounts[resticController][resticController]...)
		}
		path := "/" + key
		for _, m := range mounts {
			switch {
			case m.SubPath != "":
				return nil, fmt.Errorf("%w: %s mounts subPath %s", ErrUnsupportedMount, key, m.SubPath)
			case m.SubPathExpr != "":
				return nil, fmt.Errorf("%w: %s mounts subPathExpr %s", ErrUnsupportedMount, key, m.SubPathExpr)
			case m.MountPropagation != "":
				return nil, fmt.Errorf("%w: %s sets mountPropagation", ErrUnsupportedMount, key)
			}
			if m.Path != "" {
				path = m.Path
			}
			out = append(out, hrVolume{key: key, kind: kind, name: name, path: path, readOnly: m.ReadOnly})
		}
	}
	return out, nil
}

func volumeKeys(volumes []hrVolume) string {
	keys := make([]string, 0, len(volumes))
	for _, v := range volumes {
		keys = append(keys, v.key)
	}
	return strings.Join(keys, ", ")
}

func splitArgs(line string) []string {
	var args []string
	var current strings.Builder
	var quote rune
	inWord := false

	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t':
			if inWord {
				args = append(args, current.String())
				current.Reset()
				inWord = false
			}
		default:
			current.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		args = append(args, current.String())
	}
	return args
}

func flagValues(args []string, name string) []string {
	prefix := "--" + name + "="
	var out []string
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, prefix); ok {
			out = append(out, v)
		}
	}
	return out
}

func parseInt32(s string) (*int32, error) {
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return nil, err
	}
	v := int32(n)
	return &v, nil
}

func joinContinuations(script string) []string {
	var lines []string
	var current strings.Builder

	for line := range strings.SplitSeq(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if after, ok := strings.CutSuffix(trimmed, "\\"); ok {
			current.WriteString(strings.TrimSpace(after))
			current.WriteString(" ")
			continue
		}
		current.WriteString(trimmed)
		if joined := strings.TrimSpace(current.String()); joined != "" {
			lines = append(lines, joined)
		}
		current.Reset()
	}
	if joined := strings.TrimSpace(current.String()); joined != "" {
		lines = append(lines, joined)
	}
	return lines
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
