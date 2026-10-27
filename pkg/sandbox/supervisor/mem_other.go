//go:build !linux && !darwin

package supervisor

import "errors"

// FreeMemory is unknown here, so Up skips the memory floor.
func FreeMemory() (uint64, error) {
	return 0, errors.New("supervisor: free memory is unknown on this platform")
}
