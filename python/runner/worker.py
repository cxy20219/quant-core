#!/usr/bin/env python3
"""quant-core Python 策略执行器(PTrade 子集,与 Go 回测引擎通过 stdio JSON-RPC 通信)。

协议(逐行 JSON):

  Go → Python
    {"type":"init","strategy_source":"...","params":{...},"meta":{...}}
    {"type":"phase","name":"before_trading_start"|"run_daily"|"after_trading_end"}
    {"type":"bar","day":"2024-01-02","bars":{"600000.SH":{...}}}
    {"type":"shutdown"}
    {"type":"result","id":N,"result":...,"error":null}     # 对 call 的应答

  Python → Go
    {"type":"ready"}                                        # initialize 完成
    {"type":"call","id":N,"method":"order","params":{...}}
    {"type":"log","level":"info","message":"..."}
    {"type":"done"}                                         # 当前回调结束
    {"type":"error","message":"..."}

策略可用的 PTrade 同名 API:g / log / order / order_target / order_value /
order_target_value / cancel_order / get_order(s) / get_open_orders / get_trades /
get_position(s) / get_history / get_price / get_stock_exrights /
set_universe / set_benchmark / set_commission / set_slippage / set_fixed_slippage /
set_volume_ratio / set_limit_mode / run_daily
"""

from __future__ import annotations

import json
import sys
import traceback
from collections import OrderedDict

try:
    import pandas as pd
except ImportError:  # pragma: no cover
    pd = None


def _send(msg: dict) -> None:
    sys.stdout.write(json.dumps(msg, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def _read() -> dict:
    line = sys.stdin.readline()
    if not line:
        raise EOFError("父进程已关闭")
    return json.loads(line)


# Windows 下 Python 默认用本地编码读写管道,必须显式切到 UTF-8(策略含中文时尤其重要)
try:
    sys.stdin.reconfigure(encoding="utf-8", errors="strict")
    sys.stdout.reconfigure(encoding="utf-8", errors="strict")
except Exception:  # noqa: BLE001 - 非 TTY 或旧版本时忽略
    pass


class RPC:
    def __init__(self) -> None:
        self._seq = 0

    def call(self, method: str, **params):
        self._seq += 1
        req_id = self._seq
        _send({"type": "call", "id": req_id, "method": method, "params": params})
        while True:
            msg = _read()
            if msg.get("type") == "result" and msg.get("id") == req_id:
                if msg.get("error"):
                    raise RuntimeError(msg["error"])
                return msg.get("result")
            if msg.get("type") == "shutdown":
                raise EOFError("父进程要求退出")
            # 忽略其它类型的消息


_rpc = RPC()
_context = None  # 由 main 注入;下单后原地下钻刷新账户快照(PTrade:现金/持仓即时变化)


def _refresh_portfolio():
    if _context is None:
        return
    snapshot = _rpc.call("portfolio")
    portfolio = _context.get("portfolio")
    if portfolio is None:
        _context["portfolio"] = _record(snapshot)
    else:
        portfolio.clear()
        portfolio.update(_record(snapshot))


# ── PTrade API(全局函数,策略直接调用)─────────────────────────────────

def order(security, amount, limit_price=None):
    result = _rpc.call("order", security=security, amount=int(amount), limit_price=limit_price)
    _refresh_portfolio()
    return result


def order_target(security, amount, limit_price=None):
    result = _rpc.call("order_target", security=security, amount=int(amount), limit_price=limit_price)
    _refresh_portfolio()
    return result


def order_value(security, value, limit_price=None):
    result = _rpc.call("order_value", security=security, value=float(value), limit_price=limit_price)
    _refresh_portfolio()
    return result


def order_target_value(security, value, limit_price=None):
    result = _rpc.call("order_target_value", security=security, value=float(value), limit_price=limit_price)
    _refresh_portfolio()
    return result


def cancel_order(order_param):
    order_id = order_param.get("id") if isinstance(order_param, dict) else getattr(order_param, "id", order_param)
    result = _rpc.call("cancel_order", order_id=str(order_id))
    _refresh_portfolio()
    return result


def get_order(order_id):
    return _record(_rpc.call("get_order", order_id=str(order_id)))


def get_orders(security=None):
    return [_record(o) for o in _rpc.call("get_orders", security=security)]


def get_open_orders(security=None):
    return [_record(o) for o in _rpc.call("get_open_orders", security=security)]


def get_trades():
    raw = _rpc.call("get_trades")
    out = OrderedDict()
    for order_id, rows in raw.items():
        out[order_id] = [tuple(r) for r in rows]
    return out


def get_position(security):
    return _record(_rpc.call("get_position", security=security))


def get_positions(security=None):
    raw = _rpc.call("get_positions", security=security)
    return {code: _record(pos) for code, pos in raw.items()}


def get_history(count, frequency="1d", field="close", security_list=None, fq=None,
                include=False, fill="nan", is_dict=False):
    result = _rpc.call(
        "history",
        count=int(count),
        frequency=frequency,
        field=field,
        security_list=_as_list(security_list),
        fq=fq,
        include=bool(include),
        fill=fill,
    )
    return _shape_history(result, field, is_dict, single=isinstance(security_list, str))


def get_price(security, start_date=None, end_date=None, frequency="1d", fields=None,
              fq=None, count=None, is_dict=False):
    result = _rpc.call(
        "price",
        security=security,
        start_date=start_date,
        end_date=end_date,
        frequency=frequency,
        fields=_as_list(fields),
        fq=fq,
        count=count,
    )
    return _shape_history(result, None, is_dict, single=True)


def get_stock_exrights(stock_code, date=None):
    return _rpc.call("get_stock_exrights", security=stock_code, date=date)


def set_universe(security_list):
    return _rpc.call("set_universe", securities=_as_list(security_list))


def set_benchmark(sids):
    return _rpc.call("set_benchmark", securities=_as_list(sids))


def set_commission(commission_ratio=0.0003, min_commission=5.0, type="STOCK"):
    return _rpc.call("set_commission", commission_ratio=float(commission_ratio),
                     min_commission=float(min_commission), commission_type=type)


def set_slippage(slippage=0.1):
    return _rpc.call("set_slippage", slippage=float(slippage))


def set_fixed_slippage(fixedslippage=0.0):
    return _rpc.call("set_fixed_slippage", slippage=float(fixedslippage))


def set_volume_ratio(volume_ratio=0.25):
    return _rpc.call("set_volume_ratio", volume_ratio=float(volume_ratio))


def set_limit_mode(limit_mode="LIMIT"):
    return _rpc.call("set_limit_mode", limit_mode=limit_mode)


def run_daily(context, func, time="9:31"):
    if getattr(context, "_runner", None) is None:
        raise RuntimeError("run_daily 必须在 initialize 内调用")
    context._runner.append((func, str(time)))
    return None


# ── 内部工具 ────────────────────────────────────────────────────────────

def _as_list(value):
    if value is None:
        return None
    if isinstance(value, str):
        return [value]
    return [str(v) for v in value]


class AttrDict(dict):
    """支持属性访问的字典(PTrade 的 g / context / 返回值都是这种风格)。"""

    def __getattr__(self, name):
        try:
            return self[name]
        except KeyError as exc:  # pragma: no cover
            raise AttributeError(name) from exc

    def __setattr__(self, name, value):
        self[name] = value


def _record(value):
    if isinstance(value, dict):
        return AttrDict({k: _record(v) for k, v in value.items()})
    if isinstance(value, list):
        return [_record(v) for v in value]
    return value


def _shape_history(result, field, is_dict, single):
    """按 PTrade 语义整理 get_history 返回:

    - is_dict=True:  OrderedDict{code: numpy 记录数组}(datetime 为 YYYYMMDD 整数);
    - 单支(security_list 为字符串): DataFrame,index=日期,columns=行情字段;
    - 多支(security_list 为列表): DataFrame,index=日期,columns 含 code 与行情字段。
    """
    dates = result.get("dates") or []
    securities = result.get("securities") or []
    fields = result.get("fields") or ([field] if field else [])
    data = result.get("data") or {}

    def date_ints():
        out = []
        for d in dates:
            compact = d.replace("-", "")
            if " " in d:
                time_part = d.split(" ", 1)[1].replace(":", "")
                out.append(int(compact[:8] + time_part[:4]))
            else:
                out.append(int(compact[:8]))
        return out

    if is_dict:
        out = OrderedDict()
        dts = date_ints()
        for code in securities:
            cols = data.get(code, {})
            if pd is None:
                out[code] = {"datetime": dts, **{f: cols.get(f, []) for f in fields}}
            else:
                frame = pd.DataFrame({"datetime": pd.Series(dts, dtype="int64"),
                                      **{f: cols.get(f, []) for f in fields}})
                out[code] = frame.to_records(index=False)
        return out

    if pd is None:
        return {"index": dates, "securities": securities, "fields": fields, "data": data}
    index = pd.to_datetime(dates)
    if single and len(securities) == 1:
        frame = pd.DataFrame({f: data.get(securities[0], {}).get(f, []) for f in fields}, index=index)
        frame.index.name = "datetime"
        return frame
    rows = []
    codes = []
    for code in securities:
        cols = data.get(code, {})
        for i in range(len(dates)):
            codes.append(code)
            rows.append([cols.get(f, [None] * len(dates))[i] if i < len(cols.get(f, [])) else None for f in fields])
    frame = pd.DataFrame(rows, columns=fields, index=index.repeat(len(securities)) if len(securities) > 1 else index)
    if len(securities) > 1:
        frame.insert(0, "code", codes)
    frame.index.name = "datetime"
    return frame


class Runner:
    def __init__(self) -> None:
        self.name = "runner"
        self.daily_jobs = []  # [(func, time)]
        self.env = {}

    # ── 生命周期 ──
    def load(self, source: str, params: dict) -> None:
        import types as _types
        module = _types.ModuleType("strategy")
        self.env = {
            "__name__": "strategy",
            "g": AttrDict({}),
            "log": _Logger(),
            "order": order, "order_target": order_target,
            "order_value": order_value, "order_target_value": order_target_value,
            "cancel_order": cancel_order,
            "get_order": get_order, "get_orders": get_orders, "get_open_orders": get_open_orders,
            "get_trades": get_trades,
            "get_position": get_position, "get_positions": get_positions,
            "get_history": get_history, "get_price": get_price,
            "get_stock_exrights": get_stock_exrights,
            "set_universe": set_universe, "set_benchmark": set_benchmark,
            "set_commission": set_commission, "set_slippage": set_slippage,
            "set_fixed_slippage": set_fixed_slippage,
            "set_volume_ratio": set_volume_ratio, "set_limit_mode": set_limit_mode,
            "run_daily": run_daily,
            "params": dict(params or {}),
            "g_params": dict(params or {}),
            "_types": _types,
        }
        code = compile(source, "<strategy>", "exec")
        exec(code, self.env)  # noqa: S102 - 策略代码由调用方提供
        if not callable(self.env.get("initialize")):
            raise RuntimeError("PTrade 策略必须定义 initialize(context)")
        if not callable(self.env.get("handle_data")):
            raise RuntimeError("PTrade 策略必须定义 handle_data(context, data)")

    def initialize(self, context) -> None:
        context._runner = self.daily_jobs
        self.env["initialize"](context)
        context._runner = None

    def before_trading_start(self, context) -> None:
        func = self.env.get("before_trading_start")
        if callable(func):
            func(context, {})

    def handle_data(self, context, data) -> None:
        self.env["handle_data"](context, data)

    def run_daily(self, context) -> None:
        for func, _time in self.daily_jobs:
            func(context)

    def after_trading_end(self, context) -> None:
        func = self.env.get("after_trading_end")
        if callable(func):
            func(context, {})


class _Logger:
    def info(self, message, *args):
        _send({"type": "log", "level": "INFO", "message": _fmt(message, args)})

    def warning(self, message, *args):
        _send({"type": "log", "level": "WARNING", "message": _fmt(message, args)})

    warn = warning

    def error(self, message, *args):
        _send({"type": "log", "level": "ERROR", "message": _fmt(message, args)})


def _fmt(message, args) -> str:
    text = str(message)
    if args:
        try:
            text = text % args
        except Exception:  # noqa: BLE001
            text = " ".join([text, *[str(a) for a in args]])
    return text


def main() -> int:
    global _context
    runner = Runner()
    context = AttrDict({
        "capital_base": 0.0,
        "previous_date": None,
        "portfolio": AttrDict({}),
        "blotter": AttrDict({"current_dt": None}),
        "_runner": None,
    })
    try:
        init = _read()
        if init.get("type") != "init":
            raise RuntimeError(f"期望 init,收到 {init.get('type')}")
        meta = init.get("meta") or {}
        _context = context
        context["capital_base"] = meta.get("capital_base", 0.0)
        runner.load(init.get("strategy_source", ""), init.get("params") or {})
        runner.initialize(context)
        _send({"type": "ready"})
        while True:
            msg = _read()
            mtype = msg.get("type")
            if mtype == "shutdown":
                break
            if mtype == "bar":
                context["blotter"]["current_dt"] = msg.get("day")
                context["previous_date"] = msg.get("previous_day")
                context["portfolio"] = _record(msg.get("portfolio") or {})
                bars = {code: _record(bar) for code, bar in (msg.get("bars") or {}).items()}
                runner.handle_data(context, bars)
                _send({"type": "done"})
            elif mtype == "phase":
                name = msg.get("name")
                if name == "before_trading_start":
                    context["blotter"]["current_dt"] = msg.get("day")
                    context["portfolio"] = _record(msg.get("portfolio") or {})
                    runner.before_trading_start(context)
                elif name == "run_daily":
                    runner.run_daily(context)
                elif name == "after_trading_end":
                    runner.after_trading_end(context)
                _send({"type": "done"})
            else:
                _send({"type": "error", "message": f"未知消息类型 {mtype}"})
    except EOFError:
        return 0
    except Exception as exc:  # noqa: BLE001
        _send({
            "type": "error",
            "message": f"{type(exc).__name__}: {exc}",
            "traceback": traceback.format_exc()[-2000:],
        })
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
