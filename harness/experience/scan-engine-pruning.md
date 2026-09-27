# 扫描引擎剪枝经验(纯 Go / parquet-go)

## 现象

单代码查询延迟高、`tools/lakediag` 显示 `rowgroups_read` 远超预期;
或热查询与冷查询延迟几乎一样(元数据缓存未生效)。

## 根因(按影响排序)

1. **行组物理布局决定剪枝上限**:只有当文件按 (ts_code, 时间) 物理排序、
   且行组覆盖少量代码时,ts_code 的 min/max 统计才能跳过绝大部分行组。
   旧湖按"每股票一块"写但块序随机,直接迁移会让每个行组横跨全市场,
   统计剪枝失效(单代码查询全表扫)。
   → 迁移器必须按代码重排块(见 `internal/ingest/migrate.go` 的 `blockOrder`)。
   注意:若同一代码有多个块(按日分块的文件),排序键要包含块内首条时间。
2. **行组过大**:8192 行/组(分钟线约 1.5 只股票/组)时,单代码单日查询约读 1~3 组;
   131072 行/组时会读 20~30 组。行组大小在注册表 `writer.row_group_rows` 控制。
3. **footer 与页索引重复解析**:每次查询打开文件都要解析 footer 与页索引;
   `internal/query/metacache.go` 缓存每文件的行组统计(带大小/时间戳校验,文件不可变)。
   缓存命中后,整文件可剪枝的文件根本不会被打开。
4. **跳过计划耗尽后落回顺序读取**(已修复的 bug):`advance()` 曾用
   `rgSkip[idx]==false` 决定读行组,但当计划数组耗尽时,另一个顺序读取分支
   会从当前行组继续读到文件尾。表现为"计划正确但读了 25 个组"。
   修复后:有计划且耗尽 → 关闭当前文件;无计划(无过滤)→ 才顺序读。
   回归测试:`internal/query/realdiag_test.go`。
5. **跨月窗口的月份迭代丢末月**(已修复的 bug):`partitionAccept` 生成月份集合时
   从 `lo` 的**日号**开始逐月 `AddDate(0,1,0)`,如 `lo=2019-12-29` → 下一跳
   `2020-01-29` 已越过 `hi=2020-01-02`,导致 `2020-01` 分区被整块剪掉、
   **静默丢数据**(查询无报错,只是少一个月的行)。
   表现:跨月/跨年窗口(起始日号 > 结束日号,如"最近 30 天"滚动窗口)结果缺数据;
   单日或月初对齐的窗口不受影响,因此容易被漏测。
   修复:从 `lo` 所在月的 **1 号**开始迭代。回归测试:
   `internal/query/partition_test.go`(纯单测)+ `crossyear_test.go`(真实湖)。

## 做法

- 排查顺序:先 `go run ./tools/lakediag --lake <湖> --dataset <数据集> --filter ts_code=... [--start/--end]`,
  看 `files_opened / files_skipped / meta_built / rowgroups_read / rowgroups_skipped / rows_seen`;
  再用 `go run ./tools/rginfo <file>` 打印行组 min/max 区间确认布局,用
  `go run ./tools/parquetprof <file>` 分离 OpenFile / ColumnIndex 耗时。
- 判据:单代码查询的 `rowgroups_read` 应 ≤3(daily 年文件 ≤2);`rows_seen` 应为
  行组行数 × 读取组数,而不是整表行数。
- 元数据缓存自检:`meta_built` 在第二次相同查询时应为 0。

## 边界

- 全市场单日横截面查询(如 `daily(trade_date=...)`)无法经 ts_code 剪枝,
  需要扫完该年文件的所有行组(约 120 万行,1.5~2 秒)。属已知代价;
  若将来成为瓶颈,考虑物化"日频横截面"小表数据集。
- `ColumnIndex` 依赖写入时开启页级统计;若对接第三方文件没有索引,剪枝退化为全扫。
- 行组 8192 行会增加文件元数据量;当前分钟线 550 个文件、单文件 150~200 MB 量级,可接受。

## 关联

- `harness/experience/parquet-ecosystem-compatibility.md`
- 扫描器:`internal/query/scan.go`、`internal/query/metacache.go`
- 诊断工具:`tools/lakediag`、`tools/rginfo`、`tools/parquetprof`
