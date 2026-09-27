# 回测服务流程(上传 Python 策略 + 参数 → 结果)

## 适用场景

提交 PTrade 兼容的 Python 策略与超参数,拿到回测结果(净值/委托/成交/日志/指标)。

## 架构

- **Go 引擎**(`internal/btengine`):日线时钟、账户/持仓、撮合与费用、指标;
  数据来自本地数据湖(与数据服务同一条查询路径)。
- **Python 子进程**(`python/runner/worker.py`):只运行策略代码,通过 stdio JSON-RPC
  调用引擎的行情/下单/账户能力;引擎持有全部语义权威。
- **服务**(`internal/btserver`):`POST /api/backtests` 提交 → 作业队列 → 结果落盘。

```
POST 策略+参数 → 作业队列 → { Go 引擎 ⇄ Python 策略子进程 } → 结果(JSON)
                                                              ↓
                                        /data/backtests/<id>.json(记录持久化)
```

## 前置条件

- 镜像已含 `python3 + py3-numpy + py3-pandas`(deploy/Dockerfile.binary);
- 本地运行时需要 Python 3.10+ 与 pandas(可用 `--python` 指定解释器);
- 数据湖覆盖回测区间 + 预热期(bars_daily/bars_adj_factor)。

## 关键步骤

1. **提交回测**(HTTP):

   ```bash
   curl -X POST http://<nas-host>:8000/api/backtests \
     -H "Content-Type: application/json" \
     -d '{"strategy_code":"<python>","strategy_name":"双均线",
          "params":{"fast":5,"slow":20,"code":"600000.SH"},
          "start_date":"20240101","end_date":"20241231",
          "capital_base":1000000,"benchmark":"000300.SH"}'
   # → {"id":"...","status":"queued"}
   ```

2. **查询结果**:

   ```bash
   curl http://<nas-host>:8000/api/backtests/<id>
   curl http://<nas-host>:8000/api/backtests          # 历史列表
   ```

3. **本地 CLI 单跑**(调试用,不经 HTTP):

   ```bash
   quantd backtest --strategy examples/strategies/dual_ma.py \
     --start 20240101 --end 20241231 --capital 1000000 --benchmark 000300.SH \
     --lake /vol1/quant-core/data/lake \
     --params '{"fast":5,"slow":20,"symbols":["600000.SH"]}' --out result.json
   ```

4. **端到端示例脚本**:

   ```bash
   python examples/use_backtest_service.py http://<nas-host>:8000
   ```

## 策略接口(PTrade 子集)

- 生命周期:`initialize(context)` / `before_trading_start(context, data)` /
  `handle_data(context, data)` / `after_trading_end(context, data)` / `run_daily(context, func, time)`
- 全局对象:`g`(属性字典)、`log.info/warning/error`、`params`(超参数字典,由请求注入)
- 行情:`get_history(count, frequency, field, security_list, fq, include, fill, is_dict)` /
  `get_price(...)`;`fq` 支持 `None/post/pre`;停牌日按 PTrade 语义(价格取前值、成交量为 0)
- 交易:`order / order_target / order_value / order_target_value / cancel_order`
- 查询:`get_order(s) / get_open_orders / get_trades / get_position(s)`
- 设置:`set_universe / set_benchmark / set_commission / set_slippage / set_fixed_slippage /
  set_volume_ratio / set_limit_mode`
- 超参数注入:策略内读 `params["fast"]`(也提供 `g_params` 同义别名)

## 撮合与费用语义(与 quantbt 对齐)

- 日线时钟:`08:30 before_trading_start` → `15:00 handle_data` → `run_daily` → 挂单撮合 → 重新估值 → `15:30 after_trading_end`
- 市价单按当前 Bar 收盘价成交;限价单当收盘价满足限价时成交,否则当日撤单(日级语义)
- 滑点按方向取半:`price × (1 ± slippage/2)`
- 费用 = 佣金 `max(成交额×万三, 5 元)` + 经手费 `成交额×0.0000487` + 卖出印花税 `成交额×0.001`
- 成交量约束:`volume_ratio`(默认 0.25)对当 Bar 成交量设上限;`set_limit_mode("UNLIMITED")` 关闭

## 验证

- 成功信号:`GET /api/backtests/{id}` 返回 `status=done`,包含 `summary/analytics/portfolio/orders/trades/logs`。
- 基准数字(2026-09-27,600000.SH 2024 全年,双均线 5/20,滑点 0.002):
  本地与 NAS **逐位一致**:期末 1,007,428.50、收益 +0.74%、夏普 1.34、最大回撤 -1.01%、22 个交易日、3 笔成交。
  另一组(2024 全年、滑点 0.002、无基准,python 直连):期末 1,272,782、+27.28%。
  > 注意:不同滑点/参数会显著改变结果,基准数字用于"同参数回归",不作跨参数承诺。
- 常见失败信号:
  - `策略错误(...): AttributeError` → 策略用了未实现的 API,按上面的接口清单核对;
  - `get_history: 未指定 security_list 且未设置股票池` → 先 `set_universe` 或显式传列表;
  - `策略进程在 X 阶段退出` → 子进程崩溃(通常是策略语法/导入错误),看返回的 traceback;
  - `json: unsupported value: +Inf` → 产生了非有限数值(如除零),检查策略计算与因子数据。

## 边界与后续

- **未实现**:分钟频率(1m)、`get_fundamentals`、`get_stock_exrights` 的公司行动影响、
  `tick_data`/`on_order_response`/`on_trade_response`(交易模块专属,回测禁用)。
- **对拍体系(spec:与 quantbt 逐项一致)**:`internal/btengine/engine_test.go` 覆盖撮合/挂单/
  费用基础;quantbt 探针与黄金快照的移植尚未完成,是下一步的核心工作。
- 作业并发上限 `--max-backtests`(默认 2);结果持久化在 `--records`(默认 `<lake>/../backtests`)。

## 最近验证

2026-09-27:镜像(206MB)部署 NAS,HTTP API 提交→轮询→结果全链路通过,结果与本地逐位一致。
