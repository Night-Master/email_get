FROM --platform=linux/amd64 mirror.ccs.tencentyun.com/library/python:3.12-slim-bookworm

# 更换 Debian 源为阿里云镜像，并安装 TLS 证书用于连接邮件服务器。
RUN sed -i 's/deb.debian.org/mirrors.aliyun.com/g' /etc/apt/sources.list.d/debian.sources && \
    sed -i 's/security.debian.org/mirrors.aliyun.com/g' /etc/apt/sources.list.d/debian.sources && \
    apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY release/email_get_linux_amd64 /app/email_get_linux_amd64

RUN chmod +x /app/email_get_linux_amd64 && mkdir -p /app/data

VOLUME ["/app/data"]

EXPOSE 80

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:80/api/health', timeout=3).read()" || exit 1

CMD ["/app/email_get_linux_amd64", "--listen", "0.0.0.0:80", "--data-dir", "/app/data"]
