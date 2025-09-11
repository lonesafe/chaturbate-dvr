package config

import (
	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/urfave/cli/v2"
)

// New initializes a new Config struct with values from the CLI context.
func New(c *cli.Context) (*entity.Config, error) {
	return &entity.Config{
		Version:       c.App.Version,
		Username:      c.String("username"),
		AdminUsername: c.String("admin-username"),
		AdminPassword: c.String("admin-password"),
		Framerate:     c.Int("framerate"),
		Resolution:    c.Int("resolution"),
		Pattern:       c.String("pattern"),
		MaxDuration:   c.Int("max-duration"),
		MaxFilesize:   c.Int("max-filesize"),
		Port:          c.String("port"),
		Interval:      c.Int("interval"),
		Cookies:       c.String("cookies"),
		UserAgent:     c.String("user-agent"),
		Domain:        c.String("domain"),
		Socks5User:    c.String("socks5User"),
		Socks5Pwd:     c.String("socks5Pwd"),
		Socks5Url:     c.String("socks5Url"),
	}, nil
}
