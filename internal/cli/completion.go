package cli

import (
	"slices"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
)

var manifestExtensions = []string{"yaml", "yml"}

// completeManifests offers YAML files, and the directories that lead to them.
func completeManifests(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
	return manifestExtensions, cobra.ShellCompDirectiveFilterFileExt
}

// completeDirs offers directories only.
func completeDirs(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveFilterDirs
}

// completeFiles offers any file, for a path whose name has no fixed extension.
func completeFiles(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveDefault
}

// completeManifestArgs offers YAML files for the first n arguments.
func completeManifestArgs(n int) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if len(args) >= n {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeManifests(cmd, args, toComplete)
	}
}

// completeNameOrManifest offers ScheduledBackup names, or YAML files once what
// is typed looks like a path rather than a name.
func (f *Factory) completeNameOrManifest(
	cmd *cobra.Command, args []string, toComplete string,
) ([]cobra.Completion, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	if looksLikePath(toComplete) {
		return completeManifests(cmd, args, toComplete)
	}
	names, directive := f.completeName(backupsOnly)(cmd, args, toComplete)
	if len(names) == 0 {
		return completeManifests(cmd, args, toComplete)
	}
	return names, directive
}

func looksLikePath(s string) bool {
	if strings.HasPrefix(s, ".") || strings.HasPrefix(s, "~") {
		return true
	}
	prefix, _, ok := strings.Cut(s, "/")
	if !ok {
		return false
	}
	_, kind := kindAliases[strings.ToLower(prefix)]
	return !kind
}

// registerConnectionFlagCompletions completes the kubeconfig flags: paths as
// paths, and the namespace, context, cluster and user from the cluster and
// kubeconfig. Every other flag value falls back to the root's no-file default.
func (f *Factory) registerConnectionFlagCompletions(cmd *cobra.Command) {
	for _, name := range []string{"kubeconfig", "certificate-authority", "client-certificate", "client-key"} {
		if cmd.PersistentFlags().Lookup(name) != nil {
			_ = cmd.RegisterFlagCompletionFunc(name, completeFiles)
		}
	}
	if cmd.PersistentFlags().Lookup("cache-dir") != nil {
		_ = cmd.RegisterFlagCompletionFunc("cache-dir", completeDirs)
	}
	_ = cmd.RegisterFlagCompletionFunc("namespace", f.completeNamespaces)
	_ = cmd.RegisterFlagCompletionFunc("context", f.completeKubeconfig(contextNames))
	_ = cmd.RegisterFlagCompletionFunc("cluster", f.completeKubeconfig(clusterNames))
	_ = cmd.RegisterFlagCompletionFunc("user", f.completeKubeconfig(userNames))
}

func (f *Factory) completeNamespaces(
	cmd *cobra.Command, _ []string, toComplete string,
) ([]cobra.Completion, cobra.ShellCompDirective) {
	c, err := f.Client()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var list corev1.NamespaceList
	if err := c.List(cmd.Context(), &list); err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var names []cobra.Completion
	for i := range list.Items {
		if strings.HasPrefix(list.Items[i].Name, toComplete) {
			names = append(names, list.Items[i].Name)
		}
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

type kubeconfigNames int

const (
	contextNames kubeconfigNames = iota
	clusterNames
	userNames
)

func (f *Factory) completeKubeconfig(which kubeconfigNames) cobra.CompletionFunc {
	return func(_ *cobra.Command, _ []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		raw, err := f.ConfigFlags.ToRawKubeConfigLoader().RawConfig()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var names []string
		switch which {
		case contextNames:
			for name := range raw.Contexts {
				names = append(names, name)
			}
		case clusterNames:
			for name := range raw.Clusters {
				names = append(names, name)
			}
		case userNames:
			for name := range raw.AuthInfos {
				names = append(names, name)
			}
		}
		names = slices.DeleteFunc(names, func(n string) bool { return !strings.HasPrefix(n, toComplete) })
		slices.Sort(names)
		return names, cobra.ShellCompDirectiveNoFileComp
	}
}
