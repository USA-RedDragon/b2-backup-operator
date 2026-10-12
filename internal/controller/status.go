package controller

import (
	"slices"

	"k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// rebaseConditions applies to live only the conditions a pass changed between
// base and modified. A merge patch replaces the whole list, so a pass that read
// a stale copy would otherwise write back that copy's other conditions.
func rebaseConditions(base, modified, live []metav1.Condition) []metav1.Condition {
	out := slices.Clone(live)
	for _, cond := range modified {
		if old := apimeta.FindStatusCondition(base, cond.Type); old != nil && equality.Semantic.DeepEqual(*old, cond) {
			continue
		}
		if i := slices.IndexFunc(out, func(c metav1.Condition) bool { return c.Type == cond.Type }); i >= 0 {
			out[i] = cond
		} else {
			out = append(out, cond)
		}
	}
	for _, old := range base {
		if apimeta.FindStatusCondition(modified, old.Type) == nil {
			apimeta.RemoveStatusCondition(&out, old.Type)
		}
	}
	return out
}
