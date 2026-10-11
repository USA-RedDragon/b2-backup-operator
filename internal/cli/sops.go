package cli

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

var (
	// ErrSopsUnavailable means sops is not on PATH.
	ErrSopsUnavailable = errors.New("sops is required to read the encrypted repository URL")

	// ErrNoRepositoryURL means no usable s3: repository URL was given or found.
	ErrNoRepositoryURL = errors.New("could not determine the restic repository URL")
)

var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{4,61}[a-z0-9]$`)

type decryptedSecret struct {
	Metadata struct {
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	StringData map[string]string `json:"stringData"`
	Data       map[string][]byte `json:"data"`
}

// repositoryURLFromSecret reads RESTIC_REPOSITORY out of a SOPS-encrypted
// Secret, such as the restic-envs Secret in the secrets repository.
//
// Decryption is delegated to the sops binary rather than linked in: the library
// pulls every KMS backend it supports (AWS, Azure, GCP, Vault, age, PGP), which
// measured at 61MB on top of a 93MB binary, for one field read once per app
// during a one-time migration. It is also the only step needing the operator's
// own decryption keys, which the sops binary already manages.
func repositoryURLFromSecret(file string) (string, string, error) {
	if _, err := os.Stat(file); err != nil {
		return "", "", fmt.Errorf("reading the encrypted Secret: %w; pass --repository to supply the URL directly", err)
	}
	if _, err := exec.LookPath("sops"); err != nil {
		return "", "", fmt.Errorf("%w: install sops, or pass --repository", ErrSopsUnavailable)
	}

	plaintext, err := exec.Command("sops", "--decrypt", file).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return "", "", fmt.Errorf("decrypting %s: %w: %s",
				file, err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", "", fmt.Errorf("decrypting %s: %w", file, err)
	}

	var secret decryptedSecret
	if err := yaml.Unmarshal(plaintext, &secret); err != nil {
		return "", "", fmt.Errorf("parsing the decrypted %s: %w", file, err)
	}

	value := secret.StringData["RESTIC_REPOSITORY"]
	if value == "" {
		value = string(secret.Data["RESTIC_REPOSITORY"])
	}
	if value == "" {
		return "", "", fmt.Errorf("%w: %s has no RESTIC_REPOSITORY", ErrNoRepositoryURL, file)
	}
	return value, secret.Metadata.Namespace, nil
}

type s3Location struct {
	Endpoint string
	Bucket   string
	Path     string
}

func parseS3Repository(raw string) (s3Location, error) {
	rest, ok := strings.CutPrefix(raw, "s3:")
	if !ok {
		return s3Location{}, fmt.Errorf("%w: %s is not an s3: repository URL", ErrNoRepositoryURL, redactURL(raw))
	}
	if !strings.Contains(rest, "://") {
		rest = "https://" + rest
	}
	u, err := url.Parse(rest)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return s3Location{}, fmt.Errorf("%w: %s does not name an https endpoint", ErrNoRepositoryURL, redactURL(raw))
	}
	if u.User != nil || u.RawQuery != "" {
		return s3Location{}, fmt.Errorf("%w: %s carries credentials or options", ErrNoRepositoryURL, redactURL(raw))
	}

	bucket, prefix, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	if bucket == "" {
		return s3Location{}, fmt.Errorf("%w: no bucket in %s", ErrNoRepositoryURL, redactURL(raw))
	}
	if !bucketName.MatchString(bucket) || strings.HasPrefix(bucket, "b2-") {
		return s3Location{}, fmt.Errorf("%w: %q is not a bucket name a Repository accepts: 6 to 63 "+
			"lowercase letters, digits and dashes, not starting with b2-", ErrNoRepositoryURL, bucket)
	}
	if strings.HasPrefix(prefix, "/") || strings.Contains(prefix, "//") {
		return s3Location{}, fmt.Errorf("%w: empty path segment in %s", ErrNoRepositoryURL, redactURL(raw))
	}
	prefix = strings.Trim(path.Clean("/"+prefix), "/")

	return s3Location{Endpoint: u.Scheme + "://" + u.Host, Bucket: bucket, Path: prefix}, nil
}

var userinfo = regexp.MustCompile(`(://|^s3:)[^/@]*@`)

// redactURL hides anything that could be credentials in a repository URL: the
// userinfo before an @, and the query.
func redactURL(raw string) string {
	out := userinfo.ReplaceAllString(raw, "${1}***@")
	if i := strings.IndexByte(out, '?'); i >= 0 {
		out = out[:i] + "?***"
	}
	return fmt.Sprintf("%q", out)
}
