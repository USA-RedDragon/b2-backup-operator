// Package borgbase is a client for the BorgBase GraphQL API.
package borgbase

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// DefaultEndpoint is the BorgBase GraphQL endpoint.
const DefaultEndpoint = "https://api.borgbase.com/graphql"

// API is the subset of the BorgBase API the operator uses.
type API interface {
	Get(ctx context.Context, id string) (*Repo, error)

	FindByName(ctx context.Context, name string) (*Repo, error)

	Add(ctx context.Context, opts AddOptions) (*Repo, error)

	Edit(ctx context.Context, id string, opts EditOptions) (*Repo, error)

	Delete(ctx context.Context, id string) error
}

// Client is a BorgBase GraphQL API client.
type Client struct {
	Endpoint string
	Token    string
	HTTP     *http.Client
}

// NewClient returns a Client authenticating with the given API token.
func NewClient(token string) *Client {
	return &Client{
		Endpoint: DefaultEndpoint,
		Token:    token,
		HTTP:     &http.Client{Timeout: 30 * time.Second},
	}
}

var _ API = (*Client)(nil)

const repoFields = `
	id
	name
	region
	format
	repoPath
	quota
	quotaEnabled
	alertDays
	appendOnly
	currentUsage
	htpasswd
	server { hostname region }
`

var repoNotFound = regexp.MustCompile(`(?i)repo(sitory)?\b[^.;]*(not found|does not exist)` +
	`|\bno\s+repo(sitory)?\b[^.;]*\bfound\b` +
	`|(not found|does not exist)[^.;]*\brepo(sitory)?\b`)

type graphQLError struct {
	Message string `json:"message"`
}

type graphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []graphQLError  `json:"errors"`
}

func (c *Client) execute(ctx context.Context, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return fmt.Errorf("borgbase: encoding request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("borgbase: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("borgbase: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		if len(snippet) > 0 {
			return fmt.Errorf("borgbase: unexpected status %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
		}
		return fmt.Errorf("borgbase: unexpected status %s", resp.Status)
	}

	var envelope graphQLResponse
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("borgbase: decoding response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		msgs := make([]string, 0, len(envelope.Errors))
		for _, e := range envelope.Errors {
			msgs = append(msgs, e.Message)
		}
		joined := strings.Join(msgs, "; ")

		if repoNotFound.MatchString(joined) {
			return fmt.Errorf("%w: %s", ErrNotFound, joined)
		}
		return fmt.Errorf("borgbase: %s", joined)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("borgbase: decoding data: %w", err)
	}
	return nil
}

func (c *Client) endpoint() string {
	if c.Endpoint != "" {
		return c.Endpoint
	}
	return DefaultEndpoint
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// Get returns the repository with the given ID.
func (c *Client) Get(ctx context.Context, id string) (*Repo, error) {
	query := `query repo($id: String!) { repo(repoId: $id) {` + repoFields + `} }`

	var data struct {
		Repo *Repo `json:"repo"`
	}
	if err := c.execute(ctx, query, map[string]any{"id": id}, &data); err != nil {
		return nil, err
	}
	if data.Repo == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return data.Repo, nil
}

// FindByName returns the repository with the given name, or ErrNotFound.
func (c *Client) FindByName(ctx context.Context, name string) (*Repo, error) {
	query := `query repoList($name: String) { repoList(name: $name) {` + repoFields + `} }`

	var data struct {
		RepoList []Repo `json:"repoList"`
	}
	if err := c.execute(ctx, query, map[string]any{"name": name}, &data); err != nil {
		return nil, err
	}

	var found *Repo
	for i := range data.RepoList {
		if data.RepoList[i].Name != name {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("borgbase: multiple repositories named %q; adopt one explicitly by ID", name)
		}
		found = &data.RepoList[i]
	}
	if found == nil {
		return nil, fmt.Errorf("%w: name %s", ErrNotFound, name)
	}
	return found, nil
}

// Add creates a repository.
func (c *Client) Add(ctx context.Context, opts AddOptions) (*Repo, error) {
	query := `mutation repoAdd(
		$name: String!
		$region: String!
		$format: String
		$quota: Int
		$quotaEnabled: Boolean
		$alertDays: Int
		$appendOnly: Boolean
	) {
		repoAdd(
			name: $name
			region: $region
			format: $format
			quota: $quota
			quotaEnabled: $quotaEnabled
			alertDays: $alertDays
			appendOnly: $appendOnly
		) { repoAdded {` + repoFields + `} }
	}`

	vars := map[string]any{
		"name":         opts.Name,
		"region":       opts.Region,
		"format":       FormatRestic,
		"quotaEnabled": opts.Quota != nil,
		"appendOnly":   opts.AppendOnly,
	}
	if opts.Quota != nil {
		vars["quota"] = *opts.Quota
	}
	if opts.AlertDays != nil {
		vars["alertDays"] = *opts.AlertDays
	}

	var data struct {
		RepoAdd struct {
			RepoAdded *Repo `json:"repoAdded"`
		} `json:"repoAdd"`
	}
	if err := c.execute(ctx, query, vars, &data); err != nil {
		return nil, err
	}
	if data.RepoAdd.RepoAdded == nil {
		return nil, fmt.Errorf("borgbase: repoAdd returned no repository")
	}
	return data.RepoAdd.RepoAdded, nil
}

// Edit changes settings on an existing repository.
func (c *Client) Edit(ctx context.Context, id string, opts EditOptions) (*Repo, error) {
	query := `mutation repoEdit(
		$id: String!
		$quota: Int
		$quotaEnabled: Boolean
		$alertDays: Int
		$appendOnly: Boolean
	) {
		repoEdit(
			id: $id
			quota: $quota
			quotaEnabled: $quotaEnabled
			alertDays: $alertDays
			appendOnly: $appendOnly
		) { repoEdited {` + repoFields + `} }
	}`

	vars := map[string]any{"id": id}
	if opts.Quota != nil {
		vars["quota"] = *opts.Quota
	}
	if opts.QuotaEnabled != nil {
		vars["quotaEnabled"] = *opts.QuotaEnabled
	}
	if opts.AlertDays != nil {
		vars["alertDays"] = *opts.AlertDays
	}
	if opts.AppendOnly != nil {
		vars["appendOnly"] = *opts.AppendOnly
	}

	var data struct {
		RepoEdit struct {
			RepoEdited *Repo `json:"repoEdited"`
		} `json:"repoEdit"`
	}
	if err := c.execute(ctx, query, vars, &data); err != nil {
		return nil, err
	}
	if data.RepoEdit.RepoEdited == nil {
		return nil, fmt.Errorf("borgbase: repoEdit returned no repository")
	}
	return data.RepoEdit.RepoEdited, nil
}

// Delete removes a repository and its snapshots.
func (c *Client) Delete(ctx context.Context, id string) error {
	query := `mutation repoDelete($id: String!) { repoDelete(id: $id) { ok } }`

	var data struct {
		RepoDelete struct {
			OK *bool `json:"ok"`
		} `json:"repoDelete"`
	}
	if err := c.execute(ctx, query, map[string]any{"id": id}, &data); err != nil {
		return err
	}

	switch ok := data.RepoDelete.OK; {
	case ok == nil:
		return fmt.Errorf("borgbase: repoDelete returned no result for repository %s", id)
	case !*ok:
		return fmt.Errorf("borgbase: repoDelete reported ok=false for repository %s", id)
	}
	return nil
}
