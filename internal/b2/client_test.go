package b2_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/b2-backup-operator/internal/b2"
	"github.com/USA-RedDragon/b2-backup-operator/internal/b2/b2fake"
)

const (
	masterID  = "masterkeyid"
	masterKey = "masterkeysecret"

	parentPrefix  = "restic/"
	appPrefix     = "restic/app/"
	siblingPrefix = "restic/app2/"
)

func newTestClient(t *testing.T) (*b2.Client, *b2fake.Server, *[]time.Duration) {
	t.Helper()
	fake := b2fake.New(masterID, masterKey)
	fake.Logf = t.Logf
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	c := b2.NewClient(masterID, masterKey)
	c.AuthURL = srv.URL
	var slept []time.Duration
	c.Sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	return c, fake, &slept
}

func TestNewClientHasATimeout(t *testing.T) {
	c := b2.NewClient("id", "key")
	if c.HTTP == nil || c.HTTP.Timeout != 30*time.Second {
		t.Fatalf("HTTP client timeout = %v, want 30s", c.HTTP)
	}
	if c.AuthURL != b2.DefaultAuthURL {
		t.Errorf("AuthURL = %q, want %q", c.AuthURL, b2.DefaultAuthURL)
	}
}

func TestAuthorizeReadsTheS3Endpoint(t *testing.T) {
	c, _, _ := newTestClient(t)
	auth, err := c.Authorize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if auth.AccountID != b2fake.AccountID {
		t.Errorf("AccountID = %q", auth.AccountID)
	}
	if auth.S3APIURL != "https://s3.us-east-005.backblazeb2.com" {
		t.Errorf("S3APIURL = %q", auth.S3APIURL)
	}
	region, err := auth.S3Region()
	if err != nil || region != "us-east-005" {
		t.Errorf("S3Region() = %q, %v; want us-east-005", region, err)
	}
	if !slices.Contains(auth.Capabilities, "writeKeys") {
		t.Errorf("capabilities = %v, want writeKeys among them", auth.Capabilities)
	}
}

func TestRegionFromS3URLRejectsOtherHosts(t *testing.T) {
	for _, raw := range []string{"https://example.com", "https://s3.", "::"} {
		if _, err := b2.RegionFromS3URL(raw); err == nil {
			t.Errorf("RegionFromS3URL(%q) succeeded", raw)
		}
	}
}

func TestAuthorizationIsCached(t *testing.T) {
	c, fake, _ := newTestClient(t)
	fake.AddBucket("cached-bucket")
	for range 3 {
		if _, err := c.FindBucket(context.Background(), "cached-bucket"); err != nil {
			t.Fatal(err)
		}
	}
	if n := fake.CountCalls("b2_authorize_account"); n != 1 {
		t.Errorf("authorized %d times, want 1", n)
	}
}

func TestExpiredTokenIsRefreshedOnce(t *testing.T) {
	c, fake, _ := newTestClient(t)
	fake.AddBucket("expiring-bucket")
	if _, err := c.FindBucket(context.Background(), "expiring-bucket"); err != nil {
		t.Fatal(err)
	}

	fake.ExpireTokens()
	if _, err := c.FindBucket(context.Background(), "expiring-bucket"); err != nil {
		t.Fatalf("FindBucket after expiry: %v", err)
	}
	if n := fake.CountCalls("b2_authorize_account"); n != 2 {
		t.Errorf("authorized %d times, want 2", n)
	}
}

func TestPersistentAuthFailureIsNotRetriedForever(t *testing.T) {
	c, fake, _ := newTestClient(t)
	if _, err := c.Authorize(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.Fail("b2_list_buckets", b2fake.Failure{Status: 401, Code: b2.CodeExpiredAuthToken, Times: 5})

	_, err := c.FindBucket(context.Background(), "whatever")
	var apiErr *b2.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != b2.CodeExpiredAuthToken {
		t.Fatalf("err = %v, want the expired_auth_token error", err)
	}
	if n := fake.CountCalls("b2_list_buckets"); n != 2 {
		t.Errorf("b2_list_buckets called %d times, want 2 (one retry after re-authorizing)", n)
	}
}

func TestBadCredentials(t *testing.T) {
	c, _, _ := newTestClient(t)
	c.Key = "wrong"
	_, err := c.Authorize(context.Background())
	if !errors.Is(err, b2.ErrBadCredentials) {
		t.Fatalf("err = %v, want ErrBadCredentials", err)
	}
}

func TestServiceUnavailableHonorsRetryAfter(t *testing.T) {
	c, fake, slept := newTestClient(t)
	fake.AddBucket("busy-bucket")
	fake.Fail("b2_list_buckets", b2fake.Failure{
		Status: 503, Code: b2.CodeServiceUnavailable, RetryAfter: "7", Times: 2,
	})

	if _, err := c.FindBucket(context.Background(), "busy-bucket"); err != nil {
		t.Fatalf("FindBucket: %v", err)
	}
	if want := []time.Duration{7 * time.Second, 7 * time.Second}; !slices.Equal(*slept, want) {
		t.Errorf("slept %v, want %v", *slept, want)
	}
}

func TestTooManyRequestsBacksOffThenGivesUp(t *testing.T) {
	c, fake, slept := newTestClient(t)
	c.MaxAttempts = 3
	if _, err := c.Authorize(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.Fail("b2_list_keys", b2fake.Failure{Status: 429, Code: b2.CodeTooManyRequests, Times: 10})

	_, err := c.ListKeys(context.Background())
	var apiErr *b2.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 429 {
		t.Fatalf("err = %v, want the 429", err)
	}
	if want := []time.Duration{time.Second, 2 * time.Second}; !slices.Equal(*slept, want) {
		t.Errorf("slept %v, want exponential backoff %v", *slept, want)
	}
	if n := fake.CountCalls("b2_list_keys"); n != 3 {
		t.Errorf("b2_list_keys called %d times, want 3", n)
	}
}

func TestRetrySleepStopsOnCancel(t *testing.T) {
	c, fake, _ := newTestClient(t)
	c.Sleep = nil
	if _, err := c.Authorize(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.Fail("b2_list_keys", b2fake.Failure{Status: 503, Code: b2.CodeServiceUnavailable, RetryAfter: "60", Times: 5})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.ListKeys(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context deadline", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("the retry sleep ignored the context")
	}
}

func TestTypedErrors(t *testing.T) {
	c, fake, _ := newTestClient(t)
	fake.AddBucket("taken-bucket")

	_, err := c.CreateBucket(context.Background(), "taken-bucket", nil)
	if !errors.Is(err, b2.ErrDuplicateBucketName) {
		t.Errorf("CreateBucket on a taken name: %v, want ErrDuplicateBucketName", err)
	}
	if _, err := c.FindBucket(context.Background(), "missing-bucket"); !errors.Is(err, b2.ErrNotFound) {
		t.Errorf("FindBucket on a missing name: %v, want ErrNotFound", err)
	}
	if err := c.DeleteKey(context.Background(), "nosuchkey"); !errors.Is(err, b2.ErrNotFound) {
		t.Errorf("DeleteKey on a missing key: %v, want ErrNotFound", err)
	}
	if err := c.DeleteBucket(context.Background(), "nosuchbucket"); !errors.Is(err, b2.ErrNotFound) {
		t.Errorf("DeleteBucket on a missing bucket: %v, want ErrNotFound", err)
	}
	fake.Fail("b2_list_file_versions", b2fake.Failure{Status: 400, Code: b2.CodeInvalidBucketID, Message: "Invalid bucketId"})
	if _, err := c.ListFileVersions(context.Background(), b2.ListFilesRequest{BucketID: "x"}); !errors.Is(err, b2.ErrNotFound) {
		t.Errorf("listing a bucket B2 calls invalid: %v, want ErrNotFound", err)
	}
	if err := c.DeleteFileVersion(context.Background(), "a", "b"); !errors.Is(err, b2.ErrNotFound) {
		t.Errorf("DeleteFileVersion on a missing file: %v, want ErrNotFound", err)
	}

	fake.Fail("b2_list_keys", b2fake.Failure{Status: 401, Code: b2.CodeUnauthorized, Message: "no listKeys"})
	if _, err := c.ListKeys(context.Background()); !errors.Is(err, b2.ErrUnauthorized) {
		t.Errorf("ListKeys without the capability: %v, want ErrUnauthorized", err)
	}
}

func TestNonJSONErrorBodyIsKept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = fmt.Fprint(w, "upstream exploded")
	}))
	defer srv.Close()

	c := b2.NewClient("id", "key")
	c.AuthURL = srv.URL
	c.MaxAttempts = 1
	_, err := c.Authorize(context.Background())
	if err == nil || !strings.Contains(err.Error(), "upstream exploded") {
		t.Fatalf("err = %v, want it to carry the body", err)
	}
}

func TestCreateAndListKeys(t *testing.T) {
	c, fake, _ := newTestClient(t)
	bucket := fake.AddBucket("key-bucket")

	key, err := c.CreateKey(context.Background(), b2.CreateKeyRequest{
		KeyName:      "b2bk-ns-name-1234",
		Capabilities: []string{"listFiles", "readFiles"},
		BucketID:     bucket.BucketID,
		NamePrefix:   appPrefix,
	})
	if err != nil {
		t.Fatal(err)
	}
	if key.ApplicationKey == "" || key.ApplicationKeyID == "" {
		t.Fatalf("CreateKey returned %+v, want both halves", key)
	}

	keys, err := c.ListKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(keys, func(k b2.Key) bool { return k.ApplicationKeyID == key.ApplicationKeyID })
	if i < 0 {
		t.Fatalf("created key %s not listed", key.ApplicationKeyID)
	}
	if keys[i].ApplicationKey != "" {
		t.Error("b2_list_keys must never return the secret")
	}
	if keys[i].BucketID != bucket.BucketID || keys[i].NamePrefix != appPrefix {
		t.Errorf("listed key = %+v, want the bucket and prefix restriction", keys[i])
	}
}

func TestListKeysFollowsPages(t *testing.T) {
	c, fake, _ := newTestClient(t)
	for i := range 1003 {
		fake.AddKey(fmt.Sprintf("k%d", i), "", "")
	}
	keys, err := c.ListKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1004 {
		t.Errorf("listed %d keys, want 1004 including the master key", len(keys))
	}
	if n := fake.CountCalls("b2_list_keys"); n != 2 {
		t.Errorf("b2_list_keys called %d times, want 2 pages", n)
	}
}

func TestFileExistsMatchesTheExactName(t *testing.T) {
	c, fake, _ := newTestClient(t)
	bucket := fake.AddBucket("files-bucket")
	fake.PutFile(bucket.BucketID, "restic/app/config2", 10)
	fake.PutFile(bucket.BucketID, "restic/other/config", 10)

	ok, err := b2.FileExists(context.Background(), c, bucket.BucketID, "restic/app/config")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("a file with a longer name was taken for the config")
	}

	fake.PutFile(bucket.BucketID, "restic/app/config", 10)
	if ok, _ := b2.FileExists(context.Background(), c, bucket.BucketID, "restic/app/config"); !ok {
		t.Error("the config file was not found")
	}

	fake.HideFile(bucket.BucketID, "restic/app/config")
	if ok, _ := b2.FileExists(context.Background(), c, bucket.BucketID, "restic/app/config"); ok {
		t.Error("a hidden file was reported as existing")
	}
}

func TestUsageSumsVisibleFilesAndStopsAtThePageCap(t *testing.T) {
	c, fake, _ := newTestClient(t)
	bucket := fake.AddBucket("usage-bucket")
	fake.PutFile(bucket.BucketID, "restic/app/config", 100)
	fake.PutFile(bucket.BucketID, "restic/app/data/aa", 1000)
	fake.PutFile(bucket.BucketID, "restic/app/data/bb", 500)
	fake.HideFile(bucket.BucketID, "restic/app/data/bb")
	fake.PutFile(bucket.BucketID, "restic/other/data/cc", 99999)

	total, truncated, err := b2.Usage(context.Background(), c, bucket.BucketID, appPrefix, 5)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1100 || truncated {
		t.Errorf("Usage = %d, truncated=%t; want 1100, false", total, truncated)
	}

	for i := range b2.MaxFileCount {
		fake.PutFile(bucket.BucketID, fmt.Sprintf("restic/app/data/x%05d", i), 1)
	}
	_, truncated, err = b2.Usage(context.Background(), c, bucket.BucketID, appPrefix, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Error("Usage did not report that it stopped at the page cap")
	}
}

func TestDeleteVersionsUnderLeavesOtherPrefixesAlone(t *testing.T) {
	c, fake, _ := newTestClient(t)
	bucket := fake.AddBucket("shared-bucket")
	fake.PutFile(bucket.BucketID, "restic/app/config", 1)
	fake.PutFile(bucket.BucketID, "restic/app/data/aa", 1)
	fake.HideFile(bucket.BucketID, "restic/app/data/aa")
	fake.StartLargeFile(bucket.BucketID, "restic/app/data/big")
	fake.PutFile(bucket.BucketID, "restic/app2/config", 1)
	fake.PutFile(bucket.BucketID, "restic/apple", 1)
	fake.StartLargeFile(bucket.BucketID, "restic/app2/data/big")

	deleted, done, err := b2.DeleteVersionsUnder(context.Background(), c, bucket.BucketID, appPrefix, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !done || deleted != 4 {
		t.Errorf("deleted %d, done=%t; want 4, true", deleted, done)
	}
	got := fake.FileNames(bucket.BucketID)
	want := []string{"restic/app2/config", "restic/apple", "restic/app2/data/big"}
	if !slices.Equal(got, want) {
		t.Errorf("remaining = %v, want %v", got, want)
	}
}

func TestDeleteVersionsUnderRespectsTheBudget(t *testing.T) {
	c, fake, _ := newTestClient(t)
	bucket := fake.AddBucket("budget-bucket")
	for i := range 10 {
		fake.PutFile(bucket.BucketID, fmt.Sprintf("p/f%d", i), 1)
	}
	deleted, done, err := b2.DeleteVersionsUnder(context.Background(), c, bucket.BucketID, "p/", 4)
	if err != nil {
		t.Fatal(err)
	}
	if done || deleted != 4 {
		t.Errorf("deleted %d, done=%t; want 4, false", deleted, done)
	}
	deleted, done, err = b2.DeleteVersionsUnder(context.Background(), c, bucket.BucketID, "p/", 100)
	if err != nil || !done || deleted != 6 {
		t.Errorf("second pass deleted %d, done=%t, err=%v; want 6, true, nil", deleted, done, err)
	}
}

func TestUpdateLifecycleRulesUsesTheRevision(t *testing.T) {
	c, fake, _ := newTestClient(t)
	fake.AddBucket("rules-bucket")
	bucket, err := c.FindBucket(context.Background(), "rules-bucket")
	if err != nil {
		t.Fatal(err)
	}
	one := 1
	rules := []b2.LifecycleRule{{FileNamePrefix: "a/", DaysFromHidingToDeleting: &one}}
	updated, err := c.UpdateLifecycleRules(context.Background(), bucket, rules)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != bucket.Revision+1 || len(updated.LifecycleRules) != 1 {
		t.Errorf("updated = %+v", updated)
	}

	_, err = c.UpdateLifecycleRules(context.Background(), bucket, nil)
	if !errors.Is(err, b2.ErrConflict) {
		t.Errorf("update with a stale revision: %v, want ErrConflict", err)
	}
}

func TestMergeLifecycleRule(t *testing.T) {
	one, two := 1, 2
	want := b2.LifecycleRule{FileNamePrefix: appPrefix, DaysFromHidingToDeleting: &one}
	other := b2.LifecycleRule{FileNamePrefix: "media/", DaysFromHidingToDeleting: &two}

	tests := []struct {
		name    string
		rules   []b2.LifecycleRule
		outcome b2.RuleOutcome
		count   int
	}{
		{"added beside an unrelated rule", []b2.LifecycleRule{other}, b2.RuleAdded, 2},
		{"already present", []b2.LifecycleRule{other, want}, b2.RuleUnchanged, 2},
		{"updated in place", []b2.LifecycleRule{
			other, {FileNamePrefix: appPrefix, DaysFromHidingToDeleting: &two},
		}, b2.RuleUpdated, 2},
		{"covered by a parent rule", []b2.LifecycleRule{
			{FileNamePrefix: parentPrefix, DaysFromHidingToDeleting: &one},
		}, b2.RuleCovered, 1},
		{"not covered by a slower parent rule", []b2.LifecycleRule{
			{FileNamePrefix: parentPrefix, DaysFromHidingToDeleting: &two},
		}, b2.RuleAdded, 2},
		{"added beside a child rule, which B2 allows", []b2.LifecycleRule{
			{FileNamePrefix: "restic/app/sub/", DaysFromHidingToDeleting: &one},
		}, b2.RuleAdded, 2},
		{"a sibling with a shared stem is not a parent", []b2.LifecycleRule{
			{FileNamePrefix: siblingPrefix, DaysFromHidingToDeleting: &one},
		}, b2.RuleAdded, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := slices.Clone(tt.rules)
			got, outcome, _ := b2.MergeLifecycleRule(tt.rules, want)
			if outcome != tt.outcome || len(got) != tt.count {
				t.Errorf("outcome %v with %d rules, want %v with %d", outcome, len(got), tt.outcome, tt.count)
			}
			if !slices.EqualFunc(tt.rules, before, func(a, b b2.LifecycleRule) bool {
				return a.FileNamePrefix == b.FileNamePrefix && a.DaysFromHidingToDeleting == b.DaysFromHidingToDeleting
			}) {
				t.Error("MergeLifecycleRule modified its input")
			}
			for _, r := range got {
				if r.FileNamePrefix == other.FileNamePrefix && *r.DaysFromHidingToDeleting != two {
					t.Error("another prefix's rule was changed")
				}
			}
		})
	}
}

func TestHidingRulesFindsRulesThatWouldHideLiveFiles(t *testing.T) {
	one, thirty := 1, 30
	rules := []b2.LifecycleRule{
		{FileNamePrefix: parentPrefix, DaysFromHidingToDeleting: &one},
		{FileNamePrefix: "", DaysFromUploadingToHiding: &thirty},
		{FileNamePrefix: "media/", DaysFromUploadingToHiding: &thirty},
	}
	got := b2.HidingRules(rules, appPrefix)
	if !slices.Equal(got, []string{""}) {
		t.Errorf("HidingRules = %q, want only the bucket-wide rule", got)
	}
}

func TestRemoveLifecycleRuleOnlyRemovesTheExactPrefix(t *testing.T) {
	one := 1
	rules := []b2.LifecycleRule{
		{FileNamePrefix: appPrefix, DaysFromHidingToDeleting: &one},
		{FileNamePrefix: siblingPrefix, DaysFromHidingToDeleting: &one},
	}
	got, removed := b2.RemoveLifecycleRule(rules, appPrefix)
	if !removed || len(got) != 1 || got[0].FileNamePrefix != siblingPrefix {
		t.Errorf("got %+v, removed=%t", got, removed)
	}
	if _, removed := b2.RemoveLifecycleRule(rules, parentPrefix); removed {
		t.Error("a parent prefix removed a rule it does not own")
	}
}

func TestAuthorizeReadsTheAllowedBlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"accountId":"acct","authorizationToken":"tok","apiInfo":{"storageApi":{
			"apiUrl":"https://api005.backblazeb2.com","s3ApiUrl":"https://s3.eu-central-003.backblazeb2.com",
			"allowed":{"bucketId":"b1","bucketName":"one","capabilities":["listFiles"],"namePrefix":"p/"}}}}`)
	}))
	defer srv.Close()

	c := b2.NewClient("id", "key")
	c.AuthURL = srv.URL
	auth, err := c.Authorize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(auth.BucketIDs, []string{"b1"}) || auth.NamePrefix != "p/" ||
		!slices.Equal(auth.Capabilities, []string{"listFiles"}) {
		t.Errorf("auth = %+v", auth)
	}
	if region, _ := auth.S3Region(); region != "eu-central-003" {
		t.Errorf("region = %q", region)
	}
}

func TestDeleteVersionsUnderStopsWhenNothingCanBeDeleted(t *testing.T) {
	c, fake, _ := newTestClient(t)
	bucket := fake.AddBucket("stuck-bucket")
	fake.PutFile(bucket.BucketID, "p/a", 1)
	fake.Fail("b2_delete_file_version", b2fake.Failure{Status: 400, Code: b2.CodeFileNotPresent, Times: 1000})

	deleted, done, err := b2.DeleteVersionsUnder(context.Background(), c, bucket.BucketID, "p/", 100)
	if !errors.Is(err, b2.ErrNoProgress) || done || deleted != 0 {
		t.Errorf("deleted %d, done=%t, err=%v; want ErrNoProgress instead of spinning", deleted, done, err)
	}
	if n := fake.CountCalls("b2_delete_file_version"); n != 1 {
		t.Errorf("tried %d deletes, want 1 before giving up", n)
	}
}

func TestDeleteVersionsUnderCancelsUnfinishedFilesItLists(t *testing.T) {
	c, fake, _ := newTestClient(t)
	bucket := fake.AddBucket("unfinished-bucket")
	for i := range 3 {
		fake.StartLargeFile(bucket.BucketID, fmt.Sprintf("p/big%d", i))
	}
	fake.PutFile(bucket.BucketID, "p/small", 1)

	deleted, done, err := b2.DeleteVersionsUnder(context.Background(), c, bucket.BucketID, "p/", 2)
	if err != nil || done || deleted != 2 {
		t.Fatalf("deleted %d, done=%t, err=%v; want the budget of 2 spent", deleted, done, err)
	}
	if _, done, err = b2.DeleteVersionsUnder(context.Background(), c, bucket.BucketID, "p/", 100); err != nil || !done {
		t.Fatalf("second pass done=%t, err=%v", done, err)
	}
	if got := fake.FileNames(bucket.BucketID); len(got) != 0 {
		t.Errorf("left %v", got)
	}
}

func TestLifecycleRulesRoundTripEveryField(t *testing.T) {
	t.Parallel()
	var sent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "b2_authorize_account"):
			_, _ = fmt.Fprintf(w, `{"accountId":"a","authorizationToken":"t","apiInfo":{"storageApi":{`+
				`"apiUrl":"http://%s","s3ApiUrl":"https://s3.us-east-005.backblazeb2.com"}}}`, r.Host)
		case strings.HasSuffix(r.URL.Path, "b2_list_buckets"):
			_, _ = io.WriteString(w, `{"buckets":[{"bucketId":"b","bucketName":"shared","revision":4,"lifecycleRules":[`+
				`{"fileNamePrefix":"","daysFromStartingToCancelingUnfinishedLargeFiles":7,`+
				`"daysFromHidingToDeleting":null,"daysFromUploadingToHiding":null,"futureField":{"x":1}}]}]}`)
		case strings.HasSuffix(r.URL.Path, "b2_update_bucket"):
			b, _ := io.ReadAll(r.Body)
			sent = string(b)
			_, _ = io.WriteString(w, `{"bucketId":"b","bucketName":"shared","revision":5}`)
		}
	}))
	t.Cleanup(srv.Close)
	c := b2.NewClient("id", "key")
	c.AuthURL = srv.URL
	ctx := context.Background()

	bucket, err := c.FindBucket(ctx, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if got := bucket.LifecycleRules[0].DaysFromStartingToCancelingUnfinishedLargeFiles; got == nil || *got != 7 {
		t.Fatalf("daysFromStartingToCancelingUnfinishedLargeFiles = %v, want 7", got)
	}
	thirty := 30
	rules, outcome, _ := b2.MergeLifecycleRule(bucket.LifecycleRules, b2.LifecycleRule{FileNamePrefix: appPrefix, DaysFromHidingToDeleting: &thirty})
	if outcome != b2.RuleAdded {
		t.Fatalf("outcome = %v, want RuleAdded", outcome)
	}
	if _, err := c.UpdateLifecycleRules(ctx, bucket, rules); err != nil {
		t.Fatal(err)
	}

	var body struct {
		LifecycleRules []map[string]json.RawMessage `json:"lifecycleRules"`
	}
	if err := json.Unmarshal([]byte(sent), &body); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		field string
		want  string
	}{
		{"daysFromStartingToCancelingUnfinishedLargeFiles", "7"},
		{"futureField", `{"x":1}`},
		{"fileNamePrefix", `""`},
	}
	for _, tt := range tests {
		if got := string(body.LifecycleRules[0][tt.field]); got != tt.want {
			t.Errorf("the other tool's rule sent %s=%s, want %s", tt.field, got, tt.want)
		}
	}
	if got := string(body.LifecycleRules[1]["daysFromHidingToDeleting"]); got != "30" {
		t.Errorf("the added rule sent daysFromHidingToDeleting=%s, want 30", got)
	}
}

func TestListFileVersionsNeverSendsAStartIDAlone(t *testing.T) {
	t.Parallel()
	c, fake, _ := newTestClient(t)
	bucket := fake.AddBucket("list-bucket")
	fake.PutFile(bucket.BucketID, "a", 1)
	if _, err := c.ListFileVersions(context.Background(), b2.ListFilesRequest{
		BucketID: bucket.BucketID, StartFileID: "stray-id",
	}); err != nil {
		t.Errorf("a start file ID without a start name was sent: %v", err)
	}
}
