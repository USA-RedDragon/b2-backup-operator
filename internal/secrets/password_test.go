package secrets

import (
	"strings"
	"testing"
)

func TestGeneratePassword(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for range 1000 {
		got, err := GeneratePassword()
		if err != nil {
			t.Fatalf("GeneratePassword() error = %v", err)
		}
		if len(got) != passwordLength {
			t.Fatalf("GeneratePassword() length = %d, want %d", len(got), passwordLength)
		}
		if strings.ContainsFunc(got, func(r rune) bool { return !strings.ContainsRune(alphabet, r) }) {
			t.Fatalf("GeneratePassword() = %q, contains a character outside the alphabet", got)
		}
		if seen[got] {
			t.Fatalf("GeneratePassword() returned a duplicate: %q", got)
		}
		seen[got] = true
	}
}

func TestAlphabetNeedsNoEscaping(t *testing.T) {
	for _, r := range alphabet {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !isAlnum {
			t.Errorf("alphabet contains non-alphanumeric character %q", r)
		}
	}
}
