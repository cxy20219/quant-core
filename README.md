# quant-core

量化数据服务与量化回测服务(Go)。数据湖采用 Parquet + 自研纯 Go 扫描引擎,
对外提供 tushare 兼容 HTTP 接口;回测服务执行 PTrade 兼容的 Python 策略(日线 MVP)。

## 定位

- **回测服务**:提交 PTrade 兼容的 Python 策略与超参数,Go 引擎执行(日线 MVP),
  返回净值/委托/成交/日志/指标。`POST /api/backtests` 提交,`GET /api/backtests/{id}` 查询。
- **数据湖**:规范化 Parquet 数据集(注册表驱动),批次 manifest 血缘,行组统计剪枝。
- **管理面板**:`GET /panel` —— 数据同步状态(行数/批次/磁盘/最后更新/数据源绑定)、
  回测结果(作业列表 + 净值曲线 + 委托/成交/日志)、因子管理与跟踪(注册表 + IC/分层曲线);
- **因子服务**:`/api/factors` —— 因子注册(内置动量/反转/波动/换手/市值/估值字段)、
  覆盖率校验、IC/RankIC 与分层收益跟踪;
- **数据服务**:tushare 兼容 HTTP API(`POST /` + `api_name`),可被 tushare SDK 直接调用;
  **在线接口文档**:`GET /docs`(由接口表+数据集注册表自动生成,含参数/字段/单位/示例),
  `GET /docs/openapi.json`(OpenAPI 3.0,可导入 Postman/Swagger)。
- **多源接入**:数据源是配置驱动的插件(`sources.yaml`):装载/卸载/换源不重编译;
  内置 `tushare-http`(官方与中转站通用)与 `exec`(任意语言外部插件);
  同一数据集可配置多源降级链,按调用自动切换。
- **回测服务**:Go 引擎 + Python 策略子进程(日线 MVP;分钟频率与 quantbt 探针对拍为下一步)。

## 目录

- `cmd/quantd/`:主程序(serve / backtest / migrate / import / source / dedupe / verify / apis)。
- `internal/schema/`:数据集注册表与值类型(单一权威)。
- `internal/lake/`:数据湖布局、part 写入器、批次 manifest。
- `internal/pq/`:注册表与 parquet-go 的桥接(列编码、值转换)。
- `internal/query/`:纯 Go 扫描引擎(分区剪枝 → 行组跳过 → 列投影)。
- `internal/tsapi/`:tushare 兼容协议层(api_name 注册表)。
- `internal/ingest/`:数据源导入(旧湖迁移已实现)。
- `schemas/datasets.yaml`:数据集注册表。
- `deploy/`:Dockerfile、compose 与部署说明。

## 常用命令

```powershell
go build -o bin/quantd.exe ./cmd/quantd
go test ./internal/... -timeout 300s

# 本地服务(数据 + 回测)
.\bin\quantd.exe serve --lake D:\quant-lake --listen 127.0.0.1:8000

# 本地跑一次回测(调试用)
.\bin\quantd.exe backtest --strategy examples\strategies\dual_ma.py --start 20240101 --end 20241231 --lake D:\quant-lake --params '{"fast":5,"slow":20,"symbols":["600000.SH"]}'

# 从旧湖(quant-data/quant-store)迁移
.\bin\quantd.exe migrate --src E:\AI-work\quant-data\quant-store\lake --lake D:\quant-lake --jobs 8

# 从 Tushare Pro 增量导入(需有效 token)
$env:TUSHARE_TOKEN="xxxx"
.\bin\quantd.exe import --source tushare --dataset stk_limit --start 20240101 --end 20240131 --lake D:\quant-lake

# 物化日频横截面(全市场单日筛选提速;数据更新后执行)
.\bin\quantd.exe build-cross --lake D:\quant-lake

# 行数核对(manifest vs parquet metadata)
.\bin\quantd.exe verify --lake D:\quant-lake

# 迁移对账(旧湖 vs 新湖逐分区行数)
python harness\scripts\verify_migration.py --src E:\AI-work\quant-data\quant-store\lake --dst D:\quant-lake

# 扫描诊断(查看剪枝效果)
go run ./tools/lakediag --lake D:\quant-lake --dataset bars_daily --filter ts_code=600000.SH --start 20240101 --end 20241231
```

## 数据服务用法(Python)

```python
import tushare as ts

pro = ts.pro_api("any-token")
pro._DataApi__http_url = "http://<nas-host>:8000"

df = pro.daily(ts_code="600000.SH", start_date="20230104", end_date="20230110")
df = pro.daily_basic(ts_code="600000.SH", trade_date="20230104",
                     fields="ts_code,trade_date,close,pe,pb,total_mv")
```

已实现接口:`daily`、`daily_basic`、`adj_factor`、`stk_mins`(1min)。

## 数据湖布局

```
<lake>/canonical/<dataset>/<partition...>/part-0001.parquet
<lake>/meta/manifest/<dataset>.jsonl      # 批次血缘
```

- 字段名与单位对齐 tushare:vol=手、amount=千元、total_mv=万元、trade_date=YYYYMMDD。
- 分钟线 amount 在接口层由内部千元换算回元,vol 由手换算回股(与 tushare `stk_mins` 一致)。
- 写入按 `(ts_code, 时间)` 有序,行组 8192 行;字符串列强制 PLAIN 编码以兼容 pyarrow/duckdb。

## 部署

内网地址与用户名不在仓库中:项目根 `.env` 配置 `QUANT_NAS_HOST` / `QUANT_NAS_USER`
(该文件已 gitignore),`harness/scripts/*` 与 Makefile 从中读取。

见 `deploy/README.md`。目标服务器:`<nas-user>@<nas-host>`(`/vol1/quant-core`)。
