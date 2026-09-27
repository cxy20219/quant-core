# -*- coding: utf-8 -*-
"""对拍探针 P03:T+1 与可卖数量(get_position 状态)。

- day2: 买入 1000,当日尝试卖出(应因 T+1 无可卖数量而不成交)
- day3: 卖出 1000(隔日可卖)
"""


def initialize(context):
    set_universe(["600570.SS"])
    set_commission(commission_ratio=0.0003, min_commission=5.0)
    set_slippage(0.002)
    g.day = 0


def handle_data(context, data):
    g.day += 1
    if g.day == 2:
        order("600570.SS", 1000)
        pos = get_position("600570.SS")
        log.info("day2 after buy: amount=%s enable=%s", pos.amount, pos.enable_amount)
        order("600570.SS", -1000)
        log.info("day2 sell attempt cash=%.2f", context.portfolio.cash)
    elif g.day == 3:
        pos = get_position("600570.SS")
        log.info("day3 open: amount=%s enable=%s", pos.amount, pos.enable_amount)
        order("600570.SS", -1000)
        log.info("day3 after sell cash=%.2f", context.portfolio.cash)
