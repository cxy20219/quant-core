# 回测引擎对拍流程(quantbt 参照)

## 适用场景

Go 回测引擎修改后,验证其与参照实现 quantbt(PTrade 兼容,已由 55+ 探针与实盘回填验证)
**逐项一致**:日终净值、现金、委托、成交、策略可见日志。

## 前置条件

- 参照侧:quant-data 仓库(`E:\AI-work\quant-data`,含 `quant-store` Python 数据湖);
- 被测侧:Go 引擎(`bin/quantd.exe`)+ 新的 Parquet 湖(`D:\quant-lake`,与旧湖内容已对账一致);
- 两侧数据内容相同(迁移对账见 `harness/workflow/data-lake-migration.md`),因此差异只来自引擎语义。

## 关键步骤

1. **单个探针对拍**(默认 2020-01-02 ~ 2020-01-10,600570.SS 日线):

   ```bash
   python harness/scripts/parity_check.py \
     --strategy examples/strategies/parity/p01_market_basic.py --check-logs
   ```

2. **全量探针批量对拍**:

   ```bash
   python harness/scripts/parity_check.py --all --check-logs
   ```

3. **新增探针**:在 `examples/strategies/parity/` 下加 `pNN_*.py`(用固定标的与日期,
   把关键状态 `log.info` 出来,便于日志级对拍)。

## 对比口径

| 项目 | 说明 |
|---|---|
| 净值序列 | 日期逐条对齐、`portfolio_value` 与 `cash` 在 1e-6 相对容差内 |
| 委托 | `dt/symbol/status/amount/filled/limit` 逐项一致(代码用 `.XSHG/.XSHE`) |
| 成交 | `security/side/amount/price/value` 逐项一致 |
| 日志 | `--check-logs` 时消息序列必须完全一致(策略可见状态的强校验) |

## 验证

- 成功信号:`PASS: 净值/委托/成交逐项一致`;批量模式输出 `8 个探针, 8 通过, 0 失败`。
- 失败信号:输出首个差异(如 `净值[3] 2020-01-07 不同: quantbt=... go=...`),
  按 `harness/experience/bt-semantics-parity.md` 的语义清单排查。
- 已覆盖探针:P01 市价买卖 / P02 限价挂单与撤单 / P03 T+1 / P04 目标单取整 /
  P05 资金与成交量上限 / P06 费用滑点变体 / P07 多标的组合 / P08 生命周期与 run_daily。

## 边界与未覆盖

- 当前对拍仅覆盖**日线**。quantbt 的 38 个对齐用例均为**分钟级**(T+1 盘中、
  Bar 内共享成交量、13:00 时点、午盘首分钟等),需要 Go 引擎实现分钟时钟后再接入。
- 公司行动(分红/送转/配股)未实现,相关探针未纳入对拍。
- `get_fundamentals`、`tick_data`/`on_order_response` 等未实现或明确不支持。

## 最近验证

2026-09-27:日线 8 探针 × 3 项(净值/委托/成交)+ 日志全量对拍通过;
NAS 部署后同参数结果与本地逐位一致(27.37% / 期末 1,273,745)。
