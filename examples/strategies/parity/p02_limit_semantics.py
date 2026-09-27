# -*- coding: utf-8 -*-
"""对拍探针 P02:限价单语义(挂单当日撤销 / 可成交限价 / 成交量上限)。

- day2: 限价买单(远低于市价,不可成交)→ 当日 after_trading_end 撤销
- day3: 限价买单(高于市价,可成交)→ 立即成交
- day5: 市价卖出
"""


def initialize(context):
    set_universe(["600570.SS"])
    set_commission(commission_ratio=0.0003, min_commission=5.0)
    set_slippage(0.002)
    g.day = 0


def handle_data(context, data):
    g.day += 1
    bar = data["600570.SS"]
    if g.day == 2:
        order("600570.SS", 1000, limit_price=round(bar.close * 0.8, 2))
        log.info("LIMIT-LOW buy @%.2f close=%.2f", bar.close * 0.8, bar.close)
    elif g.day == 3:
        orders = get_open_orders("600570.SS")
        log.info("open orders at day3 open: %s", len(orders))
        order("600570.SS", 1000, limit_price=round(bar.close * 1.05, 2))
        log.info("LIMIT-HIGH buy @%.2f close=%.2f", bar.close * 1.05, bar.close)
    elif g.day == 5:
        order("600570.SS", -1000)
        log.info("SELL day5")
