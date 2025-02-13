//go:build !windows

package serframe

func requiresTermination(err error) bool {
	return false
}
