package quota

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"midroute/internal/domain"
	"midroute/internal/repository"
)

// Service 额度快照持久化服务（MR-009）。
// 职责：把被动观测/手工数据规范化为 domain.QuotaSnapshot 并落库；
// 未知数值保持 nil；手工数据不覆盖官方快照；失败保留上次成功值。
type Service struct {
	Store            *repository.Store
	Now              func() time.Time
	ConnectorVersion string
}

// New 创建额度服务。
func New(store *repository.Store) *Service {
	return &Service{
		Store:            store,
		Now:              func() time.Time { return time.Now().UTC() },
		ConnectorVersion: "0.1.0",
	}
}

// ObservationToSnapshots 把被动观测转换为可落库快照（unknown 保持 nil，不做换算）。
func (s *Service) ObservationToSnapshots(accountID string, obs Observation) []domain.QuotaSnapshot {
	now := s.Now()
	out := make([]domain.QuotaSnapshot, 0, len(obs.Snapshots))
	for _, sn := range obs.Snapshots {
		snap := domain.QuotaSnapshot{
			ID:               "qsnap_" + sn.LimitName + "_" + randID(8),
			AccountID:        accountID,
			WindowType:       mapWindowType(sn.LimitName),
			Source:           domain.QuotaSourceObserved,
			SourceRef:        "x-codex-* 响应头被动观测",
			Confidence:       domain.QuotaConfReported,
			Freshness:        domain.QuotaFreshFresh,
			Unit:             "percent",
			ConnectorVersion: s.ConnectorVersion,
			TakenAt:          now.Format(time.RFC3339),
			LastSuccessAt:    now.Format(time.RFC3339),
		}
		if sn.UsedPercent != nil {
			used := *sn.UsedPercent
			snap.Used = &used
			if sn.Allowed != nil && *sn.Allowed {
				rem := 100 - used
				snap.Remaining = &rem
			}
		}
		if sn.WindowMinutes != nil {
			lim := float64(*sn.WindowMinutes)
			snap.Limit = &lim
		}
		if sn.ResetAt != nil {
			rs := sn.ResetAt.UTC().Format(time.RFC3339)
			snap.ResetAt = &rs
		}
		out = append(out, snap)
	}
	return out
}

// RecordObservation 把一次观测写入账户快照历史。
func (s *Service) RecordObservation(ctx context.Context, accountID string, obs Observation) (int, error) {
	snaps := s.ObservationToSnapshots(accountID, obs)
	for _, snap := range snaps {
		if err := s.Store.SaveQuotaSnapshot(ctx, snap); err != nil {
			return 0, err
		}
	}
	return len(snaps), nil
}

// ManualSupplement 写入手工额度值（source=manual，不覆盖官方快照）。
// operator 记录操作者；expiresIn 为空表示长期有效。
func (s *Service) ManualSupplement(ctx context.Context, accountID, operator string, in domain.QuotaSnapshot) (domain.QuotaSnapshot, error) {
	now := s.Now()
	in.ID = "qsnap_manual_" + randID(8)
	in.AccountID = accountID
	in.Source = domain.QuotaSourceManual
	in.SourceRef = "用户手工录入"
	in.Confidence = domain.QuotaConfReported
	in.Freshness = domain.QuotaFreshFresh
	in.Operator = operator
	in.ConnectorVersion = s.ConnectorVersion
	in.TakenAt = now.Format(time.RFC3339)
	in.LastSuccessAt = now.Format(time.RFC3339)
	if in.Unit == "" {
		in.Unit = "percent"
	}
	if err := s.Store.SaveQuotaSnapshot(ctx, in); err != nil {
		return domain.QuotaSnapshot{}, err
	}
	return in, nil
}

func mapWindowType(name string) domain.QuotaWindowType {
	switch name {
	case "primary":
		return domain.QuotaWindowPrimary
	case "secondary":
		return domain.QuotaWindowSecondary
	case "code-review":
		return domain.QuotaWindowCodeReview
	default:
		if len(name) > len("additional-") && name[:len("additional-")] == "additional-" {
			return domain.QuotaWindowAdditional
		}
		return domain.QuotaWindowUnknown
	}
}

// randID 生成十六进制随机串（避免固定时钟下快照 ID 冲突）。
func randID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
