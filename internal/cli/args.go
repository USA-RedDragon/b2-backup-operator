package cli

import (
	"errors"
	"fmt"
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
