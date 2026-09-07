// Package quota 提供套餐/额度数据的规范化表示与被动采集解析（MR-008）。
//
// 数据语义对齐 docs/contracts/data-contracts-v1.md：
//   - 未知数值用 nil 指针表示，与真实 0 严格区分；
//   - 单位保持 percent，不在分母未知时换算成 Token；
//   - 多窗口（如 primary/secondary）分列，不相加、不合并；
//   - 来源标记 observed（本地观测），采集时间单独记录。
//
// 解析依据（上游冻结提交 17a65ee，见 docs/provider-capabilities.md）：
// Codex/ChatGPT 无公开的主动余额查询端点；额度信号随推理响应被动携带，
// 形式为 x-codex-* 系列响应头与 SSE 流内 codex.rate_limits 帧。
// 本包当前实现响应头解析；流内帧解析随 MR-013 的 Responses 协议接入。
package quota

import "time"

// Snapshot 一个额度窗口的规范化观测值。指针字段 nil = 上游未提供（未知）。
type Snapshot struct {
	// LimitName 窗口标识：primary / secondary / additional-<名称> / code-review。
	LimitName string
	// UsedPercent 已用百分比（0-100）。不做 Token 换算。
	UsedPercent *float64
	// WindowMinutes 窗口长度（分钟）。上游未说明窗口语义时保持未知。
	WindowMinutes *int
	// ResetAfterSeconds 距重置的秒数（相对值）。
	ResetAfterSeconds *int64
	// ResetAt 重置时间（绝对值，UTC）。无法解析时为 nil，原始值在 RawResetAt。
	ResetAt *time.Time
	// RawResetAt 上游原始 reset-at 字符串（仅在解析失败时保留，供诊断）。
	RawResetAt string
	// Allowed 该窗口是否仍允许请求。
	Allowed *bool
	// LimitReached 该窗口是否已触顶。
	LimitReached *bool

	// Source 数据来源：固定 observed（本地被动观测）。
	Source string
	// CollectedAt 采集时间（UTC）。
	CollectedAt time.Time
}

// Observation 一次响应携带的全部额度信号。
type Observation struct {
	// PlanType 套餐档位名（如 pro），来自 x-codex-plan-type；不是数值。
	PlanType string
	// ActiveLimit 当前生效的限制名称，来自 x-codex-active-limit。
	ActiveLimit string
	// Snapshots 按 LimitName 分组的窗口观测；同组多个同义头取最后一个。
	Snapshots []Snapshot
	// Warnings 解析中被跳过的非法值（字段名级描述，不含凭据）。
	Warnings []string
}

// SourceObserved 是被动观测数据的来源标记。
const SourceObserved = "observed"
