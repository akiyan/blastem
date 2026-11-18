//go:build !linux && !darwin && !freebsd

package session

import (
	"fmt"
	"os"
)

func lockProfile(path string) (string, *os.File, error) {
	return "", nil, fmt.Errorf("persistent save profiles are not implemented on this platform")
}
