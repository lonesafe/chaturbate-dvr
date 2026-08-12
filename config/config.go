package config

import (
	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/urfave/cli/v2"
)

const defaultPairToleranceMS = 1000

// New initializes a new Config struct with values from the CLI context.
func New(c *cli.Context) (*entity.Config, error) {
	conf := &entity.Config{
		Version:               c.App.Version,
		Username:              c.String("username"),
		AdminUsername:         c.String("admin-username"),
		AdminPassword:         c.String("admin-password"),
		Framerate:             c.Int("framerate"),
		Resolution:            c.Int("resolution"),
		Pattern:               c.String("pattern"),
		MaxDuration:           c.Int("max-duration"),
		MaxFilesize:           c.Int("max-filesize"),
		Compress:              c.Bool("compress"),
		Port:                  c.String("port"),
		Interval:              c.Int("interval"),
		Cookies:               c.String("cookies"),
		UserAgent:             c.String("user-agent"),
		Domain:                c.String("domain"),
		OutputDir:             c.String("output-dir"),
		PerModelFolder:        c.Bool("per-model-folder"),
		SegmentWorkers:        max(c.Int("segment-workers"), 1),
		PendingSeconds:        max(c.Int("pending-seconds"), 1),
		MaxPendingMB:          max(c.Int("max-pending-mb"), 1),
		MinFreeDiskMB:         max(c.Int("min-free-disk-mb"), 0),
		SyncSeconds:           max(c.Int("sync-seconds"), 1),
		SyncFragments:         max(c.Int("sync-fragments"), 1),
		MaxTextMB:             max(c.Int("max-text-mb"), 1),
		MaxSegmentMB:          max(c.Int("max-segment-mb"), 1),
		HTTPTimeoutSeconds:    max(c.Int("http-timeout-seconds"), 1),
		SegmentTimeoutSeconds: max(c.Int("segment-timeout-seconds"), 1),
		PairToleranceMS:       defaultPairToleranceMS,
	}
	settings, err := LoadGlobalSettings()
	if err != nil {
		return nil, err
	}
	if settings != nil {
		if !c.IsSet("cookies") {
			conf.Cookies = settings.Cookies
		}
		if !c.IsSet("user-agent") {
			conf.UserAgent = settings.UserAgent
		}
		if !c.IsSet("framerate") && settings.Framerate > 0 {
			conf.Framerate = settings.Framerate
		}
		if !c.IsSet("resolution") && settings.Resolution > 0 {
			conf.Resolution = settings.Resolution
		}
		if !c.IsSet("pattern") && settings.Pattern != "" {
			conf.Pattern = settings.Pattern
		}
		if !c.IsSet("max-duration") {
			conf.MaxDuration = settings.MaxDuration
		}
		if !c.IsSet("max-filesize") {
			conf.MaxFilesize = settings.MaxFilesize
		}
		if !c.IsSet("compress") {
			conf.Compress = settings.Compress
		}
		if settings.PairToleranceMS > 0 {
			conf.PairToleranceMS = settings.PairToleranceMS
		}
	}
	return conf, nil
}
