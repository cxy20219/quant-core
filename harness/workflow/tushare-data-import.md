# Tushare 数据导入流程(增量/快照)

## 适用场景

用 `quantd import` 从 Tushare Pro 拉取数据写入新湖,覆盖:
`stock_basic`、`trade_cal`、`index_basic`(快照)、`stk_limit`、`suspend_d`、`namechange`、`index_daily`(按月区间)。

## 前置条件

- **有效 Tushare token**(项目 `.env` 中两个历史 token 均已失效,需要向用户索取新的)。
  接口地址默认 `https://api.tushare.pro`,客户端在 `internal/source/tushare`。
- 目标数据集已在 `schemas/datasets.yaml` 注册。
- 新湖目录可写。

## 导入模式(选择依据)

| 模式 | 适用 | 行为 |
|---|---|---|
| `snapshot` | 全量快照表(stock_basic/index_basic/trade_cal) | 一次拉全量,按分区字段切分,`--replace` 先清空目标分区 |
| `month-range` | 支持 start_date/end_date 的时序接口(stk_limit 等) | 逐月查询,按行内日期字段写入对应年份分区;API 调用量约逐日的 1/20 |
| `date-range` | 只支持单日 trade_date 的接口 | 逐日查询,写入当日年份分区(当前规格未使用,保留给单日接口) |

## 关键步骤

1. **查看可用数据集与模式**:

   ```bash
   .\bin\quantd.exe import --source tushare
   ```

2. **导入快照类**(示例):

   ```bash
   set TUSHARE_TOKEN=xxxx
   .\bin\quantd.exe import --source tushare --dataset stock_basic --lake D:\quant-lake --replace
   .\bin\quantd.exe import --source tushare --dataset trade_cal --lake D:\quant-lake --replace
   ```

3. **导入区间类**(示例:涨跌停价 2024 全年):

   ```bash
   .\bin\quantd.exe import --source tushare --dataset stk_limit --start 20240101 --end 20241231 --lake D:\quant-lake
   ```

4. **对账**:

   ```bash
   .\bin\quantd.exe verify --lake D:\quant-lake --dataset stk_limit
   ```

## 验证

- 成功信号:命令输出 `dataset <name> imported: N rows`,且 `quantd verify` 显示 `OK ... batches=N files=M rows=R`。
- 接口可用性:`curl -s "http://127.0.0.1:8000/healthz"` 中对应数据集 `partitions>0`;
  用 tushare SDK 验证 `pro.stk_limit(trade_date="20240102")` 返回数据。
- 常见失败信号:
  - `code=40101 msg=抱歉,您的token不对` → token 失效。
  - `field "xxx" is not available for api daily` → 请求字段不在接口白名单。
  - 导入后查询为空 → 分区年份与行内日期字段不一致,检查 `DateField` 配置。

## 完成情况

- 2026-09-26:导入器与 mock 测试已完成(`internal/ingest/tushare_test.go`);
  **真实导入尚未执行**(缺有效 token),首次导入后需回填本文档的验证结果。
