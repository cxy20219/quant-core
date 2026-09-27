# 回测语义对拍要点(Go 引擎 ↔ quantbt)

## 现象

两个引擎在相同策略/数据下结果不一致时,差异通常来自以下六类语义细节;
逐条按 quantbt 实现对齐后,日线 8 探针可全通过(净值/委托/成交/日志)。

## 六类语义(按踩坑顺序)

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

## 代码规范(策略侧)

- 策略侧看到的代码**保留其原始写法**(策略写 `.SS` 就显示 `.SS`,写 `.SH` 就显示 `.SH`);
  内部查询统一转 `.SH/.SZ/.BJ`;委托与成交统一 `.XSHG/.XSHE`。
- 若引擎统一强制一种写法,用另一种写法写的策略会在 `data[code]`/`get_position(code)`
  处静默取不到数据(表现为零成交),排查成本高。

## 做法

- 新增语义前先读 quantbt 对应实现(`quantbt/broker.py` 的 `order/_apply_fill/_cap_*`、
  `data.py` 的 `history/_daily_frame`),再写 Go 实现;
- 每修一类语义,补一个对拍探针(`examples/strategies/parity/`),探针里用 `log.info`
  打印策略可见状态,配合 `--check-logs` 做状态级对拍;
- 单测(`internal/btengine/engine_test.go`)覆盖引擎内部不变量(费用保留、T+1、
  撮合价),探针负责跨引擎一致性。

## 边界

- 分钟频率的语义(T+1 盘中、Bar 内共享成交量、`13:00` 不触发、午盘首分钟等)
  尚未移植,见 `harness/workflow/bt-parity.md` 的未覆盖清单;
- 公司行动(分红/送转/配股)未实现。

## 关联

- 流程:`harness/workflow/bt-parity.md`
- 参照实现:`E:\AI-work\quant-data\quantbt`(只读);探针:quant-data 的
  `tests/test_ptrade_alignment.py`(38 个用例,分钟级)
