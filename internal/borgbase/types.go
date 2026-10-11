package borgbase

import (
	"errors"
	"net/url"
)

// FormatRestic is the repository format the operator supports.
const FormatRestic = "restic"

var (
	// ErrNotFound means no such repository exists.
	ErrNotFound = errors.New("borgbase: repository not found")

	// ErrNotRestic means the repository is a borg repository, not a restic one.
	ErrNotRestic = errors.New("borgbase: repository is not restic format")

	// ErrNoCredentials means BorgBase returned no REST password for the repository.
	ErrNoCredentials = errors.New("borgbase: repository has no REST password")
)

// Repo is a BorgBase repository.
type Repo struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Region       string  `json:"region"`
	Format       string  `json:"format"`
	RepoPath     string  `json:"repoPath"`
	Quota        int64   `json:"quota"`
	QuotaEnabled bool    `json:"quotaEnabled"`
	AlertDays    int64   `json:"alertDays"`
	AppendOnly   bool    `json:"appendOnly"`
	CurrentUsage float64 `json:"currentUsage"`

	Htpasswd string `json:"htpasswd"`

	Server Server `json:"server"`
}

// Server is the host a Repo lives on.
type Server struct {
	Hostname string `json:"hostname"`
	Region   string `json:"region"`
}

// IsRestic reports whether the repository is in restic format.
func (r *Repo) IsRestic() bool { return r.Format == FormatRestic }

// Password returns the repository's REST password.
func (r *Repo) Password() string { return r.Htpasswd }

// Host returns the hostname serving this repository over REST.
//
// BorgBase serves each restic repository from a subdomain named after its ID.
// This ignores Server.Hostname on purpose: that is the physical box the
// repository is stored on (box-us00.borgbase.com and the like) and does not
// answer for the REST endpoint.
func (r *Repo) Host() string {
	return r.ID + ".repo.borgbase.com"
}

// ResticURL returns the rest: URL restic uses to reach the repository.
func (r *Repo) ResticURL() (string, error) {
	if !r.IsRestic() {
		return "", ErrNotRestic
	}
	pass := r.Password()
	if pass == "" {
		return "", ErrNoCredentials
	}
	return "rest:https://" + url.UserPassword(r.ID, pass).String() + "@" + r.Host(), nil
}

// AddOptions are the settings for creating a repository.
type AddOptions struct {
	Name       string
	Region     string
	Quota      *int64
	AlertDays  *int64
	AppendOnly bool
}

// EditOptions are the settings to change on an existing repository. Nil fields are left alone.
type EditOptions struct {
	Quota        *int64
	QuotaEnabled *bool
	AlertDays    *int64
	AppendOnly   *bool
}

// IsZero reports whether there is nothing to change.
func (o EditOptions) IsZero() bool {
	return o.Quota == nil && o.QuotaEnabled == nil && o.AlertDays == nil && o.AppendOnly == nil
}
