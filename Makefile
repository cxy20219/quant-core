# quant-core 部署目标:NAS 服务器(地址与用户从环境变量读取,见 .env)
#
# 用法:
#   make build-linux          # 交叉编译 linux/amd64 二进制
#   make image                # 构建 Docker 镜像
#   make deploy               # 上传镜像与配置并启动
#   make migrate-upload       # 上传数据湖(大,按需执行)
#   make status / make logs   # 远端状态

SHELL := /bin/bash
# 先 export NAS_USER/NAS_ADDR(或写入 .env 后 source),例如:
#   export NAS_USER=<nas-user> NAS_ADDR=<nas-host>
NAS_USER ?=
NAS_ADDR ?=
NAS_HOST := $(NAS_USER)@$(NAS_ADDR)
NAS_ROOT ?= /vol1/quant-core
BINARY := bin/quantd-linux-amd64
IMAGE := quant-core:latest
SSH := ssh -o BatchMode=yes $(NAS_HOST)
SCP := scp -o BatchMode=yes

.PHONY: build build-linux test image save deploy migrate-upload status logs stop clean

build:
	go build -o bin/quantd.exe ./cmd/quantd

build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/quantd

test:
	go test ./internal/... -timeout 300s

image: build-linux
	docker build -f deploy/Dockerfile.binary -t $(IMAGE) .

# 无 Docker Desktop 时:在服务器上直接构建镜像(容器内编译)
image-remote:
	$(SSH) "mkdir -p $(NAS_ROOT)/src $(NAS_ROOT)/etc $(NAS_ROOT)/data"
	rsync -az --delete --exclude=.git --exclude=lake --exclude=lake-test --exclude=bin \
		./ $(NAS_HOST):$(NAS_ROOT)/src/
	$(SSH) "cd $(NAS_ROOT)/src && docker build -f deploy/Dockerfile -t $(IMAGE) ."

# 本地构建镜像后导出、上传、加载
save: image
	docker save $(IMAGE) -o bin/quant-core-image.tar
	$(SCP) bin/quant-core-image.tar $(NAS_HOST):$(NAS_ROOT)/
	$(SSH) "docker load -i $(NAS_ROOT)/quant-core-image.tar && rm -f $(NAS_ROOT)/quant-core-image.tar"

deploy:
	$(SSH) "mkdir -p $(NAS_ROOT)/etc $(NAS_ROOT)/data $(NAS_ROOT)/compose"
	$(SCP) deploy/compose.yaml $(NAS_HOST):$(NAS_ROOT)/compose/compose.yaml
	$(SCP) schemas/datasets.yaml $(NAS_HOST):$(NAS_ROOT)/etc/datasets.yaml
	$(SSH) "cd $(NAS_ROOT)/compose && docker compose up -d && docker compose ps"

# 上传数据湖(先本地迁移产出;带 --delete 保持远端与本地一致,慎用)
migrate-upload:
	rsync -az --info=progress2 lake/ $(NAS_HOST):$(NAS_ROOT)/data/lake/

status:
	$(SSH) "cd $(NAS_ROOT)/compose && docker compose ps; curl -s http://127.0.0.1:8000/healthz | head -c 400; echo"

logs:
	$(SSH) "cd $(NAS_ROOT)/compose && docker compose logs --tail=100 -f"

stop:
	$(SSH) "cd $(NAS_ROOT)/compose && docker compose down"

clean:
	rm -rf bin lake-test
