package tsapi

// docsHTML 是接口文档页模板(自包含:内联样式,无外部 CDN/JS 依赖)。
const docsHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>quant-core 接口文档</title>
<style>
  :root { --fg:#1f2328; --muted:#656d76; --line:#d0d7de; --bg:#ffffff; --code:#f6f8fa; --accent:#0969da; }
  * { box-sizing: border-box; }
  body { margin:0; font:14px/1.6 -apple-system,"Segoe UI",Roboto,"Helvetica Neue","PingFang SC","Microsoft YaHei",sans-serif; color:var(--fg); background:var(--bg); }
  header { padding:20px 24px; border-bottom:1px solid var(--line); }
  header h1 { margin:0 0 6px; font-size:20px; }
  header p { margin:0; color:var(--muted); }
  nav { padding:12px 24px; border-bottom:1px solid var(--line); display:flex; gap:16px; flex-wrap:wrap; }
  nav a { color:var(--accent); text-decoration:none; }
  main { padding:24px; max-width:1100px; }
  section { margin-bottom:36px; }
  h2 { font-size:17px; border-bottom:1px solid var(--line); padding-bottom:6px; }
  h3 { font-size:15px; margin:18px 0 8px; }
  table { border-collapse:collapse; width:100%; margin:8px 0 14px; }
  th,td { border:1px solid var(--line); padding:6px 8px; text-align:left; vertical-align:top; }
  th { background:var(--code); font-weight:600; }
  code,pre { font-family:ui-monospace,SFMono-Regular,Consolas,"Liberation Mono",monospace; }
  code { background:var(--code); padding:1px 4px; border-radius:4px; }
  pre { background:var(--code); padding:10px 12px; border-radius:6px; overflow:auto; }
  .muted { color:var(--muted); }
  .tag { display:inline-block; background:var(--code); border:1px solid var(--line); border-radius:999px; padding:0 8px; font-size:12px; margin-right:6px; }
  .card { border:1px solid var(--line); border-radius:8px; padding:14px 16px; margin:14px 0; }
  .card h3 { margin-top:0; }
</style>
</head>
<body>
<header>
  <h1>quant-core 接口文档</h1>
  <p>tushare 兼容数据接口 + 回测作业服务 · 本页由接口表与数据集注册表自动生成</p>
</header>
<nav>
  <a href="#overview">概览</a>
  <a href="#data">数据接口({{len .APIs}})</a>
  <a href="#backtest">回测接口</a>
  <a href="#strategy">策略 API</a>
  <a href="#datasets">数据集与字段</a>
  <a href="/docs/openapi.json">OpenAPI JSON</a>
  <a href="/healthz">健康检查</a>
</nav>
<main>

<section id="overview">
  <h2>概览</h2>
  <p>数据接口沿用 tushare 协议:向 <code>POST /</code> 发送 JSON 信封,响应为 <code>{code, msg, data:{fields, items, has_more}}</code>。
  也支持 GET 平铺参数(<code>GET /?api_name=daily&amp;ts_code=600000.SH</code>)。</p>
  <table>
    <tr><th>字段</th><th>说明</th></tr>
    <tr><td><code>api_name</code></td><td>接口名(见下表),必填</td></tr>
    <tr><td><code>params</code></td><td>接口参数对象(各接口参数见卡片)</td></tr>
    <tr><td><code>fields</code></td><td>逗号分隔的输出字段;缺省为接口默认字段集</td></tr>
    <tr><td><code>token</code></td><td>服务启用鉴权时必填(<code>QUANTD_TOKENS</code> 配置)</td></tr>
  </table>
  <p class="muted">分页:<code>limit</code>(行数上限,超过单次上限按上限截断)、<code>offset</code>(跳过行数);
  响应 <code>has_more</code> 指示是否还有更多行。错误响应 <code>code=-1</code> 且 <code>msg</code> 给出原因。</p>
  <pre>curl -s -X POST http://HOST:8000/ -H 'Content-Type: application/json' \
  -d '{"api_name":"daily","params":{"ts_code":"600000.SH","start_date":"20240102","end_date":"20240110"},"fields":"ts_code,trade_date,close"}'</pre>
</section>

<section id="data">
  <h2>数据接口</h2>
  {{range .APIs}}
  <div class="card">
    <h3><code>{{.Name}}</code> <span class="muted">{{.Description}}</span></h3>
    <p class="muted">数据集 <code>{{.Dataset}}</code> · 默认 limit {{.DefaultLimit}} · 单次上限 {{.MaxLimit}}
      {{if .Transforms}} · 单位换算:{{range .Transforms}}<span class="tag">{{.}}</span>{{end}}{{end}}</p>
    <table>
      <tr><th>参数</th><th>类型</th><th>说明</th></tr>
      {{range .Params}}<tr><td><code>{{.Name}}</code></td><td>{{.Type}}</td><td>{{.Desc}}</td></tr>{{end}}
    </table>
    <table>
      <tr><th>输出字段</th><th>类型</th><th>单位</th><th>说明</th></tr>
      {{range .Fields}}<tr><td><code>{{.Name}}</code></td><td>{{.Type}}</td><td>{{.Unit}}</td><td>{{.Desc}}</td></tr>{{end}}
    </table>
    <pre>{{.Example}}</pre>
  </div>
  {{end}}
</section>

<section id="backtest">
  <h2>回测接口</h2>
  <h3>POST /api/backtests — 提交作业</h3>
  <table>
    <tr><th>字段</th><th>类型</th><th>必填</th><th>说明</th></tr>
    <tr><td><code>strategy_code</code></td><td>string</td><td>是</td><td>PTrade 兼容 Python 策略源码(≤500KB)</td></tr>
    <tr><td><code>start_date</code> / <code>end_date</code></td><td>string</td><td>是</td><td>YYYYMMDD 或 YYYY-MM-DD</td></tr>
    <tr><td><code>strategy_name</code></td><td>string</td><td>否</td><td>作业名</td></tr>
    <tr><td><code>params</code></td><td>object</td><td>否</td><td>策略超参数(策略内读 <code>params["x"]</code>)</td></tr>
    <tr><td><code>capital_base</code></td><td>number</td><td>否</td><td>初始资金,默认 1,000,000</td></tr>
    <tr><td><code>frequency</code></td><td>string</td><td>否</td><td><code>1d</code>(默认)/ <code>1m</code></td></tr>
    <tr><td><code>warmup_days</code></td><td>int</td><td>否</td><td>预热交易日/自然日数(分钟模式默认 30)</td></tr>
    <tr><td><code>minute_max_rows</code></td><td>int</td><td>否</td><td>分钟窗口 Bar 上限(默认 300 万,按池大小自动收缩)</td></tr>
    <tr><td><code>benchmark</code></td><td>string</td><td>否</td><td>基准代码(如 000300.SH)</td></tr>
  </table>
  <pre>curl -s -X POST http://HOST:8000/api/backtests -H 'Content-Type: application/json' \
  -d '{"strategy_code":"def initialize(context):\n    set_universe([\"600000.SH\"])\n\ndef handle_data(context, data):\n    pass\n","strategy_name":"demo","start_date":"20240102","end_date":"20240131","frequency":"1d"}'
# → {"id":"...","status":"queued"}</pre>
  <h3>GET /api/backtests/{id} — 查询状态与结果</h3>
  <p>状态机:<code>queued</code> → <code>running</code> → <code>done</code> / <code>failed</code>。
  服务重启会把在途作业标记为 <code>failed</code>(<code>error="服务重启,作业被中断"</code>),不会出现悬空作业。
  结果含净值序列 <code>portfolio</code>、委托 <code>orders</code>、成交 <code>trades</code>、日志 <code>logs</code>、
  概要 <code>summary</code> 与指标 <code>analytics</code>。</p>
  <h3>GET /api/backtests — 作业列表</h3>
  <p>返回 <code>{"records":[{id,status,strategy_name,created_at,summary...}]}</code>。</p>
</section>

<section id="strategy">
  <h2>策略 API(PTrade 子集)</h2>
  <p class="muted">策略在沙箱子进程中执行,以下为可用接口;其余 PTrade 接口(<code>tick_data</code>、
  <code>run_interval</code>、<code>get_snapshot</code> 等)在回测模式下会明确报错。</p>
  <table>
    <tr><th>类别</th><th>接口</th></tr>
    <tr><td>生命周期</td><td><code>initialize(context)</code> / <code>before_trading_start(context, data)</code> /
      <code>handle_data(context, data)</code> / <code>after_trading_end(context, data)</code> /
      <code>run_daily(context, func, time)</code></td></tr>
    <tr><td>交易</td><td><code>order / order_target / order_value / order_target_value / cancel_order</code></td></tr>
    <tr><td>查询</td><td><code>get_order(s) / get_open_orders / get_trades / get_position(s)</code></td></tr>
    <tr><td>行情</td><td><code>get_history(count, frequency, field, security_list, fq, include, fill, is_dict)</code> /
      <code>get_price(...)</code> / <code>get_stock_exrights(code, date)</code> /
      <code>get_fundamentals(security, "valuation", fields, date)</code></td></tr>
    <tr><td>设置</td><td><code>set_universe / set_benchmark / set_commission / set_slippage /
      set_fixed_slippage / set_volume_ratio / set_limit_mode</code></td></tr>
    <tr><td>全局对象</td><td><code>g</code>(属性字典)、<code>log.info/warning/error</code>、<code>params</code>(超参数)</td></tr>
  </table>
</section>

<section id="datasets">
  <h2>数据集与字段(单位口径)</h2>
  <p class="muted">接口输出已换算为 tushare 口径;下表为湖内存储口径(vol=手、amount=千元、*_share=万股、*_mv=万元)。</p>
  {{range .Datasets}}
  <div class="card">
    <h3><code>{{.Name}}</code> <span class="muted">{{.Description}}</span></h3>
    <p class="muted">分区:{{.Partitions}}</p>
    <table>
      <tr><th>字段</th><th>类型</th><th>单位</th><th>说明</th></tr>
      {{range .Fields}}<tr><td><code>{{.Name}}</code></td><td>{{.Type}}</td><td>{{.Unit}}</td><td>{{.Desc}}</td></tr>{{end}}
    </table>
  </div>
  {{end}}
</section>

</main>
</body>
</html>
`
