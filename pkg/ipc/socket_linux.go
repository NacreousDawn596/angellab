package ipc

import "os"

// removeSocket removes a Unix socket file before binding.
// It does nothing if the path does not exist.
func removeSocket(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
