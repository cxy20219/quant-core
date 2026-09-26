package main

import (
	"fmt"
	"os"

	"github.com/parquet-go/parquet-go"
)

func main() {
	path := os.Args[1]
	fh, _ := os.Open(path)
	defer fh.Close()
	st, _ := fh.Stat()
	f, err := parquet.OpenFile(fh, st.Size())
	if err != nil {
		panic(err)
	}
	meta := f.Metadata()
	fmt.Println("rowgroups:", len(meta.RowGroups))
	for i := 0; i < 3; i++ {
		rg := meta.RowGroups[i]
		fmt.Printf("rg[%d] numRows=%d numCols=%d\n", i, rg.NumRows, len(rg.Columns))
		for j, col := range rg.Columns {
			stats := col.MetaData.Statistics
			fmt.Printf("   col[%d] codec=%v min=%q max=%q nulls=%d\n", j, col.MetaData.Codec, string(stats.Min), string(stats.Max), stats.NullCount)
		}
		break
	}
	// 打印 rg0 全部列名对应关系
	rg := f.RowGroups()[0]
	fields := rg.Schema().Fields()
	chunks := rg.ColumnChunks()
	for j, cc := range chunks {
		name := fields[j].Name()
		idx, err := cc.ColumnIndex()
		if err != nil {
			fmt.Printf("field[%d] %s: no col index\n", j, name)
			continue
		}
		fmt.Printf("field[%d] %s: pages=%d min=%v max=%v\n", j, name, idx.NumPages(), idx.MinValue(0), idx.MaxValue(0))
	}
}
