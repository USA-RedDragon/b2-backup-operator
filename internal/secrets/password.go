// Package secrets generates the credentials the operator stores for a repository.
package secrets

import (
	"crypto/rand"
	"fmt"
)

const passwordLength = 128

const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// GeneratePassword returns a new random restic password.
func GeneratePassword() (string, error) {
	buf := make([]byte, passwordLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating password: %w", err)
	}

	const maxUnbiased = 256 - (256 % len(alphabet))
	out := make([]byte, 0, passwordLength)
	for len(out) < passwordLength {
		for _, b := range buf {
			if int(b) >= maxUnbiased {
				continue
			}
			out = append(out, alphabet[int(b)%len(alphabet)])
			if len(out) == passwordLength {
				break
			}
		}
		if len(out) < passwordLength {
			if _, err := rand.Read(buf); err != nil {
				return "", fmt.Errorf("generating password: %w", err)
			}
		}
	}
	return string(out), nil
}
