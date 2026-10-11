package controller

import (
	"fmt"
	"strings"
)

const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	managedByValue = "b2-backup-operator"
)

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	value := float64(n)
	suffixes := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	i := -1
	for value >= unit && i < len(suffixes)-1 {
		value /= unit
		i++
	}
	if i <= 1 {
		return fmt.Sprintf("%.0f%s", value, suffixes[i])
	}
	return fmt.Sprintf("%.1f%s", value, suffixes[i])
}

func filePrefix(path string) string {
	if path == "" {
		return ""
	}
	return path + "/"
}

func joinPath(path, name string) string {
	return filePrefix(path) + name
}

func displayPath(path string) string {
	if path == "" {
		return "(bucket root)"
	}
	return path
}

func resticURL(s3APIURL, bucket, path string) string {
	url := "s3:" + strings.TrimRight(s3APIURL, "/") + "/" + bucket
	if path != "" {
		url += "/" + path
	}
	return url
}
