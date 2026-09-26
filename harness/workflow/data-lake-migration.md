# 数据湖迁移流程(旧湖 → quant-core 新湖)

## 适用场景

把 `E:\AI-work\quant-data\quant-store\lake`(旧湖,TDX/研究管线产物)的数据迁移到
quant-core 新湖格式(注册表驱动、tushare 字段名、按 (ts_code, 时间) 排序)。

## 前置条件

- 新湖目标目录可用(本地开发用 `D:\quant-lake`,生产在 NAS `/vol1/quant-core/data/lake`)。
- 旧湖保持只读;迁移工具只读旧湖、只写新湖。
- 磁盘:1m 数据新湖约占 45 GB(源 66 GB)。

## 关键步骤

1. **盘点与体检**(先跑对账基线,同时暴露行组 ordinal 溢出的源文件):

   ```bash
   python harness/scripts/verify_migration.py --src E:\AI-work\quant-data\quant-store\lake --dst D:\quant-lake
   ```

2. **归一化异常源文件**(若有文件行组数 > 32767,parquet-go 无法打开):

   ```bash
   python harness/scripts/normalize_source.py --src E:\AI-work\quant-data\quant-store\lake --staging D:\quant-lake-src-fix
   ```

   归一化会按 (code, datetime) 排序并用 131072 行行组重写;已完成的对账脚本会自动跳过。

3. **迁移**(先日线/复权/公司行动,再分钟线;`--jobs` 控制并行):

   ```bash
   go build -o bin/quantd.exe ./cmd/quantd
   .\bin\quantd.exe migrate --src E:\AI-work\quant-data\quant-store\lake --lake D:\quant-lake --dataset bars_daily --jobs 6
   .\bin\quantd.exe migrate --src E:\AI-work\quant-data\quant-store\lake --lake D:\quant-lake --dataset adj_factor --jobs 6
   .\bin\quantd.exe migrate --src E:\AI-work\quant-data\quant-store\lake --lake D:\quant-lake --dataset corporate_actions --jobs 6
   .\bin\quantd.exe migrate --src E:\AI-work\quant-data\quant-store\lake --lake D:\quant-lake --dataset bars_1m --jobs 8
   # 归一化目录同样并入(分区目录结构一致,自动续写 part 编号)
   .\bin\quantd.exe migrate --src D:\quant-lake-src-fix --lake D:\quant-lake --dataset bars_1m --jobs 4
   ```

4. **逐分区对账**(必须 100% 一致才算迁移完成):

   ```bash
   python harness/scripts/verify_migration.py --src E:\AI-work\quant-data\quant-store\lake --dst D:\quant-lake
   ```

5. **manifest 自校验**(写入器记录 vs parquet 实际行数):

   ```bash
   .\bin\quantd.exe verify --lake D:\quant-lake
   ```

## 验证

- 成功信号:`verify_migration.py` 各数据集 `[OK]` 且行数一致;`quantd verify` 输出各数据集 `OK`。
  基准数字:`bars_1m=3,950,683,458`、`bars_daily=16,267,551`、`adj_factor=38,089,668`、`corporate_actions=519`。
- 采样校验:分钟线单代码单日查询应只读 1~3 个行组(`tools/lakediag` 的 `rowgroups_read`),且与日线 vol 口径一致(日线 vol×100 = 分钟 vol 合计)。
- 常见失败信号:
  - `row group ordinal -32768 at index 32768 does not match its position` → 源文件行组数超 int16,先跑 `normalize_source.py`。
  - 分区行数翻倍 → 同一源文件被迁移两次;删除重复 part 文件并同步清理 `meta/manifest/<dataset>.jsonl` 中的重复记录(参见下面"已知事故")。
  - `verify_migration.py` 报 `missing partitions` → 迁移未覆盖全部年份,检查源文件枚举与 `--year` 参数。

## 已知事故与处理

- 2026-09-26 迁移时,2026-01 分钟分区因"先试跑一次、再全量一次"被写入两份(part-0004..0006 为重复)。
  处理:删除重复 part 文件 + 删除 manifest 中对应记录(脚本见 git 历史中的 dedupe 一次性脚本);教训是试跑后重跑要检查目标分区是否已有同名内容。
- 2026-01 至 2026-05 的 5 个分钟文件行组数为 72k~114k(超出 int16),必须经 `normalize_source.py` 处理。

## 最近验证

2026-09-26:全量迁移完成,四个数据集逐分区对账 100% 一致(39.5 亿行分钟数据)。
