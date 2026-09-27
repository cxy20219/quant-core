package factor

import (
	"math"
	"path/filepath"
	"testing"
)

// TestValidateAndCatalog 校验因子定义与内置目录。
func TestValidateAndCatalog(t *testing.T) {
	if len(Catalog()) == 0 {
		t.Fatal("内置因子目录为空")
	}
	ok := &Definition{ID: "mom20", Name: "20日动量", Kind: "momentum", Params: map[string]any{"window": 20}}
	if err := Validate(ok); err != nil {
		t.Fatalf("合法定义被拒: %v", err)
	}
	if got := ok.Window(); got != 20 {
		t.Fatalf("window=%d, want 20", got)
	}
	bad := []*Definition{
		{ID: "", Name: "x", Kind: "momentum"},
		{ID: "x", Name: "", Kind: "momentum"},
		{ID: "x", Name: "x", Kind: "unknown_kind"},
		{ID: "x", Name: "x", Kind: "momentum", Params: map[string]any{"window": "abc"}},
		{ID: "x", Name: "x", Kind: "momentum", Direction: "up"},
	}
	for i, def := range bad {
		if err := Validate(def); err == nil {
			t.Errorf("非法定义 #%d 未被拒绝: %+v", i, def)
		}
	}
}

// TestRegistryRoundTrip 注册表读写与删除。
func TestRegistryRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "factors.json")
	reg, err := LoadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	def := &Definition{ID: "rev5", Name: "5日反转", Kind: "reversal", Params: map[string]any{"window": 5}}
	if err := reg.Put(def); err != nil {
		t.Fatal(err)
	}
	again, err := LoadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := again.Get("rev5")
	if !ok || got.Name != "5日反转" || got.Window() != 5 {
		t.Fatalf("读回不一致: %+v", got)
	}
	if err := again.Delete("rev5"); err != nil {
		t.Fatal(err)
	}
	if _, ok := again.Get("rev5"); ok {
		t.Fatal("删除后仍能取到")
	}
	if err := again.Delete("rev5"); err == nil {
		t.Fatal("重复删除应报错")
	}
}

// TestStats 相关性/秩相关/分位统计的正确性。
func TestStats(t *testing.T) {
	xs := []float64{1, 2, 3, 4, 5}
	ys := []float64{2, 4, 6, 8, 10}
	if got := pearson(xs, ys); math.Abs(got-1) > 1e-9 {
		t.Fatalf("pearson=%v, want 1", got)
	}
	if got := spearman(xs, []float64{5, 4, 3, 2, 1}); math.Abs(got+1) > 1e-9 {
		t.Fatalf("spearman=%v, want -1", got)
	}
	// 并列值取平均秩
	ranks := ranks([]float64{10, 20, 20, 30})
	want := []float64{1, 2.5, 2.5, 4}
	for i := range want {
		if math.Abs(ranks[i]-want[i]) > 1e-9 {
			t.Fatalf("ranks=%v, want %v", ranks, want)
		}
	}
	if got := stddevOrNaN([]float64{1, 1, 1}); got != 0 {
		t.Fatalf("stddev=%v, want 0", got)
	}
	if got := positiveRatio([]float64{1, -1, 2}); math.Abs(got-2.0/3) > 1e-9 {
		t.Fatalf("positiveRatio=%v", got)
	}
}
