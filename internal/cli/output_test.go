package cli

import (
	"io"
	"strings"
	"testing"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

func TestPrintObjectRejectsNonMachineFormats(t *testing.T) {
	for _, format := range []string{OutputTable, OutputWide, OutputName, "toml"} {
		err := PrintObject(io.Discard, format, &b2backupv1.RepositoryList{})
		if err == nil {
			t.Errorf("PrintObject(%q) returned no error", format)
			continue
		}
		if strings.Contains(err.Error(), "%!w") {
			t.Errorf("PrintObject(%q) = %v, which is a formatting bug, not a message", format, err)
		}
	}
}
