package cli

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// ErrUsage means a command was given the wrong arguments.
var ErrUsage = errors.New("wrong arguments")

// sentinelError reads as its message alone and matches its sentinel.
type sentinelError struct {
	sentinel error
	msg      string
}

func (e *sentinelError) Error() string { return e.msg }

func (e *sentinelError) Is(target error) bool { return target == e.sentinel }

// usageError explains a wrong argument count with the command's usage line.
func usageError(cmd *cobra.Command, problem string) error {
	return &sentinelError{ErrUsage, fmt.Sprintf("%s %s: %s", cmd.Name(), problem, usageLine(cmd))}
}

// usageLine is the command's usage without the trailing [flags].
func usageLine(cmd *cobra.Command) string {
	return strings.TrimSuffix(cmd.UseLine(), " [flags]")
}

// argCount accepts between lo and hi arguments, each described as noun.
func argCount(lo, hi int, noun string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) >= lo && len(args) <= hi {
			return nil
		}
		return usageError(cmd, countProblem(lo, hi, noun, len(args)))
	}
}

func countProblem(lo, hi int, noun string, got int) string {
	var want string
	switch {
	case hi == 0:
		return "takes no " + plural(noun, 2) + ", got " + strconv.Itoa(got)
	case lo == hi:
		want = "takes exactly " + countOf(hi, noun)
	case lo == 0:
		want = "takes at most " + countOf(hi, noun)
	case got < lo:
		want = "needs at least " + countOf(lo, noun)
	default:
		want = "takes at most " + countOf(hi, noun)
	}
	if got == 0 {
		return want + ", got none"
	}
	return want + ", got " + strconv.Itoa(got)
}

func countOf(n int, noun string) string {
	words := []string{"no", "one", "two", "three"}
	count := strconv.Itoa(n)
	if n < len(words) {
		count = words[n]
	}
	return count + " " + plural(noun, n)
}

func plural(noun string, n int) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}

// nameArg returns the name in args or, when there is none, the only resource
// of kinds in the namespace, saying on stderr which it picked.
func (f *Factory) nameArg(cmd *cobra.Command, args []string, kinds nameKinds) (string, error) {
	if len(args) > 0 {
		return args[0], nil
	}
	c, err := f.Client()
	if err != nil {
		return "", err
	}
	ns, err := f.Namespace()
	if err != nil {
		return "", err
	}
	arg, err := onlyTarget(cmd.Context(), cmd, c, ns, kinds)
	if err != nil {
		return "", err
	}
	if _, err := fmt.Fprintf(f.Streams.ErrOut, "using %s\n", arg); err != nil {
		return "", err
	}
	return arg, nil
}

// requiredName wants exactly one name, for a command that can destroy data and
// so never picks one on its own; does says why. Without a name, the error lists
// the candidates in the namespace when it can read them.
func (f *Factory) requiredName(kinds nameKinds, does string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) == 1 {
			return nil
		}
		if len(args) > 1 {
			return usageError(cmd, countProblem(1, 1, "name", len(args)))
		}
		msg := fmt.Sprintf("%s needs a name, even when there is only one, since it %s: %s",
			cmd.Name(), does, usageLine(cmd))
		return &sentinelError{ErrUsage, msg + f.candidates(cmd, kinds)}
	}
}

// candidates lists what the namespace holds of kinds, or nothing when that
// cannot be read.
func (f *Factory) candidates(cmd *cobra.Command, kinds nameKinds) string {
	c, err := f.Client()
	if err != nil {
		return ""
	}
	ns, err := f.Namespace()
	if err != nil {
		return ""
	}
	backups, repos, err := namesIn(cmd.Context(), c, ns, kinds)
	if err != nil {
		return ""
	}
	names := make([]string, 0, len(backups)+len(repos))
	for _, name := range backups {
		names = append(names, string(TargetScheduledBackup)+"/"+name)
	}
	for _, name := range repos {
		names = append(names, string(TargetRepository)+"/"+name)
	}
	if len(names) == 0 {
		return fmt.Sprintf("\nnamespace %q has no %s", ns, kinds.noun())
	}
	return fmt.Sprintf("\nnamespace %q has:\n%s\ne.g. %s", ns, listNames(names), example(cmd, names[0]))
}

// execArgs wants at most one name, then --, then the command.
func execArgs(cmd *cobra.Command, args []string) error {
	dash := cmd.ArgsLenAtDash()
	switch {
	case dash < 0:
		return usageError(cmd, "needs -- before the command")
	case dash > 1:
		return usageError(cmd, "takes at most one name before --, got "+strconv.Itoa(dash))
	case len(args) == dash:
		return usageError(cmd, "needs a command after --")
	}
	return nil
}

// completeName offers the names of kinds in the namespace for a command's
// one name, qualifying a name that both kinds use.
func (f *Factory) completeName(kinds nameKinds) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		c, err := f.Client()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		ns, err := f.Namespace()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		backups, repos, err := namesIn(cmd.Context(), c, ns, kinds)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		names := make([]cobra.Completion, 0, len(backups)+len(repos))
		for _, name := range backups {
			if slices.Contains(repos, name) {
				name = string(TargetScheduledBackup) + "/" + name
			}
			names = append(names, name)
		}
		for _, name := range repos {
			if slices.Contains(backups, name) {
				name = string(TargetRepository) + "/" + name
			}
			names = append(names, name)
		}
		return names, cobra.ShellCompDirectiveNoFileComp
	}
}
