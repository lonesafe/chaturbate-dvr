//go:build !windows

package manager

import "os"

func replaceFile(source, destination string) error {
	return os.Rename(source, destination)
}
