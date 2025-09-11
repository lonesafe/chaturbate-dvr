package main

import (
	"fmt"
	"log"
	"os"

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

func main() {
	app := &cli.App{
		Name:    "chaturbate-dvr",
		Version: "2.0.3",
		Usage:   "自动录制您最喜爱的Chaturbate直播。 😎🫵",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "username",
				Aliases: []string{"u"},
				Usage:   "要录制的频道的用户名",
				Value:   "",
			},
			&cli.StringFlag{
				Name:  "admin-username",
				Usage: "web身份验证的用户名（可选）",
				Value: "",
			},
			&cli.StringFlag{
				Name:  "admin-password",
				Usage: "web身份验证密码（可选）",
				Value: "",
			},
			&cli.IntFlag{
				Name:  "framerate",
				Usage: "期望帧率（FPS）",
				Value: 60,
			},
			&cli.IntFlag{
				Name:  "resolution",
				Usage: "期望录制的分辨路 (例如： 1080 对应 1080p)",
				Value: 2160,
			},
			&cli.StringFlag{
				Name:  "pattern",
				Usage: "视频文件存储方式",
				Value: "videos/{{.Username}}/{{.Year}}-{{.Month}}-{{.Day}}_{{.Hour}}-{{.Minute}}-{{.Second}}{{if .Sequence}}_{{.Sequence}}{{end}}",
			},
			&cli.IntFlag{
				Name:  "max-duration",
				Usage: "每N分钟将视频分割成片段 ('0' 为关闭)",
				Value: 0,
			},
			&cli.IntFlag{
				Name:  "max-filesize",
				Usage: "每N MB将视频分割成片段 ('0' 为关闭)",
				Value: 0,
			},
			&cli.StringFlag{
				Name:    "port",
				Aliases: []string{"p"},
				Usage:   "web界面和API的端口",
				Value:   "8080",
			},
			&cli.IntFlag{
				Name:  "interval",
				Usage: "每N分钟检查一次频道是否在线",
				Value: 1,
			},
			&cli.StringFlag{
				Name:  "cookies",
				Usage: "请求中使用的Cookie (格式: key=value; key2=value2)",
				Value: "",
			},
			&cli.StringFlag{
				Name:  "user-agent",
				Usage: "请求中使用的 User-Agent",
				Value: "",
			},
			&cli.StringFlag{
				Name:  "domain",
				Usage: "Chaturbate 的地址",
				Value: "https://chaturbate.com/",
			},
			&cli.StringFlag{
				Name:  "socks5User",
				Usage: "socks5用户名（如果有）",
				Value: "",
			},
			&cli.StringFlag{
				Name:  "socks5Pwd",
				Usage: "socks5密码（如果有）",
				Value: "",
			},
			&cli.StringFlag{
				Name:  "socks5Url",
				Usage: "socks5地址（非必填，例：127.0.0.1:1070）",
				Value: "",
			},
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
		return fmt.Errorf("new config: %w", err)
	}
	server.Manager, err = manager.New()
	if err != nil {
		return fmt.Errorf("new manager: %w", err)
	}

	// init web interface if username is not provided
	if server.Config.Username == "" {
		fmt.Printf("👋 访问 http://localhost:%s 使用web控制台\n\n\n", c.String("port"))

		if err := server.Manager.LoadConfig(); err != nil {
			return fmt.Errorf("加载配置: %w", err)
		}

		return router.SetupRouter().Run(":" + c.String("port"))
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
	}, false); err != nil {
		return fmt.Errorf("创建频道: %w", err)
	}

	// block forever
	select {}
}
