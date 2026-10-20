package main

import (
	"testing"
	"time"

	"mocenter/server/internal/share"
)

// TestScheduleShareExpiryPast 已经过期的分享（例如时钟抖动/记录刚被清理）必须立刻收口。
func TestScheduleShareExpiryPast(t *testing.T) {
	sh := &share.Share{ExpiresAt: time.Now().Add(-time.Minute).Unix()}
	called := make(chan struct{}, 1)
	scheduleShareExpiry(sh, func() { called <- struct{}{} })
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("已过期的分享应立即执行关闭")
	}
}

// TestScheduleShareExpiryFuture 未到期的分享不得提前关闭。
func TestScheduleShareExpiryFuture(t *testing.T) {
	sh := &share.Share{ExpiresAt: time.Now().Add(time.Hour).Unix()}
	called := make(chan struct{}, 1)
	scheduleShareExpiry(sh, func() { called <- struct{}{} })
	select {
	case <-called:
		t.Fatal("未到期的分享不应被提前关闭")
	case <-time.After(20 * time.Millisecond):
	}
}
