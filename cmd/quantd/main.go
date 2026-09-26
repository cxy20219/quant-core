// quantd 是量化数据服务的主程序。
//
// 子命令:
//
//	quantd serve   --lake <dir> --registry <yaml> --listen :8000
//	quantd migrate --src <old-lake> --lake <dir> [--dataset bars_daily] [--year 2024]
//	quantd verify  --lake <dir> [--dataset bars_daily]
//	quantd apis    --registry <yaml>
package main

import (
	"context"
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

	"quant-core/internal/ingest"
	"quant-core/internal/lake"
	"quant-core/internal/schema"
	"quant-core/internal/source/tushare"
	"quant-core/internal/tsapi"
)

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "migrate":
		cmdMigrate(os.Args[2:])
	case "verify":
		cmdVerify(os.Args[2:])
	case "apis":
		cmdAPIs(os.Args[2:])
	case "import":
		cmdImport(os.Args[2:])
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
  quantd import  --source tushare --dataset stock_basic --lake <dir> [--start 20240101 --end 20240131] [--replace]
  quantd verify  --lake <dir> [--dataset bars_daily]
  quantd apis    --registry <yaml>
`)
}

func loadRegistry(path string) *schema.Registry {
	reg, err := schema.Load(path)
	if err != nil {
		log.Fatalf("load registry %s: %v", path, err)
	}
	return reg
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	lakeDir := fs.String("lake", "lake", "数据湖根目录")
	registryPath := fs.String("registry", filepath.Join("schemas", "datasets.yaml"), "数据集注册表")
	listen := fs.String("listen", ":8000", "监听地址")
	tokens := fs.String("tokens", os.Getenv("QUANTD_TOKENS"), "逗号分隔的访问令牌(默认读 QUANTD_TOKENS)")
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

	handler := http.NewServeMux()
	handler.Handle("/healthz", server.HealthHandler())
	handler.Handle("/", server.Handler())
	log.Printf("quantd serving on %s lake=%s registry=%s", *listen, *lakeDir, *registryPath)
	if err := http.ListenAndServe(*listen, handler); err != nil {
		log.Fatalf("serve: %v", err)
	}
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

func cmdImport(args []string) {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	source := fs.String("source", "", "数据源(当前支持 tushare)")
	dataset := fs.String("dataset", "", "数据集名(默认该源全部)")
	lakeDir := fs.String("lake", "lake", "新湖根目录")
	registryPath := fs.String("registry", filepath.Join("schemas", "datasets.yaml"), "数据集注册表")
	start := fs.String("start", "", "起始日期 YYYYMMDD(区间模式必需)")
	end := fs.String("end", "", "结束日期 YYYYMMDD(区间模式必需)")
	replace := fs.Bool("replace", false, "快照模式先清空目标分区")
	token := fs.String("token", os.Getenv("TUSHARE_TOKEN"), "数据源令牌(默认读 TUSHARE_TOKEN)")
	_ = fs.Parse(args)

	if *source != "tushare" {
		log.Fatalf("unsupported source %q (only tushare)", *source)
	}
	if *token == "" {
		log.Fatal("missing token: pass --token or set TUSHARE_TOKEN")
	}
	if *dataset == "" {
		specs := ingest.TushareSpecs()
		for _, spec := range specs {
			log.Printf("available tushare dataset: %-16s mode=%s api=%s", spec.Dataset, spec.Mode, spec.APIName)
		}
		return
	}

	reg := loadRegistry(*registryPath)
	l := lake.New(*lakeDir, reg)
	client := tushare.NewClient(*token)
	client.Logf = log.Printf
	client.Pause = 300 * time.Millisecond
	importer := &ingest.TushareImporter{
		Client: client,
		Lake:   l,
		Logger: lake.NewBatchLogger(*lakeDir),
		Logf:   log.Printf,
	}

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

	matched := false
	for _, spec := range ingest.TushareSpecs() {
		if spec.Dataset != *dataset {
			continue
		}
		matched = true
		rows, err := importer.Import(context.Background(), spec, opts)
		if err != nil {
			log.Fatalf("import %s: %v", spec.Dataset, err)
		}
		log.Printf("dataset %s imported: %d rows", spec.Dataset, rows)
	}
	if !matched {
		log.Fatalf("unknown tushare dataset %q", *dataset)
	}
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
