// billingcheck 演示官方用量/费用/额度读取与对账（M2-G2）。
//
// 用法：
//
//	billingcheck -provider openai [-key <org-admin-key>] [-window-day 1] [-observed observed.json]
//	billingcheck -provider anthropic -key dry-run
//	billingcheck -provider gemini
//
// 未提供真实管理凭据时进入 dry-run：返回 estimated/unavailable，不发真实网络请求。
// 对账：传入 -observed 本地观测 JSON（{"input_tokens":..,"output_tokens":..,"cache_tokens":..,"requests":..}）
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"midroute/internal/usage"
)

func main() {
	provider := flag.String("provider", "openai", "openai | anthropic | gemini")
	key := flag.String("key", "", "组织级管理凭据（留空或 dry-run 进入 dry-run）")
	days := flag.Int("window-day", 1, "时间窗口天数")
	observedFile := flag.String("observed", "", "本地观测用量 JSON 文件")
	flag.Parse()

	ctx := context.Background()
	adapter := billing.NewAdapter(billing.Provider(*provider))
	now := time.Now().UTC()
	window := billing.Window{Start: now.AddDate(0, 0, -*days), End: now}

	fmt.Printf("== %s 能力矩阵 ==\n", adapter.Provider())
	matrix, err := adapter.CapabilityMatrix(ctx, *key)
	if err != nil {
		fmt.Fprintln(os.Stderr, "matrix:", err)
		os.Exit(1)
	}
	for _, c := range matrix {
		status := "不支持"
		if c.Supported {
			status = "支持"
		}
		fmt.Printf("  %-18s %s  %s\n", c.Capability, status, c.Reason)
	}

	fmt.Printf("\n== %s 用量（窗口 %d 天）==\n", adapter.Provider(), *days)
	u, err := adapter.FetchUsage(ctx, *key, window)
	if err != nil {
		fmt.Fprintln(os.Stderr, "usage:", err)
		os.Exit(1)
	}
	out, _ := json.Marshal(u)
	fmt.Println(" ", string(out))

	fmt.Printf("\n== %s 费用 ==\n", adapter.Provider())
	cost, err := adapter.FetchCost(ctx, *key, window)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cost:", err)
		os.Exit(1)
	}
	out, _ = json.Marshal(cost)
	fmt.Println(" ", string(out))

	if *observedFile != "" {
		data, err := os.ReadFile(*observedFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "observed:", err)
			os.Exit(1)
		}
		var observed billing.Usage
		if err := json.Unmarshal(data, &observed); err != nil {
			fmt.Fprintln(os.Stderr, "observed parse:", err)
			os.Exit(1)
		}
		observed.Provider = billing.Provider(*provider)
		observed.Source = billing.SourceObserved
		observed.Confidence = billing.ConfidenceExact
		r := billing.Reconcile(adapter.Provider(), window, u, &observed)
		fmt.Printf("\n== 对账 ==\n")
		for _, e := range r.Entries {
			fmt.Printf("  %-14s official=%.0f observed=%.0f diff=%.0f [%s/%s] %s\n",
				e.Metric, e.Official, e.Observed, e.Diff, e.OfficialConf, e.ObservedConf, e.Interpretation)
		}
	}
}
