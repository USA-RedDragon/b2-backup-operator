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

func TestDeleteResticRepositoryTouchesOnlyResticLayout(t *testing.T) {
	t.Parallel()
	c, fake, _ := newTestClient(t)
	bucket := fake.AddBucket("shared-bucket")
	for _, name := range []string{
		"restic/app/config", "restic/app/keys/k1", "restic/app/data/aa", "restic/app/index/i1",
		"restic/app/snapshots/s1", "restic/app/locks/l1",
	} {
		fake.PutFile(bucket.BucketID, name, 1)
	}
	fake.HideFile(bucket.BucketID, "restic/app/data/aa")
	fake.StartLargeFile(bucket.BucketID, "restic/app/data/big")
	keep := []string{
		"restic/app/config.bak", "restic/app/notes.txt", "restic/app/other/config", "restic/app/datax/f",
		"restic/app2/config", "restic/apple",
	}
	for _, name := range keep {
		fake.PutFile(bucket.BucketID, name, 1)
	}
	fake.StartLargeFile(bucket.BucketID, "restic/app/elsewhere/big")

	deleted, done, err := b2.DeleteResticRepository(context.Background(), c, bucket.BucketID, "restic/app", 100)
	if err != nil {
		t.Fatal(err)
	}
	if !done || deleted != 8 {
		t.Errorf("deleted %d, done=%t; want 8, true", deleted, done)
	}
	want := append(slices.Clone(keep), "restic/app/elsewhere/big")
	if got := fake.FileNames(bucket.BucketID); !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
		t.Errorf("remaining = %v, want %v", got, want)
	}
	if left, err := b2.ResticRepositoryRemains(context.Background(), c, bucket.BucketID, "restic/app"); err != nil || left {
		t.Errorf("ResticRepositoryRemains = %t, %v; want false", left, err)
	}
}

func TestDeleteResticRepositoryAtTheBucketRoot(t *testing.T) {
	t.Parallel()
	c, fake, _ := newTestClient(t)
	bucket := fake.AddBucket("root-bucket")
	fake.PutFile(bucket.BucketID, "config", 1)
	fake.PutFile(bucket.BucketID, "data/00/aa", 1)
	fake.PutFile(bucket.BucketID, "restic/other/config", 1)

	if _, done, err := b2.DeleteResticRepository(context.Background(), c, bucket.BucketID, "", 100); err != nil || !done {
		t.Fatalf("done=%t, err=%v", done, err)
	}
	if got := fake.FileNames(bucket.BucketID); !slices.Equal(got, []string{"restic/other/config"}) {
		t.Errorf("remaining = %v, want another repository's files untouched", got)
	}
}

func TestDeleteResticRepositoryRespectsTheBudget(t *testing.T) {
	t.Parallel()
	c, fake, _ := newTestClient(t)
	bucket := fake.AddBucket("budget-bucket")
	for i := range 10 {
		fake.PutFile(bucket.BucketID, fmt.Sprintf("p/data/f%d", i), 1)
	}
	deleted, done, err := b2.DeleteResticRepository(context.Background(), c, bucket.BucketID, "p", 4)
	if err != nil {
		t.Fatal(err)
	}
	if done || deleted != 4 {
		t.Errorf("deleted %d, done=%t; want 4, false", deleted, done)
	}
	deleted, done, err = b2.DeleteResticRepository(context.Background(), c, bucket.BucketID, "p", 100)
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
	t.Parallel()
	one, seven, thirty := 1, 7, 30
	cancel := 3
	other := b2.LifecycleRule{FileNamePrefix: "media/", DaysFromHidingToDeleting: &seven}
	parent := func(days *int) b2.LifecycleRule {
		return b2.LifecycleRule{FileNamePrefix: parentPrefix, DaysFromHidingToDeleting: days}
	}
	exact := func(days *int) b2.LifecycleRule {
		return b2.LifecycleRule{FileNamePrefix: appPrefix, DaysFromHidingToDeleting: days}
	}

	tests := []struct {
		name     string
		rules    []b2.LifecycleRule
		days     int
		explicit bool
		outcome  b2.RuleOutcome
		decider  string
		count    int
	}{
		{"added beside an unrelated rule", []b2.LifecycleRule{other}, 30, false, b2.RuleAdded, appPrefix, 2},
		{"a longer parent covers", []b2.LifecycleRule{parent(&thirty)}, 1, false, b2.RuleCovered, parentPrefix, 1},
		{"a shorter parent covers", []b2.LifecycleRule{parent(&one)}, 30, false, b2.RuleCovered, parentPrefix, 1},
		{"the bucket-wide rule covers", []b2.LifecycleRule{{FileNamePrefix: "", DaysFromHidingToDeleting: &thirty}},
			1, false, b2.RuleCovered, "", 1},
		{"a parent that only cancels large files does not cover", []b2.LifecycleRule{
			{FileNamePrefix: parentPrefix, DaysFromStartingToCancelingUnfinishedLargeFiles: &cancel},
		}, 30, false, b2.RuleAdded, appPrefix, 2},
		{"an own rule under a parent is removed", []b2.LifecycleRule{parent(&thirty), exact(&one)},
			30, false, b2.RuleRemoved, parentPrefix, 1},
		{"someone else's rule under a parent is kept", []b2.LifecycleRule{parent(&thirty), {
			FileNamePrefix: appPrefix, DaysFromHidingToDeleting: &one, DaysFromStartingToCancelingUnfinishedLargeFiles: &cancel,
		}}, 30, false, b2.RuleCovered, parentPrefix, 2},
		{"an exact rule with other days covers", []b2.LifecycleRule{exact(&seven)}, 30, false, b2.RuleCovered, appPrefix, 1},
		{"an exact rule with the same days is unchanged", []b2.LifecycleRule{exact(&thirty)}, 30, false, b2.RuleUnchanged, appPrefix, 1},
		{"an exact rule without hiding days gains them", []b2.LifecycleRule{
			{FileNamePrefix: appPrefix, DaysFromStartingToCancelingUnfinishedLargeFiles: &cancel},
		}, 30, false, b2.RuleUpdated, appPrefix, 1},
		{"explicit adds under a parent", []b2.LifecycleRule{parent(&thirty)}, 7, true, b2.RuleAdded, appPrefix, 2},
		{"explicit updates the exact rule", []b2.LifecycleRule{parent(&thirty), exact(&one)}, 7, true, b2.RuleUpdated, appPrefix, 2},
		{"explicit with the same days", []b2.LifecycleRule{exact(&seven)}, 7, true, b2.RuleUnchanged, appPrefix, 1},
		{"a child rule neither covers nor blocks", []b2.LifecycleRule{
			{FileNamePrefix: "restic/app/sub/", DaysFromHidingToDeleting: &one},
		}, 30, false, b2.RuleAdded, appPrefix, 2},
		{"a sibling with a shared stem is not a parent", []b2.LifecycleRule{
			{FileNamePrefix: siblingPrefix, DaysFromHidingToDeleting: &one},
		}, 30, false, b2.RuleAdded, appPrefix, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			before := slices.Clone(tt.rules)
			got, outcome, decider := b2.MergeLifecycleRule(tt.rules, appPrefix, tt.days, tt.explicit)
			if outcome != tt.outcome || len(got) != tt.count || decider.FileNamePrefix != tt.decider {
				t.Errorf("outcome %v by %q with %d rules, want %v by %q with %d",
					outcome, decider.FileNamePrefix, len(got), tt.outcome, tt.decider, tt.count)
			}
			if !slices.EqualFunc(tt.rules, before, func(a, b b2.LifecycleRule) bool {
				return a.FileNamePrefix == b.FileNamePrefix && a.DaysFromHidingToDeleting == b.DaysFromHidingToDeleting
			}) {
				t.Error("MergeLifecycleRule modified its input")
			}
			for _, r := range got {
				if r.FileNamePrefix == other.FileNamePrefix && *r.DaysFromHidingToDeleting != seven {
					t.Error("another prefix's rule was changed")
				}
				if r.FileNamePrefix == appPrefix && r.DaysFromStartingToCancelingUnfinishedLargeFiles == nil &&
					slices.ContainsFunc(before, func(b b2.LifecycleRule) bool {
						return b.FileNamePrefix == appPrefix && b.DaysFromStartingToCancelingUnfinishedLargeFiles != nil
					}) {
					t.Error("an updated rule lost a field it had")
				}
			}
		})
	}
}

func TestEffectiveHidingDaysIsTheSmallestMatchingRule(t *testing.T) {
	t.Parallel()
	one, seven, thirty := 1, 7, 30
	rules := []b2.LifecycleRule{
		{FileNamePrefix: "", DaysFromHidingToDeleting: &thirty},
		{FileNamePrefix: appPrefix, DaysFromHidingToDeleting: &seven},
		{FileNamePrefix: siblingPrefix, DaysFromHidingToDeleting: &one},
	}
	if days, ok := b2.EffectiveHidingDays(rules, appPrefix); !ok || days != seven {
		t.Errorf("EffectiveHidingDays = %d, %t; want 7", days, ok)
	}
	if _, ok := b2.EffectiveHidingDays(rules[1:], "media/"); ok {
		t.Error("an unrelated prefix reported a window")
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
	rules, outcome, _ := b2.MergeLifecycleRule(bucket.LifecycleRules, appPrefix, 30, false)
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

func TestRemoveLifecycleRuleOnlyRemovesItsOwnExactPrefix(t *testing.T) {
	t.Parallel()
	one, three := 1, 3
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
	shared := []b2.LifecycleRule{{
		FileNamePrefix: appPrefix, DaysFromHidingToDeleting: &one, DaysFromStartingToCancelingUnfinishedLargeFiles: &three,
	}}
	if _, removed := b2.RemoveLifecycleRule(shared, appPrefix); removed {
		t.Error("a rule carrying another tool's field was removed")
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

func TestDeleteResticRepositoryStopsWhenNothingCanBeDeleted(t *testing.T) {
	t.Parallel()
	c, fake, _ := newTestClient(t)
	bucket := fake.AddBucket("stuck-bucket")
	fake.PutFile(bucket.BucketID, "p/data/a", 1)
	fake.Fail("b2_delete_file_version", b2fake.Failure{Status: 400, Code: b2.CodeFileNotPresent, Times: 1000})

	deleted, done, err := b2.DeleteResticRepository(context.Background(), c, bucket.BucketID, "p", 100)
	if !errors.Is(err, b2.ErrNoProgress) || done || deleted != 0 {
		t.Errorf("deleted %d, done=%t, err=%v; want ErrNoProgress instead of spinning", deleted, done, err)
	}
	if n := fake.CountCalls("b2_delete_file_version"); n != 1 {
		t.Errorf("tried %d deletes, want 1 before giving up", n)
	}
}

func TestDeleteResticRepositoryCancelsUnfinishedFilesItLists(t *testing.T) {
	t.Parallel()
	c, fake, _ := newTestClient(t)
	bucket := fake.AddBucket("unfinished-bucket")
	for i := range 3 {
		fake.StartLargeFile(bucket.BucketID, fmt.Sprintf("p/data/big%d", i))
	}
	fake.PutFile(bucket.BucketID, "p/data/small", 1)

	deleted, done, err := b2.DeleteResticRepository(context.Background(), c, bucket.BucketID, "p", 2)
	if err != nil || done || deleted != 2 {
		t.Fatalf("deleted %d, done=%t, err=%v; want the budget of 2 spent", deleted, done, err)
	}
	if _, done, err = b2.DeleteResticRepository(context.Background(), c, bucket.BucketID, "p", 100); err != nil || !done {
		t.Fatalf("second pass done=%t, err=%v", done, err)
	}
	if got := fake.FileNames(bucket.BucketID); len(got) != 0 {
		t.Errorf("left %v", got)
	}
}
