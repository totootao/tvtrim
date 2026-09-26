# tvtrim 镜像:Go 编译 + ffmpeg-trim(静态裁剪版 ffmpeg)双阶段打包。
#
# 之所以把 ffmpeg 一起打包进去:tvtrim 运行时默认会自动下载 ffmpeg-trim,
# 在容器里每次运行都下载既慢又依赖网络;预置后直接可用,离线也能跑。

# 阶段一:编译 tvtrim。CGO 关掉,产物是纯静态二进制。
FROM golang:1.21-alpine AS builder

WORKDIR /src
COPY go.mod ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/tvtrim ./cmd/tvtrim

# 阶段二:按目标架构取对应的 ffmpeg-trim,并做大小校验(防止代理返回错误页)。
# 全程不装任何包:alpine 自带 busybox wget,完成后该阶段只留下二进制。
FROM alpine:3.20 AS ffmpeg

ARG TARGETARCH
ARG FFMPEG_BASE_URL=https://raw.githubusercontent.com/totootao/ffmpeg-trim/main
ARG FFMPEG_AMD64_SIZE=2994224
ARG FFMPEG_ARM64_SIZE=2690944

RUN case "${TARGETARCH}" in \
      amd64)  name=ffmpeg;          want=${FFMPEG_AMD64_SIZE} ;; \
      arm64)  name=ffmpeg-aarch64;  want=${FFMPEG_ARM64_SIZE} ;; \
      *) echo "ffmpeg-trim 未提供 ${TARGETARCH} 架构的构建" >&2; exit 1 ;; \
    esac && \
    mkdir -p /ffbin && \
    wget -q -O /ffbin/ffmpeg "${FFMPEG_BASE_URL}/${name}" && \
    got=$(wc -c < /ffbin/ffmpeg) && \
    if [ "${got}" != "${want}" ]; then \
      echo "ffmpeg-trim 大小异常: 期望 ${want}, 实际 ${got}" >&2; exit 1; \
    fi && \
    chmod 0755 /ffbin/ffmpeg

# 阶段三:运行镜像。ffmpeg-trim 静态链接无需 glibc,alpine 足够。
# 证书从 builder 镜像里带过来,同样不需要 apk add —— 这样整个构建过程不依赖
# 任何软件源可达性,只依赖基础镜像与 ffmpeg-trim 下载地址。
FROM alpine:3.20

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=ffmpeg  /ffbin/ffmpeg /usr/local/bin/ffmpeg
COPY --from=builder /out/tvtrim   /usr/local/bin/tvtrim

# 直接指定 ffmpeg 路径,跳过运行时查找与自动下载。
ENV TVTRIM_FFMPEG=/usr/local/bin/ffmpeg

WORKDIR /media
ENTRYPOINT ["/usr/local/bin/tvtrim"]
CMD ["-h"]

LABEL org.opencontainers.image.title="tvtrim" \
      org.opencontainers.image.description="电视剧剧集批量去头去尾(基于 ffmpeg-trim,零重编码)" \
      org.opencontainers.image.source="https://github.com/totootao/tvtrim" \
      org.opencontainers.image.url="https://github.com/totootao/tvtrim" \
      org.opencontainers.image.licenses="MIT"
