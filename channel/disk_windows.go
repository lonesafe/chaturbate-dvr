//go:build windows

package channel

import "golang.org/x/sys/windows"

func availableDiskBytes(path string) (uint64, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free uint64
	if err := windows.GetDiskFreeSpaceEx(pointer, &free, nil, nil); err != nil {
		return 0, err
	}
	return free, nil
}
