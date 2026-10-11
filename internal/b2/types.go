package b2

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Bucket types accepted by b2_create_bucket.
const (
	BucketTypePrivate = "allPrivate"
)

// Error codes returned by the B2 native API.
const (
	CodeBadAuthToken        = "bad_auth_token"
	CodeExpiredAuthToken    = "expired_auth_token"
	CodeUnauthorized        = "unauthorized"
	CodeNotFound            = "not_found"
	CodeFileNotPresent      = "file_not_present"
	CodeBadBucketID         = "bad_bucket_id"
	CodeDuplicateBucketName = "duplicate_bucket_name"
	CodeTooManyBuckets      = "too_many_buckets"
	CodeConflict            = "conflict"
	CodeCannotDeleteBucket  = "cannot_delete_non_empty_bucket"
	CodeBadRequest          = "bad_request"
	CodeTooManyRequests     = "too_many_requests"
	CodeServiceUnavailable  = "service_unavailable"
)

var (
	// ErrNotFound means the bucket, key or file does not exist.
	ErrNotFound = errors.New("b2: not found")

	// ErrDuplicateBucketName means the bucket name is taken, possibly by another account.
	ErrDuplicateBucketName = errors.New("b2: bucket name is already in use")

	// ErrUnauthorized means the credentials lack a capability the call needs.
	ErrUnauthorized = errors.New("b2: not authorized")

	// ErrBadCredentials means the application key ID or key was rejected.
	ErrBadCredentials = errors.New("b2: application key rejected")

	// ErrConflict means a conditional update lost a race with another writer.
	ErrConflict = errors.New("b2: conflicting update")

	// ErrBucketNotEmpty means a bucket still holds files and cannot be deleted.
	ErrBucketNotEmpty = errors.New("b2: bucket is not empty")
)

// APIError is an error response from the B2 API.
type APIError struct {
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Op      string `json:"-"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("b2: %s: %d %s: %s", e.Op, e.Status, e.Code, e.Message)
}

// Is maps B2 error codes onto the package's sentinel errors.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.Code == CodeNotFound || e.Code == CodeFileNotPresent || e.Code == CodeBadBucketID ||
			(e.Code == CodeBadRequest && isMissingResourceMessage(e.Message))
	case ErrDuplicateBucketName:
		return e.Code == CodeDuplicateBucketName
	case ErrUnauthorized:
		return e.Code == CodeUnauthorized
	case ErrBadCredentials:
		return e.Code == CodeBadAuthToken || (e.Status == 401 && e.Op == opAuthorize)
	case ErrConflict:
		return e.Code == CodeConflict
	case ErrBucketNotEmpty:
		return e.Code == CodeCannotDeleteBucket
	}
	return false
}

func isMissingResourceMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "not found") || strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "no such")
}

// Authorization is the result of b2_authorize_account.
type Authorization struct {
	AccountID          string
	AuthorizationToken string
	APIURL             string
	S3APIURL           string
	Capabilities       []string
	BucketIDs          []string
	NamePrefix         string
}

// S3Region returns the region embedded in the S3 endpoint, such as us-east-005.
func (a *Authorization) S3Region() (string, error) {
	return RegionFromS3URL(a.S3APIURL)
}

// RegionFromS3URL extracts the region from an endpoint like https://s3.us-east-005.backblazeb2.com.
func RegionFromS3URL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("b2: parsing S3 endpoint %q: %w", raw, err)
	}
	host := u.Hostname()
	rest, ok := strings.CutPrefix(host, "s3.")
	if !ok {
		return "", fmt.Errorf("b2: S3 endpoint %q does not start with s3", raw)
	}
	region, _, ok := strings.Cut(rest, ".")
	if !ok || region == "" {
		return "", fmt.Errorf("b2: no region in S3 endpoint %q", raw)
	}
	return region, nil
}

// LifecycleRule is a bucket lifecycle rule.
type LifecycleRule struct {
	FileNamePrefix            string `json:"fileNamePrefix"`
	DaysFromUploadingToHiding *int   `json:"daysFromUploadingToHiding"`
	DaysFromHidingToDeleting  *int   `json:"daysFromHidingToDeleting"`
}

// Bucket is a B2 bucket.
type Bucket struct {
	AccountID      string          `json:"accountId"`
	BucketID       string          `json:"bucketId"`
	BucketName     string          `json:"bucketName"`
	BucketType     string          `json:"bucketType"`
	LifecycleRules []LifecycleRule `json:"lifecycleRules"`
	Revision       int             `json:"revision"`
}

// Key is a B2 application key. ApplicationKey is only set on creation.
type Key struct {
	AccountID        string   `json:"accountId"`
	ApplicationKeyID string   `json:"applicationKeyId"`
	ApplicationKey   string   `json:"applicationKey,omitempty"`
	KeyName          string   `json:"keyName"`
	Capabilities     []string `json:"capabilities"`
	BucketID         string   `json:"bucketId,omitempty"`
	BucketIDs        []string `json:"bucketIds,omitempty"`
	NamePrefix       string   `json:"namePrefix,omitempty"`
}

// CreateKeyRequest describes a new application key.
type CreateKeyRequest struct {
	KeyName      string
	Capabilities []string
	BucketID     string
	NamePrefix   string
}

// File is one file version.
type File struct {
	FileID        string `json:"fileId"`
	FileName      string `json:"fileName"`
	Action        string `json:"action"`
	ContentLength int64  `json:"contentLength"`
}

// FilePage is one page of a file listing. NextFileName is empty on the last page.
type FilePage struct {
	Files        []File
	NextFileName string
	NextFileID   string
}

// ListFilesRequest selects files in a bucket.
type ListFilesRequest struct {
	BucketID      string
	Prefix        string
	StartFileName string
	StartFileID   string
	MaxFileCount  int
}
