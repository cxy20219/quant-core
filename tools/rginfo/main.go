package main

import (
	"fmt"
	"os"
	"sort"

	"github.com/parquet-go/parquet-go"
)

// 打印文件各行的 ts_code 行组统计,用于诊断剪枝。
func main() {
	path := os.Args[1]
	fh, _ := os.Open(path)
	defer fh.Close()
	st, _ := fh.Stat()
	f, err := parquet.OpenFile(fh, st.Size())
	if err != nil {
		panic(err)
	}
	type row struct {
		i   int
		min string
		max string
	}
	var rows []row
	for i, rg := range f.RowGroups() {
		fields := rg.Schema().Fields()
		chunks := rg.ColumnChunks()
		var minS, maxS string
		for j, field := range fields {
			if field.Name() != "ts_code" {
				continue
			}
			idx, err := chunks[j].ColumnIndex()
			if err != nil {
				minS, maxS = "<noindex>", ""
				break
			}
			minS = string(idx.MinValue(0).ByteArray())
			maxS = string(idx.MaxValue(0).ByteArray())
		}
		rows = append(rows, row{i, minS, maxS})
	}
	fmt.Println("row groups:", len(rows))
	// 排序后打印不重叠区间
	sorted := append([]row(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].min < sorted[j].min })
	prevMax := ""
	for _, r := range sorted {
		flag := ""
		if r.min <= "600000.SH" && "600000.SH" <= r.max {
			flag = " <== contains 600000.SH range"
		}
		if prevMax != "" && r.min < prevMax {
			flag += " [OVERLAP]"
		}
		if r.max > prevMax {
			prevMax = r.max
		}
		fmt.Printf("  rg[%3d] %s .. %s%s\n", r.i, r.min, r.max, flag)
	}
}
