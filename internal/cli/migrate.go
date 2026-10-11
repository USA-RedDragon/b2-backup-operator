package cli

import (
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

// Errors returned by the migrate command.
var (
	ErrExcludeCount   = errors.New("refusing to emit a partial exclude list")
	ErrUnsupportedEnv = errors.New("the restic container sets an environment variable a ScheduledBackup cannot")
)

type notStructuredError struct{ reason string }

func (e *notStructuredError) Error() string { return "not expressible as sources: " + e.reason }

func notStructured(format string, args ...any) error {
	return &notStructuredError{reason: fmt.Sprintf(format, args...)}
}

const (
	migratedName       = "restic"
	passwordSecretName = "restic-envs"
	backupCNPGScript   = "backup-cnpg.sh"
	cnpgSecretPathEnv  = "CNPG_SECRET_PATH"
	cnpgAppSuffix      = "-app"
	cnpgSuperSuffix    = "-superuser"
	defaultCNPGCluster = "postgresql"
	envDBHost          = "DB_HOST"
	envDBDatabase      = "DB_DATABASE"
	envDBUsername      = "DB_USERNAME"
)

var (
	preambleLines  = []string{"exec > >(ts '%H:%M:%S') 2>&1", "set -o pipefail", "{", "set -eu", "set -euo pipefail"}
	operatorShell  = "set -euo pipefail"
	handledEnvVars = []string{"TZ", envResticHost, cnpgSecretPathEnv, envResticCacheDir}
)

type migrateOptions struct {
	repository string
	fromSecret string
}

func newMigrateCommand(f *Factory) *cobra.Command {
	var opts migrateOptions

	cmd := &cobra.Command{
		Use:     "migrate <resources/restic directory | helmrelease.yaml>",
		Short:   "Generate resources from a hand-written backup",
		GroupID: GroupDevelopment,
		Long: `Convert an app's hand-written restic backup into operator resources.

Reads the app-template HelmRelease (helmrelease.yaml or helm.yaml in the
directory, or the file given) and emits a Repository and a ScheduledBackup on
stdout, in the namespace Flux would apply it to: the HelmRelease's
targetNamespace, then its Flux Kustomization's, then kustomization.yaml's,
then its own. Nothing is applied and nothing is modified; diff the output
against the HelmRelease, then check it with b2bk parity.

The Repository is in Prefix mode and adopts the existing restic repository in
place: same bucket, same path, password read from restic-envs. Its location
lives only in the encrypted RESTIC_REPOSITORY, so pass it with --repository,
or let b2bk decrypt the app's restic-envs Secret with sops via --from-secret.

A script whose every line maps onto sources becomes sources. Anything else, such
as extra shell commands, several tags on one backup or a custom
--stdin-filename, is kept verbatim as spec.script.

Once the operator reports the Repository ready, restic-envs only needs
RESTIC_PASSWORD; it is the only off-cluster copy of the key that decrypts
these backups, so keep it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigrate(f.Streams.Out, args[0], opts)
		},
	}

	cmd.Flags().StringVar(&opts.repository, "repository", "",
		"The existing repository, as the s3: URL in RESTIC_REPOSITORY")
	cmd.Flags().StringVar(&opts.fromSecret, "from-secret", "",
		"A SOPS-encrypted Secret to read RESTIC_REPOSITORY from, such as <app>-restic.yaml in the secrets repository")
	cmd.MarkFlagsMutuallyExclusive("repository", "from-secret")
	return cmd
}

// repositoryURL returns the original RESTIC_REPOSITORY from --repository, or
// from the encrypted Secret --from-secret names, which must belong to namespace.
func (o migrateOptions) repositoryURL(namespace string, required bool) (string, error) {
	url, _, err := o.source(namespace, required)
	return url, err
}

// source returns the original RESTIC_REPOSITORY and, given --from-secret, every
// key of the decrypted Secret. The Secret must belong to namespace.
func (o migrateOptions) source(namespace string, required bool) (string, map[string]string, error) {
	switch {
	case o.repository != "":
		return o.repository, nil, nil
	case o.fromSecret != "":
		secret, err := decryptSecret(o.fromSecret)
		if err != nil {
			return "", nil, err
		}
		values := secret.values()
		if values["RESTIC_REPOSITORY"] == "" {
			return "", nil, fmt.Errorf("%w: %s has no RESTIC_REPOSITORY", ErrNoRepositoryURL, o.fromSecret)
		}
		if ns := secret.Metadata.Namespace; ns != "" && namespace != "" && ns != namespace {
			return "", nil, fmt.Errorf("%w: %s is a Secret in namespace %s, but the HelmRelease is in %s",
				ErrNoRepositoryURL, o.fromSecret, ns, namespace)
		}
		return values["RESTIC_REPOSITORY"], values, nil
	case required:
		return "", nil, fmt.Errorf("%w: pass --repository or --from-secret", ErrNoRepositoryURL)
	}
	return "", nil, nil
}

func runMigrate(out io.Writer, arg string, opts migrateOptions) error {
	helmReleasePath, err := resolveHelmRelease(arg)
	if err != nil {
		return err
	}
	hr, err := readHelmRelease(helmReleasePath)
	if err != nil {
		return err
	}

	repoURL, resticEnvs, err := opts.source(hr.Metadata.Namespace, true)
	if err != nil {
		return err
	}
	loc, err := parseS3Repository(repoURL)
	if err != nil {
		return err
	}

	m, err := migrateHelmRelease(hr, loc, resticEnvs)
	if err != nil {
		return fmt.Errorf("%s: %w", helmReleasePath, err)
	}
	if err := m.validate(); err != nil {
		return fmt.Errorf("%s: the operator would refuse what migrate generated: %w", helmReleasePath, err)
	}

	p := newPrinter(out)
	p.printf("# Generated by b2bk migrate from %s\n", helmReleasePath)
	p.println("# Review against the HelmRelease, then check it with b2bk parity.")
	for _, note := range m.notes {
		p.println("#")
		for line := range strings.SplitSeq(note, "\n") {
			p.printf("# %s\n", line)
		}
	}
	if err := p.Err(); err != nil {
		return err
	}
	return writeDocuments(out, m.repo, m.backup)
}

func writeDocuments(out io.Writer, objects ...any) error {
	p := newPrinter(out)
	for i, obj := range objects {
		data, err := yaml.Marshal(obj)
		if err != nil {
			return err
		}
		if i > 0 {
			p.println("---")
		}
		p.print(string(data))
	}
	return p.Err()
}

type migration struct {
	repo   *b2backupv1.Repository
	backup *b2backupv1.ScheduledBackup
	notes  []string
}

// validate builds the CronJob the operator would, with the CRD's defaults.
func (m *migration) validate() error {
	sb := m.backup.DeepCopy()
	applyCRDDefaults(sb)
	if _, err := backup.BuildCronJob(sb, m.repo, backup.DefaultConfig()); err != nil {
		return err
	}
	script, err := backup.Render(sb)
	if err != nil {
		return err
	}
	switch problem, checked := scriptSyntax(script); {
	case !checked:
		m.notes = append(m.notes, "syntax: bash is not on PATH, so the script the operator would run was not\n"+
			"parsed. Run b2bk parity where bash is, or check it with bash -n.")
	case problem != "":
		return fmt.Errorf("%w: bash -n rejects the script it would run: %s", ErrUnsupportedCommand, problem)
	}
	return nil
}

func migrateHelmRelease(hr *helmRelease, loc s3Location, resticEnvs map[string]string) (*migration, error) {
	namespace := hr.Metadata.Namespace
	controller, container, err := hr.backupController()
	if err != nil {
		return nil, err
	}
	script, err := container.script()
	if err != nil {
		return nil, err
	}

	others, err := hr.otherControllers()
	if err != nil {
		return nil, err
	}

	m := &migration{repo: migratedRepository(namespace, loc), notes: append(others, repositoryNotes(loc)...)}

	name := hr.Metadata.Name
	if name == "" {
		name = migratedName
	}
	sb := &b2backupv1.ScheduledBackup{
		APIVersion: b2backupv1.SchemeGroupVersion.String(), Kind: "ScheduledBackup",
		Namespace: namespace, Name: name,
		Spec: b2backupv1.ScheduledBackupSpec{
			RepositoryRef:            corev1.LocalObjectReference{Name: migratedName},
			Schedule:                 shorthandSchedule(controller.CronJob.Schedule),
			TimeZone:                 controller.CronJob.TimeZone,
			Resources:                container.Resources,
			Affinity:                 hr.affinity(),
			PodSecurityContext:       hr.podSecurityContext(),
			ContainerSecurityContext: container.SecurityContext,
		},
	}
	m.backup = sb
	if p := controller.CronJob.ConcurrencyPolicy; p != "" {
		sb.Spec.ConcurrencyPolicy = batchv1.ConcurrencyPolicy(p)
	}
	if sb.Spec.TimeZone == "" {
		sb.Spec.TimeZone = controllerManagerZone
		m.notes = append(m.notes, fmt.Sprintf("timeZone: the CronJob sets none, so it ran in the kube-controller-manager's\n"+
			"zone, taken as %s, and keeps it. The operator's default would be %s.\n"+
			"The operator sets TZ from timeZone, so a TZ in env is replaced.", controllerManagerZone, defaultTimeZone))
	}
	if err := migratePod(hr, controller, container, m); err != nil {
		return nil, err
	}
	if err := migrateResticEnvs(resticEnvs, container, m); err != nil {
		return nil, err
	}
	if original := controller.CronJob.Schedule; original != sb.Spec.Schedule {
		m.notes = append(m.notes, fmt.Sprintf(
			"schedule: was %q, hand-jittered. Handed back to the operator as %q,\n"+
				"which derives its own minute from a hash of this resource, so the\n"+
				"spread is kept without anyone maintaining it. Check\n"+
				"status.effectiveSchedule for the expression it settles on.",
			original, sb.Spec.Schedule))
	}

	mounts, dbSecret, err := mountedVolumes(hr, container, script, m)
	if err != nil {
		return nil, err
	}
	if err := migrateContainerEnv(container, m); err != nil {
		return nil, err
	}

	if added := shellOptionsAdded(script, operatorShell); len(added) > 0 {
		m.notes = append(m.notes, fmt.Sprintf("shell: the operator runs the script under set -euo pipefail, and the\n"+
			"HelmRelease's did not turn on %s. A failing command, unset variable or failing\n"+
			"pipe stage now stops the rest of the script, dump and forget included.", strings.Join(added, ", ")))
	}
	parsed, err := parseBackupScript(script, mounts)
	var unstructured *notStructuredError
	switch {
	case errors.As(err, &unstructured):
		m.notes = append(m.notes, fmt.Sprintf(
			"script: kept verbatim as spec.script, because %s.\n"+
				"b2bk snapshots and restore cannot map tags to sources for a script backup.",
			unstructured.reason))
		parsed = parsedScript{}
		sb.Spec.Script = scriptBody(script)
		if err := checkVerbatimDatabase(script); err != nil {
			return nil, err
		}
		switch {
		case strings.Contains(script, backupCNPGScript) || strings.Contains(script, "dumpdb cnpg"):
			parsed.engine = b2backupv1.DatabaseEngineCNPG
		case strings.Contains(script, "dumpdb mariadb"):
			parsed.engine = b2backupv1.DatabaseEngineMariaDB
		}
		if strings.Contains(script, backupCNPGScript) {
			setEnv(sb, cnpgSecretPathEnv, b2backupv1.DBSecretMountPath)
		}
	case err != nil:
		return nil, err
	default:
		sb.Spec.Sources = parsed.sources
		sb.Spec.Retention = parsed.retention
		m.notes = append(m.notes, parsed.notes()...)
	}

	if err := migrateDatabase(parsed.engine, dbSecret, container, sb); err != nil {
		return nil, err
	}

	if sb.Spec.Affinity == nil && len(sb.Spec.Volumes) > 0 {
		m.notes = append(m.notes, fmt.Sprintf(
			"affinity: the HelmRelease sets none. The operator requires the backup pod on the\n"+
				"node of a pod labelled app.kubernetes.io/name=%s, so the claim can attach;\n"+
				"set spec.affinity: {} if nothing in this namespace carries that label.", namespace))
	}
	return m, nil
}

// migratePod carries over what app-template puts on the restic CronJob and its
// pod that a ScheduledBackup can hold, and refuses what it cannot.
func migratePod(hr *helmRelease, controller hrController, container hrContainer, m *migration) error {
	spec := &m.backup.Spec
	if len(controller.Labels) > 0 || len(controller.Annotations) > 0 {
		return fmt.Errorf("%w: the restic controller sets labels or annotations on its CronJob, "+
			"which a ScheduledBackup cannot", ErrUnsupportedValues)
	}
	cronJob := controller.CronJob
	if ttl := cronJob.TTLSecondsAfterFinished; ttl != nil && *ttl != backup.JobTTLSeconds {
		return fmt.Errorf("%w: cronjob.ttlSecondsAfterFinished is %d, and the operator keeps finished Jobs %d seconds",
			ErrUnsupportedValues, *ttl, backup.JobTTLSeconds)
	}
	spec.Suspend = cronJob.Suspend
	spec.SuccessfulJobsHistoryLimit = cronJob.SuccessfulJobsHistory
	spec.FailedJobsHistoryLimit = cronJob.FailedJobsHistory

	pod := hr.podOptions()
	spec.NodeSelector = pod.NodeSelector
	spec.Tolerations = pod.Tolerations
	spec.PriorityClassName = pod.PriorityClassName
	spec.PodLabels = pod.Labels
	if spec.Resources == nil {
		spec.Resources = &corev1.ResourceRequirements{}
	}

	if err := checkEnvFrom(container.EnvFrom); err != nil {
		return err
	}
	return migrateImage(container.Image, m)
}

func migrateImage(img hrImage, m *migration) error {
	if img.Repository == "" {
		return fmt.Errorf("%w: the restic container names no image", ErrUnsupportedValues)
	}
	image := img.Repository
	if tag := scalarString(img.Tag); tag != "" {
		image += ":" + tag
	}
	if img.Digest != "" {
		image += "@" + img.Digest
	}
	if isUpstreamImage(image) {
		m.notes = append(m.notes, fmt.Sprintf("image: %s is replaced by the operator's backup image, which is\n"+
			"built on %s with restoredb added. spec.image is left unset so it\n"+
			"follows the operator's version.", image, upstreamImage))
		return nil
	}
	m.backup.Spec.Image = image
	m.notes = append(m.notes, fmt.Sprintf("image: %s is kept as spec.image. It must meet the backup image\n"+
		"contract in the README: restic behind the wrapper that reads RESTIC_GROUP_BY and\n"+
		"RESTIC_HOST, ts and dumpdb.", image))
	return nil
}

// checkEnvFrom accepts the restic-envs Secret, whose RESTIC_PASSWORD the
// Repository reads, and the restic-b2 Secret of B2 credentials the
// Repository's own key replaces. Anything else would be dropped.
func checkEnvFrom(sources []hrEnvFrom) error {
	readsEnvs := false
	for _, e := range sources {
		name := e.Secret
		if e.SecretRef != nil {
			name = e.SecretRef.Name
		}
		refs := 0
		for _, set := range []bool{e.Secret != "", e.SecretRef != nil, e.ConfigMap != "", e.ConfigMapRef != nil} {
			if set {
				refs++
			}
		}
		switch {
		case refs > 1:
			return fmt.Errorf("%w: an envFrom item names more than one Secret or ConfigMap, and app-template "+
				"reads only one of them", ErrUnsupportedValues)
		case e.Prefix != "":
			return fmt.Errorf("%w: envFrom adds the prefix %s", ErrUnsupportedValues, e.Prefix)
		case e.ConfigMap != "" || e.ConfigMapRef != nil:
			return fmt.Errorf("%w: the restic container reads its environment from a ConfigMap", ErrUnsupportedValues)
		case e.SecretRef != nil && e.SecretRef.Optional != nil && *e.SecretRef.Optional:
			return fmt.Errorf("%w: envFrom reads %s as optional", ErrUnsupportedValues, name)
		case name == passwordSecretName:
			readsEnvs = true
		case name == credentialsSecretName:
		default:
			return fmt.Errorf("%w: the restic container reads its environment from the Secret %s, "+
				"which a ScheduledBackup cannot", ErrUnsupportedValues, name)
		}
	}
	if !readsEnvs {
		return fmt.Errorf("%w: the restic container does not read %s, where the Repository reads the password",
			ErrUnsupportedValues, passwordSecretName)
	}
	return nil
}

var (
	// resticEnvsReplaced are the restic-envs keys the Repository's own Secret supplies.
	resticEnvsReplaced = []string{"RESTIC_REPOSITORY", envResticPassword,
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_DEFAULT_REGION"}
	// resticEnvsRefused would point restic at another repository or key.
	resticEnvsRefused = []string{"RESTIC_REPOSITORY_FILE", "RESTIC_PASSWORD_FILE", "RESTIC_PASSWORD_COMMAND"}

	credentialName = regexp.MustCompile(`PASSWORD|SECRET|TOKEN|_KEY`)
)

// migrateResticEnvs carries restic-envs' other RESTIC_* keys into spec.env.
// Without --from-secret its keys cannot be seen, which the output says.
func migrateResticEnvs(envs map[string]string, container hrContainer, m *migration) error {
	if envs == nil {
		m.notes = append(m.notes, "restic-envs: only RESTIC_REPOSITORY and RESTIC_PASSWORD are accounted for.\n"+
			"Any other key it holds, such as RESTIC_COMPRESSION or RESTIC_GROUP_BY, set the old\n"+
			"backup's environment and is not carried over. Pass --from-secret to check and carry them.")
		return nil
	}
	if envs[envResticPassword] == "" {
		return fmt.Errorf("%w: %s holds no %s, which the Repository reads the password from",
			ErrUnsupportedValues, passwordSecretName, envResticPassword)
	}
	var carried []string
	for _, k := range sortedKeys(envs) {
		_, overridden := container.Env[k]
		switch {
		case slices.Contains(resticEnvsReplaced, k), k == envResticCacheDir:
		case k == envResticHost && overridden:
		case !strings.HasPrefix(k, "RESTIC_") || k == envResticHost:
			return fmt.Errorf("%w: %s holds %s, which a ScheduledBackup cannot carry", ErrUnsupportedValues,
				passwordSecretName, k)
		case slices.Contains(resticEnvsRefused, k):
			return fmt.Errorf("%w: %s sets %s, which would point restic elsewhere", ErrUnsupportedValues,
				passwordSecretName, k)
		case credentialName.MatchString(k):
			return fmt.Errorf("%w: %s holds the credential %s, and spec.env is plaintext", ErrUnsupportedValues,
				passwordSecretName, k)
		case overridden:
		default:
			setEnv(m.backup, k, envs[k])
			carried = append(carried, k)
		}
	}
	if len(carried) > 0 {
		m.notes = append(m.notes, fmt.Sprintf("restic-envs: %s carried into spec.env from the decrypted Secret.",
			strings.Join(carried, ", ")))
	}
	return nil
}

// migrateDatabase sets spec.database for the engine the script dumps.
func migrateDatabase(engine b2backupv1.DatabaseEngine, dbSecret *hrVolume, container hrContainer,
	sb *b2backupv1.ScheduledBackup) error {
	switch {
	case engine == "" && dbSecret != nil:
		return fmt.Errorf("%w: %s mounts the Secret %s, and a ScheduledBackup mounts a Secret only for "+
			"a database dump", ErrUnsupportedMount, dbSecret.key, dbSecret.name)
	case engine == "":
		return nil
	}
	db := &b2backupv1.DatabaseSpec{Engine: engine}
	if dbSecret != nil {
		db.SecretName = dbSecret.name
		if cluster := cnpgClusterOf(dbSecret.name); engine == b2backupv1.DatabaseEngineCNPG &&
			cluster != "" && cluster != defaultCNPGCluster {
			db.CNPGCluster = cluster
		}
	}
	if engine == b2backupv1.DatabaseEngineMariaDB {
		db.Host = container.env(envDBHost)
		db.Name = container.env(envDBDatabase)
		db.User = container.env(envDBUsername)
		for _, name := range []string{envDBHost, envDBDatabase, envDBUsername} {
			delete(sb.Spec.Env, name)
		}
		if len(sb.Spec.Env) == 0 {
			sb.Spec.Env = nil
		}
	}
	sb.Spec.Database = db
	return nil
}

func repositoryNotes(loc s3Location) []string {
	var notes []string
	if loc.Path == "" {
		notes = append(notes, fmt.Sprintf(
			"mode: the repository sits at the root of %s, so the Repository adopts the whole\n"+
				"bucket. Under deletionPolicy: Delete every file in it would be deleted, restic's\n"+
				"or not; the default, Retain, deletes nothing.", loc.Bucket))
	}
	if !strings.HasPrefix(loc.Endpoint, "https://s3.") || !strings.HasSuffix(loc.Endpoint, ".backblazeb2.com") {
		notes = append(notes, fmt.Sprintf(
			"%s does not look like a B2 S3 endpoint. The operator builds the endpoint from\n"+
				"the account it is given, so check this repository really lives in B2.", loc.Endpoint))
	}
	return notes
}

func migratedRepository(namespace string, loc s3Location) *b2backupv1.Repository {
	mode := b2backupv1.StorageModePrefix
	if loc.Path == "" {
		mode = b2backupv1.StorageModeBucket
	}
	return &b2backupv1.Repository{
		APIVersion: b2backupv1.SchemeGroupVersion.String(), Kind: "Repository",
		Namespace: namespace, Name: migratedName,
		Spec: b2backupv1.RepositorySpec{
			Mode:              mode,
			Bucket:            loc.Bucket,
			Path:              loc.Path,
			PasswordSecretRef: &corev1.SecretKeySelector{Name: passwordSecretName, Key: envResticPassword},
		},
	}
}

func mountedVolumes(hr *helmRelease, container hrContainer, script string, m *migration) ([]hrVolume, *hrVolume, error) {
	sb := m.backup
	cacheDir := container.cacheDir(script)
	volumes, err := hr.volumes()
	if err != nil {
		return nil, nil, err
	}
	var claims, secretVolumes, scratch, others []hrVolume
	for _, v := range volumes {
		mountPath := path.Clean(v.path)
		switch {
		case v.kind != volumeSecret && cacheDir != "" && mountPath == cacheDir && !scriptNames(script, cacheDir, container.WorkingDir):
			if err := migrateCache(hr, v, cacheDir, script, m); err != nil {
				return nil, nil, err
			}
		case v.kind != volumeSecret && mountPath == operatorCacheDir:
			return nil, nil, fmt.Errorf("%w: %s is mounted at %s, where the operator mounts restic's cache, "+
				"and it is not restic's cache here", ErrUnsupportedMount, v.key, operatorCacheDir)
		case v.kind == volumeClaim && v.name != "":
			claims = append(claims, v)
		case v.kind == volumeSecret:
			secretVolumes = append(secretVolumes, v)
		case v.kind == volumeEmptyDir:
			scratch = append(scratch, v)
		default:
			others = append(others, v)
		}
	}

	switch {
	case len(others) > 0:
		parts := make([]string, 0, len(others))
		for _, v := range others {
			kind := v.kind
			if kind == "" {
				kind = "a chart-created claim"
			}
			parts = append(parts, fmt.Sprintf("%s (%s at %s)", v.key, kind, v.path))
		}
		return nil, nil, fmt.Errorf("%w: %s; a ScheduledBackup mounts existing claims, one emptyDir, "+
			"one database Secret and its cache", ErrUnsupportedMount, strings.Join(parts, ", "))
	case len(scratch) > 1:
		return nil, nil, fmt.Errorf("%w: %s are all emptyDirs; a ScheduledBackup mounts one scratch space",
			ErrUnsupportedMount, volumeKeys(scratch))
	case len(secretVolumes) > 1:
		return nil, nil, fmt.Errorf("%w (%s); a ScheduledBackup mounts one database secret, so migrate "+
			"cannot choose between them", ErrMultipleDBSecrets, volumeKeys(secretVolumes))
	}

	workingDir := path.Clean(cmpOr(container.WorkingDir, "/"))
	slices.SortStableFunc(claims, func(a, b hrVolume) int {
		return boolOrder(path.Clean(a.path) != workingDir, path.Clean(b.path) != workingDir)
	})
	for _, claim := range claims {
		volume := b2backupv1.VolumeSpec{ExistingClaim: claim.name, ReadOnly: claim.readOnly}
		if path.Clean(claim.path) != "/"+claim.name {
			volume.MountPath = path.Clean(claim.path)
		}
		sb.Spec.Volumes = append(sb.Spec.Volumes, volume)
	}
	if len(scratch) == 1 {
		sc := &b2backupv1.ScratchSpec{MountPath: path.Clean(scratch[0].path)}
		if raw := hr.Spec.Values.Persistence[scratch[0].key].SizeLimit; raw != "" {
			q, err := resource.ParseQuantity(raw)
			if err != nil {
				return nil, nil, fmt.Errorf("%s sizeLimit %q: %w", scratch[0].key, raw, err)
			}
			sc.SizeLimit = &q
		}
		sb.Spec.Scratch = sc
	}
	if workingDir != sb.EffectiveWorkingDir() {
		sb.Spec.WorkingDir = workingDir
	}

	mounts := append(slices.Clone(claims), scratch...)
	var secret *hrVolume
	if len(secretVolumes) == 1 {
		secret = &secretVolumes[0]
	}
	return mounts, secret, nil
}

// migrateCache takes the volume restic keeps its cache on as the cache. The
// operator mounts a claim of its own at /cache, the image's RESTIC_CACHE_DIR.
func migrateCache(hr *helmRelease, v hrVolume, cacheDir, script string, m *migration) error {
	if cacheDir != operatorCacheDir && cacheDirFlag.MatchString(script) {
		return fmt.Errorf("%w: the script passes --cache-dir=%s, where %s is mounted, and the operator "+
			"mounts restic's cache at %s", ErrUnsupportedMount, cacheDir, v.key, operatorCacheDir)
	}
	p := hr.Spec.Values.Persistence[v.key]
	switch {
	case v.kind == volumeClaim && v.name != "":
		m.notes = append(m.notes, fmt.Sprintf("cache: %s, the existing claim %s at %s, held restic's cache. "+
			"The operator creates a cache claim of its own, so %s is no longer used.", v.key, v.name, v.path, v.name))
	case p.StorageClass != "":
		m.backup.Spec.Cache = &b2backupv1.CacheSpec{StorageClass: p.StorageClass}
	}
	return nil
}

func cmpOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func boolOrder(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	}
	return 1
}

func migrateContainerEnv(container hrContainer, m *migration) error {
	if err := container.checkNumbers(); err != nil {
		return err
	}
	if err := migrateEnv(container, m.backup); err != nil {
		return err
	}
	if dir := container.env(envResticCacheDir); dir != "" && path.Clean(dir) != operatorCacheDir {
		m.notes = append(m.notes, fmt.Sprintf("cache: RESTIC_CACHE_DIR=%s is dropped. restic keeps its cache at "+
			"the image's %s, where the operator mounts its cache claim.", dir, operatorCacheDir))
	}
	return nil
}

func migrateEnv(container hrContainer, sb *b2backupv1.ScheduledBackup) error {
	for _, name := range sortedKeys(container.Env) {
		value := container.Env[name]
		if name == envResticHost {
			if host, ok := value.(string); ok {
				sb.Spec.ResticHost = host
				continue
			}
			if !isNamespaceFieldRef(value) {
				return fmt.Errorf("%w: RESTIC_HOST is neither a string nor the namespace", ErrUnsupportedEnv)
			}
			continue
		}
		if slices.Contains(handledEnvVars, name) {
			continue
		}
		if slices.Contains(resticEnvsRefused, name) {
			return fmt.Errorf("%w: env sets %s, which would open the repository with another key or point "+
				"restic elsewhere than the Repository", ErrUnsupportedValues, name)
		}
		s := container.env(name)
		if s == "" {
			return fmt.Errorf("%w: %s is not a plain value", ErrUnsupportedEnv, name)
		}
		setEnv(sb, name, s)
	}
	return nil
}

func isNamespaceFieldRef(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	ref, ok := m["fieldRef"].(map[string]any)
	if !ok {
		return false
	}
	return ref["fieldPath"] == "metadata.namespace"
}

func setEnv(sb *b2backupv1.ScheduledBackup, name, value string) {
	if sb.Spec.Env == nil {
		sb.Spec.Env = map[string]string{}
	}
	sb.Spec.Env[name] = value
}

var dumpdbCall = regexp.MustCompile(`\bdumpdb\s+\S+[^\n;|&]*`)

// checkVerbatimDatabase refuses a script kept verbatim whose dump would look
// for its credentials anywhere but where the operator mounts the database
// Secret. dumpdb reads its --secret-mount, by default /postgresql-app or
// /mariadb; backup-mariadb.sh reads /mariadb and DB_USER and DB_NAME, which
// the operator does not set. backup-cnpg.sh reads CNPG_SECRET_PATH, which
// migrate points at the operator's mount.
func checkVerbatimDatabase(script string) error {
	if strings.Contains(script, "backup-mariadb.sh") {
		return fmt.Errorf("%w: the script runs backup-mariadb.sh, which reads its credentials from /mariadb, "+
			"and the operator mounts the database Secret at %s", ErrUnsupportedMount, b2backupv1.DBSecretMountPath)
	}
	for _, call := range dumpdbCall.FindAllString(script, -1) {
		if !strings.Contains(call, "--secret-mount="+b2backupv1.DBSecretMountPath) {
			return fmt.Errorf("%w: the script runs %q, which reads its credentials from its own --secret-mount, "+
				"and the operator mounts the database Secret at %s", ErrUnsupportedMount, strings.TrimSpace(call),
				b2backupv1.DBSecretMountPath)
		}
	}
	return nil
}

func cnpgClusterOf(secret string) string {
	for _, suffix := range []string{cnpgAppSuffix, cnpgSuperSuffix} {
		if cluster, ok := strings.CutSuffix(secret, suffix); ok && cluster != "" {
			return cluster
		}
	}
	return ""
}

type parsedScript struct {
	sources     []b2backupv1.BackupSource
	retention   *b2backupv1.Retention
	engine      b2backupv1.DatabaseEngine
	dumpChanged bool
	untagged    []string
	unpruned    bool
}

// notes says what the operator's rendering of the parsed script changes.
func (p parsedScript) notes() []string {
	var out []string
	if p.dumpChanged {
		out = append(out, "database: "+backupCNPGScript+" becomes dumpdb cnpg. The dump changes from plain SQL\n"+
			"stored as <host> to pg_dump's custom format stored as <host>.dmp, which is\n"+
			"what b2bk restore --to-database reads. Snapshots taken before the migration\n"+
			"keep the old format and name.")
	}
	if len(p.untagged) > 0 {
		out = append(out, fmt.Sprintf("tags: %s carried no --tag, and the operator tags every source, here\n"+
			"as %s. New snapshots start a series apart from the untagged ones, which this\n"+
			"backup's forget, scoped to its own tags, no longer expires.",
			strings.Join(p.untagged, " and "), strings.Join(defaultTags(p.sources), ", ")))
	}
	if p.unpruned {
		out = append(out, "prune: restic forget ran without --prune, and the operator's forget always\n"+
			"prunes, so data no snapshot references is now deleted on every run.")
	}
	return out
}

func defaultTags(sources []b2backupv1.BackupSource) []string {
	var out []string
	for _, src := range sources {
		if src.Tag == "" {
			out = append(out, src.EffectiveTag())
		}
	}
	return slices.Compact(out)
}

func parseBackupScript(script string, mounts []hrVolume) (parsedScript, error) {
	var parsed parsedScript
	forgets := 0

	for _, line := range joinContinuations(scriptBody(script)) {
		if shellSpecial(line) {
			return parsed, notStructured("%q relies on the shell to expand or escape something", line)
		}
		args := splitArgs(line)
		if len(args) < 2 || args[0] != "restic" {
			return parsed, notStructured("it runs %q", line)
		}

		switch args[1] {
		case "backup":
			source, engine, changed, err := parseBackupLine(args)
			if err != nil {
				return parsed, err
			}
			if source.Type == b2backupv1.SourceTypeFiles {
				for _, p := range source.Paths {
					if !underMount(p, mounts) {
						return parsed, notStructured("%q backs up %s, which is not an absolute path on a mounted "+
							"claim or emptyDir", line, p)
					}
				}
			}
			if engine != "" {
				parsed.engine = engine
			}
			parsed.dumpChanged = parsed.dumpChanged || changed
			if source.Tag == "" {
				parsed.untagged = append(parsed.untagged, fmt.Sprintf("%q", line))
			}
			parsed.sources = append(parsed.sources, source)
		case "forget":
			if forgets++; forgets > 1 {
				return parsed, notStructured("it runs restic forget more than once, and spec.retention is one policy")
			}
			r, prune, err := parseRetention(args)
			if err != nil {
				return parsed, err
			}
			if r == nil {
				return parsed, notStructured("%q keeps nothing by policy, which spec.retention cannot say", line)
			}
			parsed.retention = r
			parsed.unpruned = !prune
		default:
			return parsed, notStructured("it runs %q", line)
		}
	}

	return parsed, nil
}

// shellSpecial reports whether the shell would change line's words before restic
// saw them: an escape, an expansion, an unquoted glob, a redirection or another
// command. splitArgs does none of that, so such a line is kept verbatim.
func shellSpecial(line string) bool {
	var quote rune
	for _, r := range line {
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
			}
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '\\', '$', '`':
				return true
			}
		case r == '\'' || r == '"':
			quote = r
		case strings.ContainsRune("\\$`*?[~;&|<>(){}#", r):
			return true
		}
	}
	return false
}

func underMount(p string, mounts []hrVolume) bool {
	if !path.IsAbs(p) {
		return false
	}
	p = path.Clean(p)
	for _, m := range mounts {
		dir := path.Clean(m.path)
		if p == dir || strings.HasPrefix(p, dir+"/") {
			return true
		}
	}
	return false
}

func scriptBody(script string) string {
	lines := strings.Split(strings.TrimRight(script, "\n"), "\n")
	closed := len(lines) > 1 && groupClosing.MatchString(strings.TrimSpace(lines[len(lines)-1]))
	grouped := false
	for _, want := range preambleLines {
		if len(lines) == 0 || strings.TrimSpace(lines[0]) != want {
			continue
		}
		if want == "{" {
			if !closed {
				break
			}
			grouped = true
		}
		lines = lines[1:]
	}
	if grouped {
		lines = lines[:len(lines)-1]
	}
	if n := len(lines); n > 0 && strings.TrimSpace(lines[n-1]) == "restic cache --cleanup" {
		lines = lines[:n-1]
	}
	return strings.Join(lines, "\n") + "\n"
}

// groupClosing is the end of a { ... } group whose output only goes to ts,
// which the operator's own group replaces. scriptBody strips a leading { only
// together with it, and leaves any other group whole.
var groupClosing = regexp.MustCompile(`^}(\s*2>&1)?(\s*\|\s*ts(\s+('[^']*'|"[^"]*"|[^\s|;&<>]+))*)?$`)

var backupFlags = []string{"--tag=", "--exclude=", "--exclude", "--stdin-from-command"}

func parseBackupLine(args []string) (b2backupv1.BackupSource, b2backupv1.DatabaseEngine, bool, error) {
	var source b2backupv1.BackupSource
	line := strings.Join(args, " ")

	if tags := flagValues(args, "tag"); len(tags) > 1 {
		return source, "", false, notStructured("%q sets %d tags and a source has one", line, len(tags))
	} else if len(tags) == 1 {
		source.Tag = tags[0]
	}

	if i := indexOf(args, "--stdin-from-command"); i >= 0 {
		if extra := unknownFlags(args[2:i]); len(extra) > 0 {
			return source, "", false, notStructured("%q passes restic %s", line, strings.Join(extra, " "))
		}
		engine, database, changed, err := parseDumpCommand(args[i+1:])
		if err != nil {
			return source, "", false, notStructured("%s", err.Error())
		}
		source.Type = b2backupv1.SourceType(engine)
		source.Database = database
		return source, engine, changed, nil
	}

	source.Type = b2backupv1.SourceTypeFiles
	paths, excludes, err := parseFileArgs(args[2:])
	if err != nil {
		return source, "", false, err
	}
	source.Paths = paths
	source.Exclude = excludes
	return source, "", false, nil
}

func unknownFlags(args []string) []string {
	var out []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		if !slices.ContainsFunc(backupFlags, func(f string) bool {
			return a == f || (strings.HasSuffix(f, "=") && strings.HasPrefix(a, f))
		}) {
			out = append(out, a)
		}
	}
	return out
}

func parseFileArgs(args []string) ([]string, []string, error) {
	var paths []string
	var excludes []string

	for i := 0; i < len(args); i++ {
		if value, ok := strings.CutPrefix(args[i], "--exclude="); ok {
			excludes = append(excludes, value)
			continue
		}
		if args[i] == "--exclude" {
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("%w: --exclude has no pattern", ErrExcludeCount)
			}
			i++
			excludes = append(excludes, args[i])
			continue
		}
		if strings.HasPrefix(args[i], "-") {
			if strings.HasPrefix(args[i], "--tag=") {
				continue
			}
			return nil, nil, notStructured("restic backup is passed %s", args[i])
		}
		paths = append(paths, args[i])
	}
	if len(paths) == 0 {
		return nil, nil, notStructured("restic backup is given no path")
	}

	return paths, excludes, nil
}

func parseDumpCommand(args []string) (b2backupv1.DatabaseEngine, string, bool, error) {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return "", "", false, errors.New("a --stdin-from-command backup with no command")
	}

	switch args[0] {
	case "dumpdb":
		if len(args) < 2 {
			return "", "", false, errors.New("dumpdb without an engine")
		}
		engine := b2backupv1.DatabaseEngine(args[1])
		if engine != b2backupv1.DatabaseEngineCNPG && engine != b2backupv1.DatabaseEngineMariaDB {
			return "", "", false, fmt.Errorf("unrecognised dumpdb engine %q", args[1])
		}
		database := ""
		for _, a := range args[2:] {
			switch v, ok := strings.CutPrefix(a, "--database="); {
			case ok:
				database = v
			case strings.HasPrefix(a, "--secret-mount="):
			default:
				return "", "", false, fmt.Errorf("dumpdb is passed %s", a)
			}
		}
		return engine, database, false, nil

	case backupCNPGScript:
		database := ""
		for _, a := range args[1:] {
			v, ok := strings.CutPrefix(a, "--dbname=")
			if !ok {
				return "", "", false, fmt.Errorf("%s is passed %s", backupCNPGScript, a)
			}
			database = v
		}
		return b2backupv1.DatabaseEngineCNPG, database, true, nil
	}
	return "", "", false, fmt.Errorf("it dumps with %s", args[0])
}

// parseRetention reads a restic forget line's policy, and whether it prunes:
// --prune, or --prune= with a value strconv reads as true.
func parseRetention(args []string) (*b2backupv1.Retention, bool, error) {
	r := &b2backupv1.Retention{}
	fields := map[string]**int32{
		"last": &r.Last, "hourly": &r.Hourly, "daily": &r.Daily,
		"weekly": &r.Weekly, "monthly": &r.Monthly, "yearly": &r.Yearly,
	}

	prune := false
	for _, a := range args[2:] {
		if a == "--prune" {
			prune = true
			continue
		}
		if v, ok := strings.CutPrefix(a, "--prune="); ok {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, false, notStructured("restic forget is passed %s", a)
			}
			prune = b
			continue
		}
		name, value, ok := strings.Cut(strings.TrimPrefix(a, "--keep-"), "=")
		target, known := fields[name]
		if !ok || !known || !strings.HasPrefix(a, "--keep-") {
			return nil, false, notStructured("restic forget is passed %s", a)
		}
		n, err := parseInt32(value)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", a, err)
		}
		*target = n
	}
	if *r == (b2backupv1.Retention{}) {
		return nil, prune, nil
	}
	return r, prune, nil
}

func shorthandSchedule(expr string) string {
	shorthand := shorthandFor(expr)
	if shorthand == expr {
		return expr
	}
	if _, err := backup.ResolveSchedule(shorthand, expr); err != nil {
		return expr
	}
	return shorthand
}

func shorthandFor(expr string) string {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return expr
	}
	minute, hour := fields[0], fields[1]
	if fields[2] != "*" || fields[3] != "*" || fields[4] != "*" {
		return expr
	}
	if !isNumeric(minute) {
		return expr
	}

	switch {
	case hour == "*":
		return "@hourly"
	case isNumeric(hour):
		return "@daily"
	default:
		if step, ok := strings.CutPrefix(hour, "*/"); ok && isNumeric(step) {
			return "@every " + step + "h"
		}
	}
	return expr
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}
