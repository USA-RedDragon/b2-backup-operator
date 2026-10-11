package controller

import (
	"math"
	"testing"

	"k8s.io/utils/ptr"
)

func TestQuotaGiBConvertsToDecimalMegabytes(t *testing.T) {
	tests := []struct {
		name string
		gib  int32
		want int64
	}{
		{"one GiB is 1073.741824 MB, rounded", 1, 1074},
		{"ten", 10, 10737},
		{"twenty", 20, 21475},
		{"a hundred", 100, 107374},
		{"the CRD maximum stays inside a 32-bit Int", 1000000, 1073741824},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := quotaMB(tt.gib); got != tt.want {
				t.Errorf("quotaMB(%d) = %d, want %d", tt.gib, got, tt.want)
			}
			if mib := int64(tt.gib) * 1048576 / 1000000; mib == tt.want {
				t.Errorf("quotaMB(%d) matches the MiB conversion %d; the factor is 1073741824/1000000",
					tt.gib, mib)
			}
		})
	}

	if got := quotaMB(1000000); got > math.MaxInt32 {
		t.Errorf("the CRD maximum converts to %d, which overflows GraphQL's Int", got)
	}
	if quotaMBPtr(nil) != nil {
		t.Error("an unset quota must stay unset")
	}
	if got := quotaMBPtr(ptr.To(int32(100))); got == nil || *got != 107374 {
		t.Errorf("quotaMBPtr(100) = %v, want 107374", got)
	}
}

func TestFormatUsageRendersBinaryUnits(t *testing.T) {
	tests := []struct {
		name string
		mb   float64
		want string
	}{
		{"empty", 0, "0"},
		{"negative", -1, "0"},
		{"a small repository", 3.858432, "4MiB"},
		{"under a GiB", 1073.741823, "1024MiB"},
		{"a typical repository", 1516.244992, "1.4GiB"},
		{"the largest observed repository", 57086.832640, "53.2GiB"},
		{"just under a TiB", 1084479.24224, "1010.0GiB"},
		{"exactly a TiB", 1099511.627776, "1.0TiB"},
		{"beyond a TiB", 2199023.255552, "2.0TiB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatUsage(tt.mb); got != tt.want {
				t.Errorf("formatUsage(%v) = %q, want %q", tt.mb, got, tt.want)
			}
		})
	}
}

func TestFormatQuotaRendersWhatWasAskedFor(t *testing.T) {
	tests := []struct {
		name string
		mb   int64
		want string
	}{
		{"unset", 0, ""},
		{"negative", -1, ""},
		{"a hand-set 10 GB quota", 10000, "9.3GiB"},
		{"quotaGiB 100 round-trips", quotaMB(100), "100.0GiB"},
		{"quotaGiB 1 round-trips", quotaMB(1), "1.0GiB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatQuota(tt.mb); got != tt.want {
				t.Errorf("formatQuota(%d) = %q, want %q", tt.mb, got, tt.want)
			}
		})
	}
}
