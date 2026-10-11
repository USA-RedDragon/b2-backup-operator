package main

import "testing"

func TestParseNamespacedName(t *testing.T) {
	tests := []struct {
		name, in, defaultNS, wantNS, wantName string
		wantErr                               bool
	}{
		{
			name:      "bare name resolves in the operator namespace",
			in:        "borgbase-api",
			defaultNS: "b2-backup-operator-system",
			wantNS:    "b2-backup-operator-system",
			wantName:  "borgbase-api",
		},
		{
			name:      "explicit namespace wins over the default",
			in:        "other-ns/other-token",
			defaultNS: "should-be-ignored",
			wantNS:    "other-ns",
			wantName:  "other-token",
		},
		{
			name:      "bare name with no default is an error",
			in:        "some-secret",
			defaultNS: "",
			wantErr:   true,
		},
		{name: "empty namespace is an error", in: "/name", defaultNS: "ns", wantErr: true},
		{name: "empty name is an error", in: "ns/", defaultNS: "ns", wantErr: true},
		{name: "empty value is an error", in: "", defaultNS: "ns", wantErr: true},
		{name: "empty value with no default is an error", in: "", defaultNS: "", wantErr: true},
		{name: "only a separator is an error", in: "/", defaultNS: "ns", wantErr: true},
		{name: "three segments are an error", in: "a/b/c", defaultNS: "ns", wantErr: true},
		{name: "trailing separator is an error", in: "ns/name/", defaultNS: "ns", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseNamespacedName(tt.in, tt.defaultNS)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseNamespacedName(%q, %q) = %v, want an error", tt.in, tt.defaultNS, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseNamespacedName(%q, %q) error = %v", tt.in, tt.defaultNS, err)
			}
			if got.Namespace != tt.wantNS || got.Name != tt.wantName {
				t.Errorf("parseNamespacedName(%q, %q) = %s/%s, want %s/%s",
					tt.in, tt.defaultNS, got.Namespace, got.Name, tt.wantNS, tt.wantName)
			}
		})
	}
}
