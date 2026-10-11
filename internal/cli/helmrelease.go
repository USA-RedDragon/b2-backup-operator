package cli

import (
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
)

const resticController = "restic"

var helmReleaseFiles = []string{"helmrelease.yaml", "helm.yaml"}

type helmRelease struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		Values struct {
			DefaultPodOptions struct {
				Affinity        *corev1.Affinity           `json:"affinity"`
				SecurityContext *corev1.PodSecurityContext `json:"securityContext"`
			} `json:"defaultPodOptions"`
			Controllers map[string]hrController  `json:"controllers"`
			Persistence map[string]hrPersistence `json:"persistence"`
		} `json:"values"`
	} `json:"spec"`
}

type hrController struct {
	Enabled *bool  `json:"enabled"`
	Type    string `json:"type"`
	CronJob struct {
		Schedule          string `json:"schedule"`
		ConcurrencyPolicy string `json:"concurrencyPolicy"`
		TimeZone          string `json:"timeZone"`
	} `json:"cronjob"`
	Pod struct {
		Affinity        *corev1.Affinity           `json:"affinity"`
		SecurityContext *corev1.PodSecurityContext `json:"securityContext"`
	} `json:"pod"`
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

	Env map[string]any `json:"env"`
}

type hrPersistence struct {
	Enabled        *bool                           `json:"enabled"`
	Type           string                          `json:"type"`
	Name           string                          `json:"name"`
	ExistingClaim  string                          `json:"existingClaim"`
	StorageClass   string                          `json:"storageClass"`
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
	hr.Metadata.Namespace = releaseNamespace(path, hr.Metadata.Namespace)
	return &hr, nil
}

// releaseNamespace is the namespace a HelmRelease file is applied in.
func releaseNamespace(path, declared string) string {
	if ns := kustomizeNamespace(filepath.Dir(path)); ns != "" {
		return ns
	}
	return declared
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

func (hr *helmRelease) affinity() *corev1.Affinity {
	if a := hr.Spec.Values.Controllers[resticController].Pod.Affinity; a != nil {
		return a
	}
	return hr.Spec.Values.DefaultPodOptions.Affinity
}

func (hr *helmRelease) podSecurityContext() *corev1.PodSecurityContext {
	if sc := hr.Spec.Values.Controllers[resticController].Pod.SecurityContext; sc != nil {
		return sc
	}
	return hr.Spec.Values.DefaultPodOptions.SecurityContext
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

func (c hrContainer) env(name string) string {
	v, ok := c.Env[name]
	if !ok {
		return ""
	}
	switch value := v.(type) {
	case string:
		return value
	case int, int64, float64, bool:
		return fmt.Sprint(value)
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
