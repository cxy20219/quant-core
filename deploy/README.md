# 部署说明

目标服务器:NAS `cxy-nas`(内网 `<nas-host>`,用户 `<nas-user>`,密钥登录)。
部署形态:Docker 单容器,数据湖外挂宿主机目录,镜像只含二进制与注册表。

## 目录布局

```
/vol1/quant-core/
├── compose/compose.yaml     # docker compose 定义
├── etc/datasets.yaml        # 数据集注册表(容器内只读挂载)
├── data/lake/               # 数据湖(外挂,不打包进镜像)
│   ├── canonical/           # 规范化 Parquet 数据集
│   └── meta/manifest/       # 批次血缘
└── src/                     # 镜像构建上下文(仅构建时需要)
```

容器内视图:

| 容器路径 | 来源 | 说明 |
|---|---|---|
| `/app/quantd` | 镜像 | 主程序 |
| `/app/schemas/` | 镜像 | 注册表兜底副本 |
| `/data/lake` | `/vol1/quant-core/data/lake` | 数据湖(读写) |
| `/etc/quant-core` | `/vol1/quant-core/etc` | 注册表覆盖(只读) |

## 首次部署

```bash
# 1. 交叉编译(开发机)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bin/quantd-linux-amd64 ./cmd/quantd

# 2. 上传构建上下文(bin/quantd-linux-amd64 + schemas/datasets.yaml + deploy/Dockerfile.binary)
#    到 /vol1/quant-core/src/

# 3. 在服务器构建镜像
ssh <nas-user>@<nas-host> "cd /vol1/quant-core/src && docker build -f Dockerfile.binary -t quant-core:latest ."

# 4. 上传 compose 与注册表,启动
scp deploy/compose.yaml <nas-user>@<nas-host>:/vol1/quant-core/compose/compose.yaml
scp schemas/datasets.yaml <nas-user>@<nas-host>:/vol1/quant-core/etc/datasets.yaml
ssh <nas-user>@<nas-host> "cd /vol1/quant-core/compose && docker compose up -d"
```

无本机 Docker 时可用 `deploy/Dockerfile`(容器内编译):

```bash
ssh <nas-user>@<nas-host> "cd /vol1/quant-core/src && docker build -f Dockerfile -t quant-core:latest ."
```

## 升级

```bash
make build-linux                      # 交叉编译
# 上传 bin/quantd-linux-amd64 到 /vol1/quant-core/src/bin/
ssh <nas-user>@<nas-host> "cd /vol1/quant-core/src && docker build -f Dockerfile.binary -t quant-core:latest . && cd ../compose && docker compose up -d"
```

## 数据导入

导入在宿主机执行(容器只读挂载数据湖,不承担写入职责)。
下方命令需在服务器上使用与容器同版本的二进制:

```bash
# 旧湖迁移(示例:把旧数据挂到 /vol1/old-lake)
/vol1/quant-core/src/bin/quantd-linux-amd64 migrate \
  --src /vol1/old-lake --lake /vol1/quant-core/data/lake --jobs 8

# 增量导入(需有效 token)
export TUSHARE_TOKEN=xxxx
/vol1/quant-core/src/bin/quantd-linux-amd64 import \
  --source tushare --dataset stk_limit --start 20240101 --end 20240131 \
  --lake /vol1/quant-core/data/lake

# 行数核对
/vol1/quant-core/src/bin/quantd-linux-amd64 verify --lake /vol1/quant-core/data/lake
```

## 访问方式

```python
import tushare as ts

pro = ts.pro_api("any-token")
pro._DataApi__http_url = "http://<nas-host>:8000"
df = pro.daily(ts_code="600000.SH", start_date="20240102", end_date="20240110")
```

- 健康检查:`GET http://<nas-host>:8000/healthz`
- 鉴权:设置环境变量 `QUANTD_TOKENS`(逗号分隔)后按 tushare 的 `token` 字段校验;
  留空则匿名可访问,仅限可信内网。

## 运维

```bash
cd /vol1/quant-core/compose
docker compose ps          # 状态
docker compose logs -f     # 日志
docker compose restart     # 重启
docker compose down        # 停止
```

Docker daemon 配置要点(`/etc/docker/daemon.json`):`data-root=/vol1/docker`,
镜像加速 `docker.m.daocloud.io` / `docker.1ms.run`(默认 `docker.fnnas.com` 不可达)。
