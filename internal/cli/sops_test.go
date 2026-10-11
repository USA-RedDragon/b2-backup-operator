package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	sopsRepoID  = "ab12cd34"
	sopsRepoURL = "rest:https://" + sopsRepoID + ":s3cret@" + sopsRepoID + ".repo.borgbase.com/./"
)

// fakeSops puts a sops on PATH whose body is the given shell script, and
// returns the path of a secret.yaml for it to be pointed at.
func fakeSops(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()

	bin := filepath.Join(dir, "sops")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	secret := filepath.Join(dir, "secret.yaml")
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

func TestRepositoryIDFromURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		want    string
		wantErr bool
	}{
		{"rest url", "rest:https://ab12cd34:s3cret@ab12cd34.repo.borgbase.com/./", "ab12cd34", false},
		{"no password", "rest:https://ab12cd34@ab12cd34.repo.borgbase.com/./", "", true},
		{"not a rest url", "s3:s3.amazonaws.com/bucket", "", true},
		{"plain https", "https://ab12cd34:s3cret@ab12cd34.repo.borgbase.com/./", "", true},
		{"empty id", "rest:https://:s3cret@ab12cd34.repo.borgbase.com/./", "", true},
		{"empty", "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := repositoryIDFromURL(tt.url)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("repositoryIDFromURL(%q) = %q, want an error", tt.url, id)
				}
				if !errors.Is(err, ErrNoRepositoryID) {
					t.Errorf("error = %v, want it to wrap ErrNoRepositoryID", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("repositoryIDFromURL(%q) error = %v", tt.url, err)
			}
			if id != tt.want {
				t.Errorf("repositoryIDFromURL(%q) = %q, want %q", tt.url, id, tt.want)
			}
		})
	}
}

func TestRepositoryIDFromSecretReadsBothSecretShapes(t *testing.T) {
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
  RESTIC_REPOSITORY: cmVzdDpodHRwczovL2FiMTJjZDM0OnMzY3JldEBhYjEyY2QzNC5yZXBvLmJvcmdiYXNlLmNvbS8uLw==
  RESTIC_PASSWORD: aHVudGVyMg==
`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := fakeSops(t, sopsEmitting(tt.plaintext))

			id, err := repositoryIDFromSecret(path)
			if err != nil {
				t.Fatalf("repositoryIDFromSecret() error = %v", err)
			}
			if id != sopsRepoID {
				t.Errorf("repositoryIDFromSecret() = %q, want %q", id, sopsRepoID)
			}
		})
	}
}

func TestRepositoryIDFromSecretDecryptsRatherThanReadingCiphertext(t *testing.T) {
	path := fakeSops(t, "printf 'stringData:\\n  RESTIC_REPOSITORY: %s\\n' "+sopsRepoURL)

	if err := os.WriteFile(path, []byte(
		"stringData:\n  RESTIC_REPOSITORY: rest:https://wrongone:pw@wrongone.repo.borgbase.com/./\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}

	id, err := repositoryIDFromSecret(path)
	if err != nil {
		t.Fatalf("repositoryIDFromSecret() error = %v", err)
	}
	if id != sopsRepoID {
		t.Errorf("repositoryIDFromSecret() = %q, want %q from the decrypted content, "+
			"not whatever the file happens to hold", id, sopsRepoID)
	}
}

func TestRepositoryIDFromSecretRefusesWhatItCannotRead(t *testing.T) {
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
			wantErr: ErrNoRepositoryID,
		},
		{
			name:    "RESTIC_REPOSITORY is not a rest url",
			script:  "printf 'stringData:\\n  RESTIC_REPOSITORY: s3:s3.amazonaws.com/bucket\\n'",
			want:    "is not a rest: repository URL",
			wantErr: ErrNoRepositoryID,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := repositoryIDFromSecret(fakeSops(t, tt.script))
			if err == nil {
				t.Fatalf("repositoryIDFromSecret() = %q, want an error", id)
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

func TestRepositoryIDFromSecretPointsAtTheWayOut(t *testing.T) {
	t.Run("no secret alongside the HelmRelease", func(t *testing.T) {
		path := fakeSops(t, "exit 0")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		_, err := repositoryIDFromSecret(path)
		if err == nil || !strings.Contains(err.Error(), "--repository-id") {
			t.Errorf("error = %v, want it to suggest --repository-id", err)
		}
	})

	t.Run("sops is not installed", func(t *testing.T) {
		path := fakeSops(t, "exit 0")
		if err := os.Remove(filepath.Join(filepath.Dir(path), "sops")); err != nil {
			t.Fatal(err)
		}
		_, err := repositoryIDFromSecret(path)
		if !errors.Is(err, ErrSopsUnavailable) {
			t.Errorf("error = %v, want ErrSopsUnavailable", err)
		}
		if err == nil || !strings.Contains(err.Error(), "--repository-id") {
			t.Errorf("error = %v, want it to suggest --repository-id", err)
		}
	})
}
