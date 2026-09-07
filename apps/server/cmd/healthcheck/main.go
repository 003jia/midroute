// healthcheck 对请求样本文件执行健康/拥挤度评分（M2-G1 演示与运维工具）。
//
// 用法：
//
//	healthcheck -file samples.json [-account-limited] [-engine]
//	cat samples.json | healthcheck
//
// 样本 JSON 格式（每行一个对象，或 JSON 数组）：
//
//	{"model_id":"gpt-4o","connection_id":"c1","ttft_ms":300,"latency_ms":1200,"status_code":200,"sampled_at":0}
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"midroute/internal/health"
)

func main() {
	file := flag.String("file", "", "样本文件路径（默认 stdin）")
	accountLimited := flag.Bool("account-limited", false, "标记本账号额度已用尽")
	engineMode := flag.Bool("engine", false, "使用带滞回的引擎输出")
	flag.Parse()

	var in io.Reader = os.Stdin
	if *file != "" {
		f, err := os.Open(*file)
		if err != nil {
			fmt.Fprintln(os.Stderr, "open:", err)
			os.Exit(1)
		}
		defer f.Close()
		in = f
	}
	data, err := io.ReadAll(in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}

	var samples []health.Sample
	if len(data) > 0 && data[0] == '[' {
		if err := json.Unmarshal(data, &samples); err != nil {
			fmt.Fprintln(os.Stderr, "parse array:", err)
			os.Exit(1)
		}
	} else {
		dec := json.NewDecoder(bytes.NewReader(data))
		for {
			var s health.Sample
			if err := dec.Decode(&s); err == io.EOF {
				break
			} else if err != nil {
				fmt.Fprintln(os.Stderr, "parse line:", err)
				os.Exit(1)
			}
			samples = append(samples, s)
		}
	}

	grouped := map[string][]health.Sample{}
	for _, s := range samples {
		key := s.ModelID
		if s.ConnectionID != "" {
			key = s.ConnectionID + "|" + s.ModelID
		}
		grouped[key] = append(grouped[key], s)
	}

	for key, group := range grouped {
		var r health.Result
		if *engineMode {
			r, err = health.NewEngine().Observe(group, *accountLimited)
			if err != nil {
				fmt.Fprintln(os.Stderr, "observe:", err)
				os.Exit(1)
			}
		} else {
			r = health.Evaluate(group, *accountLimited)
		}
		out, _ := json.Marshal(r)
		fmt.Printf("%s: %s\n", key, out)
	}
}
