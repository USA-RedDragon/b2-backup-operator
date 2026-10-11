package cli

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/controller"
)

const redacted = "***"

func newEnvCommand(f *Factory) *cobra.Command {
	var showPassword bool

	cmd := &cobra.Command{
		Use:     "env <name>",
		Short:   "Print the restic environment for a repository",
		GroupID: GroupEscape,
		Long: `Print the restic credentials as shell export statements.

The password is redacted unless --show-password is passed. Note that
RESTIC_REPOSITORY embeds the password too, so it is redacted along with it.

With --show-password the output is suitable for:

    eval "$(b2bk env repo/store --show-password)"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := f.Client()
			if err != nil {
				return err
			}
			ns, err := f.Namespace()
			if err != nil {
				return err
			}
			return runEnv(cmd.Context(), c, f.Streams.Out, f.Streams.ErrOut, ns, args[0], showPassword)
		},
	}

	cmd.Flags().BoolVar(&showPassword, "show-password", false,
		"Print the real password instead of redacting it")
	return cmd
}

func runEnv(
	ctx context.Context,
	c client.Client,
	out, errOut io.Writer,
	namespace, arg string,
	showPassword bool,
) error {
	target, err := Resolve(ctx, c, namespace, arg)
	if err != nil {
		return err
	}

	repo := target.Repository
	if target.Kind == TargetScheduledBackup {
		if repo, err = RepositoryFor(ctx, c, target.ScheduledBackup); err != nil {
			return err
		}
	}

	secret, err := credentials(ctx, c, repo)
	if err != nil {
		return err
	}

	repoURL := string(secret.Data[controller.KeyResticRepository])
	password := string(secret.Data[controller.KeyResticPassword])

	if !showPassword {
		repoURL = RedactResticURL(repoURL)
		password = redacted
		hint := newPrinter(errOut)
		hint.println("# password redacted; pass --show-password to reveal it")
		if err := hint.Err(); err != nil {
			return err
		}
	}

	p := newPrinter(out)
	p.printf("export %s=%s\n", controller.KeyResticRepository, shellQuote(repoURL))
	p.printf("export %s=%s\n", controller.KeyResticPassword, shellQuote(password))
	return p.Err()
}

func credentials(ctx context.Context, c client.Client, repo *b2backupv1.Repository) (*corev1.Secret, error) {
	name := repo.SecretName()
	var secret corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Namespace: repo.Namespace, Name: name}, &secret)
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("%w: credentials Secret %q for repository %q; run: b2bk doctor repo/%s",
			ErrTargetNotFound, name, repo.Name, repo.Name)
	}
	if err != nil {
		return nil, err
	}
	return &secret, nil
}

// RedactResticURL replaces the password in a restic rest: URL.
func RedactResticURL(raw string) string {
	if raw == "" {
		return ""
	}

	scheme, rest, ok := strings.Cut(raw, ":")
	if !ok || scheme != "rest" {
		return redacted
	}

	u, err := url.Parse(rest)
	if err != nil {
		return redacted
	}
	if u.User == nil {
		return raw
	}
	if _, hasPassword := u.User.Password(); !hasPassword {
		return raw
	}

	u.User = url.UserPassword(u.User.Username(), redacted)

	return scheme + ":" + strings.ReplaceAll(u.String(), url.QueryEscape(redacted), redacted)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
