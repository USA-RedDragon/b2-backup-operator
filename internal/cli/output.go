package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/duration"
	"sigs.k8s.io/yaml"
)

const (
	// OutputTable is the default human-readable table.
	OutputTable = ""
	// OutputWide is the table with extra columns.
	OutputWide = "wide"
	// OutputJSON prints the object as JSON.
	OutputJSON = "json"
	// OutputYAML prints the object as YAML.
	OutputYAML = "yaml"
	// OutputName prints only resource names.
	OutputName = "name"
)

var outputFormats = []string{OutputWide, OutputJSON, OutputYAML, OutputName}

// AddOutputFlag registers the -o/--output flag on cmd.
func AddOutputFlag(cmd *cobra.Command, target *string) {
	cmd.Flags().StringVarP(target, "output", "o", OutputTable,
		fmt.Sprintf("Output format. One of: %s", strings.Join(outputFormats, "|")))

	_ = cmd.RegisterFlagCompletionFunc("output",
		func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return outputFormats, cobra.ShellCompDirectiveNoFileComp
		})
}

// ValidateOutput reports whether format is a supported output format.
func ValidateOutput(format string) error {
	switch format {
	case OutputTable, OutputWide, OutputJSON, OutputYAML, OutputName:
		return nil
	default:
		return fmt.Errorf("unsupported output format %q, expected one of: %s",
			format, strings.Join(outputFormats, "|"))
	}
}

// IsMachineOutput reports whether format is meant for machines rather than a terminal.
func IsMachineOutput(format string) bool {
	return format == OutputJSON || format == OutputYAML
}

// PrintObject writes obj to w as JSON or YAML.
func PrintObject(w io.Writer, format string, obj runtime.Object) error {
	switch format {
	case OutputJSON:
		data, err := json.MarshalIndent(obj, "", "    ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "%s\n", data)
		return err
	case OutputYAML:
		data, err := yaml.Marshal(obj)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	default:
		return fmt.Errorf("cannot print an object as %q, expected %s or %s",
			format, OutputJSON, OutputYAML)
	}
}

// NewTabWriter returns a tab writer with the column layout used by the table output.
func NewTabWriter(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 6, 4, 3, ' ', 0)
}

// Age renders how long ago t was.
func Age(t metav1.Time) string {
	if t.IsZero() {
		return "<unknown>"
	}
	return duration.HumanDuration(time.Since(t.Time))
}

// Since renders how long ago t was, or <none>.
func Since(t *metav1.Time) string {
	if t == nil || t.IsZero() {
		return "<none>"
	}
	return duration.HumanDuration(time.Since(t.Time)) + " ago"
}

// FindCondition returns the condition of the given type, or nil.
func FindCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}

// ConditionStatus returns the status of the given condition type, or Unknown.
func ConditionStatus(conds []metav1.Condition, condType string) string {
	if c := FindCondition(conds, condType); c != nil {
		return string(c.Status)
	}
	return string(metav1.ConditionUnknown)
}

func yesNo(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}

type printer struct {
	w   io.Writer
	err error
}

func newPrinter(w io.Writer) *printer { return &printer{w: w} }

func (p *printer) Write(b []byte) (int, error) {
	if p.err != nil {
		return 0, p.err
	}
	n, err := p.w.Write(b)
	p.err = err
	return n, err
}

func (p *printer) print(a ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprint(p.w, a...)
	}
}

func (p *printer) printf(format string, a ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format, a...)
	}
}

func (p *printer) println(a ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintln(p.w, a...)
	}
}

func (p *printer) Err() error { return p.err }

func flushTable(p *printer, tw *tabwriter.Writer) error {
	if err := p.Err(); err != nil {
		return err
	}
	return tw.Flush()
}
