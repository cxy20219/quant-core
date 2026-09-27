# syntax=docker/dockerfile:1
#
# quant-core 镜像(容器内编译版)。无需本机 Go 环境,适合直接在服务器上构建。
# 目标平台可用 --platform 指定;依赖通过 GOPROXY 拉取。

FROM golang:1.26-alpine AS builder
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY} CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY schemas/ schemas/
RUN go build -trimpath -ldflags="-s -w" -o /out/quantd ./cmd/quantd

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata python3 py3-numpy py3-pandas
ENV TZ=Asia/Shanghai \
    PYTHONIOENCODING=utf-8 \
    PYTHONUTF8=1
WORKDIR /app
COPY --from=builder /out/quantd /app/quantd
COPY schemas/ /app/schemas/
COPY python/ /app/python/
EXPOSE 8000
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8000/healthz >/dev/null 2>&1 || exit 1
ENTRYPOINT ["/app/quantd"]
CMD ["serve", "--lake", "/data/lake", "--registry", "/app/schemas/datasets.yaml", "--listen", ":8000"]
