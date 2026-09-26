# Parquet 生态兼容坑(parquet-go ↔ pyarrow/duckdb)

## 现象

自研 Go 写入器产出的 parquet 文件,pyarrow/pandas 读取时报
`Not yet implemented: DecodeArrow for DeltaLengthByteArrayDecoder`;
或 parquet-go 打开旧管线文件时报
`row group ordinal -32768 at index 32768 does not match its position`。

## 根因

1. **字符串编码不兼容**:parquet-go 按 parquet-format 文档建议,对 BYTE_ARRAY 默认使用
   `DeltaLengthByteArray`;但 pyarrow/duckdb 的批量(Arrow)解码路径不支持该编码,
   只能逐值解码或直接报错。
2. **行组 ordinal 为 int16**:parquet 规范中 `RowGroup.ordinal` 是 int16。
   部分 pyarrow(parquet-cpp)产物按"每股票每日一行组"写出 10 万+ 行组,
   ordinal 溢出为负数;parquet-go 严格校验 ordinal 必须等于行组下标,直接拒绝打开。
3. **列统计与页索引**:pyarrow 老版本产物默认不写 page index(ColumnIndex),
   parquet-go 的 `ColumnChunk().ColumnIndex()` 返回 `missing column index`;
   依赖统计剪枝的代码必须把"没有索引"当作保守不剪枝处理。

## 做法

- 写入器统一强制:所有字段 optional(def level 1/0)、字符串列 PLAIN 编码、
  Zstd 压缩、页级统计开启、行组大小由注册表 `writer.row_group_rows` 控制(当前 8192)。
  见 `internal/lake/writer.go` 的 `defaultEncodingFor(parquet.ByteArray, &parquet.Plain)`。
- 读取器:统计缺失时保守继续扫描;有统计则用行组 min/max 剪枝
  (见 `internal/query/metacache.go`)。
- 迁移前先体检源文件行组数;超限的用 `harness/scripts/normalize_source.py`
  重写(排序 + 131072 行组),再走正常迁移。
- 兼容性验收方式:任何写入器改动后,用 pyarrow 与 `duckdb` 各读一次,
  并跑 `harness/scripts/verify_migration.py` 对账。

## 边界

- `plain` 编码的字符串列压缩率略低于字典编码;实测分钟线 66 GB → 44.7 GB(Zstd),
  仍可接受。若未来数据量显著增长,可评估 `RLEDictionary`(需同时验证 pyarrow/duckdb)。
- 本项目的 parquet-go 版本为 v0.32.0;升级时要重跑"pyarrow/duckdb 双工具读取"验收。

## 关联

- 写入器:`internal/lake/writer.go`;桥接层:`internal/pq/bridge.go`。
- 迁移流程:`harness/workflow/data-lake-migration.md`。
- 剪枝机制:`harness/experience/scan-engine-pruning.md`。
