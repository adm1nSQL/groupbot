package db

import (
	"database/sql"
	"testing"
	"time"
)

func TestSubscriptionEligibility(t *testing.T) {
	now := time.Now().Unix()
	base := V2User{
		PlanID:         sql.NullInt64{Int64: 1, Valid: true},
		TransferEnable: sql.NullInt64{Int64: 100, Valid: true},
		U:              sql.NullInt64{Int64: 100, Valid: true},
	}
	tests := []struct {
		name    string
		reset   sql.NullInt64
		expires int64
		valid   bool
	}{
		{"一次性无到期时间流量耗尽", sql.NullInt64{Int64: 2, Valid: true}, 0, false},
		{"一次性未到期流量耗尽", sql.NullInt64{Int64: 2, Valid: true}, now + 3600, false},
		{"一次性刚过期未续费", sql.NullInt64{Int64: 2, Valid: true}, now - 60, false},
		{"周期无到期时间流量耗尽", sql.NullInt64{Int64: 0, Valid: true}, 0, true},
		{"周期未到期流量耗尽", sql.NullInt64{Int64: 1, Valid: true}, now + 3600, true},
		{"周期刚过期流量耗尽", sql.NullInt64{Int64: 1, Valid: true}, now - 60, true},
		{"周期过期两天", sql.NullInt64{Int64: 0, Valid: true}, now - 2*24*3600, true},
		{"周期过期超过三天", sql.NullInt64{Int64: 0, Valid: true}, now - 4*24*3600, false},
		{"未知重置方式不按流量踢出", sql.NullInt64{}, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := base
			u.PlanResetMethod = tt.reset
			u.ExpiredAt = sql.NullInt64{Int64: tt.expires, Valid: tt.expires != 0}
			if got := IsUserValid(&u); got != tt.valid {
				t.Fatalf("IsUserValid() = %v, 期望 %v", got, tt.valid)
			}
		})
	}
}

func TestExpiryGraceBoundary(t *testing.T) {
	const now int64 = 1000000
	u := &V2User{ExpiredAt: sql.NullInt64{Int64: now - expiryGraceSeconds, Valid: true}}
	if !IsExpiredBeyondGrace(u, now) || IsExpiredBeyondGrace(u, now-1) {
		t.Fatal("过期三天的宽限期边界错误")
	}
}
