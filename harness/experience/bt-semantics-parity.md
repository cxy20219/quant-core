# 回测语义对拍要点(Go 引擎 ↔ quantbt)

## 现象

两个引擎在相同策略/数据下结果不一致时,差异通常来自以下语义细节;
逐条按 quantbt 实现对齐后,日线 8 探针与分钟 19 用例可全通过(净值/委托/成交/日志)。

## 日线语义(按踩坑顺序)

1. **挂单冻结资金/股数**(PTrade 语义)
   - 不可成交的限价买单:下单当刻冻结 `数量×限价×(1+滑点/2) + 费用` 并从现金扣除;
     卖出挂单冻结 `enable_amount -= 数量`。
   - 当日 `after_trading_end` 后撤销:**买入仅释放 数量×限价**(滑点与费用不退),
     卖出释放冻结股数;订单从有效列表移除(PTrade 的 `get_order` 次日为空),
     对拍结果里只能出现在"取消记录"。
   - 表现:挂单日的净值因冻结而下降(不是等成交才降)。

2. **触发成交按当前价,不按限价**
   挂单在后续 Bar 满足限价时,成交价 = 当前 Bar 收盘价×(1±滑点/2)(比限价更优),
   而不是按限价成交。

3. **部分成交按"原始下单量"计费**
   被 `set_volume_ratio` 截断时:委托记录 `amount=原始数量`、`filled=实际成交`、
   `status=6`;费用按**原始下单量**计提,滑点差额一并扣减(未成交部分不退);
   完全被截断时也记一笔 `status=6`、`filled=0` 的委托并计费用。
   即时成交路径的现金 = `成交数量×未滑点价 + 原始数量×(滑点价−未滑点价)`,
   费用基数为 `原始数量×滑点价`。

4. **T+1 与账户即时性**
   - 买入当日 `enable_amount` 不增加,次日开盘 `enable_amount = amount`(当日买入解禁);
   - 同一回调内下单后,`context.portfolio.cash/portfolio_value` **立即变化**
     (position 按当前 Bar 收盘价估值、现金按含滑点成交价扣减);
     若引擎按 Bar 粒度刷新快照,策略在回调内读到的账户就是过期值。

5. **value 系列下单的取整顺序**
   `order_target_value(v)`:先 `int(v/price)` 再按手数向下取整得到目标股数,最后与当前持仓求差;
   不是"先算差额再取整"。`order_value` 同理先 `int(v/price)` 再由下单环节取整。

6. **history 的截止日与停牌填充**
   `include=False` 截止到**当前交易日之前**(首日取数据中的上一交易日);
   停牌日价格用停牌前值填充、**成交量为 0**(不是 0 价格或跳过该日)。

7. **撤单语义**:撤单后状态置 `"6"` 且委托**仍留在当日委托列表**
   (`get_order` 查得到、`get_open_orders` 查不到),冻结资源全部释放;
   与"当日到期失效"不同——后者从有效列表移除。

## 分钟语义(2026-09-27 补齐)

1. **时钟**:每交易日 240 根,上午 `09:31–11:30`、下午 `13:01–15:00`;
   `13:00` 不触发 `handle_data`。`run_daily(time="13:00")` 顺延到 `13:01` 触发;
   分钟模式下 `run_daily` 只在到点的分钟触发(日线模式全部在 15:00 触发)。
2. **调用顺序**(每个 Bar):估值 → `handle_data` → 撮合挂单 → 估值 → `run_daily`;
   日切换时先 `08:30` 盘前(`before_trading_start`),日末 `15:30` 收尾
   (`after_trading_end` → 挂单失效)。`after_trading_end` 只在日末调用一次。
3. **开盘集合竞价折叠**:`09:30` Bar 并入同日 `09:31`——`09:31` 的 open 取 `09:30` 的 open,
   high/low 取两者极值,量额相加;`09:30` 不再可见。不折叠会导致 09:31 的价格/量额全错。
4. **停牌分钟填充**:窗口内缺失的分钟用**前收盘价**填充,`volume=0`、`money=NaN`
   (JSON 传 `null`,策略侧还原 NaN)。因此停牌证券在 `data` 中仍存在(不是 KeyError),
   但成交量上限为 0;长期停牌(窗口内无任何 Bar)用最近有效日收盘价做种子。
5. **挂单共享 Bar 量预算**:同一分钟内多笔委托共享 `该 Bar 成交量×volume_ratio`,
   按提交顺序先到先得,超出部分部分成交并**自动撤销剩余**(status 6);
   卖单部分成交后 `enable_amount` 立即恢复为持仓数量。
6. **`order()` 前先撮合**:`handle_data` 阶段调用任何下单 API 前,先用当前 Bar
   撮合可成交的挂单(与 quantbt 的 `_match_open_orders_before_order` 一致),
   新单再与剩余量预算竞争;不做这一步会出现"新市价单抢走挂单成交量"的差异。
7. **单位口径(PTrade)**:成交量 = 股(湖内日线/分钟均为手,×100);
   成交额 = 元(湖内千元,×1000)。分钟源数据迁移时按 ÷1000 归一化为千元,
   乘回时需吸附浮点噪声(否则 `16502079.000000002` 这类值会出现在日志里)。
8. **委托号/成交号**:每交易日重置——委托号 `700000` 起、成交号 `5000` 起,每笔成交 +1;
   委托 id 为 32 位十六进制随机串(对拍日志需归一化)。
9. **分钟 history/get_price**:`get_history(count,'1m')` 截止当前分钟
   (`include=False` 时排除当前分钟);`get_price(count=..., frequency='1m')` 的截止
   取 `end_date` 当天 0 点前 1 微秒(即前一自然日结束),按 count 取尾部;
   分钟日程只含**交易日**(把周末/假日也排进去会产生虚假 Bar)。

## 代码规范(策略侧)

- 策略侧看到的代码**保留其原始写法**(策略写 `.SS` 就显示 `.SS`,写 `.SH` 就显示 `.SH`);
  内部查询统一转 `.SH/.SZ/.BJ`;委托与成交统一 `.XSHG/.XSHE`。
- 若引擎统一强制一种写法,用另一种写法写的策略会在 `data[code]`/`get_position(code)`
  处静默取不到数据(表现为零成交),排查成本高。
- 策略可见对象需与 quantbt 的 dataclass **同 repr**(`Order`/`Position`/`SimulationParameters`):
  探针大量用 `{}` 直接格式化对象,repr 不同会淹没真正的语义差异。
  `get_order` 返回**列表**(非当日或不存在时为空列表),`get_trades` 返回普通 dict。

## 做法

- 新增语义前先读 quantbt 对应实现(`quantbt/broker.py` 的 `order/_apply_fill/_cap_*`、
  `runtime.py` 的下单包装、`data.py` 的 `history/price/_fold_opening_auction/
  _fill_minute_suspensions`),再写 Go 实现;
- 每修一类语义,补一个对拍探针(`examples/strategies/parity/` 或 quant-data 的
  `strategies/probes/`),探针里用 `log.info` 打印策略可见状态,配合 `--check-logs`
  做状态级对拍;分钟用例参数登记在 `harness/assets/quant-minute-cases.json`;
- 单测(`internal/btengine/engine_test.go`、`minute_test.go`、`internal/query/*_test.go`)
  覆盖引擎内部不变量(费用保留、T+1、撮合价、分钟门户、分区剪枝),探针负责跨引擎一致性。

## 边界

- 公司行动(分红/送转/配股)与 `get_fundamentals` 未实现,相关探针未纳入对拍;
- 分钟数据按交易日滚动窗口加载(默认 30 天),超长区间/大股票池未做内存与速度优化;
- 分钟停牌填充的 `money` 为 NaN,若策略直接比较 NaN 需自行处理(与 PTrade 一致)。

## 关联

- 流程:`harness/workflow/bt-parity.md`
- 参照实现:`E:\AI-work\quant-data\quantbt`(只读);探针:quant-data 的
  `tests/test_ptrade_alignment.py`(38 个用例,分钟级)
- 扫描引擎剪枝:`harness/experience/scan-engine-pruning.md`(跨月窗口丢数据缺陷)
