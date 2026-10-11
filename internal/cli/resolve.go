package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

// TargetKind is the kind of resource a Target refers to.
type TargetKind string

const (
	// TargetRepository is a Repository.
	TargetRepository TargetKind = "repository"
	// TargetScheduledBackup is a ScheduledBackup.
	TargetScheduledBackup TargetKind = "scheduledbackup"
)

var (
	// ErrTargetNotFound means no resource matched the argument.
	ErrTargetNotFound = errors.New("not found")
	// ErrAmbiguous means the name matched more than one kind.
	ErrAmbiguous = errors.New("ambiguous name")
	// ErrUnknownKind means the kind prefix is not recognised.
	ErrUnknownKind = errors.New("unknown resource type")
)

var kindAliases = map[string]TargetKind{
	"repository":       TargetRepository,
	"repositories":     TargetRepository,
	"repo":             TargetRepository,
	"repos":            TargetRepository,
	"scheduledbackup":  TargetScheduledBackup,
	"scheduledbackups": TargetScheduledBackup,
	"sb":               TargetScheduledBackup,
	"backup":           TargetScheduledBackup,
	"backups":          TargetScheduledBackup,
}

// Target is a Repository or ScheduledBackup resolved from a command-line argument.
type Target struct {
	Kind            TargetKind
	Repository      *b2backupv1.Repository
	ScheduledBackup *b2backupv1.ScheduledBackup
}

// Name returns the target's name.
func (t *Target) Name() string {
	if t.Kind == TargetRepository {
		return t.Repository.Name
	}
	return t.ScheduledBackup.Name
}

// Namespace returns the target's namespace.
func (t *Target) Namespace() string {
	if t.Kind == TargetRepository {
		return t.Repository.Namespace
	}
	return t.ScheduledBackup.Namespace
}

// Resolve looks up a target from an argument such as "name", "repo/name" or "sb/name".
func Resolve(ctx context.Context, c client.Client, namespace, arg string) (*Target, error) {
	if arg == "" {
		return nil, fmt.Errorf("%w: empty name", ErrTargetNotFound)
	}

	if prefix, name, ok := strings.Cut(arg, "/"); ok {
		kind, known := kindAliases[strings.ToLower(prefix)]
		if !known {
			return nil, fmt.Errorf("%w %q, expected one of: %s",
				ErrUnknownKind, prefix, strings.Join(knownPrefixes(), ", "))
		}
		if name == "" {
			return nil, fmt.Errorf("%w: %q has no name", ErrTargetNotFound, arg)
		}
		return get(ctx, c, namespace, kind, name)
	}

	sb, sbErr := getScheduledBackup(ctx, c, namespace, arg)
	repo, repoErr := getRepository(ctx, c, namespace, arg)

	switch {
	case sbErr == nil && repoErr == nil:
		return nil, fmt.Errorf(
			"%w: %q is both a ScheduledBackup and a Repository; qualify it as sb/%s or repo/%s",
			ErrAmbiguous, arg, arg, arg)
	case sbErr == nil:
		return &Target{Kind: TargetScheduledBackup, ScheduledBackup: sb}, nil
	case repoErr == nil:
		return &Target{Kind: TargetRepository, Repository: repo}, nil
	}

	for _, err := range []error{sbErr, repoErr} {
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%w: no ScheduledBackup or Repository named %q in namespace %q",
		ErrTargetNotFound, arg, namespace)
}

func get(ctx context.Context, c client.Client, namespace string, kind TargetKind, name string) (*Target, error) {
	if kind == TargetRepository {
		repo, err := getRepository(ctx, c, namespace, name)
		if err != nil {
			return nil, wrapNotFound(err, "Repository", namespace, name)
		}
		return &Target{Kind: kind, Repository: repo}, nil
	}

	sb, err := getScheduledBackup(ctx, c, namespace, name)
	if err != nil {
		return nil, wrapNotFound(err, "ScheduledBackup", namespace, name)
	}
	return &Target{Kind: kind, ScheduledBackup: sb}, nil
}

func getRepository(ctx context.Context, c client.Client, namespace, name string) (*b2backupv1.Repository, error) {
	var repo b2backupv1.Repository
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &repo); err != nil {
		return nil, err
	}
	return &repo, nil
}

func getScheduledBackup(ctx context.Context, c client.Client, namespace, name string) (*b2backupv1.ScheduledBackup, error) {
	var sb b2backupv1.ScheduledBackup
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &sb); err != nil {
		return nil, err
	}
	return &sb, nil
}

// RepositoryFor returns the Repository a backup writes to.
func RepositoryFor(ctx context.Context, c client.Client, sb *b2backupv1.ScheduledBackup) (*b2backupv1.Repository, error) {
	name := sb.Spec.RepositoryRef.Name
	repo, err := getRepository(ctx, c, sb.Namespace, name)
	if err != nil {
		return nil, wrapNotFound(err, "Repository", sb.Namespace,
			fmt.Sprintf("%s (referenced by scheduledbackup/%s)", name, sb.Name))
	}
	return repo, nil
}

func wrapNotFound(err error, kind, namespace, name string) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: no %s named %q in namespace %q", ErrTargetNotFound, kind, name, namespace)
	}
	return err
}

func knownPrefixes() []string {
	return slices.Sorted(maps.Keys(kindAliases))
}

var (
	// ErrNothingToName means a command given no name found nothing it could act on.
	ErrNothingToName = errors.New("nothing to act on")
	// ErrNameNeeded means a command given no name found several things it could act on.
	ErrNameNeeded = errors.New("a name is needed")
)

const maxListed = 10

// nameKinds says which kinds a command accepts as its name argument.
type nameKinds struct {
	backups      bool
	repositories bool
}

var (
	anyKind          = nameKinds{backups: true, repositories: true}
	backupsOnly      = nameKinds{backups: true}
	repositoriesOnly = nameKinds{repositories: true}
)

func (k nameKinds) noun() string {
	switch {
	case k.backups && k.repositories:
		return kindScheduledBackupDoc + " or " + kindRepositoryDoc
	case k.repositories:
		return kindRepositoryDoc
	}
	return kindScheduledBackupDoc
}

// namesIn returns the sorted names of the ScheduledBackups and Repositories
// of kinds in namespace.
func namesIn(ctx context.Context, c client.Client, namespace string, kinds nameKinds) ([]string, []string, error) {
	var backups, repos []string
	if kinds.backups {
		var list b2backupv1.ScheduledBackupList
		if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
			return nil, nil, err
		}
		for i := range list.Items {
			backups = append(backups, list.Items[i].Name)
		}
		slices.Sort(backups)
	}
	if kinds.repositories {
		var list b2backupv1.RepositoryList
		if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
			return nil, nil, err
		}
		for i := range list.Items {
			repos = append(repos, list.Items[i].Name)
		}
		slices.Sort(repos)
	}
	return backups, repos, nil
}

// onlyTarget returns, qualified, the one ScheduledBackup of kinds in namespace,
// or, when there are none and kinds takes Repositories, the one Repository.
// The example names how to run cmd with a name.
func onlyTarget(
	ctx context.Context, cmd *cobra.Command, c client.Client, namespace string, kinds nameKinds,
) (string, error) {
	backups, repos, err := namesIn(ctx, c, namespace, kinds)
	if err != nil {
		return "", fmt.Errorf("listing what to act on in namespace %q: %w", namespace, err)
	}

	names, prefix, plural := backups, string(TargetScheduledBackup)+"/", "ScheduledBackups"
	if len(backups) == 0 && kinds.repositories {
		names, prefix, plural = repos, string(TargetRepository)+"/", "Repositories"
	}

	switch len(names) {
	case 0:
		return "", &sentinelError{ErrNothingToName, fmt.Sprintf(
			"no %s in namespace %q; pick the namespace with -n NAMESPACE, or find them with %s get -A",
			kinds.noun(), namespace, cmd.Root().CommandPath())}
	case 1:
		return prefix + names[0], nil
	}

	qualified := make([]string, len(names))
	for i, name := range names {
		qualified[i] = prefix + name
	}
	return "", &sentinelError{ErrNameNeeded, fmt.Sprintf("namespace %q has %d %s; pick one, e.g. %s\n%s",
		namespace, len(names), plural, example(cmd, qualified[0]), listNames(qualified))}
}

func example(cmd *cobra.Command, name string) string {
	ex := cmd.CommandPath() + " " + name
	if _, after, ok := strings.Cut(cmd.Use, " -- "); ok {
		ex += " -- " + after
	}
	return ex
}

func listNames(names []string) string {
	shown := names[:min(len(names), maxListed)]
	out := "  " + strings.Join(shown, "\n  ")
	if more := len(names) - len(shown); more > 0 {
		out += fmt.Sprintf("\n  and %d more", more)
	}
	return out
}
