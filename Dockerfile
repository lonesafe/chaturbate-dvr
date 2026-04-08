# 使用官方的Ubuntu基础镜像
FROM ubuntu:latest

# 创建应用程序目录
RUN mkdir -p /opt/videos

# 将本地二进制文件复制到容器中
COPY x64_linux_chaturbate-dvr /opt/

# 设置执行权限
RUN chmod +x /opt/x64_linux_chaturbate-dvr

RUN apt update
RUN apt install -y ffmpeg
# 暴露应用程序使用的端口
EXPOSE 8080

# 设置工作目录
WORKDIR /opt

# 设置容器启动命令
CMD ["/opt/x64_linux_chaturbate-dvr", "--domain", "https://zh-hans.chaturbate.com/"]
