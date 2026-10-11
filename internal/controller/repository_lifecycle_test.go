package controller

import (
	"strings"
	"testing"

	"k8s.io/utils/ptr"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/b2"
)

func TestLifecycleRuleSemantics(t *testing.T) {
	t.Parallel()
	one, seven, thirty := 1, 7, 30
	tests := []struct {
		name     string
		rules    []b2.LifecycleRule
		explicit *int32
		reason   string
		want     map[string]int
	}{
		{"a longer parent covers", []b2.LifecycleRule{{FileNamePrefix: resticPrefix, DaysFromHidingToDeleting: &thirty}},
			nil, reasonCovered, map[string]int{resticPrefix: 30}},
		{"an old one-day rule of ours is dropped under a parent", []b2.LifecycleRule{
			{FileNamePrefix: resticPrefix, DaysFromHidingToDeleting: &thirty},
			{FileNamePrefix: myappPrefix, DaysFromHidingToDeleting: &one},
		}, nil, reasonCovered, map[string]int{resticPrefix: 30}},
		{"an explicit value adds a rule under a parent", []b2.LifecycleRule{
			{FileNamePrefix: resticPrefix, DaysFromHidingToDeleting: &thirty},
		}, ptr.To(int32(7)), reasonManaged, map[string]int{resticPrefix: 30, myappPrefix: 7}},
		{"nothing covers the path", nil, nil, reasonManaged, map[string]int{myappPrefix: 30}},
		{"an explicit longer value under a shorter parent", []b2.LifecycleRule{
			{FileNamePrefix: resticPrefix, DaysFromHidingToDeleting: &seven},
		}, ptr.To(int32(30)), reasonManaged, map[string]int{resticPrefix: 7, myappPrefix: 30}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeB2(t)
			f.AddBucket(sharedBucket, tt.rules...)
			r, c := newHarness(t, f, prefixRepo(func(r *b2backupv1.Repository) {
				if tt.explicit != nil {
					r.Spec.Lifecycle = &b2backupv1.LifecycleSpec{DaysFromHidingToDeleting: tt.explicit}
				}
			}))
			reconcileN(t, r, 2)

			got := map[string]int{}
			for _, rule := range f.Bucket(sharedBucket).LifecycleRules {
				got[rule.FileNamePrefix] = *rule.DaysFromHidingToDeleting
			}
			if !mapsEqual(got, tt.want) {
				t.Errorf("rules = %v, want %v", got, tt.want)
			}
			cond := condition(t, c, b2backupv1.RepositoryConditionLifecycleRule)
			if cond.Reason != tt.reason {
				t.Errorf("LifecycleRule = %+v, want %s", cond, tt.reason)
			}
			if tt.name == "an explicit longer value under a shorter parent" &&
				!strings.Contains(cond.Message, "after 7 day(s)") {
				t.Errorf("message %q does not say the parent's shorter window applies", cond.Message)
			}
		})
	}
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}
