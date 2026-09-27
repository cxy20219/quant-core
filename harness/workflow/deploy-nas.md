# NAS 部署流程(quant-core 数据服务)

## 适用场景

把 quant-core 数据服务发布到 NAS(`<nas-host>`),或更新已部署版本。
部署形态:Docker 单容器,镜像只含二进制与注册表,数据湖外挂宿主机目录。

## 前置条件

- 本机可无密码登录 NAS:`ssh <nas-user>@<nas-host>`(密钥已配置;若 host key 变更需先 `ssh-keygen -R <nas-host>`)。
- NAS 上 Docker 可用且已配置可达镜像源,见 `harness/references/nas-server.md`。
- 数据湖已迁移并完成对账(见 `harness/workflow/data-lake-migration.md`)。

## 关键步骤

1. **交叉编译**(Windows 开发机):

   ```powershell
   $env:CGO_ENABLED="0"; $env:GOOS="linux"; $env:GOARCH="amd64"
   go build -trimpath -ldflags="-s -w" -o bin/quantd-linux-amd64 ./cmd/quantd
   Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED
   ```

2. **上传构建上下文并构建镜像**:把 `bin/quantd-linux-amd64`、`schemas/datasets.yaml`、`deploy/Dockerfile.binary` 传到 `/vol1/quant-core/src/`,然后在服务器上:

   ```bash
   python harness/scripts/remote_exec.py "cd /vol1/quant-core/src && docker build -f Dockerfile.binary -t quant-core:latest ."
   ```

   上传文件可用 `harness/scripts/upload_lake.py`(单文件也可用 `scp`);
   远端命令统一走 `harness/scripts/remote_exec.py`(远程单测工具,避免 PowerShell 转义问题)。

3. **同步注册表**(注册表在容器内只读挂载,内容变化必须重传):

   ```bash
   # 上传 schemas/datasets.yaml 到 /vol1/quant-core/etc/datasets.yaml
   python harness/scripts/upload_lake.py schemas /vol1/quant-core/etc-staging
   ```

4. **上传数据湖**(首次或增量;脚本支持断点续传,按文件大小跳过已传):

   ```bash
   python harness/scripts/upload_lake.py "D:\quant-lake\canonical" "/vol1/quant-core/data/lake/canonical"
   ```

5. **启动/重启容器并核对**:

   ```bash
   python harness/scripts/remote_exec.py "cd /vol1/quant-core/compose && docker compose up -d && docker compose ps"
   python harness/scripts/remote_exec.py "/vol1/quant-core/src/bin/quantd-linux-amd64 verify --lake /vol1/quant-core/data/lake --registry /vol1/quant-core/etc/datasets.yaml"
   ```

6. **验收**(本机执行,覆盖接口、单位与延迟):

   ```bash
   python harness/scripts/accept_deploy.py --url http://<nas-host>:8000 --full
   ```

## 验证

- 成功信号:`accept_deploy.py` 输出 `ALL PASS`;健康检查返回 `"status":"ok"` 且 `bars_1m.partitions=550`、`bars_daily.partitions=27`。
- 服务器端 `verify` 输出各数据集 `OK` 且行数:`bars_1m=3950683458`、`bars_daily=16267551`、`adj_factor=38089668`、`corporate_actions=519`。
- 分钟线 vol 交叉校验:单日分钟 vol 合计(股)= 日线 vol(手)×100,diff 应为 0.0%。
- 常见失败信号:
  - `exec: "/app/quantd": permission denied` → 二进制上传后未加执行位;Dockerfile 已带 `chmod +x`,若是旧镜像需重建。
  - `docker build` 拉基础镜像超时 → 镜像源失效,见 `harness/references/nas-server.md`。
  - 容器起来但接口报 `unknown api_name` → 服务器上 `etc/datasets.yaml` 是旧版注册表。
  - 数据分区数偏少 → 数据湖未传完(用 upload 脚本重跑,它会跳过已传文件)。

## 回滚

- 保留上一版二进制:`/vol1/quant-core/src/bin/quantd-linux-amd64` 覆盖前先备份为 `.prev`,重建镜像即可回退。
- 数据湖只增不删;迁移事故用本地 `D:\quant-lake` 重新上传对应分区。

## 最近验证

2026-09-26:首次全量部署(39.5 亿行分钟数据 + 日线),`accept_deploy.py --full` 全过。
2026-09-27:更新镜像(分钟回测引擎 + 对拍修复),重建容器后 `accept_deploy.py --full` 全过;
NAS 端分钟回测(POST `frequency=1m`)与本地 CLI 结果逐位一致(1,000,058.616244)。
