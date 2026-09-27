# -*- coding: utf-8 -*-
"""对拍探针 P05:资金上限与成交量上限。

- day2: 买单远超可用资金 → 按可负担手数下单(不部分成交)
- day3: 买单远超当日成交量×volume_ratio → 部分成交并自动撤余量(status 6)
- day5: 市价卖出(可能受成交量上限约束)
"""


def initialize(context):
    set_universe(["600570.SS"])
    set_commission(commission_ratio=0.0003, min_commission=5.0)
    set_slippage(0.002)
    set_volume_ratio(0.25)
    g.day = 0


def handle_data(context, data):
    g.day += 1
    if g.day == 2:
        # 资金上限:100000 资金买 100000 股(必然不足)
        order("600570.SS", 100000)
        pos = get_position("600570.SS")
        log.info("day2 cash-capped: amount=%s cash=%.2f", pos.amount, context.portfolio.cash)
    elif g.day == 3:
        # 成交量上限:远超当日成交量的买入
        order("600570.SS", 100000)
        log.info("day3 volume-capped: amount=%s", get_position("600570.SS").amount)
    elif g.day == 5:
        order("600570.SS", -get_position("600570.SS").amount)
        log.info("day5 sell all: amount=%s", get_position("600570.SS").amount)
