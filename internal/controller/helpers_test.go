package controller

import "testing"

func TestFormatBytesUsesBinaryUnits(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0B"},
		{1023, "1023B"},
		{1024, "1KiB"},
		{4 << 20, "4MiB"},
		{1516244992, "1.4GiB"},
		{57086832640, "53.2GiB"},
		{3 << 40, "3.0TiB"},
	}
	for _, tt := range tests {
		if got := formatBytes(tt.in); got != tt.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestResticURL(t *testing.T) {
	tests := []struct {
		endpoint, bucket, path, want string
	}{
		{"https://s3.us-east-005.backblazeb2.com", "shared", "restic/kosync",
			"s3:https://s3.us-east-005.backblazeb2.com/shared/restic/kosync"},
		{"https://s3.us-east-005.backblazeb2.com/", "own-bucket", "",
			"s3:https://s3.us-east-005.backblazeb2.com/own-bucket"},
	}
	for _, tt := range tests {
		if got := resticURL(tt.endpoint, tt.bucket, tt.path); got != tt.want {
			t.Errorf("resticURL(%q, %q, %q) = %q, want %q", tt.endpoint, tt.bucket, tt.path, got, tt.want)
		}
	}
}

func TestPathsOverlap(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"r/a", "r/a", true},
		{"r", "r/b", true},
		{"r/c/sub", "r", true},
		{"", "anything", true},
		{"r/d", "r/d2", false},
		{"r/e", "r/eel", false},
		{"a/b", "c/d", false},
	}
	for _, tt := range tests {
		if got := pathsOverlap(tt.a, tt.b); got != tt.want {
			t.Errorf("pathsOverlap(%q, %q) = %t, want %t", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestFilePrefixAndConfigName(t *testing.T) {
	if got := configPath("backups/app"); got != "backups/app/config" {
		t.Errorf("configPath = %q", got)
	}
	if got := configPath(""); got != "config" {
		t.Errorf("configPath at the root = %q, want config", got)
	}
}
