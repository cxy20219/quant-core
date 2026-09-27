// quantd 是量化数据服务的主程序。
//
// 子命令:
//
//	quantd serve   --lake <dir> --registry <yaml> --listen :8000
//	quantd migrate --src <old-lake> --lake <dir> [--dataset bars_daily] [--year 2024] [--jobs 8]
//	quantd import  --dataset stock_basic [--source relay-b] [--start 20240101] [--end 20240131]
//	quantd source  list|check [name]
//	quantd verify  --lake <dir> [--dataset bars_daily]
//	quantd apis    --registry <yaml>
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/parquet-go/parquet-go"

	"quant-core/internal/btengine"
	"quant-core/internal/btserver"
	"quant-core/internal/btworker"
	"quant-core/internal/ingest"
	"quant-core/internal/lake"
	"quant-core/internal/schema"
	"quant-core/internal/source"
	"quant-core/internal/tsapi"
)

func main() {
	log.SetFlags(log.LstdFlags)
	loadDotEnv(".env")
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "migrate":
		cmdMigrate(os.Args[2:])
	case "import":
		cmdImport(os.Args[2:])
	case "source":
		cmdSource(os.Args[2:])
	case "backtest":
		cmdBacktest(os.Args[2:])
	case "dedupe":
		cmdDedupe(os.Args[2:])
	case "verify":
		cmdVerify(os.Args[2:])
	case "apis":
		cmdAPIs(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `quantd - 量化数据服务

用法:
  quantd serve   --lake <dir> --registry <yaml> --listen :8000
  quantd migrate --src <old-lake> --lake <dir> [--dataset bars_daily] [--year 2024] [--jobs 8]
  quantd import  --dataset stock_basic [--source relay-b] [--start 20240101] [--end 20240131] [--replace]
  quantd source  list | check [name]
  quantd verify  --lake <dir> [--dataset bars_daily]
  quantd apis    --registry <yaml>
`)
}

// loadDotEnv 读取 .env(不入库)并注入环境变量;已存在的环境变量优先。
func loadDotEnv(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
}

func loadRegistry(path string) *schema.Registry {
	reg, err := schema.Load(path)
	if err != nil {
		log.Fatalf("load registry %s: %v", path, err)
	}
	return reg
}

func loadSourceRegistry(path string, logf func(string, ...any)) *source.Registry {
	reg, err := source.LoadRegistry(path)
	if err != nil {
		log.Fatalf("load sources %s: %v", path, err)
	}
	if logf != nil {
		reg.SetSourceLoggers(logf)
		for _, info := range reg.Infos() {
			if info.Err != nil {
				logf("源 %s 不可用: %v", info.Name, info.Err)
			}
		}
	}
	return reg
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	lakeDir := fs.String("lake", "lake", "数据湖根目录")
	registryPath := fs.String("registry", filepath.Join("schemas", "datasets.yaml"), "数据集注册表")
	listen := fs.String("listen", ":8000", "监听地址")
	tokens := fs.String("tokens", os.Getenv("QUANTD_TOKENS"), "逗号分隔的访问令牌(默认读 QUANTD_TOKENS)")
	recordsDir := fs.String("records", "", "回测记录目录(默认 <lake>/../backtests)")
	pythonBin := fs.String("python", os.Getenv("QUANT_PYTHON"), "Python 解释器(默认 python3/python)")
	workerScript := fs.String("worker-script", "", "策略 worker.py 路径(默认自动定位)")
	maxConcurrent := fs.Int("max-backtests", 2, "并发回测数上限")
	_ = fs.Parse(args)

	reg := loadRegistry(*registryPath)
	l := lake.New(*lakeDir, reg)
	server := tsapi.NewServer(l)
	server.Logf = log.Printf
	if *tokens != "" {
		for _, token := range strings.Split(*tokens, ",") {
			if token = strings.TrimSpace(token); token != "" {
				server.Tokens = append(server.Tokens, token)
			}
		}
		log.Printf("token authentication enabled (%d token(s))", len(server.Tokens))
	}

	if *recordsDir == "" {
		*recordsDir = filepath.Join(filepath.Dir(*lakeDir), "backtests")
	}
	btServer := btserver.NewServer(l, btserver.Config{
		MaxConcurrent: *maxConcurrent,
		RecordsDir:    *recordsDir,
		PythonBinary:  *pythonBin,
		WorkerScript:  *workerScript,
	})
	btServer.Logf = log.Printf
	btServer.LoadRecords()

	handler := http.NewServeMux()
	handler.Handle("/healthz", server.HealthHandler())
	handler.Handle("/api/backtests", btServer.Handler())
	handler.Handle("/api/backtests/", btServer.Handler())
	handler.Handle("/", server.Handler())
	log.Printf("quantd serving on %s lake=%s registry=%s records=%s", *listen, *lakeDir, *registryPath, *recordsDir)
	if err := http.ListenAndServe(*listen, handler); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

// cmdBacktest 本地执行一次回测(不经过 HTTP)。
func cmdBacktest(args []string) {
	fs := flag.NewFlagSet("backtest", flag.ExitOnError)
	strategyPath := fs.String("strategy", "", "策略文件路径(必需)")
	paramsJSON := fs.String("params", "{}", "策略参数 JSON")
	start := fs.String("start", "", "开始日期 YYYYMMDD(必需)")
	end := fs.String("end", "", "结束日期 YYYYMMDD(必需)")
	capital := fs.Float64("capital", 1_000_000, "初始资金")
	frequency := fs.String("frequency", "1d", "回测频率: 1d / 1m")
	benchmark := fs.String("benchmark", "", "基准代码(如 000300.SH)")
	warmup := fs.Int("warmup", 365, "预热自然日数")
	lakeDir := fs.String("lake", "lake", "数据湖根目录")
	registryPath := fs.String("registry", filepath.Join("schemas", "datasets.yaml"), "数据集注册表")
	outPath := fs.String("out", "", "结果输出 JSON 路径(可选)")
	pythonBin := fs.String("python", os.Getenv("QUANT_PYTHON"), "Python 解释器")
	workerScript := fs.String("worker-script", "", "策略 worker.py 路径")
	_ = fs.Parse(args)

	if *strategyPath == "" || *start == "" || *end == "" {
		fs.Usage()
		os.Exit(2)
	}
	source, err := os.ReadFile(*strategyPath)
	if err != nil {
		log.Fatalf("读取策略: %v", err)
	}
	params := map[string]any{}
	if strings.TrimSpace(*paramsJSON) != "" {
		if err := json.Unmarshal([]byte(*paramsJSON), &params); err != nil {
			log.Fatalf("解析 --params: %v", err)
		}
	}
	startDays, err := schema.ParseDate(*start)
	if err != nil {
		log.Fatalf("start: %v", err)
	}
	endDays, err := schema.ParseDate(*end)
	if err != nil {
		log.Fatalf("end: %v", err)
	}

	reg := loadRegistry(*registryPath)
	l := lake.New(*lakeDir, reg)
	cfg := btengine.Config{
		StrategyName: filepath.Base(*strategyPath),
		StartDate:    schema.TimeFromDays(startDays),
		EndDate:      schema.TimeFromDays(endDays),
		Frequency:    *frequency,
		CapitalBase:  *capital,
		Benchmark:    *benchmark,
		WarmupDays:   *warmup,
		Params:       params,
	}
	worker := btworker.New(btworker.Config{
		PythonBinary:   *pythonBin,
		WorkerScript:   *workerScript,
		StrategySource: string(source),
	})
	engine := btengine.NewEngine(l, cfg)
	result, err := engine.Run(worker)
	if err != nil {
		log.Fatalf("回测失败: %v", err)
	}
	btworker.ApplyDisplayCodes(result)
	summary := result.Summary
	log.Printf("回测完成: %s ~ %s", result.StartDate, result.EndDate)
	log.Printf("初始资金 %.0f → 期末 %.0f(收益 %.2f%%)",
		summary.InitialValue, summary.FinalValue, summary.TotalReturn*100)
	log.Printf("委托 %d 笔,成交 %d 笔,用时 %.1fs",
		summary.OrderCount, summary.TradeCount, summary.ElapsedSecond)
	if result.Analytics != nil {
		log.Printf("年化 %.2f%% 夏普 %.2f 最大回撤 %.2f%%",
			toFloat(result.Analytics["annualized_return"])*100,
			toFloat(result.Analytics["sharpe_ratio"]),
			toFloat(result.Analytics["max_drawdown"])*100)
	}
	if *outPath != "" {
		raw, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			log.Fatalf("序列化结果: %v", err)
		}
		if err := os.WriteFile(*outPath, raw, 0o644); err != nil {
			log.Fatalf("写入结果: %v", err)
		}
		log.Printf("结果已写入 %s", *outPath)
	}
}

func toFloat(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

func cmdMigrate(args []string) {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	src := fs.String("src", "", "旧湖根目录(必需)")
	lakeDir := fs.String("lake", "lake", "新湖根目录")
	registryPath := fs.String("registry", filepath.Join("schemas", "datasets.yaml"), "数据集注册表")
	dataset := fs.String("dataset", "", "只迁移指定数据集(默认全部)")
	year := fs.String("year", "", "只迁移指定年份")
	jobs := fs.Int("jobs", 1, "并行处理的源文件数")
	_ = fs.Parse(args)

	if *src == "" {
		fs.Usage()
		os.Exit(2)
	}
	reg := loadRegistry(*registryPath)
	l := lake.New(*lakeDir, reg)
	migrator := &ingest.Migrator{
		SrcRoot: *src,
		Lake:    l,
		Logger:  lake.NewBatchLogger(*lakeDir),
		Logf:    log.Printf,
	}

	migrations := ingest.OldLakeMigrations()
	matched := false
	start := time.Now()
	var totalRows int64
	for _, mig := range migrations {
		if *dataset != "" && mig.Dataset != *dataset {
			continue
		}
		matched = true
		log.Printf("migrating dataset %s from %s ...", mig.Dataset, *src)
		rows, err := migrator.Migrate(mig, ingest.MigrateOptions{Year: *year, Jobs: *jobs})
		if err != nil {
			log.Fatalf("migrate %s: %v", mig.Dataset, err)
		}
		totalRows += rows
		log.Printf("dataset %s done: %d rows", mig.Dataset, rows)
	}
	if !matched {
		log.Fatalf("unknown dataset %q; available: %v", *dataset, migrationNames(migrations))
	}
	log.Printf("migration finished: %d rows in %s", totalRows, time.Since(start).Round(time.Second))
}

func migrationNames(migrations []*ingest.Migration) []string {
	names := make([]string, 0, len(migrations))
	for _, m := range migrations {
		names = append(names, m.Dataset)
	}
	return names
}

// cmdImport 从数据源插件导入数据集,按 bindings 顺序自动降级。
func cmdImport(args []string) {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	dataset := fs.String("dataset", "", "数据集名(空则列出全部)")
	sourceName := fs.String("source", "", "指定数据源(默认用 sources.yaml 的 bindings 降级链)")
	lakeDir := fs.String("lake", "lake", "新湖根目录")
	registryPath := fs.String("registry", filepath.Join("schemas", "datasets.yaml"), "数据集注册表")
	sourcesPath := fs.String("sources", filepath.Join("sources.yaml"), "数据源注册表")
	start := fs.String("start", "", "起始日期 YYYYMMDD(区间模式必需)")
	end := fs.String("end", "", "结束日期 YYYYMMDD(区间模式必需)")
	replace := fs.Bool("replace", false, "快照模式先清空目标分区")
	_ = fs.Parse(args)

	if *dataset == "" {
		for _, spec := range ingest.Specs() {
			log.Printf("可用数据集: %-16s mode=%-12s api=%s", spec.Dataset, spec.Mode, spec.APIName)
		}
		return
	}
	spec, err := ingest.SpecByName(*dataset)
	if err != nil {
		log.Fatal(err)
	}

	reg := loadRegistry(*registryPath)
	l := lake.New(*lakeDir, reg)
	opts := ingest.ImportOptions{Replace: *replace}
	if *start != "" {
		days, err := schema.ParseDate(*start)
		if err != nil {
			log.Fatalf("start: %v", err)
		}
		opts.StartDate = schema.TimeFromDays(days)
	}
	if *end != "" {
		days, err := schema.ParseDate(*end)
		if err != nil {
			log.Fatalf("end: %v", err)
		}
		opts.EndDate = schema.TimeFromDays(days)
	}

	sources := loadSourceRegistry(*sourcesPath, log.Printf)
	defer sources.Close()

	var chain []source.Source
	if *sourceName != "" {
		src, err := sources.Get(*sourceName)
		if err != nil {
			log.Fatalf("source: %v", err)
		}
		chain = []source.Source{src}
	} else {
		chain, err = sources.Resolve(*dataset)
		if err != nil {
			log.Fatal(err)
		}
	}

	// 多源时组成按调用降级的复合源:单次瞬时故障不会导致整批换源
	active := chain[0]
	if len(chain) > 1 {
		names := make([]string, 0, len(chain))
		for _, src := range chain {
			names = append(names, src.Name())
		}
		fb, err := source.NewFallback(strings.Join(names, "→"), chain)
		if err != nil {
			log.Fatal(err)
		}
		fb.SetLogger(log.Printf)
		active = fb
	}
	importer := &ingest.Importer{
		Source: active,
		Lake:   l,
		Logger: lake.NewBatchLogger(*lakeDir),
		Logf:   log.Printf,
	}
	log.Printf("importing %s from source %s (kind=%s) ...", *dataset, active.Name(), active.Kind())
	started := time.Now()
	rows, err := importer.Import(context.Background(), spec, opts)
	if err != nil {
		log.Fatalf("import %s: %v", *dataset, err)
	}
	log.Printf("dataset %s imported: %d rows in %s (source=%s)",
		*dataset, rows, time.Since(started).Round(time.Second), active.Name())
}

// cmdSource 管理数据源插件。
func cmdSource(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: quantd source list|check [name] [--sources sources.yaml]")
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("source list", flag.ExitOnError)
		sourcesPath := fs.String("sources", filepath.Join("sources.yaml"), "数据源注册表")
		_ = fs.Parse(args[1:])
		reg := loadSourceRegistry(*sourcesPath, nil)
		defer reg.Close()
		fmt.Printf("config: %s\n\n", *sourcesPath)
		fmt.Printf("%-18s %-14s %-8s %s\n", "SOURCE", "KIND", "STATUS", "DESCRIPTION")
		for _, info := range reg.Infos() {
			status := "ready"
			switch {
			case info.Disabled:
				status = "disabled"
			case info.Err != nil:
				status = "error"
			}
			fmt.Printf("%-18s %-14s %-8s %s\n", info.Name, info.Kind, status, info.Description)
			if info.Err != nil {
				fmt.Printf("%-18s %s\n", "", "└─ "+info.Err.Error())
			}
		}
		fmt.Printf("\nbindings (数据集 → 源降级链):\n")
		datasets := make([]string, 0)
		bindings := reg.Bindings()
		for dataset := range bindings {
			datasets = append(datasets, dataset)
		}
		sort.Strings(datasets)
		for _, dataset := range datasets {
			fmt.Printf("  %-16s %s\n", dataset, strings.Join(bindings[dataset], " → "))
		}
	case "check":
		fs := flag.NewFlagSet("source check", flag.ExitOnError)
		sourcesPath := fs.String("sources", filepath.Join("sources.yaml"), "数据源注册表")
		api := fs.String("api", "trade_cal", "探活使用的接口")
		_ = fs.Parse(args[1:])
		reg := loadSourceRegistry(*sourcesPath, log.Printf)
		defer reg.Close()
		targets := fs.Args()
		exitCode := 0
		for _, info := range reg.Infos() {
			if len(targets) > 0 && !contains(targets, info.Name) {
				continue
			}
			if info.Disabled {
				fmt.Printf("[skip] %-18s disabled\n", info.Name)
				continue
			}
			if info.Err != nil {
				fmt.Printf("[fail] %-18s %v\n", info.Name, info.Err)
				exitCode = 1
				continue
			}
			src, err := reg.Get(info.Name)
			if err != nil {
				fmt.Printf("[fail] %-18s %v\n", info.Name, err)
				exitCode = 1
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			started := time.Now()
			result, err := src.Call(ctx, *api, map[string]any{
				"exchange":   "SSE",
				"start_date": "20240101",
				"end_date":   "20240105",
			}, "")
			cancel()
			if err != nil {
				fmt.Printf("[fail] %-18s %v\n", info.Name, err)
				exitCode = 1
				continue
			}
			fmt.Printf("[ok]   %-18s kind=%-12s api=%s rows=%d count=%d %s\n",
				info.Name, info.Kind, *api, len(result.Items), result.Count, time.Since(started).Round(time.Millisecond))
		}
		os.Exit(exitCode)
	case "call":
		fs := flag.NewFlagSet("source call", flag.ExitOnError)
		sourcesPath := fs.String("sources", filepath.Join("sources.yaml"), "数据源注册表")
		name := fs.String("name", "", "数据源名(必需)")
		api := fs.String("api", "", "接口名(tushare 标准名,必需)")
		fields := fs.String("fields", "", "列裁剪(逗号分隔)")
		limit := fs.Int("limit", 0, "覆盖 limit")
		var params paramList
		fs.Var(&params, "param", "查询参数 k=v,可重复")
		_ = fs.Parse(args[1:])
		if *name == "" || *api == "" {
			fmt.Fprintln(os.Stderr, "用法: quantd source call --name <源> --api <接口> [--param k=v ...] [--fields a,b] [--limit N]")
			os.Exit(2)
		}
		reg := loadSourceRegistry(*sourcesPath, log.Printf)
		defer reg.Close()
		src, err := reg.Get(*name)
		if err != nil {
			log.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		callParams := map[string]any(params)
		if *limit > 0 {
			callParams["limit"] = *limit
		}
		started := time.Now()
		result, err := src.Call(ctx, *api, callParams, *fields)
		if err != nil {
			log.Fatalf("call 失败: %v", err)
		}
		fmt.Printf("source=%s api=%s rows=%d count=%d has_more=%v 用时=%s\n",
			*name, *api, len(result.Items), result.Count, result.HasMore, time.Since(started).Round(time.Millisecond))
		if len(result.Items) > 0 {
			fmt.Printf("fields: %v\n", result.Fields)
			fmt.Printf("first : %v\n", result.Items[0])
			fmt.Printf("last  : %v\n", result.Items[len(result.Items)-1])
		}
		if len(result.Meta) > 0 {
			fmt.Printf("meta  : %v\n", result.Meta)
		}
	case "compare":
		fs := flag.NewFlagSet("source compare", flag.ExitOnError)
		sourcesPath := fs.String("sources", filepath.Join("sources.yaml"), "数据源注册表")
		dataset := fs.String("dataset", "", "数据集名(用其 bindings 的源链,必需)")
		api := fs.String("api", "", "接口名(默认取数据集规格的 api)")
		fields := fs.String("fields", "", "列裁剪")
		var params paramList
		fs.Var(&params, "param", "查询参数 k=v,可重复")
		_ = fs.Parse(args[1:])
		if *dataset == "" {
			fmt.Fprintln(os.Stderr, "用法: quantd source compare --dataset <数据集> [--param k=v ...]")
			os.Exit(2)
		}
		apiName := *api
		if apiName == "" {
			spec, err := ingest.SpecByName(*dataset)
			if err != nil {
				log.Fatal(err)
			}
			apiName = spec.APIName
			if *fields == "" && spec.Fields != "" {
				*fields = spec.Fields
			}
		}
		reg := loadSourceRegistry(*sourcesPath, log.Printf)
		defer reg.Close()
		chain, err := reg.Resolve(*dataset)
		if err != nil {
			log.Fatal(err)
		}
		type outcome struct {
			name   string
			rows   int
			count  int64
			first  []any
			last   []any
			fields []string
			err    error
		}
		var outcomes []outcome
		for _, src := range chain {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			result, err := src.Call(ctx, apiName, map[string]any(params), *fields)
			cancel()
			o := outcome{name: src.Name(), err: err}
			if err == nil {
				o.rows = len(result.Items)
				o.count = result.Count
				o.fields = result.Fields
				if o.rows > 0 {
					o.first = result.Items[0]
					o.last = result.Items[o.rows-1]
				}
			}
			outcomes = append(outcomes, o)
		}
		fmt.Printf("dataset=%s api=%s params=%v\n\n", *dataset, apiName, map[string]any(params))
		for _, o := range outcomes {
			if o.err != nil {
				fmt.Printf("%-18s ERROR %v\n", o.name, o.err)
				continue
			}
			fmt.Printf("%-18s rows=%-7d count=%-7d first=%v last=%v\n", o.name, o.rows, o.count, fmt.Sprint(o.first), fmt.Sprint(o.last))
		}
		if len(outcomes) == 2 && outcomes[0].err == nil && outcomes[1].err == nil {
			a, b := outcomes[0], outcomes[1]
			switch {
			case a.rows != b.rows:
				fmt.Printf("\n[注意] 两源行数不同: %s=%d vs %s=%d(排查语义差异,如 trade_cal 是否含休市日)\n",
					a.name, a.rows, b.name, b.rows)
			case a.count != b.count:
				fmt.Printf("\n[注意] 行数相同但 count 不同: %d vs %d\n", a.count, b.count)
			default:
				fmt.Printf("\n[ok] 两源行数与 count 一致(%d 行)\n", a.rows)
			}
			if len(a.fields) > 0 && len(b.fields) > 0 && strings.Join(a.fields, ",") != strings.Join(b.fields, ",") {
				fmt.Printf("[注意] 字段列表不同:\n  %s: %v\n  %s: %v\n", a.name, a.fields, b.name, b.fields)
			}
		}
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q(可用 list/check/call/compare)\n", args[0])
		os.Exit(2)
	}
}

// verify 对每个数据集核对 manifest 记录行数与 part 文件实际行数。
func cmdVerify(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	lakeDir := fs.String("lake", "lake", "数据湖根目录")
	registryPath := fs.String("registry", filepath.Join("schemas", "datasets.yaml"), "数据集注册表")
	dataset := fs.String("dataset", "", "只核对指定数据集")
	_ = fs.Parse(args)

	reg := loadRegistry(*registryPath)
	l := lake.New(*lakeDir, reg)
	logger := lake.NewBatchLogger(*lakeDir)

	exitCode := 0
	names := reg.Names()
	if *dataset != "" {
		names = []string{*dataset}
	}
	for _, name := range names {
		ds, err := reg.Get(name)
		if err != nil {
			log.Printf("%s: %v", name, err)
			exitCode = 1
			continue
		}
		if err := verifyDataset(l, logger, ds); err != nil {
			log.Printf("FAIL %s: %v", name, err)
			exitCode = 1
		}
	}
	os.Exit(exitCode)
}

func verifyDataset(l *lake.Lake, logger *lake.BatchLogger, ds *schema.Dataset) error {
	files, err := l.WalkFiles(ds, nil)
	if err != nil {
		return err
	}
	var actual int64
	for _, ref := range files {
		fh, err := os.Open(ref.Path)
		if err != nil {
			return err
		}
		st, err := fh.Stat()
		if err != nil {
			_ = fh.Close()
			return err
		}
		pf, err := parquet.OpenFile(fh, st.Size())
		if err != nil {
			_ = fh.Close()
			return err
		}
		actual += pf.NumRows()
		if err := fh.Close(); err != nil {
			return err
		}
	}
	summary, err := logger.Summarize(ds.Name)
	if err != nil {
		return err
	}
	if summary.Batches == 0 {
		log.Printf("no-manifest %s: files=%d rows=%d", ds.Name, len(files), actual)
		return nil
	}
	if summary.Rows != actual {
		return fmt.Errorf("manifest rows=%d actual rows=%d (files=%d)", summary.Rows, actual, len(files))
	}
	log.Printf("OK %s: batches=%d files=%d rows=%d", ds.Name, summary.Batches, len(files), actual)
	return nil
}

func cmdAPIs(args []string) {
	fs := flag.NewFlagSet("apis", flag.ExitOnError)
	registryPath := fs.String("registry", filepath.Join("schemas", "datasets.yaml"), "数据集注册表")
	_ = fs.Parse(args)
	reg := loadRegistry(*registryPath)
	names := reg.Names()
	log.Printf("datasets: %v", names)
	apis := tsapi.DefaultAPIs()
	apiNames := make([]string, 0, len(apis))
	for name := range apis {
		apiNames = append(apiNames, name)
	}
	sort.Strings(apiNames)
	for _, name := range apiNames {
		api := apis[name]
		log.Printf("api %-12s -> dataset %-14s fields=%d", api.Name, api.Dataset, len(api.SelectFields))
	}
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// paramList 收集可重复的 --param k=v 参数。
type paramList map[string]any

func (p *paramList) String() string { return fmt.Sprint(map[string]any(*p)) }

func (p *paramList) Set(value string) error {
	key, val, ok := strings.Cut(value, "=")
	if !ok {
		return fmt.Errorf("参数格式应为 k=v: %q", value)
	}
	if *p == nil {
		*p = map[string]any{}
	}
	(*p)[strings.TrimSpace(key)] = strings.TrimSpace(val)
	return nil
}
