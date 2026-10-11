package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	sopsAppPath = "restic/app"
	sopsRepoURL = "s3:" + testEndpoint + "/" + testBucket + "/" + sopsAppPath
)

// fakeSops puts a sops on PATH whose body is the given shell script, and
// returns the path of an encrypted Secret for it to be pointed at.
func fakeSops(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()

	bin := filepath.Join(dir, "sops")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	secret := filepath.Join(dir, "app-restic.yaml")
	if err := os.WriteFile(secret, []byte("sops: encrypted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return secret
}

// sopsEmitting is a fake sops body that prints the given plaintext. PATH holds
// nothing but the fake, so the script may use shell builtins only.
func sopsEmitting(plaintext string) string {
	return "printf '%s' '" + strings.ReplaceAll(plaintext, "'", `'\''`) + "'"
}

func TestParseS3Repository(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		want    s3Location
		wantErr bool
	}{
		{"prefix", sopsRepoURL, s3Location{
			Endpoint: testEndpoint, Bucket: testBucket, Path: sopsAppPath,
		}, false},
		{"trailing slash", sopsRepoURL + "/", s3Location{
			Endpoint: testEndpoint, Bucket: testBucket, Path: sopsAppPath,
		}, false},
		{"bucket root", "s3:" + testEndpoint + "/" + testBucket, s3Location{
			Endpoint: testEndpoint, Bucket: testBucket,
		}, false},
		{"native b2 backend", "b2:acme-backups:restic/app", s3Location{}, true},
		{"rest server", "rest:https://u:p@host/", s3Location{}, true},
		{"no scheme", "s3:s3.amazonaws.com/bucket", s3Location{}, true},
		{"no bucket", "s3:https://s3.us-east-005.backblazeb2.com/", s3Location{}, true},
		{"double slash", "s3:https://s3.us-east-005.backblazeb2.com/b//a", s3Location{}, true},
		{"credentials", "s3:https://k:s@s3.us-east-005.backblazeb2.com/b/a", s3Location{}, true},
		{"empty", "", s3Location{}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseS3Repository(tt.url)
			if tt.wantErr {
				if !errors.Is(err, ErrNoRepositoryURL) {
					t.Fatalf("parseS3Repository(%q) = %+v, %v; want ErrNoRepositoryURL", tt.url, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseS3Repository(%q) error = %v", tt.url, err)
			}
			if got != tt.want {
				t.Errorf("parseS3Repository(%q) = %+v, want %+v", tt.url, got, tt.want)
			}
		})
	}
}

func TestRepositoryURLFromSecretReadsBothSecretShapes(t *testing.T) {
	tests := []struct {
		name      string
		plaintext string
	}{
		{"stringData", `
apiVersion: v1
kind: Secret
metadata:
  name: restic-envs
stringData:
  RESTIC_REPOSITORY: ` + sopsRepoURL + `
  RESTIC_PASSWORD: hunter2
`},
		{"base64 data", `
apiVersion: v1
kind: Secret
metadata:
  name: restic-envs
data:
  RESTIC_REPOSITORY: czM6aHR0cHM6Ly9zMy51cy1lYXN0LTAwNS5iYWNrYmxhemViMi5jb20vYWNtZS1iYWNrdXBzL3Jlc3RpYy9hcHA=
  RESTIC_PASSWORD: aHVudGVyMg==
`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := fakeSops(t, sopsEmitting(tt.plaintext))

			got, err := repositoryURLFromSecret(path)
			if err != nil {
				t.Fatalf("repositoryURLFromSecret() error = %v", err)
			}
			if got != sopsRepoURL {
				t.Errorf("repositoryURLFromSecret() = %q, want %q", got, sopsRepoURL)
			}
		})
	}
}

func TestRepositoryURLFromSecretDecryptsRatherThanReadingCiphertext(t *testing.T) {
	path := fakeSops(t, "printf 'stringData:\\n  RESTIC_REPOSITORY: %s\\n' "+sopsRepoURL)

	if err := os.WriteFile(path, []byte(
		"stringData:\n  RESTIC_REPOSITORY: s3:https://s3.us-east-005.backblazeb2.com/wrong/one\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := repositoryURLFromSecret(path)
	if err != nil {
		t.Fatalf("repositoryURLFromSecret() error = %v", err)
	}
	if got != sopsRepoURL {
		t.Errorf("repositoryURLFromSecret() = %q, want %q from the decrypted content, "+
			"not whatever the file happens to hold", got, sopsRepoURL)
	}
}

func TestRepositoryURLFromSecretRefusesWhatItCannotRead(t *testing.T) {
	tests := []struct {
		name    string
		script  string
		want    string
		wantErr error
	}{
		{
			name:   "sops fails",
			script: "echo 'no key could decrypt this' >&2; exit 1",
			want:   "no key could decrypt this",
		},
		{
			name:   "the plaintext is not yaml",
			script: "printf 'stringData: [oops\\n'",
			want:   "parsing the decrypted",
		},
		{
			name:    "no RESTIC_REPOSITORY in the secret",
			script:  "printf 'stringData:\\n  RESTIC_PASSWORD: hunter2\\n'",
			want:    "has no RESTIC_REPOSITORY",
			wantErr: ErrNoRepositoryURL,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := repositoryURLFromSecret(fakeSops(t, tt.script))
			if err == nil {
				t.Fatalf("repositoryURLFromSecret() = %q, want an error", got)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want it to wrap %v", err, tt.wantErr)
			}
		})
	}
}

func TestRepositoryURLFromSecretPointsAtTheWayOut(t *testing.T) {
	t.Run("no such file", func(t *testing.T) {
		path := fakeSops(t, "exit 0")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		_, err := repositoryURLFromSecret(path)
		if err == nil || !strings.Contains(err.Error(), "--repository") {
			t.Errorf("error = %v, want it to suggest --repository", err)
		}
	})

	t.Run("sops is not installed", func(t *testing.T) {
		path := fakeSops(t, "exit 0")
		if err := os.Remove(filepath.Join(filepath.Dir(path), "sops")); err != nil {
			t.Fatal(err)
		}
		_, err := repositoryURLFromSecret(path)
		if !errors.Is(err, ErrSopsUnavailable) {
			t.Errorf("error = %v, want ErrSopsUnavailable", err)
		}
		if err == nil || !strings.Contains(err.Error(), "--repository") {
			t.Errorf("error = %v, want it to suggest --repository", err)
		}
	})
}
