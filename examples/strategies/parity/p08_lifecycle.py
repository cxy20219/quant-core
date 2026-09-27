# -*- coding: utf-8 -*-
"""对拍探针 P08:生命周期与 run_daily。

- initialize / before_trading_start / handle_data / after_trading_end 各阶段日志
- run_daily 注册的回调在 handle_data 之后触发
- 历史查询 get_history(含 include 与复权)在盘前/盘中可用
"""


def initialize(context):
    set_universe(["600570.SS"])
    set_commission(commission_ratio=0.0003, min_commission=5.0)
    g.day = 0
    run_daily(context, rebalance, time="15:00")


def before_trading_start(context, data):
    g.day += 1
    hist = get_history(3, "1d", "close", "600570.SS")
    log.info("day%s BTS hist=%s", g.day, list(hist["close"]) if len(hist) else None)


def handle_data(context, data):
    log.info("day%s HD close=%.2f", g.day, data["600570.SS"].close)


def after_trading_end(context, data):
    pos = get_position("600570.SS")
    log.info("day%s ATE amount=%s", g.day, pos.amount)


def rebalance(context):
    if g.day == 2:
        order("600570.SS", 1000)
        log.info("day2 RUN_DAILY buy")
