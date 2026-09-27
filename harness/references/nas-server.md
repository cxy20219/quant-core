# NAS 服务器环境(部署目标)

> 本仓库为公开仓库,内网地址与用户名一律以占位符表示:`<nas-host>` / `<nas-user>`。
> 真实值放在项目根 `.env`(已 gitignore):`QUANT_NAS_HOST`、`QUANT_NAS_USER`;
> `harness/scripts/*` 会优先读环境变量,其次读 `.env`。

## 基本信息

- 主机名 `cxy-nas`,内网 `<nas-host>:22`,用户 `<nas-user>`(密钥登录)。
- 系统为飞牛 OS(内核 `*.trim`,卷名为 `trim_*`),x86_64。
- 数据盘:`/vol1`(826 GB,部署根目录 `/vol1/quant-core`);系统盘 `/` 126 GB。
- Docker:28.5.2 + Compose v2.40.3,`data-root=/vol1/docker`(daemon.json)。

## 已完成的服务器配置

- `<nas-user>` 已加入 `docker` 组(免 sudo 管理容器);Docker 服务开机自启。
- **镜像源**:飞牛默认 `docker.fnnas.com` 不可达,已改为
  `["https://docker.m.daocloud.io", "https://docker.1ms.run"]`
  (改 `/etc/docker/daemon.json` 后 `systemctl restart docker`;改前备份原文件)。
- `/vol1/quant-core/` 布局:
  - `compose/compose.yaml`(部署定义)
  - `etc/datasets.yaml`(注册表,容器只读挂载)
  - `data/lake/`(数据湖 canonical + meta/manifest)
  - `src/`(镜像构建上下文:二进制 + 注册表 + Dockerfile)

## 网络性能(2026-09-26 实测)

- `scp`/SFTP 上传约 60~72 MB/s(千兆局域网,瓶颈在链路而非磁盘)。
- NAS 本地写入 `dd oflag=direct` 约 786 MB/s。
- 49 GB 数据湖上传耗时约 11.4 分钟(6 并发 SFTP,断点续传脚本)。

## 辅助脚本

- 远程命令:`python harness/scripts/remote_exec.py "<cmd>"`(支持 `--sudo`,密码从环境变量 `QUANT_SUDO_PASS` 读取,不落盘)。
- 目录上传:`python harness/scripts/upload_lake.py <local> <remote> [--workers 6]`(按文件大小跳过已传,支持重跑)。
- 首次配置登录时若 host key 变更:`ssh-keygen -R <nas-host>` 后重新连接。

## 注意

- 不要把密码写进仓库或脚本;`QUANT_SUDO_PASS` 只在本地环境变量中设置。
- 该服务器此前部署过旧项目(`/data/quant-data`,Python venv),当前系统已重置,
  只有 `/vol1/quant-core` 属于本项目。
