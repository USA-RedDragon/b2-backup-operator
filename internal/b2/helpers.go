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

// ResticLayout is what restic writes under a repository's path: the config
// file and these directories. Deletion touches nothing else.
var ResticLayout = []string{"locks/", "snapshots/", "index/", "data/", "keys/", "config"}

// DeleteResticRepository permanently deletes the file versions and unfinished
// large files that make up the restic repository at path, at most budget of
// them. Only names in ResticLayout under path are touched, so anything else
// that shares the path or the bucket is left alone. done is true when none of
// them is left. An empty path means the bucket root.
func DeleteResticRepository(ctx context.Context, api API, bucketID, path string, budget int) (int, bool, error) {
	base := ""
	if path != "" {
		base = path + "/"
	}
	deleted := 0
	for _, entry := range ResticLayout {
		n, done, err := deleteUnder(ctx, api, bucketID, base+entry, !strings.HasSuffix(entry, "/"), budget-deleted)
		deleted += n
		if err != nil || !done {
			return deleted, false, err
		}
	}
	return deleted, true, nil
}

// ResticRepositoryRemains reports whether any version of a name in ResticLayout
// under path is left.
func ResticRepositoryRemains(ctx context.Context, api API, bucketID, path string) (bool, error) {
	base := ""
	if path != "" {
		base = path + "/"
	}
	for _, entry := range ResticLayout {
		left, err := remains(ctx, api, bucketID, base+entry, !strings.HasSuffix(entry, "/"))
		if err != nil || left {
			return left, err
		}
	}
	return false, nil
}

func matcher(prefix string, exact bool) func(string) bool {
	if exact {
		return func(name string) bool { return name == prefix }
	}
	return func(name string) bool { return strings.HasPrefix(name, prefix) }
}

func remains(ctx context.Context, api API, bucketID, prefix string, exact bool) (bool, error) {
	page, err := api.ListFileVersions(ctx, ListFilesRequest{BucketID: bucketID, Prefix: prefix, MaxFileCount: 1})
	if err != nil {
		return false, err
	}
	return len(page.Files) > 0 && matcher(prefix, exact)(page.Files[0].FileName), nil
}

func deleteUnder(ctx context.Context, api API, bucketID, prefix string, exact bool, budget int) (int, bool, error) {
	match := matcher(prefix, exact)
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
			if !match(f.FileName) {
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
		var matching []File
		for _, f := range page.Files {
			if match(f.FileName) {
				matching = append(matching, f)
			}
		}
		if len(matching) == 0 {
			return deleted, true, nil
		}
		removed := 0
		for _, f := range matching {
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
				ErrNoProgress, len(matching), prefix)
		}
	}

	left, err := remains(ctx, api, bucketID, prefix, exact)
	if err != nil {
		return deleted, false, err
	}
	return deleted, !left, nil
}

// RuleOutcome says what MergeLifecycleRule decided.
type RuleOutcome int

const (
	// RuleUnchanged means the rule for exactly this prefix already says what is wanted.
	RuleUnchanged RuleOutcome = iota
	// RuleAdded means a rule for the prefix was appended.
	RuleAdded
	// RuleUpdated means the rule for exactly this prefix had another daysFromHidingToDeleting
	// and was changed, keeping every other field it had.
	RuleUpdated
	// RuleCovered means a rule on this prefix or a parent already sets
	// daysFromHidingToDeleting, so nothing was added.
	RuleCovered
	// RuleRemoved means a rule for exactly this prefix that only set
	// daysFromHidingToDeleting was dropped because a parent rule covers the prefix.
	RuleRemoved
)

// MergeLifecycleRule decides the bucket's rules for files under prefix.
//
// B2 applies the smallest non-null value of every rule whose prefix matches a
// file. Unless explicit is set, a rule on prefix or any parent of it that
// already sets daysFromHidingToDeleting covers the prefix whatever its days
// are, and nothing is added; a rule for exactly prefix that sets nothing but
// daysFromHidingToDeleting is then removed if a parent covers it, since such a
// rule could only shorten the parent's window. Only when nothing covers the
// prefix is a rule with days added.
//
// With explicit set, the rule for exactly prefix is added or set to days even
// under a parent rule, which shortens the window when days is the smaller.
//
// It returns the new rules, what it decided, and the rule that decided it.
func MergeLifecycleRule(rules []LifecycleRule, prefix string, days int, explicit bool) (
	[]LifecycleRule, RuleOutcome, LifecycleRule,
) {
	out := slices.Clone(rules)
	exact := slices.IndexFunc(out, func(r LifecycleRule) bool { return r.FileNamePrefix == prefix })
	want := LifecycleRule{FileNamePrefix: prefix, DaysFromHidingToDeleting: &days}

	if explicit {
		if exact < 0 {
			return append(out, want), RuleAdded, want
		}
		r := out[exact]
		if r.DaysFromHidingToDeleting != nil && *r.DaysFromHidingToDeleting == days {
			return out, RuleUnchanged, r
		}
		r.DaysFromHidingToDeleting = &days
		out[exact] = r
		return out, RuleUpdated, r
	}

	if parent, ok := coveringParent(out, prefix); ok {
		if exact >= 0 && onlyHidingDays(out[exact]) {
			return slices.Delete(out, exact, exact+1), RuleRemoved, parent
		}
		return out, RuleCovered, parent
	}
	if exact >= 0 {
		r := out[exact]
		if r.DaysFromHidingToDeleting != nil {
			if onlyHidingDays(r) && *r.DaysFromHidingToDeleting == days {
				return out, RuleUnchanged, r
			}
			return out, RuleCovered, r
		}
		r.DaysFromHidingToDeleting = &days
		out[exact] = r
		return out, RuleUpdated, r
	}
	return append(out, want), RuleAdded, want
}

func coveringParent(rules []LifecycleRule, prefix string) (LifecycleRule, bool) {
	var best LifecycleRule
	found := false
	for _, r := range rules {
		if r.FileNamePrefix == prefix || !strings.HasPrefix(prefix, r.FileNamePrefix) || r.DaysFromHidingToDeleting == nil {
			continue
		}
		if !found || *r.DaysFromHidingToDeleting < *best.DaysFromHidingToDeleting {
			best, found = r, true
		}
	}
	return best, found
}

func onlyHidingDays(r LifecycleRule) bool {
	return r.DaysFromHidingToDeleting != nil && r.DaysFromUploadingToHiding == nil &&
		r.DaysFromStartingToCancelingUnfinishedLargeFiles == nil && len(r.Extra) == 0
}

// EffectiveHidingDays returns how many days B2 keeps a hidden file under
// prefix: the smallest daysFromHidingToDeleting of every rule on prefix or a
// parent of it. ok is false when no such rule exists.
func EffectiveHidingDays(rules []LifecycleRule, prefix string) (int, bool) {
	days, ok := 0, false
	for _, r := range rules {
		if r.DaysFromHidingToDeleting == nil || !strings.HasPrefix(prefix, r.FileNamePrefix) {
			continue
		}
		if !ok || *r.DaysFromHidingToDeleting < days {
			days, ok = *r.DaysFromHidingToDeleting, true
		}
	}
	return days, ok
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

// RemoveLifecycleRule returns rules without the rule for exactly prefix, if
// that rule sets nothing but daysFromHidingToDeleting. A rule with any other
// field is someone else's and is kept.
func RemoveLifecycleRule(rules []LifecycleRule, prefix string) ([]LifecycleRule, bool) {
	out := slices.DeleteFunc(slices.Clone(rules), func(r LifecycleRule) bool {
		return r.FileNamePrefix == prefix && onlyHidingDays(r)
	})
	return out, len(out) != len(rules)
}

