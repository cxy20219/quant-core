# 回测引擎对拍流程(quantbt 参照)

## 适用场景

Go 回测引擎修改后,验证其与参照实现 quantbt(PTrade 兼容,已由 55+ 探针与实盘回填验证)
**逐项一致**:净值序列、现金、委托、成交、策略可见日志。日线与分钟(1m)两种频率都覆盖。

## 前置条件

- 参照侧:quant-data 仓库(`E:\AI-work\quant-data`,含 `quant-store` Python 数据湖);
- 被测侧:Go 引擎(`bin/quantd.exe`)+ 新的 Parquet 湖(`D:\quant-lake`,与旧湖内容已对账一致);
- 两侧数据内容相同(迁移对账见 `harness/workflow/data-lake-migration.md`),因此差异只来自引擎语义。

## 关键步骤

1. **日线单探针对拍**(默认 2020-01-02 ~ 2020-01-10,600570.SS):

   ```bash
   python harness/scripts/parity_check.py \
     --strategy examples/strategies/parity/p01_market_basic.py --check-logs
   ```

2. **日线全量探针批量对拍**:

   ```bash
   python harness/scripts/parity_check.py --all --check-logs
   ```

3. **分钟对拍**(quantbt 的 PTrade 对齐探针,参数取自其测试文件):

   ```bash
   python harness/scripts/parity_check.py --cases harness/assets/quant-minute-cases.json --check-logs
   ```

   **公司行动对拍**(除权除息/送转/配股/红利税,2020 前后多个区间):

   ```bash
   python harness/scripts/parity_check.py --cases harness/assets/quant-corporate-cases.json --check-logs
   ```

   单探针示例(注意 Go 侧预热按天,分钟模式默认 30 天):

   ```bash
   python harness/scripts/parity_check.py --frequency 1m --warmup-bars 5 \
     --start 2020-01-02 --end 2020-01-03 \
     --strategy E:/AI-work/quant-data/strategies/probes/ptrade_alignment_limit_fill.py --check-logs
   ```

4. **新增探针**:日线探针放 `examples/strategies/parity/pNN_*.py`;分钟探针沿用
   quant-data 的 `strategies/probes/ptrade_alignment_*.py`,把参数登记到
   `harness/assets/quant-minute-cases.json`(字段:`test/probe/start/end/frequency/capital/warmup_bars/warmup_days`)。

## 对比口径

| 项目 | 说明 |
|---|---|
| 净值序列 | 逐条对齐:日线用 `date`、分钟用 `datetime`;`portfolio_value` 与 `cash` 在 1e-6 相对容差内 |
| 委托 | `dt/symbol/status/amount/filled/limit` 逐项一致(代码用 `.XSHG/.XSHE`) |
| 成交 | `security/side/amount/price/value` 逐项一致 |
| 日志 | `--check-logs` 时消息序列必须完全一致(策略可见状态的强校验);委托号(32 位十六进制)对比前归一化为 `<id>` |

## 验证

- 成功信号:`PASS: 净值/委托/成交逐项一致`;批量模式输出
  `8 个探针, 8 通过, 0 失败`(日线)或 `19 个用例, 19 通过, 0 失败`(分钟)。
- 失败信号:输出首个差异(如 `净值[3] 2020-01-07 不同: quantbt=... go=...`),
  按 `harness/experience/bt-semantics-parity.md` 的语义清单排查。
- 已覆盖(日线):P01 市价买卖 / P02 限价挂单与撤单 / P03 T+1 / P04 目标单取整 /
  P05 资金与成交量上限 / P06 费用滑点变体 / P07 多标的组合 / P08 生命周期与 run_daily。
- 已覆盖(公司行动,12 用例):分红+送转入账 / 日线生命周期 / 红利税(含边界、
  持有满一年、结算日)/ 零股送转取整 / 跨标的公司行动现金 / 多笔配股现金 /
  配股 / 配股现金不足 / 候选事件扫描(get_stock_exrights 全量)。
- 已覆盖(分钟,19 用例):分钟基线(日线 history/账户)/ T+1 与剩余成本 /
  可成交限价与部分成交自动撤余量 / 市价与目标单量额上限 / 资金上限取整 /
  UNLIMITED 跳过量上限 / value 与 target_value 取整 / 资金上限挂单缩量 /
  涨跌停价与市价单 / 停牌用前收与量 0 / 限价触发撮合时点 / 触发限价量上限 /
  零滑点触发卖单费用 / order() 前先撮合挂单(量竞争)/ 挂单共享 Bar 量预算且跨 Bar 延续 /
  跨标量预算独立 / 同回调撤单换单释放资源。

## 边界与未覆盖

- `get_fundamentals`(估值表)未实现;`tick_data`/`on_order_response` 等未实现。
- `ptrade_alignment_mixed_corporate_action_candidates.py` 依赖 PTrade 的
  `get_Ashares`(quantbt 亦不提供),无法对拍,已排除在用例集外。
- 分钟模式为按交易日滚动窗口加载(默认 30 天),超长区间/大股票池的内存与速度优化未做。

## 最近验证

2026-09-27:日线 8 探针、分钟 19 用例、公司行动 12 用例全通过(净值/委托/成交/日志四项);
Go 单测全绿。分钟引擎实现期间修复了扫描引擎跨月分区剪枝丢数据的缺陷(见
`harness/experience/scan-engine-pruning.md`);公司行动对拍期间修复了旧湖
2022 年公司行动日期迁移损坏(见 `harness/experience/bt-semantics-parity.md`)。
