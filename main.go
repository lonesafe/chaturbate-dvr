package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	nethttp "net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/teacat/chaturbate-dvr/config"
	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/manager"
	"github.com/teacat/chaturbate-dvr/router"
	"github.com/teacat/chaturbate-dvr/server"
	"github.com/urfave/cli/v2"
)

const logo = `
 ██████╗██╗  ██╗ █████╗ ████████╗██╗   ██╗██████╗ ██████╗  █████╗ ████████╗███████╗
██╔════╝██║  ██║██╔══██╗╚══██╔══╝██║   ██║██╔══██╗██╔══██╗██╔══██╗╚══██╔══╝██╔════╝
██║     ███████║███████║   ██║   ██║   ██║██████╔╝██████╔╝███████║   ██║   █████╗
██║     ██╔══██║██╔══██║   ██║   ██║   ██║██╔══██╗██╔══██╗██╔══██║   ██║   ██╔══╝
╚██████╗██║  ██║██║  ██║   ██║   ╚██████╔╝██║  ██║██████╔╝██║  ██║   ██║   ███████╗
 ╚═════╝╚═╝  ╚═╝╚═╝  ╚═╝   ╚═╝    ╚═════╝ ╚═╝  ╚═╝╚═════╝ ╚═╝  ╚═╝   ╚═╝   ╚══════╝
██████╗ ██╗   ██╗██████╗
██╔══██╗██║   ██║██╔══██╗
██║  ██║██║   ██║██████╔╝
██║  ██║╚██╗ ██╔╝██╔══██╗
██████╔╝ ╚████╔╝ ██║  ██║
╚═════╝   ╚═══╝  ╚═╝  ╚═╝`

var version = "dev"

func main() {
	app := &cli.App{
		Name:    "chaturbate-dvr",
		Version: version,
		Usage:   "Record your favorite Chaturbate streams automatically. 😎🫵",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "username",
				Aliases: []string{"u"},
				Usage:   "The username of the channel to record",
				Value:   "",
			},
			&cli.StringFlag{
				Name:  "admin-username",
				Usage: "Username for web authentication (optional)",
				Value: "",
			},
			&cli.StringFlag{
				Name:  "admin-password",
				Usage: "Password for web authentication (optional)",
				Value: "",
			},
			&cli.IntFlag{
				Name:  "framerate",
				Usage: "Desired framerate (FPS)",
				Value: 30,
			},
			&cli.IntFlag{
				Name:  "resolution",
				Usage: "Desired resolution (e.g., 1080 for 1080p)",
				Value: 1080,
			},
			&cli.StringFlag{
				Name:  "pattern",
				Usage: "Template for naming recorded videos",
				Value: "videos/{{.Username}}_{{.Year}}-{{.Month}}-{{.Day}}_{{.Hour}}-{{.Minute}}-{{.Second}}{{if .Sequence}}_{{.Sequence}}{{end}}",
			},
			&cli.IntFlag{
				Name:  "max-duration",
				Usage: "Split video into segments every N minutes ('0' to disable)",
				Value: 0,
			},
			&cli.IntFlag{
				Name:  "max-filesize",
				Usage: "Split video into segments every N MB ('0' to disable)",
				Value: 0,
			},
			&cli.StringFlag{
				Name:    "port",
				Aliases: []string{"p"},
				Usage:   "Port for the web interface and API",
				Value:   "8080",
			},
			&cli.IntFlag{
				Name:  "interval",
				Usage: "Check if the channel is online every N minutes",
				Value: 1,
			},
			&cli.StringFlag{
				Name:  "cookies",
				Usage: "Cookies to use in the request (format: key=value; key2=value2)",
				Value: "",
			},
			&cli.StringFlag{
				Name:  "user-agent",
				Usage: "Custom User-Agent for the request",
				Value: "",
			},
			&cli.StringFlag{
				Name:  "domain",
				Usage: "Chaturbate domain to use",
				Value: "https://chaturbate.com/",
			},
			&cli.BoolFlag{
				Name:  "compress",
				Usage: "Compress recorded files (.ts or .mp4) to .mkv using ffmpeg after recording",
				Value: false,
			},
			&cli.StringFlag{
				Name:    "output-dir",
				Usage:   "Directory to move completed recordings to (empty = keep in place)",
				EnvVars: []string{"OUTPUT_DIR"},
				Value:   "",
			},
			&cli.BoolFlag{
				Name:    "per-model-folder",
				Usage:   "Create a subdirectory per model inside --output-dir",
				EnvVars: []string{"PER_MODEL_FOLDER"},
				Value:   false,
			},
			&cli.IntFlag{Name: "segment-workers", Usage: "Maximum parallel segment downloads per track", Value: 6},
			&cli.IntFlag{Name: "pending-seconds", Usage: "Maximum unmatched A/V timeline buffer", Value: 60},
			&cli.IntFlag{Name: "max-pending-mb", Usage: "Maximum unmatched A/V memory per track", Value: 512},
			&cli.IntFlag{Name: "min-free-disk-mb", Usage: "Stop recording below this free disk space", Value: 512},
			&cli.IntFlag{Name: "sync-seconds", Usage: "Maximum interval between recording fsync calls", Value: 3},
			&cli.IntFlag{Name: "sync-fragments", Usage: "Maximum fragments between recording fsync calls", Value: 10},
			&cli.IntFlag{Name: "max-text-mb", Usage: "Maximum API or playlist response size", Value: 4},
			&cli.IntFlag{Name: "max-segment-mb", Usage: "Maximum init or media segment size", Value: 64},
			&cli.IntFlag{Name: "http-timeout-seconds", Usage: "API and playlist request timeout", Value: 30},
			&cli.IntFlag{Name: "segment-timeout-seconds", Usage: "Media segment request timeout", Value: 120},
		},
		Action: start,
	}
	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

func start(c *cli.Context) error {
	fmt.Println(logo)

	var err error
	server.Config, err = config.New(c)
	if err != nil {
		return fmt.Errorf("初始化配置：%w", err)
	}
	server.Manager, err = manager.New()
	if err != nil {
		return fmt.Errorf("初始化频道管理器：%w", err)
	}
	shutdownCtx, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()

	// init web interface if username is not provided
	if server.Config.Username == "" {
		fmt.Printf("👋 Web 管理界面：http://localhost:%s\n\n\n", c.String("port"))

		if err := server.Manager.LoadConfig(); err != nil {
			return fmt.Errorf("加载频道配置：%w", err)
		}

		httpServer := &nethttp.Server{Addr: ":" + c.String("port"), Handler: router.SetupRouter()}
		errCh := make(chan error, 1)
		go func() { errCh <- httpServer.ListenAndServe() }()
		var serveErr error
		select {
		case err := <-errCh:
			if !errors.Is(err, nethttp.ErrServerClosed) {
				serveErr = err
			}
		case <-shutdownCtx.Done():
		}
		graceCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = httpServer.Shutdown(graceCtx)
		shutdownErr := server.Manager.Shutdown(graceCtx)
		if serveErr != nil {
			return serveErr
		}
		return shutdownErr
	}

	// else create a channel with the provided username
	if err := server.Manager.CreateChannel(&entity.ChannelConfig{
		IsPaused:    false,
		Username:    c.String("username"),
		Framerate:   c.Int("framerate"),
		Resolution:  c.Int("resolution"),
		Pattern:     c.String("pattern"),
		MaxDuration: c.Int("max-duration"),
		MaxFilesize: c.Int("max-filesize"),
		Compress:    c.Bool("compress"),
	}, false); err != nil {
		return fmt.Errorf("创建频道：%w", err)
	}

	<-shutdownCtx.Done()
	graceCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return server.Manager.Shutdown(graceCtx)
}
