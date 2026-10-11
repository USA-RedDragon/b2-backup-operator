package controller

import (
	"fmt"
	"math"
)

const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	managedByValue = "b2-backup-operator"
)

const (
	bytesPerMB  = 1000000
	bytesPerGiB = 1073741824

	mbPerMiB = 1.048576
	mbPerGiB = float64(bytesPerGiB) / bytesPerMB
)

func formatUsage(mb float64) string {
	if mb <= 0 {
		return "0"
	}
	gib := mb / mbPerGiB
	if gib < 1 {
		return fmt.Sprintf("%dMiB", int(math.Round(mb/mbPerMiB)))
	}
	if gib >= 1024 {
		return fmt.Sprintf("%.1fTiB", gib/1024)
	}
	return fmt.Sprintf("%.1fGiB", gib)
}

func formatQuota(mb int64) string {
	if mb <= 0 {
		return ""
	}
	return formatUsage(float64(mb))
}

func quotaMB(gib int32) int64 {
	return (int64(gib)*bytesPerGiB + bytesPerMB/2) / bytesPerMB
}

func quotaMBPtr(v *int32) *int64 {
	if v == nil {
		return nil
	}
	out := quotaMB(*v)
	return &out
}

func int64Ptr(v *int32) *int64 {
	if v == nil {
		return nil
	}
	out := int64(*v)
	return &out
}
