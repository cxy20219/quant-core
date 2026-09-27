# 回测服务流程(上传 Python 策略 + 参数 → 结果)

## 适用场景

提交 PTrade 兼容的 Python 策略与超参数,拿到回测结果(净值/委托/成交/日志/指标)。

## 架构

- **Go 引擎**(`internal/btengine`):日线/分钟(1m)时钟、账户/持仓、撮合与费用、指标;
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
- 数据湖覆盖回测区间 + 预热期(bars_daily/adj_factor;分钟模式还需 bars_1m,
  预热按交易日数计,默认 30 天)。
- 容器内 numpy/pandas 版本与开发机不同(NAS numpy 2.1.3 / 本地 1.26.4):
  数值结果一致,但用 `%r` 直接格式化 numpy 标量时文本会带 `np.float64(...)` 前缀;
  策略日志请用显式格式(如 `{:.4f}`),避免依赖 numpy 的 repr。

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

### 作业生命周期与重启语义

- 提交即落盘(`/data/backtests/<id>.json`,状态 `queued`),开始执行时更新为 `running`,
  结束时写 `done`/`failed` + 结果;客户端可随时按 id 查询(进程内作业与磁盘记录一致)。
- **服务重启**:收到 SIGTERM 后先做优雅停机(最多 30s 等在途请求),
  并把排队中/运行中的作业标记为 `failed`(`error="服务重启,作业被中断"`)后落盘;
  启动时也会把上次遗留的 `queued/running` 记录标记为中断。
  因此重启不会"静默丢作业":轮询方一定能看到明确终态。
- 作业在**进程内**执行(不跨重启续跑);需要长任务请分批提交。

### HTTP 运维硬化(标准库实现,见 `internal/httpx`)

- 超时:`ReadHeaderTimeout=10s`、`ReadTimeout=60s`、`WriteTimeout=5min`、`IdleTimeout=2min`;
- 请求体上限 8MB(策略代码本身限 500KB);
- panic 恢复为 JSON 500(数据接口用 tushare 信封,`/api/*` 用 `{"error":...}`)+ 堆栈日志;
- 访问日志:`POST / 200 494B 25.7ms` 形式,便于排障与延迟观察;
- 优雅停机:容器 `docker compose restart/up -d` 不会打断在途请求。

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

## 在线文档与管理面板

- 管理面板 `GET /panel`:概览(服务/湖/作业)、数据同步状态(manifest 行数/批次/磁盘/数据源绑定)、
  回测结果(作业列表 + 净值曲线 + 委托/成交/日志);因子管理/跟踪为预留页签。
  面板只读、无鉴权(与 `/docs` 一致,限可信内网);回测数据复用 `/api/backtests`。

- 服务内置接口文档页:`GET /docs`(数据接口参数/字段/单位/示例 + 回测接口 + 策略 API),
  由接口表(`internal/tsapi/apis.go`)与数据集注册表(`schemas/datasets.yaml`)自动生成;
- `GET /docs/openapi.json` 为 OpenAPI 3.0 规范(导入 Postman/Swagger UI 即可调用)。

## 策略接口(PTrade 子集)

- 生命周期:`initialize(context)` / `before_trading_start(context, data)` /
  `handle_data(context, data)` / `after_trading_end(context, data)` / `run_daily(context, func, time)`
- 全局对象:`g`(属性字典)、`log.info/warning/error`、`params`(超参数字典,由请求注入)
- 行情:`get_history(count, frequency, field, security_list, fq, include, fill, is_dict)` /
  `get_price(...)`;`fq` 支持 `None/post/pre`;停牌日按 PTrade 语义(价格取前值、成交量为 0)
- 交易:`order / order_target / order_value / order_target_value / cancel_order`
- 查询:`get_order(s) / get_open_orders / get_trades / get_position(s) / get_stock_exrights /
  get_fundamentals`(仅 `table="valuation"` 的 date 模式)
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
