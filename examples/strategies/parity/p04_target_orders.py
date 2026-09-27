# -*- coding: utf-8 -*-
"""对拍探针 P04:目标单与手数取整。

- day2: order_target(1234) → 向下取整到 1200
- day3: order_target_value(50000) → 按市值换算并取整
- day5: order_target(0) → 清仓
- day7: order_value(30000) → 按金额买入
"""


def initialize(context):
    set_universe(["600570.SS"])
    set_commission(commission_ratio=0.0003, min_commission=5.0)
    set_slippage(0.002)
    g.day = 0


def handle_data(context, data):
    g.day += 1
    if g.day == 2:
        order_target("600570.SS", 1234)
        log.info("day2 target 1234 amount=%s", get_position("600570.SS").amount)
    elif g.day == 3:
        order_target_value("600570.SS", 50000)
        log.info("day3 target_value 50000 amount=%s", get_position("600570.SS").amount)
    elif g.day == 5:
        order_target("600570.SS", 0)
        log.info("day5 close amount=%s", get_position("600570.SS").amount)
    elif g.day == 7:
        order_value("600570.SS", 30000)
        log.info("day7 order_value 30000 amount=%s", get_position("600570.SS").amount)
