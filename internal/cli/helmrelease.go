package cli

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// Errors returned when reading a HelmRelease.
var (
	ErrNoResticController = errors.New("no restic controller in the HelmRelease")
	ErrNoResticScript     = errors.New("no restic command in the HelmRelease")
	ErrMultipleClaims     = errors.New("more than one persistence entry sets existingClaim")
	ErrMultipleDBSecrets  = errors.New("more than one persistence entry is a secret")
)

const resticController = "restic"

type helmRelease struct {
	Metadata struct {
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		Values struct {
			Controllers map[string]hrController  `json:"controllers"`
			Persistence map[string]hrPersistence `json:"persistence"`
		} `json:"values"`
	} `json:"spec"`
}

type hrController struct {
	CronJob struct {
		Schedule          string `json:"schedule"`
		ConcurrencyPolicy string `json:"concurrencyPolicy"`
		TimeZone          string `json:"timeZone"`
	} `json:"cronjob"`
	Containers map[string]hrContainer `json:"containers"`
}

type hrContainer struct {
	Command    []string `json:"command"`
	WorkingDir string   `json:"workingDir"`

	Env map[string]any `json:"env"`
}

type hrPersistence struct {
	Type          string `json:"type"`
	Name          string `json:"name"`
	ExistingClaim string `json:"existingClaim"`
	StorageClass  string `json:"storageClass"`
}

func readHelmRelease(path string) (*helmRelease, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var hr helmRelease
	if err := yaml.Unmarshal(raw, &hr); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &hr, nil
}

func (hr *helmRelease) backupController() (hrController, hrContainer, error) {
	controller, ok := hr.Spec.Values.Controllers[resticController]
	if !ok {
		return hrController{}, hrContainer{}, ErrNoResticController
	}
	container, ok := controller.Containers[resticController]
	if !ok {
		return hrController{}, hrContainer{}, ErrNoResticScript
	}
	return controller, container, nil
}

func (c hrContainer) script() (string, error) {
	if len(c.Command) == 0 {
		return "", ErrNoResticScript
	}
	return c.Command[len(c.Command)-1], nil
}

func (c hrContainer) env(name string) string {
	v, ok := c.Env[name]
	if !ok {
		return ""
	}
	switch value := v.(type) {
	case string:
		return value
	case int, int64, float64, bool:
		return fmt.Sprint(value)
	default:

		return ""
	}
}

func (hr *helmRelease) existingClaim() (string, error) {
	claims := hr.persistenceMatching(func(p hrPersistence) string { return p.ExistingClaim })
	if len(claims) > 1 {
		return "", fmt.Errorf("%w (%s); a ScheduledBackup mounts one volume, so migrate cannot "+
			"choose between them", ErrMultipleClaims, strings.Join(sortedKeys(claims), ", "))
	}
	for _, name := range sortedKeys(claims) {
		return claims[name], nil
	}
	return "", nil
}

func (hr *helmRelease) databaseSecret() (string, error) {
	secrets := hr.persistenceMatching(func(p hrPersistence) string {
		if p.Type != "secret" {
			return ""
		}
		return p.Name
	})
	if len(secrets) > 1 {
		return "", fmt.Errorf("%w (%s); a ScheduledBackup mounts one database secret, so migrate "+
			"cannot choose between them", ErrMultipleDBSecrets, strings.Join(sortedKeys(secrets), ", "))
	}
	for _, name := range sortedKeys(secrets) {
		return secrets[name], nil
	}
	return "", nil
}

func (hr *helmRelease) persistenceMatching(value func(hrPersistence) string) map[string]string {
	found := map[string]string{}
	for name, p := range hr.Spec.Values.Persistence {
		if v := value(p); v != "" {
			found[name] = v
		}
	}
	return found
}

func (hr *helmRelease) cacheStorageClass() string {
	return hr.Spec.Values.Persistence["cache"].StorageClass
}

func splitArgs(line string) []string {
	var args []string
	var current strings.Builder
	var quote rune
	inWord := false

	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t':
			if inWord {
				args = append(args, current.String())
				current.Reset()
				inWord = false
			}
		default:
			current.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		args = append(args, current.String())
	}
	return args
}

func flagValue(args []string, name string) (string, bool) {
	prefix := "--" + name + "="
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, prefix); ok {
			return v, true
		}
	}
	return "", false
}

func parseInt32(s string) (*int32, error) {
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return nil, err
	}
	v := int32(n)
	return &v, nil
}

func joinContinuations(script string) []string {
	var lines []string
	var current strings.Builder

	for line := range strings.SplitSeq(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if after, ok := strings.CutSuffix(trimmed, "\\"); ok {
			current.WriteString(strings.TrimSpace(after))
			current.WriteString(" ")
			continue
		}
		current.WriteString(trimmed)
		if joined := strings.TrimSpace(current.String()); joined != "" {
			lines = append(lines, joined)
		}
		current.Reset()
	}
	if joined := strings.TrimSpace(current.String()); joined != "" {
		lines = append(lines, joined)
	}
	return lines
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

func (hr *helmRelease) scheduleOf() string {
	return hr.Spec.Values.Controllers[resticController].CronJob.Schedule
}
