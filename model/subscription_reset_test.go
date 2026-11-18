package model

import (
	"fmt"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/money"
	"gorm.io/gorm"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedSubscriptionResetPlan(t *testing.T, plan *SubscriptionPlan) {
	t.Helper()
	require.NoError(t, DB.Create(plan).Error)
}

func seedSubscriptionResetSub(t *testing.T, sub *UserSubscription, consumed money.Amount) {
	t.Helper()
	user := User{Id: sub.UserId, Username: fmt.Sprintf("reset-user-%d", sub.UserId), AffCode: fmt.Sprintf("reset-%d", sub.UserId), Status: common.UserStatusEnabled}
	require.NoError(t, DB.Where("id = ?", sub.UserId).FirstOrCreate(&user).Error)
	require.NoError(t, DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(sub).Error; err != nil {
			return err
		}
		return createSubscriptionPeriodTx(tx, sub, consumed, "legacy_opening")
	}))
}

func getSubscriptionResetConsumed(t *testing.T, id int) int64 {
	t.Helper()
	period, err := GetSubscriptionPeriodSummary(t.Context(), id)
	require.NoError(t, err)
	return period.Consumed.Nanos
}

func getSubscriptionResetSub(t *testing.T, id int) UserSubscription {
	t.Helper()
	var sub UserSubscription
	require.NoError(t, DB.Where("id = ?", id).First(&sub).Error)
	return sub
}

func TestAdminResetUserSubscriptionsByPlanResetsAllActiveMatchesAndAdvancesTime(t *testing.T) {
	usePaymentCompletionDB(t)

	now := GetDBTimestamp()
	plan := &SubscriptionPlan{
		Id:    9101,
		Title: "Pro",
		Price: usd(10000000000), MoneyVersion: 1,
		DurationUnit:     SubscriptionDurationMonth,
		DurationValue:    1,
		Allowance:        usd(2000000),
		QuotaResetPeriod: SubscriptionResetDaily,
	}
	otherPlan := &SubscriptionPlan{
		Id:    9102,
		Title: "Basic",
		Price: usd(1000000000), MoneyVersion: 1,
		DurationUnit:     SubscriptionDurationMonth,
		DurationValue:    1,
		Allowance:        usd(200000),
		QuotaResetPeriod: SubscriptionResetDaily,
	}
	seedSubscriptionResetPlan(t, plan)
	seedSubscriptionResetPlan(t, otherPlan)

	activeEnd := now + 30*24*3600
	expiredEnd := now - 1
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9201, UserId: 101, PlanId: plan.Id, Allowance: usd(2000000), MoneyVersion: 1, StartTime: now - 3600, EndTime: activeEnd, Status: "active", LastResetTime: now - 3600, NextResetTime: now + 120}, usd(600000))
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9202, UserId: 101, PlanId: plan.Id, Allowance: usd(2000000), MoneyVersion: 1, StartTime: now - 3600, EndTime: activeEnd, Status: "active", LastResetTime: now - 3600, NextResetTime: now + 120}, usd(1000000))
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9203, UserId: 101, PlanId: otherPlan.Id, Allowance: usd(200000), MoneyVersion: 1, StartTime: now - 3600, EndTime: activeEnd, Status: "active", LastResetTime: now - 3600, NextResetTime: now + 120}, usd(120000))
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9204, UserId: 101, PlanId: plan.Id, Allowance: usd(2000000), MoneyVersion: 1, StartTime: now - 7200, EndTime: expiredEnd, Status: "active", LastResetTime: now - 3600, NextResetTime: now - 10}, usd(1400000))
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9205, UserId: 102, PlanId: plan.Id, Allowance: usd(2000000), MoneyVersion: 1, StartTime: now - 3600, EndTime: activeEnd, Status: "active", LastResetTime: now - 3600, NextResetTime: now + 120}, usd(1600000))
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9206, UserId: 101, PlanId: plan.Id, Allowance: usd(2000000), MoneyVersion: 1, StartTime: now - 3600, EndTime: activeEnd, Status: "cancelled", LastResetTime: now - 3600, NextResetTime: now + 120}, usd(1800000))

	beforeReset := GetDBTimestamp()
	result, err := AdminResetUserSubscriptionsByPlan(101, plan.Id, true)
	afterReset := GetDBTimestamp()

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, plan.Id, result.PlanId)
	assert.Equal(t, 2, result.MatchedCount)
	assert.Equal(t, 2, result.ResetCount)
	assert.Equal(t, 1, result.UserCount)
	assert.Equal(t, []int{101}, result.AffectedUserIds)
	assert.True(t, result.AdvanceResetTime)

	for _, id := range []int{9201, 9202} {
		sub := getSubscriptionResetSub(t, id)
		assert.Zero(t, getSubscriptionResetConsumed(t, sub.Id))
		period, err := GetSubscriptionPeriodSummary(t.Context(), sub.Id)
		require.NoError(t, err)
		assert.EqualValues(t, 2, period.Sequence)
		assert.GreaterOrEqual(t, sub.LastResetTime, beforeReset)
		assert.LessOrEqual(t, sub.LastResetTime, afterReset)
		assert.Equal(t, calcNextResetTime(time.Unix(sub.LastResetTime, 0), plan, sub.EndTime), sub.NextResetTime)
	}
	assert.EqualValues(t, 120000, getSubscriptionResetConsumed(t, 9203))
	assert.EqualValues(t, 1400000, getSubscriptionResetConsumed(t, 9204))
	assert.EqualValues(t, 1600000, getSubscriptionResetConsumed(t, 9205))
	assert.EqualValues(t, 1800000, getSubscriptionResetConsumed(t, 9206))
}

func TestAdminResetUserSubscriptionsByPlanKeepsResetTimes(t *testing.T) {
	usePaymentCompletionDB(t)

	now := GetDBTimestamp()
	plan := &SubscriptionPlan{
		Id:    9301,
		Title: "Team",
		Price: usd(20000000000), MoneyVersion: 1,
		DurationUnit:     SubscriptionDurationMonth,
		DurationValue:    1,
		Allowance:        usd(4000000),
		QuotaResetPeriod: SubscriptionResetMonthly,
	}
	seedSubscriptionResetPlan(t, plan)

	lastReset := now - 86400
	nextReset := now + 86400
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9302, UserId: 201, PlanId: plan.Id, Allowance: usd(4000000), MoneyVersion: 1, StartTime: now - 172800, EndTime: now + 30*24*3600, Status: "active", LastResetTime: lastReset, NextResetTime: nextReset}, usd(2400000))

	result, err := AdminResetUserSubscriptionsByPlan(201, plan.Id, false)

	require.NoError(t, err)
	assert.False(t, result.AdvanceResetTime)
	sub := getSubscriptionResetSub(t, 9302)
	assert.Zero(t, getSubscriptionResetConsumed(t, sub.Id))
	period, err := GetSubscriptionPeriodSummary(t.Context(), sub.Id)
	require.NoError(t, err)
	assert.EqualValues(t, 2, period.Sequence)
	assert.Equal(t, lastReset, sub.LastResetTime)
	assert.Equal(t, nextReset, sub.NextResetTime)
}

func TestAdminResetUserSubscriptionsByPlanNoActiveMatchReturnsError(t *testing.T) {
	usePaymentCompletionDB(t)

	now := GetDBTimestamp()
	plan := &SubscriptionPlan{
		Id:    9401,
		Title: "Expired",
		Price: usd(10000000000), MoneyVersion: 1,
		DurationUnit:  SubscriptionDurationMonth,
		DurationValue: 1,
		Allowance:     usd(2000000),
	}
	seedSubscriptionResetPlan(t, plan)
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9402, UserId: 301, PlanId: plan.Id, Allowance: usd(2000000), MoneyVersion: 1, StartTime: now - 7200, EndTime: now - 1, Status: "active"}, usd(1000000))

	result, err := AdminResetUserSubscriptionsByPlan(301, plan.Id, true)

	require.Error(t, err)
	assert.Nil(t, result)
	assert.True(t, strings.Contains(err.Error(), "This user has no active subscription to this plan"))
}

func TestAdminResetPlanSubscriptionsResetsAllActiveUsers(t *testing.T) {
	usePaymentCompletionDB(t)

	now := GetDBTimestamp()
	plan := &SubscriptionPlan{
		Id:    9501,
		Title: "Business",
		Price: usd(30000000000), MoneyVersion: 1,
		DurationUnit:     SubscriptionDurationMonth,
		DurationValue:    1,
		Allowance:        usd(6000000),
		QuotaResetPeriod: SubscriptionResetNever,
	}
	seedSubscriptionResetPlan(t, plan)

	activeEnd := now + 30*24*3600
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9502, UserId: 401, PlanId: plan.Id, Allowance: usd(6000000), MoneyVersion: 1, StartTime: now - 3600, EndTime: activeEnd, Status: "active", LastResetTime: now - 3600, NextResetTime: now + 10}, usd(2000000))
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9503, UserId: 401, PlanId: plan.Id, Allowance: usd(6000000), MoneyVersion: 1, StartTime: now - 3500, EndTime: activeEnd, Status: "active", LastResetTime: now - 3600, NextResetTime: now + 10}, usd(2200000))
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9504, UserId: 402, PlanId: plan.Id, Allowance: usd(6000000), MoneyVersion: 1, StartTime: now - 3400, EndTime: activeEnd, Status: "active", LastResetTime: now - 3600, NextResetTime: now + 10}, usd(2400000))
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9505, UserId: 403, PlanId: plan.Id, Allowance: usd(6000000), MoneyVersion: 1, StartTime: now - 7200, EndTime: now - 1, Status: "active", LastResetTime: now - 3600, NextResetTime: now - 10}, usd(2600000))
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9506, UserId: 404, PlanId: plan.Id, Allowance: usd(6000000), MoneyVersion: 1, StartTime: now - 3600, EndTime: activeEnd, Status: "cancelled", LastResetTime: now - 3600, NextResetTime: now + 10}, usd(2800000))

	result, err := AdminResetPlanSubscriptions(plan.Id, true)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 3, result.MatchedCount)
	assert.Equal(t, 3, result.ResetCount)
	assert.Equal(t, 2, result.UserCount)
	assert.Equal(t, []int{401, 402}, result.AffectedUserIds)
	for _, id := range []int{9502, 9503, 9504} {
		sub := getSubscriptionResetSub(t, id)
		assert.Zero(t, getSubscriptionResetConsumed(t, sub.Id))
		period, err := GetSubscriptionPeriodSummary(t.Context(), sub.Id)
		require.NoError(t, err)
		assert.EqualValues(t, 2, period.Sequence)
		assert.Zero(t, sub.LastResetTime)
		assert.Zero(t, sub.NextResetTime)
	}
	assert.EqualValues(t, 2600000, getSubscriptionResetConsumed(t, 9505))
	assert.EqualValues(t, 2800000, getSubscriptionResetConsumed(t, 9506))
}

func TestAdminResetPlanSubscriptionsNoMatchSucceeds(t *testing.T) {
	usePaymentCompletionDB(t)

	plan := &SubscriptionPlan{
		Id:    9601,
		Title: "Empty",
		Price: usd(10000000000), MoneyVersion: 1,
		DurationUnit:  SubscriptionDurationMonth,
		DurationValue: 1,
		Allowance:     usd(2000000),
	}
	seedSubscriptionResetPlan(t, plan)

	result, err := AdminResetPlanSubscriptions(plan.Id, true)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Zero(t, result.MatchedCount)
	assert.Zero(t, result.ResetCount)
	assert.Zero(t, result.UserCount)
	assert.Empty(t, result.AffectedUserIds)
}
