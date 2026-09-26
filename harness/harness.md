# 项目上下文

## 项目定位

- quant-core 提供两件事:**量化数据服务**(纯 Go 数据湖 + tushare 兼容 HTTP 接口)与**量化回测服务**(Go 引擎 + Python 策略子进程,规划中)。
- 数据服务已上线:NAS `<nas-user>@<nas-host>:8000`,Docker 部署,数据湖外挂 `/vol1/quant-core/data/lake`。
- 回测服务未实现;数据服务是当前唯一的生产组件。

## 关键约束(容易猜错)

- **纯 Go 技术栈**:查询引擎是自研的(parquet-go + 分区剪枝 + 行组统计 + 元数据缓存),没有 DuckDB / CGO / 外部查询引擎。不要建议引入 CGO 或改写为 Python。
- **注册表是单一权威**:所有数据集定义在 `schemas/datasets.yaml`;字段名、类型、分区、主键、写入参数都从这里读,禁止在代码里硬编码字段名。
- **字段名与单位对齐 tushare**:内部字段直接用 tushare 名(ts_code/trade_date/vol/amount/...);vol=手、amount=千元、*_share=万股、*_mv=万元。分钟线 API 输出时由接口层换算回 tushare `stk_mins` 口径(vol=股、amount=元)。
- **字符串列必须写 PLAIN 编码**:parquet-go 默认的 DeltaLengthByteArray 不被 pyarrow/duckdb 的批量解码路径支持;写入器已强制 `DefaultEncodingFor(parquet.ByteArray, &parquet.Plain)`。
- **行组大小 8192 行**:配合按 (ts_code, 时间) 排序的文件布局,单代码查询可剪枝到 1~2 个行组;行组太大或排序打乱会让剪枝失效。
- **数据湖写入必须经 `lake.PartWriter`**:它负责排序布局约定、行组大小、PLAIN 编码、Zstd 压缩和 part 编号续写;不要绕过它直接写 parquet。
- **数据湖 39.5 亿行分钟数据**(45.8 GB,550 个 part 文件,2000-2026 年);任何全表聚合都会拖垮服务,全市场单日横截面查询约 1.8 秒是已知代价。
- 分钟线文件名有历史遗留差异(部分 2026 文件来自不同导出管线),迁移前必须先跑 `harness/scripts/normalize_source.py` 检查行组 ordinal 溢出。

## 关键目录与职责

- `cmd/quantd/`:主程序,子命令 `serve` / `migrate` / `import` / `verify` / `apis`。
- `internal/schema/`:数据集注册表与逻辑值类型(单一权威)。
- `internal/lake/`:数据湖布局、PartWriter、批次 manifest。
- `internal/pq/`:parquet-go 桥接(schema 构建、值转换、编码约定)。
- `internal/query/`:纯 Go 扫描引擎(分区剪枝 → 行组跳过 → 列投影 → 元数据缓存)。
- `internal/tsapi/`:tushare 兼容协议层(api_name 注册表、参数→谓词、单位换算)。
- `internal/ingest/`:旧湖迁移与 Tushare 导入器。
- `internal/source/tushare/`:Tushare Pro HTTP 客户端。
- `schemas/datasets.yaml`:数据集注册表。
- `deploy/`:Dockerfile(二进制版/容器内构建版)、compose、部署说明。
- `harness/`:项目经验、流程与脚本(见根目录 `AGENTS.md` 索引)。
- `tools/`:Go 诊断工具(lakediag/rginfo/parquetprof),不属于生产链路。

## 常用命令

- 构建与测试:`go build ./...` / `go test ./internal/... -timeout 300s`
- 本地服务:`go run ./cmd/quantd serve --lake D:\quant-lake --listen 127.0.0.1:8000`
- 数据湖自校验:`go run ./cmd/quantd verify --lake D:\quant-lake`
- 扫描诊断:`go run ./tools/lakediag --lake D:\quant-lake --dataset bars_daily --filter ts_code=600000.SH --start 20240101 --end 20241231`
- 部署到 NAS:`harness/workflow/deploy-nas.md`
- 迁移旧湖:`harness/workflow/data-lake-migration.md`

## 环境

- 开发机 Windows,工作目录 `E:\AI-work\quant-core`;本地数据湖 `D:\quant-lake`(开发副本)。
- 部署目标 NAS:见 `harness/references/nas-server.md`(连接方式、Docker 镜像源修复、磁盘布局)。
- 旧湖(数据来源)`E:\AI-work\quant-data\quant-store\lake`,只读,不再更新。
- Tushare token:项目 `.env` 里的两个历史 token 均已失效;导入前需用户提供有效 token。
