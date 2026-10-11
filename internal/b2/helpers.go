package b2

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// FileExists reports whether a visible file with exactly this name exists.
func FileExists(ctx context.Context, api API, bucketID, name string) (bool, error) {
	page, err := api.ListFileNames(ctx, ListFilesRequest{
		BucketID: bucketID, Prefix: name, StartFileName: name, MaxFileCount: 1,
	})
	if err != nil {
		return false, err
	}
	return len(page.Files) > 0 && page.Files[0].FileName == name, nil
}

// AnyVersionUnder reports whether any file version, hidden or not, has the prefix.
func AnyVersionUnder(ctx context.Context, api API, bucketID, prefix string) (bool, error) {
	page, err := api.ListFileVersions(ctx, ListFilesRequest{
		BucketID: bucketID, Prefix: prefix, MaxFileCount: 1,
	})
	if err != nil {
		return false, err
	}
	return len(page.Files) > 0, nil
}

// Usage sums the size of the visible files under prefix, reading at most
// maxPages pages. truncated is true when it stopped before the end.
func Usage(ctx context.Context, api API, bucketID, prefix string, maxPages int) (int64, bool, error) {
	var total int64
	start := ""
	for range maxPages {
		page, err := api.ListFileNames(ctx, ListFilesRequest{
			BucketID: bucketID, Prefix: prefix, StartFileName: start, MaxFileCount: MaxFileCount,
		})
		if err != nil {
			return 0, false, err
		}
		for _, f := range page.Files {
			if f.Action == "" || f.Action == ActionUpload {
				total += f.ContentLength
			}
		}
		if page.NextFileName == "" {
			return total, false, nil
		}
		start = page.NextFileName
	}
	return total, true, nil
}

// DeleteVersionsUnder permanently deletes file versions and unfinished large
// files whose names start with prefix, at most budget of them. done is true when
// nothing under the prefix is left. An empty prefix means the whole bucket.
func DeleteVersionsUnder(ctx context.Context, api API, bucketID, prefix string, budget int) (int, bool, error) {
	deleted := 0

	start := ""
	for deleted < budget {
		page, err := api.ListUnfinishedLargeFiles(ctx, ListFilesRequest{
			BucketID: bucketID, Prefix: prefix, StartFileID: start, MaxFileCount: min(budget-deleted, 100),
		})
		if err != nil {
			return deleted, false, err
		}
		for _, f := range page.Files {
			if deleted >= budget {
				return deleted, false, nil
			}
			if !strings.HasPrefix(f.FileName, prefix) {
				continue
			}
			if err := api.CancelLargeFile(ctx, f.FileID); err != nil && !errors.Is(err, ErrNotFound) {
				return deleted, false, err
			}
			deleted++
		}
		if page.NextFileID == "" || page.NextFileID == start {
			break
		}
		start = page.NextFileID
	}

	for deleted < budget {
		page, err := api.ListFileVersions(ctx, ListFilesRequest{
			BucketID: bucketID, Prefix: prefix, MaxFileCount: min(budget-deleted, 1000),
		})
		if err != nil {
			return deleted, false, err
		}
		if len(page.Files) == 0 {
			return deleted, true, nil
		}
		removed := 0
		for _, f := range page.Files {
			if !strings.HasPrefix(f.FileName, prefix) {
				continue
			}
			if f.Action == ActionStart {
				err = api.CancelLargeFile(ctx, f.FileID)
			} else {
				err = api.DeleteFileVersion(ctx, f.FileName, f.FileID)
			}
			switch {
			case err == nil:
				removed++
			case !errors.Is(err, ErrNotFound):
				return deleted, false, err
			}
		}
		deleted += removed
		if removed == 0 {
			return deleted, false, fmt.Errorf("%w: B2 still lists %d versions under %q that it says are gone",
				ErrNoProgress, len(page.Files), prefix)
		}
	}

	remaining, err := AnyVersionUnder(ctx, api, bucketID, prefix)
	if err != nil {
		return deleted, false, err
	}
	return deleted, !remaining, nil
}

// RuleOutcome says what MergeLifecycleRule decided.
type RuleOutcome int

const (
	// RuleUnchanged means a rule for exactly this prefix already says what is wanted.
	RuleUnchanged RuleOutcome = iota
	// RuleAdded means no rule covered the prefix and one was appended.
	RuleAdded
	// RuleUpdated means the rule for this prefix had other settings and was replaced.
	RuleUpdated
	// RuleCovered means a rule for a shorter prefix already deletes hidden files at least as soon.
	RuleCovered
)

// MergeLifecycleRule returns rules with want added or updated, leaving every
// rule for any other prefix untouched. B2 applies the smallest value of every
// rule whose prefix matches a file, so a rule on a parent prefix that already
// deletes hidden files as soon as want covers it, and nothing is added.
func MergeLifecycleRule(rules []LifecycleRule, want LifecycleRule) ([]LifecycleRule, RuleOutcome, string) {
	out := slices.Clone(rules)
	for i, r := range out {
		if r.FileNamePrefix != want.FileNamePrefix {
			continue
		}
		if sameDays(r.DaysFromHidingToDeleting, want.DaysFromHidingToDeleting) &&
			sameDays(r.DaysFromUploadingToHiding, want.DaysFromUploadingToHiding) {
			return out, RuleUnchanged, r.FileNamePrefix
		}
		out[i] = want
		return out, RuleUpdated, r.FileNamePrefix
	}
	if want.DaysFromHidingToDeleting != nil && want.DaysFromUploadingToHiding == nil {
		for _, r := range out {
			if strings.HasPrefix(want.FileNamePrefix, r.FileNamePrefix) &&
				r.DaysFromHidingToDeleting != nil && *r.DaysFromHidingToDeleting <= *want.DaysFromHidingToDeleting {
				return out, RuleCovered, r.FileNamePrefix
			}
		}
	}
	return append(out, want), RuleAdded, want.FileNamePrefix
}

// HidingRules returns the rules that would hide live files under prefix after
// some days. restic never rewrites a pack, so such a rule eventually hides data
// a snapshot still needs.
func HidingRules(rules []LifecycleRule, prefix string) []string {
	var out []string
	for _, r := range rules {
		if r.DaysFromUploadingToHiding != nil &&
			(strings.HasPrefix(prefix, r.FileNamePrefix) || strings.HasPrefix(r.FileNamePrefix, prefix)) {
			out = append(out, r.FileNamePrefix)
		}
	}
	return out
}

// RemoveLifecycleRule returns rules without the rule for exactly prefix.
func RemoveLifecycleRule(rules []LifecycleRule, prefix string) ([]LifecycleRule, bool) {
	out := slices.DeleteFunc(slices.Clone(rules), func(r LifecycleRule) bool {
		return r.FileNamePrefix == prefix
	})
	return out, len(out) != len(rules)
}

func sameDays(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
