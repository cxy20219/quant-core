# 多源接入与换源踩坑(tushare 双源中转)

## 现象

同一数据集换源后结果不一致或静默缺失:行数少了、字段顺序不同、报"结果被截断"、
`unknown api_name`,或重跑导入后行数翻倍。

## 背景

两个中转站都是 tushare 数据的 HTTP 中转,协议相近但语义有别:

| 维度 | A 站 datahubco(87 接口) | B 站 pcd.mobcvb.cn(298 接口) |
|---|---|---|
| 接口命名 | 连字符 `stock-basic` | 下划线 `stock_basic` |
| limit | **硬上限 5000**(超出 400) | 默认 10000,语义不稳定(见下) |
| 无内容参数 | 400 报错并列出必填参数 | 返回 5 行样例(`meta.probe=true`) |
| 缺失接口 | 无 namechange 等 | 较全 |
| 稳定性 | 稳定但慢 | 上游池偶发 `upstream_pool_exhausted` |

## 根因与做法

- **limit 语义不一致(最容易踩)**:B 站实测 `limit=100` 被忽略返回全量 6716 行,
  `limit=1250` 却按 1250 截断且 `count` 一起变小(无法用 count 识别截断)。
  → 做法:导入器同时用两种信号检测截断(`count>行数`、`行数恰好==limit`);
  数据源端**不做"减半"降级**(曾导致成功但截断的静默数据),只做同 limit 瞬时重试。
- **503 upstream_pool_exhausted 是持续而非瞬时**:B 站按接口分配上游池,
  大池故障时小 limit 可走小池(但会截断)、接口整体不可用也是常态。
  → 做法:窗口级重试(默认 3 次 + 退避)+ 稍后重跑;必要时临时调整 bindings 换源;
  不要靠自动减半"猜"一个能过的 limit。
- **跨源语义差异**:`trade_cal` 在 B 站曾出现同一请求 242 行(只含交易日)与 366 行(含休市日)交替;
  A 站稳定 366 行。→ 做法:换源前用 `quantd source compare` 对比行数与首末行,
  按数据集固定可靠源(`bindings`),并对关键不变量做覆盖率校验。
- **重跑产生重复**:导入中断后重跑会重复写入已完成窗口。→ 做法:
  `Importer.Resume` 从 manifest 提取已完成窗口并跳过;
  历史重复用 `quantd dedupe`(按主键去重 + 自动记账),账目用 `--reconcile` 修正。
- **字段顺序不稳定**:B 站不同请求返回的 `fields` 顺序可能变化(pre_close 位置漂移)。
  → 做法:按字段名映射(`result.Fields` → 数据集字段),绝不按位置取值。
- **快照与窗口上限**:中转站对 `start_date/end_date` 有窗口限制(>366 天报
  `date_range_too_large`);单次行数也有上限。→ 做法:快照按 交易所/状态 × 年份 切片,
  指数日线按 代码 × 年窗口;逐日接口(如 stk_limit)按天调用。

## 换源检查清单

1. `quantd source list` — 确认源状态与 bindings;
2. `quantd source compare --dataset <x> --param ...` — 行数/字段/首末行一致性;
3. 小窗口试导 + `quantd verify` — 账目与行数;
4. 关键不变量校验(交易日历覆盖率、单日股票数、单位);
5. 更换 bindings 后重跑全窗口,`dedupe --reconcile` 修账。

## 边界

- 两站都是第三方中转,数据完整性不受我们控制;生产导入必须保留 `verify` 与 `dedupe` 两步。
- `exec` 类型插件(外部命令)是最终兜底:任何新源都可以用脚本接入,不重编译主程序。

## 关联

- 流程:`harness/workflow/tushare-data-import.md`
- 配置:`sources.yaml`(仓库根)
- 相关实现:`internal/source/`(tushare-http/exec/fallback/registry)、`internal/ingest/import.go`
