// Package b2 is a client for the management calls of the Backblaze B2 native API.
package b2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultAuthURL is where b2_authorize_account is called.
const DefaultAuthURL = "https://api.backblazeb2.com"

const (
	apiPath = "/b2api/v3/"

	opAuthorize = "b2_authorize_account"

	defaultTimeout     = 30 * time.Second
	defaultMaxAttempts = 5
	maxBackoff         = 30 * time.Second

	// MaxFileCount is the largest page b2_list_file_names and b2_list_file_versions return.
	MaxFileCount = 10000

	maxKeyCount = 1000

	fieldAccountID = "accountId"
	fieldBucketID  = "bucketId"
)

// API is the subset of the B2 native API the operator uses.
type API interface {
	Authorize(ctx context.Context) (*Authorization, error)

	FindBucket(ctx context.Context, name string) (*Bucket, error)
	CreateBucket(ctx context.Context, name string, rules []LifecycleRule) (*Bucket, error)
	UpdateLifecycleRules(ctx context.Context, bucket *Bucket, rules []LifecycleRule) (*Bucket, error)
	DeleteBucket(ctx context.Context, bucketID string) error

	CreateKey(ctx context.Context, req CreateKeyRequest) (*Key, error)
	ListKeys(ctx context.Context) ([]Key, error)
	DeleteKey(ctx context.Context, keyID string) error

	ListFileNames(ctx context.Context, req ListFilesRequest) (*FilePage, error)
	ListFileVersions(ctx context.Context, req ListFilesRequest) (*FilePage, error)
	DeleteFileVersion(ctx context.Context, fileName, fileID string) error
	ListUnfinishedLargeFiles(ctx context.Context, req ListFilesRequest) (*FilePage, error)
	CancelLargeFile(ctx context.Context, fileID string) error
}

// Client calls the B2 native API with one application key. It caches the
// authorization token and re-authorizes when B2 reports it expired.
type Client struct {
	KeyID string
	Key   string

	AuthURL     string
	HTTP        *http.Client
	MaxAttempts int

	Sleep func(ctx context.Context, d time.Duration) error

	mu   sync.Mutex
	auth *Authorization
}

var _ API = (*Client)(nil)

// NewClient returns a Client for the given application key.
func NewClient(keyID, key string) *Client {
	return &Client{
		KeyID:       keyID,
		Key:         key,
		AuthURL:     DefaultAuthURL,
		HTTP:        &http.Client{Timeout: defaultTimeout},
		MaxAttempts: defaultMaxAttempts,
	}
}

// Authorize returns the cached authorization, calling b2_authorize_account if there is none.
func (c *Client) Authorize(ctx context.Context) (*Authorization, error) {
	c.mu.Lock()
	auth := c.auth
	c.mu.Unlock()
	if auth != nil {
		return auth, nil
	}
	return c.reauthorize(ctx, nil)
}

func (c *Client) reauthorize(ctx context.Context, stale *Authorization) (*Authorization, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.auth != nil && c.auth != stale {
		return c.auth, nil
	}
	c.auth = nil

	var resp authorizeResponse
	err := c.withRetry(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			strings.TrimRight(c.authURL(), "/")+apiPath+opAuthorize, nil)
		if err != nil {
			return err
		}
		req.SetBasicAuth(c.KeyID, c.Key)
		return c.do(req, opAuthorize, &resp)
	})
	if err != nil {
		return nil, err
	}

	auth := resp.authorization()
	if auth.APIURL == "" || auth.AuthorizationToken == "" {
		return nil, fmt.Errorf("b2: %s returned no apiUrl or token", opAuthorize)
	}
	c.auth = auth
	return auth, nil
}

func (c *Client) authURL() string {
	if c.AuthURL != "" {
		return c.AuthURL
	}
	return DefaultAuthURL
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: defaultTimeout}
}

func (c *Client) maxAttempts() int {
	if c.MaxAttempts > 0 {
		return c.MaxAttempts
	}
	return defaultMaxAttempts
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type retryable struct {
	err   error
	after time.Duration
}

func (r *retryable) Error() string { return r.err.Error() }
func (r *retryable) Unwrap() error { return r.err }

func (c *Client) withRetry(ctx context.Context, attempt func() error) error {
	var err error
	for i := range c.maxAttempts() {
		err = attempt()
		var retry *retryable
		if !errors.As(err, &retry) {
			return err
		}
		if i == c.maxAttempts()-1 {
			break
		}
		wait := retry.after
		if wait <= 0 {
			wait = min(time.Second<<i, maxBackoff)
		}
		if sleepErr := c.sleep(ctx, wait); sleepErr != nil {
			return errors.Join(retry.err, sleepErr)
		}
	}
	if retry, ok := errors.AsType[*retryable](err); ok {
		return retry.err
	}
	return err
}

func (c *Client) do(req *http.Request, op string, out any) error {
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("b2: %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK {
		if out == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			return nil
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("b2: %s: decoding response: %w", op, err)
		}
		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	apiErr := &APIError{Op: op}
	if json.Unmarshal(body, apiErr) != nil || apiErr.Code == "" {
		apiErr.Message = strings.TrimSpace(string(body))
	}
	apiErr.Op = op
	apiErr.Status = resp.StatusCode

	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusGatewayTimeout:
		return &retryable{err: apiErr, after: retryAfter(resp.Header.Get("Retry-After"))}
	}
	return apiErr
}

func retryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs >= 0 {
		return min(time.Duration(secs)*time.Second, 5*time.Minute)
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(time.Until(t), 0), 5*time.Minute)
	}
	return 0
}

func (c *Client) call(ctx context.Context, op string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("b2: %s: encoding request: %w", op, err)
	}

	auth, err := c.Authorize(ctx)
	if err != nil {
		return err
	}

	reauthorized := false
	for {
		err = c.withRetry(ctx, func() error {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost,
				strings.TrimRight(auth.APIURL, "/")+apiPath+op, bytes.NewReader(payload))
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", auth.AuthorizationToken)
			req.Header.Set("Content-Type", "application/json")
			return c.do(req, op, out)
		})

		var apiErr *APIError
		if reauthorized || !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized ||
			(apiErr.Code != CodeExpiredAuthToken && apiErr.Code != CodeBadAuthToken) {
			return err
		}
		if auth, err = c.reauthorize(ctx, auth); err != nil {
			return err
		}
		reauthorized = true
	}
}

type authorizeResponse struct {
	AccountID          string `json:"accountId"`
	AuthorizationToken string `json:"authorizationToken"`
	APIInfo            struct {
		StorageAPI struct {
			APIURL       string   `json:"apiUrl"`
			S3APIURL     string   `json:"s3ApiUrl"`
			BucketID     *string  `json:"bucketId"`
			Capabilities []string `json:"capabilities"`
			NamePrefix   *string  `json:"namePrefix"`
			Allowed      *allowed `json:"allowed"`
		} `json:"storageApi"`
	} `json:"apiInfo"`
	Allowed *allowed `json:"allowed"`
}

type allowed struct {
	BucketID *string `json:"bucketId"`
	Buckets  []struct {
		ID string `json:"id"`
	} `json:"buckets"`
	Capabilities []string `json:"capabilities"`
	NamePrefix   *string  `json:"namePrefix"`
}

func (r *authorizeResponse) authorization() *Authorization {
	s := r.APIInfo.StorageAPI
	auth := &Authorization{
		AccountID:          r.AccountID,
		AuthorizationToken: r.AuthorizationToken,
		APIURL:             s.APIURL,
		S3APIURL:           s.S3APIURL,
		Capabilities:       s.Capabilities,
	}
	if s.BucketID != nil && *s.BucketID != "" {
		auth.BucketIDs = []string{*s.BucketID}
	}
	if s.NamePrefix != nil {
		auth.NamePrefix = *s.NamePrefix
	}
	for _, a := range []*allowed{s.Allowed, r.Allowed} {
		if a == nil {
			continue
		}
		if len(a.Capabilities) > 0 {
			auth.Capabilities = a.Capabilities
		}
		if a.BucketID != nil && *a.BucketID != "" && !slices.Contains(auth.BucketIDs, *a.BucketID) {
			auth.BucketIDs = append(auth.BucketIDs, *a.BucketID)
		}
		for _, b := range a.Buckets {
			if !slices.Contains(auth.BucketIDs, b.ID) {
				auth.BucketIDs = append(auth.BucketIDs, b.ID)
			}
		}
		if a.NamePrefix != nil {
			auth.NamePrefix = *a.NamePrefix
		}
	}
	return auth
}

func (c *Client) accountID(ctx context.Context) (string, error) {
	auth, err := c.Authorize(ctx)
	if err != nil {
		return "", err
	}
	return auth.AccountID, nil
}

// FindBucket returns the bucket with the given name in this account, or ErrNotFound.
func (c *Client) FindBucket(ctx context.Context, name string) (*Bucket, error) {
	account, err := c.accountID(ctx)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Buckets []Bucket `json:"buckets"`
	}
	if err := c.call(ctx, "b2_list_buckets", map[string]any{
		fieldAccountID: account,
		"bucketName":   name,
	}, &resp); err != nil {
		return nil, err
	}
	for i := range resp.Buckets {
		if resp.Buckets[i].BucketName == name {
			return &resp.Buckets[i], nil
		}
	}
	return nil, fmt.Errorf("%w: bucket %q", ErrNotFound, name)
}

// CreateBucket creates a private bucket.
func (c *Client) CreateBucket(ctx context.Context, name string, rules []LifecycleRule) (*Bucket, error) {
	account, err := c.accountID(ctx)
	if err != nil {
		return nil, err
	}
	if rules == nil {
		rules = []LifecycleRule{}
	}
	var bucket Bucket
	if err := c.call(ctx, "b2_create_bucket", map[string]any{
		fieldAccountID:   account,
		"bucketName":     name,
		"bucketType":     BucketTypePrivate,
		"lifecycleRules": rules,
	}, &bucket); err != nil {
		return nil, err
	}
	return &bucket, nil
}

// UpdateLifecycleRules replaces the bucket's lifecycle rules, failing with
// ErrConflict if the bucket changed since it was read.
func (c *Client) UpdateLifecycleRules(
	ctx context.Context, bucket *Bucket, rules []LifecycleRule,
) (*Bucket, error) {
	account, err := c.accountID(ctx)
	if err != nil {
		return nil, err
	}
	if rules == nil {
		rules = []LifecycleRule{}
	}
	var updated Bucket
	if err := c.call(ctx, "b2_update_bucket", map[string]any{
		fieldAccountID:   account,
		fieldBucketID:    bucket.BucketID,
		"lifecycleRules": rules,
		"ifRevisionIs":   bucket.Revision,
	}, &updated); err != nil {
		return nil, err
	}
	return &updated, nil
}

// DeleteBucket deletes an empty bucket.
func (c *Client) DeleteBucket(ctx context.Context, bucketID string) error {
	account, err := c.accountID(ctx)
	if err != nil {
		return err
	}
	return c.call(ctx, "b2_delete_bucket", map[string]any{
		fieldAccountID: account,
		fieldBucketID:  bucketID,
	}, nil)
}

// CreateKey creates an application key. The returned ApplicationKey is never shown again.
func (c *Client) CreateKey(ctx context.Context, req CreateKeyRequest) (*Key, error) {
	account, err := c.accountID(ctx)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		fieldAccountID: account,
		"keyName":      req.KeyName,
		"capabilities": req.Capabilities,
	}
	if req.BucketID != "" {
		body[fieldBucketID] = req.BucketID
	}
	if req.NamePrefix != "" {
		body["namePrefix"] = req.NamePrefix
	}
	var key Key
	if err := c.call(ctx, "b2_create_key", body, &key); err != nil {
		return nil, err
	}
	if key.ApplicationKeyID == "" || key.ApplicationKey == "" {
		return nil, fmt.Errorf("b2: b2_create_key returned no key")
	}
	return &key, nil
}

// ListKeys returns every application key in the account.
func (c *Client) ListKeys(ctx context.Context) ([]Key, error) {
	account, err := c.accountID(ctx)
	if err != nil {
		return nil, err
	}
	var keys []Key
	start := ""
	for {
		body := map[string]any{fieldAccountID: account, "maxKeyCount": maxKeyCount}
		if start != "" {
			body["startApplicationKeyId"] = start
		}
		var resp struct {
			Keys                 []Key   `json:"keys"`
			NextApplicationKeyID *string `json:"nextApplicationKeyId"`
		}
		if err := c.call(ctx, "b2_list_keys", body, &resp); err != nil {
			return nil, err
		}
		keys = append(keys, resp.Keys...)
		if resp.NextApplicationKeyID == nil || *resp.NextApplicationKeyID == "" || *resp.NextApplicationKeyID == start {
			return keys, nil
		}
		start = *resp.NextApplicationKeyID
	}
}

// DeleteKey deletes an application key.
func (c *Client) DeleteKey(ctx context.Context, keyID string) error {
	return c.call(ctx, "b2_delete_key", map[string]any{"applicationKeyId": keyID}, nil)
}

func listBody(req ListFilesRequest, startIDField string) map[string]any {
	body := map[string]any{fieldBucketID: req.BucketID}
	if req.Prefix != "" {
		body["prefix"] = req.Prefix
	}
	if req.StartFileName != "" {
		body["startFileName"] = req.StartFileName
		if req.StartFileID != "" && startIDField != "" {
			body[startIDField] = req.StartFileID
		}
	}
	count := req.MaxFileCount
	if count <= 0 || count > MaxFileCount {
		count = MaxFileCount
	}
	body["maxFileCount"] = count
	return body
}

type listFilesResponse struct {
	Files        []File  `json:"files"`
	NextFileName *string `json:"nextFileName"`
	NextFileID   *string `json:"nextFileId"`
}

func (r *listFilesResponse) page() *FilePage {
	p := &FilePage{Files: r.Files}
	if r.NextFileName != nil {
		p.NextFileName = *r.NextFileName
	}
	if r.NextFileID != nil {
		p.NextFileID = *r.NextFileID
	}
	return p
}

// ListFileNames returns one page of the latest visible version of each file.
func (c *Client) ListFileNames(ctx context.Context, req ListFilesRequest) (*FilePage, error) {
	var resp listFilesResponse
	if err := c.call(ctx, "b2_list_file_names", listBody(req, ""), &resp); err != nil {
		return nil, err
	}
	return resp.page(), nil
}

// ListFileVersions returns one page of every version of every file, hidden ones included.
func (c *Client) ListFileVersions(ctx context.Context, req ListFilesRequest) (*FilePage, error) {
	var resp listFilesResponse
	if err := c.call(ctx, "b2_list_file_versions", listBody(req, "startFileId"), &resp); err != nil {
		return nil, err
	}
	return resp.page(), nil
}

// DeleteFileVersion permanently deletes one file version.
func (c *Client) DeleteFileVersion(ctx context.Context, fileName, fileID string) error {
	return c.call(ctx, "b2_delete_file_version", map[string]any{
		"fileName": fileName,
		"fileId":   fileID,
	}, nil)
}

// ListUnfinishedLargeFiles returns one page of large file uploads that were never finished.
func (c *Client) ListUnfinishedLargeFiles(ctx context.Context, req ListFilesRequest) (*FilePage, error) {
	body := map[string]any{fieldBucketID: req.BucketID}
	if req.Prefix != "" {
		body["namePrefix"] = req.Prefix
	}
	if req.StartFileID != "" {
		body["startFileId"] = req.StartFileID
	}
	count := req.MaxFileCount
	if count <= 0 || count > 100 {
		count = 100
	}
	body["maxFileCount"] = count
	var resp listFilesResponse
	if err := c.call(ctx, "b2_list_unfinished_large_files", body, &resp); err != nil {
		return nil, err
	}
	return resp.page(), nil
}

// CancelLargeFile cancels an unfinished large file upload and deletes its parts.
func (c *Client) CancelLargeFile(ctx context.Context, fileID string) error {
	return c.call(ctx, "b2_cancel_large_file", map[string]any{"fileId": fileID}, nil)
}
