# Chaturbate 录播工具
## 翻译自 https://github.com/teacat/chaturbate-dvr
### 增加三个启动命令：--socks5Url=socks5地址例如：127.0.0.1:1070 --socks5Pwd=代理的密码 --socks5User=代理的密码
# 命令行选项

可用选项：

```
--username value, -u value      要录制的频道用户名
--admin-username value          Web 界面登录用户名（可选）
--admin-password value          Web 界面登录密码（可选）
--framerate value               期望的帧率（FPS）（默认：30）
--resolution value              期望的分辨率（例如：1080 表示 1080p）（默认：1080）
--pattern value                 录制视频的命名模板（默认："videos/{{.Username}}_{{.Year}}-{{.Month}}-{{.Day}}_{{.Hour}}-{{.Minute}}-{{.Second}}{{if .Sequence}}_{{.Sequence}}{{end}}"）
--max-duration value            每 N 分钟分割视频（设为 '0' 以禁用）（默认：0）
--max-filesize value            每 N MB 分割视频（设为 '0' 以禁用）（默认：0）
--port value, -p value          Web 界面和 API 的端口（默认："8080"）
--interval value                每 N 分钟检查频道是否在线（默认：1）
--cookies value                 请求中使用的 Cookie（格式：key=value; key2=value2）
--user-agent value              请求使用的自定义 User-Agent
--domain value                  使用的 Chaturbate 域名（默认："https://chaturbate.global/"）
--help, -h                      显示帮助信息
--version, -v                   打印版本信息
--socks5Url                     socks5地址例如：127.0.0.1：0:1070
--socks5Pwd                     代理的密码
--socks5User                    代理的密码
```
![marionxue's github stats](https://github-readme-stats.vercel.app/api?username=lonesafe&theme=radical) 
