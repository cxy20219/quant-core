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
from datetime import date as _date
from datetime import datetime as _datetime

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
    snapshot = _portfolio_snapshot(_rpc.call("portfolio"))
    portfolio = _context.get("portfolio")
    if portfolio is None:
        _context["portfolio"] = snapshot
    else:
        portfolio.clear()
        portfolio.update(snapshot)


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
    if isinstance(order_param, dict):
        order_id = order_param.get("id")
    else:
        order_id = getattr(order_param, "id", order_param)
    result = _rpc.call("cancel_order", order_id=str(order_id))
    _refresh_portfolio()
    return result


def get_order(order_id):
    """quantbt 语义:返回当日该委托的列表(不存在或非当日为空列表)。"""
    return [Order(o) for o in _rpc.call("get_order", order_id=str(order_id))]


def get_orders(security=None):
    return [Order(o) for o in _rpc.call("get_orders", security=security)]


def get_open_orders(security=None):
    return [Order(o) for o in _rpc.call("get_open_orders", security=security)]


def get_trades():
    """quantbt 语义:普通 dict{order_id: [成交行...]},成交行为列表。"""
    out = {}
    for order_id, rows in _rpc.call("get_trades"):
        out[order_id] = [
            [r[0], r[1], r[2], r[3], float(r[4]), float(r[5]), float(r[6]), r[7]]
            for r in rows
        ]
    return out


def get_position(security):
    return Position(_rpc.call("get_position", security=security))


def get_positions(security=None):
    raw = _rpc.call("get_positions", security=security)
    return {code: Position(pos) for code, pos in raw.items()}


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
    """除权除息事件(与 quantbt 一致:DataFrame,index 为 YYYYMMDD 整数;无事件返回 None)。"""
    result = _rpc.call("get_stock_exrights", security=stock_code, date=date)
    if not result:
        return None
    fields = result.get("fields") or []
    rows = result.get("rows") or []
    dates = result.get("dates") or []
    if pd is None:
        return {"index": dates, "fields": fields, "rows": rows}
    frame = pd.DataFrame([[float(value) for value in row] for row in rows], columns=fields)
    frame.index = pd.Index(dates, name="date")
    return frame


def get_fundamentals(security, table, fields=None, date=None, start_year=None, end_year=None,
                     report_types=None, date_type=None, merge_type=None):
    """估值表(与 quantbt 一致:仅支持 date 模式的 table="valuation")。"""
    if any(value is not None for value in (start_year, end_year, report_types, date_type, merge_type)):
        raise RuntimeError("本地估值数据仅支持 get_fundamentals 的 date 模式")
    if table != "valuation":
        raise RuntimeError('本地数据仅支持 get_fundamentals(..., "valuation", ...)')
    requested = [fields] if isinstance(fields, str) else list(fields or [])
    mapping = {
        "total_value": ("total_mv", 10000.0),
        "float_value": ("circ_mv", 10000.0),
        "total_shares": ("total_share", 10000.0),
        "a_shares": ("total_share", 10000.0),
        "a_floats": ("float_share", 10000.0),
        "pe_dynamic": ("pe", 1.0),
        "pe_static": ("pe", 1.0),
        "pe_ttm": ("pe_ttm", 1.0),
        "pb": ("pb", 1.0),
        "ps": ("ps", 1.0),
        "ps_ttm": ("ps_ttm", 1.0),
        "turnover_rate": ("turnover_rate", 1.0),
        "dividend_ratio": ("dv_ttm", 1.0),
    }
    unknown = sorted(set(requested) - set(mapping))
    if unknown:
        raise RuntimeError("不支持的估值字段: {}".format(unknown))
    query_date = date
    if query_date is None:
        current_dt = _context["blotter"]["current_dt"] if _context else None
        query_date = current_dt.strftime("%Y-%m-%d") if current_dt is not None else None
    rows = _rpc.call("get_fundamentals", securities=_as_list(security), table=table,
                     fields=requested, date=query_date)
    wanted = list(dict.fromkeys(["total_value", *requested]))
    columns = ["trading_day", "total_value", *[f for f in requested if f != "total_value"]]
    if pd is None:
        return {"rows": rows or [], "columns": columns}
    if not rows:
        return pd.DataFrame(columns=columns, index=pd.Index([], name="secu_code"))
    index = pd.Index([row["code"] for row in rows], name="secu_code")
    result = pd.DataFrame(index=index)
    result["trading_day"] = [row["trading_day"] for row in rows]
    for field in wanted:
        source, multiplier = mapping[field]
        values = [float(row.get(source) or 0.0) * multiplier for row in rows]
        if field in ("turnover_rate", "dividend_ratio"):
            result[field] = ["{:.6f}%".format(value) for value in values]
        else:
            result[field] = values
    return result.loc[:, columns]


def _unsupported_trade_api(name):
    """回测不支持的下单/行情 API(与 quantbt 的 unsupported_trade_api 一致)。"""
    def _raise(*_args, **_kwargs):
        raise RuntimeError("{} is not available in PTrade backtest mode".format(name))
    return _raise


def run_interval(context, func, time="every_bar", interval="1m"):
    return _unsupported_trade_api("run_interval")()


def get_snapshot(security):
    return _unsupported_trade_api("get_snapshot")()


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
    scheduled = str(time).strip()
    parts = scheduled.split(":")
    if len(parts) != 2:
        raise RuntimeError("run_daily time 需为 HH:MM")
    hour, minute = int(parts[0]), int(parts[1])
    # PTrade:13:00 不是可见分钟,顺延到 13:01
    if hour == 13 and minute == 0:
        scheduled = "13:01"
    else:
        scheduled = "%02d:%02d" % (hour, minute)
    context._runner.append((func, scheduled))
    return None


# ── 内部工具 ────────────────────────────────────────────────────────────

def _as_list(value):
    if value is None:
        return None
    if isinstance(value, str):
        return [value]
    return [str(v) for v in value]


def _parse_dt(text):
    """把时钟文本解析为 datetime(与 PTrade 的 context.blotter.current_dt 一致)。"""
    if not text:
        return None
    for fmt in ("%Y-%m-%d %H:%M:%S", "%Y-%m-%d %H:%M", "%Y-%m-%d"):
        try:
            return _datetime.strptime(text, fmt)
        except ValueError:
            continue
    return None


def _parse_date(text):
    if not text:
        return None
    parsed = _parse_dt(text)
    return parsed.date() if parsed else None


class AttrDict(dict):
    """支持属性访问的字典(PTrade 的 g / context / 返回值都是这种风格)。"""

    def __getattr__(self, name):
        try:
            return self[name]
        except KeyError as exc:  # pragma: no cover
            raise AttributeError(name) from exc

    def __setattr__(self, name, value):
        self[name] = value


class PTradeObject:
    """PTrade 风格对象:属性与下标双访问,repr 与 quantbt 数据类一致。"""

    _fields = ()
    _type = ""

    def __init__(self, data):
        self._data = dict(data)
        for name in self._fields:
            setattr(self, name, self._data.get(name))

    def __getitem__(self, key):
        return self._data[key]

    def get(self, key, default=None):
        return self._data.get(key, default)

    def __repr__(self):
        inner = ", ".join("%s=%r" % (name, getattr(self, name)) for name in self._fields)
        return "%s(%s)" % (self._type, inner)


class Order(PTradeObject):
    """委托对象(字段与 quantbt.objects.Order 一致)。"""

    _type = "Order"
    _fields = ("id", "dt", "limit", "symbol", "amount", "created", "filled",
               "entrust_no", "cancel_entrust_no", "priceGear", "status")

    def __init__(self, data):
        dt = _parse_dt(data.get("created_at"))
        limit = None
        if str(data.get("order_type")) == "limit":
            limit = float(data.get("limit_price") or 0.0)
        super().__init__({
            "id": data.get("id"),
            "dt": dt,
            "limit": limit,
            "symbol": data.get("security"),
            "amount": int(data.get("amount") or 0),
            "created": dt,
            "filled": int(data.get("filled") or 0),
            "entrust_no": None,
            "cancel_entrust_no": None,
            "priceGear": 0,
            "status": str(data.get("status") or "0"),
        })


class SimulationParameters(PTradeObject):
    """回测参数(字段与 quantbt.objects.SimulationParameters 一致)。"""

    _type = "SimulationParameters"
    _fields = ("capital_base", "data_frequency")


class Position(PTradeObject):
    """持仓对象(字段与 quantbt.objects.Position 一致)。"""

    _type = "Position"
    _fields = ("sid", "enable_amount", "amount", "last_sale_price", "cost_basis",
               "business_type", "today_amount", "update_time")

    def __init__(self, data):
        super().__init__({
            "sid": data.get("security"),
            "enable_amount": int(data.get("enable_amount") or 0),
            "amount": int(data.get("amount") or 0),
            "last_sale_price": float(data.get("last_sale_price") or 0.0),
            "cost_basis": float(data.get("cost_basis") or 0.0),
            "business_type": "stock",
            "today_amount": int(data.get("today_amount") or 0),
            "update_time": None,
        })


class BarDict:
    """当分钟/日线行情:与 quantbt 的 BarDict 表面一致([] / in / 迭代),
    列式载荷按需构建单个 bar(大股票池下避免每分钟构造全部对象)。"""

    __slots__ = ("_index", "_series", "_dt", "_cache")

    def __init__(self, codes, series, dt):
        self._index = {code: i for i, code in enumerate(codes or [])}
        self._series = series or {}
        self._dt = dt
        self._cache = {}

    def __getitem__(self, security):
        cached = self._cache.get(security)
        if cached is not None:
            return cached
        idx = self._index[security]  # 缺失时 KeyError(与 quantbt 一致)
        series = self._series
        bar = AttrDict({
            "dt": self._dt,
            "open": float(series["open"][idx]),
            "close": float(series["close"][idx]),
            "price": float(series["price"][idx]),
            "low": float(series["low"][idx]),
            "high": float(series["high"][idx]),
            "volume": float(series["volume"][idx]),
            "money": _nan_or_float(series["money"][idx]),
        })
        self._cache[security] = bar
        return bar

    def __contains__(self, security):
        return security in self._index

    def __iter__(self):
        return iter(self._index)

    def __len__(self):
        return len(self._index)

    def keys(self):
        return self._index.keys()

    def items(self):
        for code in self._index:
            yield code, self[code]

    def values(self):
        for code in self._index:
            yield self[code]


def _nan_or_float(value):
    """列式载荷中 money 可能为 null(停牌),还原为 NaN。"""
    return float("nan") if value is None else float(value)


def _bar_record(bar):
    """行情切片:dt 解析为 datetime,数值转 float,缺失值还原为 NaN(与 quantbt 的 bar 一致)。"""
    numeric = {"open", "close", "price", "low", "high", "volume", "money"}
    out = AttrDict()
    for key, value in (bar or {}).items():
        if key == "dt":
            out[key] = _parse_dt(value)
        elif value is None:
            out[key] = float("nan")
        elif key in numeric:
            out[key] = float(value)
        else:
            out[key] = value
    return out


def _portfolio_snapshot(data):
    """把 Go 侧账户快照转为 PTrade 风格对象(持仓为 Position)。"""
    out = AttrDict()
    for key, value in (data or {}).items():
        if key == "positions":
            out[key] = {code: Position(pos) for code, pos in (value or {}).items()}
        else:
            out[key] = _record(value)
    return out


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
            "get_fundamentals": get_fundamentals,
            "set_universe": set_universe, "set_benchmark": set_benchmark,
            "set_commission": set_commission, "set_slippage": set_slippage,
            "set_fixed_slippage": set_fixed_slippage,
            "set_volume_ratio": set_volume_ratio, "set_limit_mode": set_limit_mode,
            "run_daily": run_daily,
            "run_interval": run_interval, "get_snapshot": get_snapshot,
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

    def run_daily(self, context, current_time: str = "") -> None:
        """日线模式:current_time 为空,全部执行;分钟模式:仅执行到点的回调。"""
        for func, scheduled in self.daily_jobs:
            if current_time and scheduled != current_time:
                continue
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
        frequency = str(meta.get("frequency") or "1d")
        context["sim_params"] = SimulationParameters({
            "capital_base": meta.get("capital_base", 0.0),
            "data_frequency": frequency,
        })
        runner.load(init.get("strategy_source", ""), init.get("params") or {})
        runner.initialize(context)
        _send({"type": "ready"})
        while True:
            msg = _read()
            mtype = msg.get("type")
            if mtype == "shutdown":
                break
            if mtype == "bar":
                context["blotter"]["current_dt"] = _parse_dt(msg.get("day"))
                context["previous_date"] = _parse_date(msg.get("previous_day"))
                context["portfolio"] = _portfolio_snapshot(msg.get("portfolio"))
                bars = BarDict(msg.get("codes") or [], msg.get("series") or {},
                               _parse_dt(msg.get("day")))
                runner.handle_data(context, bars)
                _send({"type": "done"})
            elif mtype == "phase":
                name = msg.get("name")
                if name == "before_trading_start":
                    context["blotter"]["current_dt"] = _parse_dt(msg.get("day"))
                    context["previous_date"] = _parse_date(msg.get("previous_day"))
                    context["portfolio"] = _portfolio_snapshot(msg.get("portfolio"))
                    runner.before_trading_start(context)
                elif name == "run_daily":
                    context["blotter"]["current_dt"] = _parse_dt(msg.get("day")) or context["blotter"]["current_dt"]
                    runner.run_daily(context, msg.get("time") or "")
                elif name == "after_trading_end":
                    context["blotter"]["current_dt"] = _parse_dt(msg.get("day")) or context["blotter"]["current_dt"]
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
