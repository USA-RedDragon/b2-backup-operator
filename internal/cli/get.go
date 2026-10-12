package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

func newGetCommand(f *Factory) *cobra.Command {
	var output string

	cmd := &cobra.Command{
		Use:     "get [repositories|backups]",
		Short:   "List repositories and scheduled backups",
		GroupID: GroupInspect,
		Long: `List the operator's resources.

With no argument both kinds are listed. Pass "repositories" or "backups" to
narrow it.`,
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: knownPrefixes(),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ValidateOutput(output); err != nil {
				return err
			}

			kinds, err := kindsToList(args)
			if err != nil {
				return err
			}

			c, err := f.Client()
			if err != nil {
				return err
			}
			ns, err := f.ListNamespace()
			if err != nil {
				return err
			}

			return runGet(cmd.Context(), c, f.Streams.Out, ns, f.AllNamespaces(), output, kinds)
		},
	}

	AddOutputFlag(cmd, &output)
	f.AddAllNamespacesFlag(cmd)
	return cmd
}

func kindsToList(args []string) ([]TargetKind, error) {
	if len(args) == 0 {
		return []TargetKind{TargetRepository, TargetScheduledBackup}, nil
	}
	kind, ok := kindAliases[strings.ToLower(args[0])]
	if !ok {
		return nil, fmt.Errorf("%w %q, expected one of: %s",
			ErrUnknownKind, args[0], strings.Join(knownPrefixes(), ", "))
	}
	return []TargetKind{kind}, nil
}

func runGet(
	ctx context.Context,
	c client.Client,
	out io.Writer,
	namespace string,
	allNamespaces bool,
	output string,
	kinds []TargetKind,
) error {
	var repos b2backupv1.RepositoryList
	var backups b2backupv1.ScheduledBackupList

	for _, kind := range kinds {
		var list client.ObjectList
		if kind == TargetRepository {
			list = &repos
		} else {
			list = &backups
		}
		if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
			return err
		}
	}

	if IsMachineOutput(output) {
		return printGetObjects(out, output, kinds, &repos, &backups)
	}

	p := newPrinter(out)
	empty := true
	for _, kind := range kinds {
		var err error
		if kind == TargetRepository {
			if len(repos.Items) > 0 {
				empty = false
				err = printRepositories(out, repos.Items, allNamespaces, output)
			}
		} else {
			if len(backups.Items) > 0 {
				if !empty {
					p.println()
				}
				empty = false
				err = printScheduledBackups(out, backups.Items, allNamespaces, output)
			}
		}
		if err != nil {
			return err
		}
	}

	if empty {
		scope := fmt.Sprintf("in namespace %q", namespace)
		if allNamespaces {
			scope = "in any namespace"
		}
		p.printf("No resources found %s.\n", scope)
	}
	return p.Err()
}

func printGetObjects(
	out io.Writer,
	output string,
	kinds []TargetKind,
	repos *b2backupv1.RepositoryList,
	backups *b2backupv1.ScheduledBackupList,
) error {
	if len(kinds) == 1 {
		if kinds[0] == TargetRepository {
			repos.TypeMeta = typeMetaFor("RepositoryList")
			return PrintObject(out, output, repos)
		}
		backups.TypeMeta = typeMetaFor("ScheduledBackupList")
		return PrintObject(out, output, backups)
	}

	list := &corev1.List{
		APIVersion: "v1", Kind: "List",
		Items: make([]runtime.RawExtension, 0, len(repos.Items)+len(backups.Items)),
	}
	for _, kind := range kinds {
		if kind == TargetRepository {
			for i := range repos.Items {
				repos.Items[i].TypeMeta = typeMetaFor("Repository")
				list.Items = append(list.Items, runtime.RawExtension{Object: &repos.Items[i]})
			}
			continue
		}
		for i := range backups.Items {
			backups.Items[i].TypeMeta = typeMetaFor("ScheduledBackup")
			list.Items = append(list.Items, runtime.RawExtension{Object: &backups.Items[i]})
		}
	}
	return PrintObject(out, output, list)
}

func typeMetaFor(kind string) metav1.TypeMeta {
	return metav1.TypeMeta{APIVersion: b2backupv1.SchemeGroupVersion.String(), Kind: kind}
}

func printRepositories(out io.Writer, items []b2backupv1.Repository, allNamespaces bool, output string) error {
	if output == OutputName {
		p := newPrinter(out)
		for i := range items {
			p.printf("repository.b2backup.mcswain.dev/%s\n", items[i].Name)
		}
		return p.Err()
	}

	tw := NewTabWriter(out)
	w := newPrinter(tw)
	writeHeader(w, allNamespaces, output,
		[]string{"NAME", "BUCKET", "PATH", "READY", "INITIALIZED", "USAGE", "AGE"},
		[]string{"MODE", "KEY ID", "SECRET", "ADOPTED", "SUSPENDED"})

	for i := range items {
		r := &items[i]
		writeNamespace(w, allNamespaces, r.Namespace)
		w.printf("%s\t%s\t%s\t%s\t%s\t%s\t%s",
			r.Name,
			orNone(r.Status.BucketName),
			repositoryPath(r),
			ConditionStatus(r.Status.Conditions, b2backupv1.RepositoryConditionReady),
			yesNo(r.Status.Initialized),
			orNone(r.Status.CurrentUsage),
			Age(r.CreationTimestamp),
		)
		if output == OutputWide {
			w.printf("\t%s\t%s\t%s\t%s\t%s",
				orNone(string(r.Spec.Mode)),
				orNone(r.Status.ApplicationKeyID),
				orNone(r.Status.SecretName),
				yesNo(r.Status.Adopted),
				yesNo(r.Spec.Suspend),
			)
		}
		w.println()
	}
	return flushTable(w, tw)
}

func repositoryPath(r *b2backupv1.Repository) string {
	switch {
	case r.Status.BucketID == "":
		return orNone("")
	case r.Status.Path == "":
		return bucketRoot
	default:
		return r.Status.Path
	}
}

func printScheduledBackups(out io.Writer, items []b2backupv1.ScheduledBackup, allNamespaces bool, output string) error {
	if output == OutputName {
		p := newPrinter(out)
		for i := range items {
			p.printf("scheduledbackup.b2backup.mcswain.dev/%s\n", items[i].Name)
		}
		return p.Err()
	}

	tw := NewTabWriter(out)
	w := newPrinter(tw)
	writeHeader(w, allNamespaces, output,
		[]string{"NAME", "REPOSITORY", "SCHEDULE", "READY", "LAST BACKUP", "SUSPENDED", "ACTIVE", "AGE"},
		[]string{"TIMEZONE", "CONCURRENCY"})

	for i := range items {
		b := &items[i]
		writeNamespace(w, allNamespaces, b.Namespace)
		w.printf("%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s",
			b.Name,
			b.Spec.RepositoryRef.Name,
			orNone(b.Status.EffectiveSchedule),
			ConditionStatus(b.Status.Conditions, b2backupv1.ScheduledBackupConditionReady),
			Since(lastSuccess(b)),
			yesNo(b.Spec.Suspend),
			b.Status.Active,
			Age(b.CreationTimestamp),
		)
		if output == OutputWide {
			w.printf("\t%s\t%s", orNone(b.Spec.TimeZone), orNone(string(b.Spec.ConcurrencyPolicy)))
		}
		w.println()
	}
	return flushTable(w, tw)
}

func writeHeader(w *printer, allNamespaces bool, output string, cols, wideCols []string) {
	if allNamespaces {
		w.print("NAMESPACE\t")
	}
	for i, c := range cols {
		if i > 0 {
			w.print("\t")
		}
		w.print(c)
	}
	if output == OutputWide {
		for _, c := range wideCols {
			w.printf("\t%s", c)
		}
	}
	w.println()
}

func writeNamespace(w *printer, allNamespaces bool, ns string) {
	if allNamespaces {
		w.printf("%s\t", ns)
	}
}

func lastSuccess(sb *b2backupv1.ScheduledBackup) *metav1.Time { return backup.LastSuccess(sb) }
