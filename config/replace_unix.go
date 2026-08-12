//go:build !windows

package config

import "os"

func replaceSettingsFile(source, destination string) error { return os.Rename(source, destination) }
