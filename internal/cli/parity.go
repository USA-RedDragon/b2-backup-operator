package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	pathpkg "path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

const (
	labelController = "app.kubernetes.io/controller"
	labelName       = "app.kubernetes.io/name"
	labelInstance   = "app.kubernetes.io/instance"

	envResticHost     = "RESTIC_HOST"
	envResticCacheDir = "RESTIC_CACHE_DIR"
	volumeDiffers     = "VOLUME DIFFERS"
	verdictEquivalent = "EQUIVALENT"

	databaseMountToken     = "<database Secret mount>"
	backupCNPGDefaultMount = "/postgresql-app"
	repositorySecretToken  = "the Repository's Secret"
	credentialsSecretName  = "restic-b2"
	operatorCacheDir       = "/cache"
	podHostname            = "(the pod's hostname)"
	unsetSetting           = "unset"
)

// upstreamImage is the image hand-written backups ran, pulled directly or
// through a mirror. The operator's own image is built on it.
const upstreamImage = "ghcr.io/gabe565/restic"

// parityView is everything parity compares, read the same way from the
// original's CronJob and from the one the operator builds.
type parityView struct {
	schedule    string
	timeZone    string
	suspend     bool
	concurrency string
	jobSettings map[string]string

	shell      []string
	script     string
	image      string
	env        map[string]string
	host       string
	envFrom    string
	volumes    string
	dbSecret   string
	workingDir string
	resources  string
	readsAll   bool

	podSecurityContext       *corev1.PodSecurityContext
	containerSecurityContext *corev1.SecurityContext

	affinity          *corev1.Affinity
	nodeSelector      string
	tolerations       string
	priorityClassName string
	labels            map[string]string
	annotations       map[string]string

	extraContainers []string
	podRest         string
	containerRest   string
}

// jobSettingNotes are CronJob and Job settings that change how a run is
// retried and kept, not what it backs up, so a change is a note.
var jobSettingNotes = []string{keyBackoffLimit, keyStartingDeadline, "successfulJobsHistoryLimit",
	"failedJobsHistoryLimit", keyTTLAfterFinished}

// jobSettingDifferences change how much of a run may happen at all.
var jobSettingDifferences = []string{keyActiveDeadline, keyParallelism, "completions"}

func viewOf(cj *batchv1.CronJob, namespace, repositorySecret, defaultZone string) (parityView, error) {
	var v parityView
	spec := cj.Spec
	job := spec.JobTemplate.Spec
	pod := job.Template.Spec

	v.schedule = spec.Schedule
	v.timeZone = ptr.Deref(spec.TimeZone, defaultZone)
	v.suspend = ptr.Deref(spec.Suspend, false)
	v.concurrency = string(spec.ConcurrencyPolicy)
	if v.concurrency == "" {
		v.concurrency = string(batchv1.AllowConcurrent)
	}
	v.jobSettings = map[string]string{
		keyBackoffLimit:              int32Setting(job.BackoffLimit, "6"),
		keyStartingDeadline:          int64Setting(spec.StartingDeadlineSeconds),
		"successfulJobsHistoryLimit": int32Setting(spec.SuccessfulJobsHistoryLimit, "3"),
		"failedJobsHistoryLimit":     int32Setting(spec.FailedJobsHistoryLimit, "1"),
		keyTTLAfterFinished:          int32Setting(job.TTLSecondsAfterFinished, unsetSetting),
		keyActiveDeadline:            int64Setting(job.ActiveDeadlineSeconds),
		keyParallelism:               int32Setting(job.Parallelism, "1"),
		"completions":                int32Setting(job.Completions, "1"),
	}

	container, err := backupContainer(pod)
	if err != nil {
		return v, err
	}
	for _, c := range append(slices.Clone(pod.InitContainers), pod.Containers...) {
		if c.Name != container.Name {
			v.extraContainers = append(v.extraContainers, c.Name)
		}
	}

	argv := append(slices.Clone(container.Command), container.Args...)
	if len(argv) == 3 && slices.Contains(scriptShells, argv[0]) && argv[1] == "-c" {
		v.shell, v.script = argv[:2], argv[2]
	} else {
		v.shell, v.script = argv, ""
	}
	v.image = container.Image

	env := map[string]string{}
	v.host = podHostname
	for _, e := range container.Env {
		if e.Name == envResticHost {
			v.host = resticHostOf(e, namespace)
			continue
		}
		env[e.Name] = envValue(e)
	}
	v.envFrom = describeEnvFrom(container.EnvFrom, repositorySecret)
	secretMounts, err := v.readMounts(pod, container, effectiveCacheDir(env, v.script, v.image))
	if err != nil {
		return v, err
	}
	delete(env, envResticCacheDir)
	normalizeCNPGSecretPath(env, v.script, secretMounts)
	v.env = env

	v.workingDir = pathpkg.Clean(cmpOr(container.WorkingDir, "/"))
	v.resources = canonicalJSON(container.Resources)
	v.podSecurityContext, v.containerSecurityContext = pod.SecurityContext, container.SecurityContext
	v.readsAll = backup.ReadsEveryFile(pod.SecurityContext, container.SecurityContext)

	v.affinity = pod.Affinity
	if v.affinity != nil && *v.affinity == (corev1.Affinity{}) {
		v.affinity = nil
	}
	v.nodeSelector = canonicalJSON(pod.NodeSelector)
	v.tolerations = canonicalJSON(pod.Tolerations)
	v.priorityClassName = pod.PriorityClassName
	v.labels = job.Template.Labels
	v.annotations = job.Template.Annotations

	v.podRest, v.containerRest = remainingSpecs(pod, container)
	return v, nil
}

func resticHostOf(e corev1.EnvVar, namespace string) string {
	if e.ValueFrom == nil {
		return e.Value
	}
	if ref := e.ValueFrom.FieldRef; ref != nil && ref.FieldPath == "metadata.namespace" {
		return namespace
	}
	return envValue(e)
}

func envValue(e corev1.EnvVar) string {
	if e.ValueFrom != nil {
		return "<valueFrom " + canonicalJSON(e.ValueFrom) + ">"
	}
	return e.Value
}

// describeEnvFrom names what the container reads its environment from, with the
// Repository's Secret and the restic-envs and restic-b2 Secrets it replaces
// read as one.
func describeEnvFrom(sources []corev1.EnvFromSource, repositorySecret string) string {
	out := make([]string, 0, len(sources))
	for _, e := range sources {
		var s string
		switch {
		case e.SecretRef != nil && (e.SecretRef.Name == repositorySecret || e.SecretRef.Name == passwordSecretName):
			s = repositorySecretToken
		case e.SecretRef != nil && e.SecretRef.Name == credentialsSecretName:
			continue
		case e.SecretRef != nil:
			s = "Secret " + e.SecretRef.Name + optionalSuffix(e.SecretRef.Optional)
		case e.ConfigMapRef != nil:
			s = "ConfigMap " + e.ConfigMapRef.Name + optionalSuffix(e.ConfigMapRef.Optional)
		}
		if e.Prefix != "" {
			s += " with prefix " + e.Prefix
		}
		out = append(out, s)
	}
	slices.Sort(out)
	return strings.Join(slices.Compact(out), " and ")
}

// readMounts describes the container's mounts, leaving out restic's cache, and
// returns where Secrets are mounted.
func (v *parityView) readMounts(pod corev1.PodSpec, container corev1.Container, cacheDir string) (map[string]bool, error) {
	volumes := map[string]corev1.Volume{}
	for _, vol := range pod.Volumes {
		volumes[vol.Name] = vol
	}
	var mounts, secretNames, secrets []string
	secretMounts := map[string]bool{}
	for _, m := range container.VolumeMounts {
		vol, ok := volumes[m.Name]
		if !ok {
			return nil, fmt.Errorf("the backup container mounts %s, which is no volume of the pod", m.Name)
		}
		mountPath := pathpkg.Clean(m.MountPath)
		switch {
		case vol.Secret != nil:
			secretNames = append(secretNames, vol.Secret.SecretName)
			secrets = append(secrets, "Secret "+vol.Secret.SecretName+" at "+mountPath)
			secretMounts[mountPath] = true
		case cacheDir != "" && mountPath == cacheDir && !scriptNames(v.script, cacheDir):
		default:
			mounts = append(mounts, describeMount(vol, m))
		}
	}
	if usesDatabase(v.script) {
		v.dbSecret = strings.Join(secretNames, " and ")
	} else {
		mounts = append(mounts, secrets...)
	}
	slices.Sort(mounts)
	v.volumes = strings.Join(mounts, " and ")
	return secretMounts, nil
}

// remainingSpecs is what is left of the pod and container spec once everything
// parity compares on its own, and the settings the operator owns, are taken out.
func remainingSpecs(pod corev1.PodSpec, container corev1.Container) (string, string) {
	rest := pod.DeepCopy()
	rest.Containers, rest.InitContainers, rest.Volumes = nil, nil, nil
	rest.Affinity, rest.NodeSelector, rest.Tolerations, rest.PriorityClassName = nil, nil, nil, ""
	rest.SecurityContext, rest.RestartPolicy, rest.AutomountServiceAccountToken = nil, "", nil
	rest.EnableServiceLinks = nil
	if rest.ServiceAccountName == "default" {
		rest.ServiceAccountName = ""
	}
	if rest.DNSPolicy == corev1.DNSClusterFirst {
		rest.DNSPolicy = ""
	}

	c := container.DeepCopy()
	c.Name, c.Image, c.ImagePullPolicy, c.Command, c.Args = "", "", "", nil, nil
	c.Env, c.EnvFrom, c.VolumeMounts, c.WorkingDir = nil, nil, nil, ""
	c.Resources, c.SecurityContext = corev1.ResourceRequirements{}, nil
	return canonicalJSON(rest), canonicalJSON(c)
}

func int32Setting(p *int32, def string) string {
	if p == nil {
		return def
	}
	return strconv.Itoa(int(*p))
}

func int64Setting(p *int64) string {
	if p == nil {
		return unsetSetting
	}
	return strconv.FormatInt(*p, 10)
}

func optionalSuffix(optional *bool) string {
	if ptr.Deref(optional, false) {
		return " (optional)"
	}
	return ""
}

func backupContainer(pod corev1.PodSpec) (corev1.Container, error) {
	for _, c := range pod.Containers {
		if c.Name == resticController {
			return c, nil
		}
	}
	if len(pod.Containers) == 1 {
		return pod.Containers[0], nil
	}
	return corev1.Container{}, fmt.Errorf("%w: the pod runs %d containers and none is named restic",
		ErrNoResticScript, len(pod.Containers))
}

// effectiveCacheDir is where restic keeps its cache: --cache-dir, then
// RESTIC_CACHE_DIR, then the default the image sets. It is empty when unknown.
func effectiveCacheDir(env map[string]string, script, image string) string {
	if m := cacheDirFlag.FindStringSubmatch(script); m != nil {
		return pathpkg.Clean(strings.Trim(m[1], `'"`))
	}
	if dir, ok := env[envResticCacheDir]; ok {
		return pathpkg.Clean(dir)
	}
	if isUpstreamImage(image) || isOperatorImage(image) {
		return operatorCacheDir
	}
	return ""
}

var cacheDirFlag = regexp.MustCompile(`--cache-dir[= ](\S+)`)

// scriptNames reports whether the script names dir or a path under it, which
// makes a volume mounted there data rather than restic's cache.
func scriptNames(script, dir string) bool {
	for _, word := range strings.FieldsFunc(script, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '"' || r == '\'' || r == '='
	}) {
		if !strings.HasPrefix(word, "/") {
			continue
		}
		if p := pathpkg.Clean(word); p == dir || strings.HasPrefix(p, dir+"/") {
			return true
		}
	}
	return false
}

func isUpstreamImage(image string) bool {
	repository, _, _ := strings.Cut(image, "@")
	if i := strings.LastIndex(repository, ":"); i > strings.LastIndex(repository, "/") {
		repository = repository[:i]
	}
	return repository == upstreamImage || strings.HasSuffix(repository, "/"+upstreamImage)
}

func isOperatorImage(image string) bool {
	return strings.HasPrefix(image, backup.DefaultImageRepository+":") ||
		strings.HasPrefix(image, backup.DefaultImageRepository+"@")
}

func describeMount(vol corev1.Volume, m corev1.VolumeMount) string {
	path := pathpkg.Clean(m.MountPath)
	var s string
	switch {
	case vol.PersistentVolumeClaim != nil:
		s = describeVolume(vol.PersistentVolumeClaim.ClaimName, path,
			m.ReadOnly || vol.PersistentVolumeClaim.ReadOnly)
	case vol.EmptyDir != nil:
		s = "scratch space at " + path
		if q := vol.EmptyDir.SizeLimit; q != nil {
			s += " of at most " + q.String()
		}
		if vol.EmptyDir.Medium != "" {
			s += " in " + string(vol.EmptyDir.Medium)
		}
	default:
		source := vol.DeepCopy()
		source.Name = ""
		s = canonicalJSON(source.VolumeSource) + " at " + path
		if m.ReadOnly {
			s += " (read-only)"
		}
	}
	if m.SubPath != "" {
		s += " subPath " + m.SubPath
	}
	if m.SubPathExpr != "" {
		s += " subPathExpr " + m.SubPathExpr
	}
	if m.MountPropagation != nil {
		s += " propagation " + string(*m.MountPropagation)
	}
	return s
}

func describeVolume(claim, path string, readOnly bool) string {
	s := claim + " at " + path
	if readOnly {
		s += " (read-only)"
	}
	return s
}

func canonicalJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	switch string(raw) {
	case "null", "{}", "[]", `""`:
		return ""
	}
	return string(raw)
}

// originalRelease is the backup being migrated away from: its CronJob, and,
// when it was read from a HelmRelease, the release's other controllers.
type originalRelease struct {
	cronJob   *batchv1.CronJob
	others    []appTemplateController
	namespace string
}

// readOriginal reads the second parity argument: a HelmRelease, rendered the
// way app-template would, or a manifest holding the CronJob itself, such as
// helm template output or a CronJob read from the cluster.
func readOriginal(arg string) (originalRelease, error) {
	path, err := resolveHelmRelease(arg)
	if err != nil {
		return originalRelease{}, err
	}
	if cj, err := readCronJob(path); err != nil {
		return originalRelease{}, err
	} else if cj != nil {
		return originalRelease{cronJob: cj, namespace: cj.Namespace}, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return originalRelease{}, err
	}
	var meta struct {
		Metadata struct {
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	}
	if err := yaml.Unmarshal(raw, &meta); err != nil {
		return originalRelease{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	namespace := releaseNamespace(path, meta.Metadata.Namespace)
	rendered, err := renderAppTemplateFile(path, namespace)
	if err != nil {
		return originalRelease{}, fmt.Errorf("%s: %w", path, err)
	}
	return originalRelease{cronJob: rendered.cronJob, others: rendered.others, namespace: namespace}, nil
}

func readCronJob(path string) (*batchv1.CronJob, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	docs := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
	for {
		doc, err := docs.Read()
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		var cj batchv1.CronJob
		if yaml.Unmarshal(doc, &cj) != nil || cj.Kind != "CronJob" {
			continue
		}
		return &cj, nil
	}
}

// generatedCronJob builds the CronJob the operator would run for the
// generated ScheduledBackup, with the CRD's defaults applied, through the
// operator's own validation.
func generatedCronJob(path string) (*batchv1.CronJob, *b2backupv1.ScheduledBackup, *b2backupv1.Repository, error) {
	sb, err := readScheduledBackup(path)
	if err != nil {
		return nil, nil, nil, err
	}
	applyCRDDefaults(sb)
	repo, err := readRepository(path, sb.Namespace, sb.Spec.RepositoryRef.Name)
	if err != nil {
		return nil, nil, nil, err
	}
	if repo == nil {
		repo = &b2backupv1.Repository{Namespace: sb.Namespace, Name: sb.Spec.RepositoryRef.Name}
	}
	cj, err := backup.BuildCronJob(sb, repo, backup.DefaultConfig())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("the operator would not run it: %w", err)
	}
	return cj, sb, repo, nil
}

func applyCRDDefaults(sb *b2backupv1.ScheduledBackup) {
	if sb.Spec.TimeZone == "" {
		sb.Spec.TimeZone = defaultTimeZone
	}
	if sb.Spec.SuccessfulJobsHistoryLimit == nil {
		sb.Spec.SuccessfulJobsHistoryLimit = ptr.To(int32(3))
	}
	if sb.Spec.FailedJobsHistoryLimit == nil {
		sb.Spec.FailedJobsHistoryLimit = ptr.To(int32(1))
	}
}

type parityReport struct {
	differences []string
	notes       []string
	scriptPair  *[2]string
}

func (r *parityReport) differ(format string, args ...any) {
	r.differences = append(r.differences, fmt.Sprintf(format, args...))
}

func (r *parityReport) note(format string, args ...any) {
	r.notes = append(r.notes, fmt.Sprintf(format, args...))
}

func runParity(out io.Writer, generatedPath, helmReleasePath string, opts migrateOptions) error {
	generated, sb, repo, err := generatedCronJob(generatedPath)
	if err != nil {
		return fmt.Errorf("rendering the generated resource: %w", err)
	}
	original, err := readOriginal(helmReleasePath)
	if err != nil {
		return fmt.Errorf("reading the original backup: %w", err)
	}
	repositoryURL, resticEnvs, err := opts.source(original.namespace, false)
	if err != nil {
		return err
	}

	o, err := viewOf(original.cronJob, original.namespace, repo.SecretName(), defaultTimeZone)
	if err != nil {
		return fmt.Errorf("reading the original backup: %w", err)
	}
	g, err := viewOf(generated, sb.Namespace, repo.SecretName(), defaultTimeZone)
	if err != nil {
		return fmt.Errorf("reading the generated backup: %w", err)
	}

	var r parityReport
	if readsSecret(original.cronJob, passwordSecretName) {
		addResticEnvs(&r, o.env, resticEnvs)
	}
	if err := compareSchedules(&r, o, g); err != nil {
		return err
	}
	compareScripts(&r, o, g, backup.SourceTags(sb.Spec.Sources))
	compareJob(&r, o, g)
	compareContainer(&r, o, g)
	comparePod(&r, o, g)
	compareControllers(&r, original.others)
	if repositoryURL != "" {
		loc, err := parseS3Repository(repositoryURL)
		if err != nil {
			return err
		}
		want := describeLocation(loc.Bucket, loc.Path)
		if got := describeLocation(repo.Spec.Bucket, repo.EffectivePath()); repo.Spec.Bucket == "" || got != want {
			r.differ("REPOSITORY DIFFERS: original %s, generated %s", want, orNone(got))
		}
	}
	return r.print(out)
}

// addResticEnvs adds what restic-envs puts in the original's environment
// beyond what the Repository's Secret replaces. A container's env wins over
// its envFrom, so a name the container sets keeps its value.
func addResticEnvs(r *parityReport, env, resticEnvs map[string]string) {
	if resticEnvs == nil {
		r.note("RESTIC-ENVS UNCHECKED: %s may hold keys besides RESTIC_REPOSITORY and RESTIC_PASSWORD "+
			"that set the original's environment; pass --from-secret to compare them", passwordSecretName)
		return
	}
	for k, v := range resticEnvs {
		if _, set := env[k]; set || slices.Contains(resticEnvsReplaced, k) || k == envResticHost ||
			k == envResticCacheDir {
			continue
		}
		if credentialName.MatchString(k) {
			v = "<from " + passwordSecretName + ">"
		}
		env[k] = v
	}
}

func readsSecret(cj *batchv1.CronJob, name string) bool {
	container, err := backupContainer(cj.Spec.JobTemplate.Spec.Template.Spec)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(container.EnvFrom, func(e corev1.EnvFromSource) bool {
		return e.SecretRef != nil && e.SecretRef.Name == name
	})
}

func (r *parityReport) print(out io.Writer) error {
	p := newPrinter(out)
	for _, n := range r.notes {
		p.println(n)
	}
	if len(r.differences) == 0 {
		if len(r.notes) == 0 {
			p.println("IDENTICAL")
		} else {
			p.println(verdictEquivalent)
		}
		return p.Err()
	}
	for _, d := range r.differences {
		p.println(d)
	}
	if r.scriptPair != nil {
		p.println("--- original")
		p.println(r.scriptPair[0])
		p.println("--- rendered")
		p.println(r.scriptPair[1])
	}
	if err := p.Err(); err != nil {
		return err
	}
	return ErrParityDiffers
}

func compareSchedules(r *parityReport, o, g parityView) error {
	generatedCadence, err := cadenceOf(g.schedule)
	if err != nil {
		return fmt.Errorf("reading the generated schedule: %w", err)
	}
	originalCadence, err := cadenceOf(o.schedule)
	if err != nil {
		return fmt.Errorf("reading the original schedule: %w", err)
	}
	switch {
	case !generatedCadence.equal(originalCadence):
		r.differ("CADENCE DIFFERS: original %q runs %s, generated %q runs %s",
			o.schedule, originalCadence, g.schedule, generatedCadence)
	case g.schedule != o.schedule:
		r.note("RESCHEDULED: %s -> %s (%s either way; the operator jittered it)",
			describeSchedule(o.schedule, o.timeZone), describeSchedule(g.schedule, g.timeZone), generatedCadence)
	}
	if o.timeZone != g.timeZone {
		r.differ("TIMEZONE DIFFERS: original %s, generated %s", o.timeZone, g.timeZone)
	}
	if o.suspend != g.suspend {
		r.differ("SUSPEND DIFFERS: original suspend %t, generated %t", o.suspend, g.suspend)
	}
	if o.concurrency != g.concurrency {
		r.differ("CONCURRENCY DIFFERS: original %s, generated %s", o.concurrency, g.concurrency)
	}
	return nil
}

func compareScripts(r *parityReport, o, g parityView, tags []string) {
	if !slices.Equal(o.shell, g.shell) {
		r.differ("COMMAND DIFFERS: original runs %q, generated %q", o.shell, g.shell)
	}
	generatedScript, generatedDumps, scoped := normalizeRendered(g.script, tags)
	originalScript, originalDumps := normalizeScript(o.script)
	if generatedScript != originalScript {
		r.differ("SCRIPT DIFFERS")
		r.scriptPair = &[2]string{o.script, g.script}
		return
	}
	for _, d := range dumpChanges(originalDumps, generatedDumps) {
		r.differ("%s", d)
	}
	if scoped != "" {
		r.note("%s", scoped)
	}
}

func compareJob(r *parityReport, o, g parityView) {
	var changes []string
	for _, k := range jobSettingNotes {
		if o.jobSettings[k] != g.jobSettings[k] {
			changes = append(changes, fmt.Sprintf("%s %s -> %s", k, o.jobSettings[k], g.jobSettings[k]))
		}
	}
	if len(changes) > 0 {
		r.note("JOB SETTINGS CHANGE: %s; these decide how a failed run is retried and how long Jobs are kept, "+
			"not what is backed up", strings.Join(changes, ", "))
	}
	for _, k := range jobSettingDifferences {
		if o.jobSettings[k] != g.jobSettings[k] {
			r.differ("JOB SETTINGS DIFFER: %s original %s, generated %s", k, o.jobSettings[k], g.jobSettings[k])
		}
	}
}

func compareContainer(r *parityReport, o, g parityView) {
	switch {
	case o.image == g.image:
	case isUpstreamImage(o.image) && isOperatorImage(g.image):
		r.note("IMAGE CHANGES: %s -> %s, the operator's image, built on %s with restoredb added",
			o.image, g.image, upstreamImage)
	default:
		r.differ("IMAGE DIFFERS: original %s, generated %s", o.image, g.image)
	}

	if _, both := g.env[cnpgSecretPathEnv]; !both {
		delete(o.env, cnpgSecretPathEnv)
	} else if _, both = o.env[cnpgSecretPathEnv]; !both {
		delete(g.env, cnpgSecretPathEnv)
	}
	if _, ok := o.env["TZ"]; !ok {
		o.env["TZ"] = ""
	}
	for _, c := range []struct{ label, original, generated string }{
		{volumeDiffers, o.volumes, g.volumes},
		{"DATABASE SECRET DIFFERS", o.dbSecret, g.dbSecret},
		{"WORKING DIR DIFFERS", o.workingDir, g.workingDir},
		{"RESTIC HOST DIFFERS", o.host, g.host},
		{"ENV DIFFERS", describeEnv(o.env), describeEnv(g.env)},
		{"ENVFROM DIFFERS", o.envFrom, g.envFrom},
		{"RESOURCES DIFFER", o.resources, g.resources},
		{"CONTAINER DIFFERS", o.containerRest, g.containerRest},
	} {
		if c.original != c.generated {
			r.differ("%s: original %s, generated %s", c.label, orNone(c.original), orNone(c.generated))
		}
	}
	if o.readsAll && !g.readsAll {
		r.differ("READ ACCESS DIFFERS: the original runs as root able to read every " +
			"file on its claims, and the generated backup cannot; files it cannot read are left out of its snapshots")
	}
	if o.containerSecurityContext != nil &&
		canonicalJSON(o.containerSecurityContext) != canonicalJSON(g.containerSecurityContext) {
		r.differ("SECURITY CONTEXT DIFFERS: original container %s, generated %s",
			canonicalJSON(o.containerSecurityContext), orNone(canonicalJSON(g.containerSecurityContext)))
	}
	if len(o.extraContainers) > 0 {
		r.differ("CONTAINERS DIFFER: the original pod also runs %s, which the generated one does not",
			strings.Join(o.extraContainers, ", "))
	}
}

func comparePod(r *parityReport, o, g parityView) {
	if o.podSecurityContext != nil && canonicalJSON(o.podSecurityContext) != canonicalJSON(g.podSecurityContext) {
		r.differ("SECURITY CONTEXT DIFFERS: original pod %s, generated %s",
			canonicalJSON(o.podSecurityContext), orNone(canonicalJSON(g.podSecurityContext)))
	}
	switch originalAffinity, generatedAffinity := canonicalJSON(o.affinity), canonicalJSON(g.affinity); {
	case originalAffinity == generatedAffinity:
	case o.affinity == nil && onlyPreferred(g.affinity):
		r.note("AFFINITY ADDED: the original sets none; the generated backup prefers %s", generatedAffinity)
	default:
		r.differ("AFFINITY DIFFERS: original %s, generated %s", orNone(originalAffinity), orNone(generatedAffinity))
	}
	for _, c := range []struct{ label, original, generated string }{
		{"NODE SELECTOR DIFFERS", o.nodeSelector, g.nodeSelector},
		{"TOLERATIONS DIFFER", o.tolerations, g.tolerations},
		{"PRIORITY CLASS DIFFERS", o.priorityClassName, g.priorityClassName},
		{"POD SPEC DIFFERS", o.podRest, g.podRest},
	} {
		if c.original != c.generated {
			r.differ("%s: original %s, generated %s", c.label, orNone(c.original), orNone(c.generated))
		}
	}

	var lost, chartLost []string
	for _, k := range sortedKeys(o.labels) {
		if g.labels[k] == o.labels[k] {
			continue
		}
		label := k + "=" + o.labels[k]
		if k == labelController || k == labelName || k == labelInstance {
			chartLost = append(chartLost, label)
		} else {
			lost = append(lost, label)
		}
	}
	if len(lost) > 0 {
		r.differ("POD LABELS DIFFER: the generated pods do not carry %s", strings.Join(lost, ", "))
	}
	if len(chartLost) > 0 {
		r.note("POD LABELS CHANGE: the generated pods no longer carry app-template's %s; "+
			"check nothing selects backup pods by them", strings.Join(chartLost, ", "))
	}
	var lostAnnotations []string
	for _, k := range sortedKeys(o.annotations) {
		if g.annotations[k] != o.annotations[k] {
			lostAnnotations = append(lostAnnotations, k+"="+o.annotations[k])
		}
	}
	if len(lostAnnotations) > 0 {
		r.differ("POD ANNOTATIONS DIFFER: the generated pods do not carry %s", strings.Join(lostAnnotations, ", "))
	}
}

func onlyPreferred(a *corev1.Affinity) bool {
	if a == nil || a.NodeAffinity != nil || a.PodAntiAffinity != nil || a.PodAffinity == nil {
		return false
	}
	return len(a.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) == 0
}

// compareControllers reports what else the HelmRelease ran. Its post-install
// restic init Job is the one thing the Repository replaces: the operator
// initializes a new repository or adopts an existing one itself.
func compareControllers(r *parityReport, others []appTemplateController) {
	for _, c := range others {
		if isResticInit(c) {
			r.note("INIT JOB REPLACED: controller %s ran restic init; the Repository initializes "+
				"or adopts the repository itself", c.name)
			continue
		}
		r.differ("CONTROLLER DROPPED: the HelmRelease also runs controller %s, a %s, which the migration drops",
			c.name, c.kind)
	}
}

func isResticInit(c appTemplateController) bool {
	if c.kind != controllerTypeJob || len(c.commands) != 1 {
		return false
	}
	argv := c.commands[0]
	if len(argv) != 3 || !slices.Contains(scriptShells, argv[0]) || argv[1] != "-c" {
		return false
	}
	script := strings.Join(strings.Fields(argv[2]), " ")
	return script == "restic init" || script == "restic init || true"
}

func describeLocation(bucket, path string) string {
	if path == "" {
		return bucket + " (bucket root)"
	}
	return bucket + "/" + path
}

// describeSchedule names a cron expression and, for the daily and hourly
// shapes migration produces, when it fires in its time zone.
func describeSchedule(expr, tz string) string {
	fields := strings.Fields(expr)
	if len(fields) != 5 || fields[2] != "*" || fields[3] != "*" || fields[4] != "*" || !isNumeric(fields[0]) {
		return fmt.Sprintf("%q", expr)
	}
	minute, _ := strconv.Atoi(fields[0])
	switch {
	case isNumeric(fields[1]):
		hour, _ := strconv.Atoi(fields[1])
		return fmt.Sprintf("%q (daily at %02d:%02d %s)", expr, hour, minute, tz)
	case fields[1] == "*":
		return fmt.Sprintf("%q (hourly at :%02d)", expr, minute)
	}
	return fmt.Sprintf("%q", expr)
}

// normalizeCNPGSecretPath leaves CNPG_SECRET_PATH in env only when the script
// runs backup-cnpg.sh, the one thing that reads it, and then as the directory it
// effectively names, or as databaseMountToken when that is where the database
// Secret is mounted. runParity compares it only when both sides run the script;
// a switch to dumpdb is reported as a dump change of its own.
func normalizeCNPGSecretPath(env map[string]string, script string, secretMounts map[string]bool) {
	if !strings.Contains(script, backupCNPGScript) {
		delete(env, cnpgSecretPathEnv)
		return
	}
	value, ok := env[cnpgSecretPathEnv]
	if !ok {
		value = backupCNPGDefaultMount
	}
	if secretMounts[pathpkg.Clean(value)] {
		value = databaseMountToken
	}
	env[cnpgSecretPathEnv] = value
}

func describeEnv(env map[string]string) string {
	parts := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		parts = append(parts, k+"="+env[k])
	}
	return strings.Join(parts, " ")
}

func readRepository(path, namespace, name string) (*b2backupv1.Repository, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	docs := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
	for {
		doc, err := docs.Read()
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		var repo b2backupv1.Repository
		if yaml.Unmarshal(doc, &repo) != nil || repo.Kind != "Repository" {
			continue
		}
		if repo.Name == name && (repo.Namespace == "" || repo.Namespace == namespace) {
			return &repo, nil
		}
	}
}

func usesDatabase(script string) bool {
	return strings.Contains(script, "dumpdb ") || strings.Contains(script, "backup-cnpg.sh") ||
		strings.Contains(script, "backup-mariadb.sh")
}
