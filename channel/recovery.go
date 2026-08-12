package channel

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RecoverInterruptedRecordings finalizes playable realtime files left by a
// crash and quarantines files whose fragmented MP4 structure is incomplete.
func RecoverInterruptedRecordings(root string) error {
	return RecoverInterruptedRecordingsBefore(root, time.Now())
}

func RecoverInterruptedRecordingsBefore(root string, cutoff time.Time) error {
	if root == "" {
		return nil
	}
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil
	}
	var recoveryErrors []error
	walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			recoveryErrors = append(recoveryErrors, walkErr)
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			// Session archives can contain hundreds of thousands of immutable
			// part files. They are not realtime working files, and descending
			// into them on every startup needlessly fills the NAS dentry cache.
			if path != root && strings.HasSuffix(entry.Name(), ".session") {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			recoveryErrors = append(recoveryErrors, err)
			return nil
		}
		if info.ModTime().After(cutoff) {
			return nil
		}
		if strings.HasPrefix(entry.Name(), ".compress-") || strings.HasPrefix(entry.Name(), ".mux-") || strings.HasPrefix(entry.Name(), ".finalize-") {
			if err := os.Remove(path); err != nil {
				recoveryErrors = append(recoveryErrors, err)
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".recording.mp4") {
			return nil
		}
		finalPath := strings.TrimSuffix(path, ".recording.mp4") + ".mp4"
		if validateErr := validateInterruptedMP4(path); validateErr == nil {
			if _, err := moveFileUnique(path, finalPath); err != nil {
				recoveryErrors = append(recoveryErrors, fmt.Errorf("恢复 %s：%w", path, err))
			}
			return nil
		}
		_, err = moveFileUnique(path, path+".corrupt")
		if err != nil {
			recoveryErrors = append(recoveryErrors, err)
		}
		return nil
	})
	if walkErr != nil {
		recoveryErrors = append(recoveryErrors, walkErr)
	}
	return errors.Join(recoveryErrors...)
}
