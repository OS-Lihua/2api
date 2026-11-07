package model

import (
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestChannelPriorityPlan(t *testing.T) {
	for _, tc := range []struct {
		name       string
		values     []int64
		id, target int
		position   string
		want       map[int]int64
	}{
		{"midpoint", []int64{100, 80, 0}, 3, 2, "before", map[int]int64{3: 90}},
		{"top preserves peers", []int64{0, 0, 0}, 3, 1, "before", map[int]int64{3: 10}},
		{"bottom", []int64{0, 0, 0}, 1, 3, "after", map[int]int64{1: -10}},
		{"same tier", []int64{100, 80, 0}, 3, 2, "same", map[int]int64{3: 80}},
		{"same slot", []int64{100, 80, 0}, 2, 1, "after", map[int]int64{}},
		{"same tier no op", []int64{0, 0}, 1, 2, "same", map[int]int64{}},
		{"shift smaller upper tier", []int64{100, 99, 99, 0}, 4, 2, "before", map[int]int64{1: 101, 4: 100}},
		{"shift smaller lower tier", []int64{100, 100, 99, 0}, 4, 3, "before", map[int]int64{3: 98, 4: 99}},
		{"shift complete adjacent tiers", []int64{102, 101, 100, 99, 0}, 5, 3, "after", map[int]int64{4: 98, 5: 99}},
		{"upper safe bound", []int64{maxSortablePriority, 0}, 2, 1, "before", map[int]int64{1: maxSortablePriority - 1, 2: maxSortablePriority}},
		{"lower safe bound", []int64{-maxSortablePriority, 0}, 2, 1, "after", map[int]int64{1: -maxSortablePriority + 1, 2: -maxSortablePriority}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := make([]ProviderPriority, len(tc.values))
			for i, value := range tc.values {
				items[i] = ProviderPriority{ID: i + 1, Priority: &value}
			}
			changes, err := planChannelPriorityMove(items, ProviderPriorityMove{ID: tc.id, TargetID: tc.target, Position: tc.position})
			require.NoError(t, err)
			got := map[int]int64{}
			for _, change := range changes {
				require.NotNil(t, change.Priority)
				got[change.ID] = *change.Priority
			}
			assert.Equal(t, tc.want, got)
		})
	}
	_, err := planChannelPriorityMove([]ProviderPriority{{ID: 1}, {ID: 2}}, ProviderPriorityMove{ID: 1, TargetID: 2, Position: "invalid"})
	assert.ErrorIs(t, err, ErrChannelPriorityInvalid)
}

func TestChannelPriorityDatabase(t *testing.T) {
	db := UseTestDatabases(t)
	require.NoError(t, db.AutoMigrate(&Provider{}, &Ability{}))
	high, low, zero := int64(100), int64(99), int64(0)
	w1, w2, w3, w4 := uint(11), uint(22), uint(33), uint(44)
	channels := []Provider{
		{Name: "high", Priority: &high, Weight: &w1, Key: "preserved", Status: 1},
		{Name: "low", Priority: &low, Weight: &w2, Status: 1},
		{Name: "peer", Priority: &low, Weight: &w3, Status: 1},
		{Name: "moving", Priority: &zero, Weight: &w4, Status: 1},
	}
	require.NoError(t, db.Create(&channels).Error)
	for _, channel := range channels {
		for _, group := range []string{"default", "second"} {
			require.NoError(t, db.Create(&Ability{Group: group, Model: "test", ChannelId: channel.Id, Priority: channel.Priority, Weight: uint(*channel.Weight), Enabled: true}).Error)
		}
	}
	initial, err := GetProviderPriorityOrder()
	require.NoError(t, err)
	move := ProviderPriorityMove{Revision: initial.Revision, ID: channels[3].Id, TargetID: channels[1].Id, Position: "before", Preview: true}
	preview, err := ReorderProviderPriority(move)
	require.NoError(t, err)
	require.Len(t, preview.Changes, 2)
	unchanged, err := GetProviderPriorityOrder()
	require.NoError(t, err)
	assert.Equal(t, initial, unchanged, "preview does not persist")
	move.Preview = false
	applied, err := ReorderProviderPriority(move)
	require.NoError(t, err)
	assert.Equal(t, preview, applied)
	for _, change := range applied.Changes {
		var abilities []Ability
		require.NoError(t, db.Where("channel_id = ?", change.ID).Find(&abilities).Error)
		require.Len(t, abilities, 2)
		for _, ability := range abilities {
			assert.Equal(t, change.Priority, ability.Priority)
			assert.True(t, ability.Enabled)
		}
	}
	var saved Provider
	require.NoError(t, db.First(&saved, channels[0].Id).Error)
	assert.Equal(t, "preserved", saved.Key)
	assert.Equal(t, channels[0].Weight, saved.Weight)
	_, err = ReorderProviderPriority(move)
	assert.ErrorIs(t, err, ErrChannelPriorityConflict)
	undo := ProviderPriorityMove{Revision: applied.Revision}
	for _, change := range applied.Changes {
		undo.Restore = append(undo.Restore, ProviderPriorityChange{ID: change.ID, Previous: change.Priority, Priority: change.Previous})
	}
	restored, err := ReorderProviderPriority(undo)
	require.NoError(t, err)
	assert.Equal(t, initial.Items, restored.Items)
	assert.Equal(t, initial.Revision, restored.Revision)
	start := make(chan struct{})
	errorsCh := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() { <-start; _, moveErr := ReorderProviderPriority(move); errorsCh <- moveErr })
	}
	close(start)
	workers.Wait()
	close(errorsCh)
	succeeded, conflicted := 0, 0
	for moveErr := range errorsCh {
		if moveErr == nil {
			succeeded++
		} else {
			require.ErrorIs(t, moveErr, ErrChannelPriorityConflict)
			conflicted++
		}
	}
	assert.Equal(t, 1, succeeded)
	assert.Equal(t, 1, conflicted)
	_, err = ReorderProviderPriority(undo)
	require.NoError(t, err)
	// An ability write failure must roll back every channel change too.
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register("priority_test_failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "abilities" {
			tx.AddError(errors.New("forced routing write failure"))
		}
	}))
	_, err = ReorderProviderPriority(move)
	require.ErrorContains(t, err, "forced routing write failure")
	require.NoError(t, db.Callback().Update().Remove("priority_test_failure"))
	unchanged, err = GetProviderPriorityOrder()
	require.NoError(t, err)
	assert.Equal(t, initial, unchanged)
	// Null priority is an effective zero and undo must restore the null itself.
	require.NoError(t, db.Model(&Provider{}).Where("id = ?", channels[3].Id).Update("priority", nil).Error)
	nullable, err := GetProviderPriorityOrder()
	require.NoError(t, err)
	var sorted []ProviderPriority
	require.NoError(t, NewProviderSortOptions("priority", "asc", false).Apply(db.Model(&Provider{}).Select("id", "priority")).Find(&sorted).Error)
	assert.Equal(t, []int{channels[3].Id, channels[2].Id, channels[1].Id, channels[0].Id}, []int{sorted[0].ID, sorted[1].ID, sorted[2].ID, sorted[3].ID})
	move.Revision = nullable.Revision
	applied, err = ReorderProviderPriority(move)
	require.NoError(t, err)
	undo = ProviderPriorityMove{Revision: applied.Revision}
	for _, change := range applied.Changes {
		undo.Restore = append(undo.Restore, ProviderPriorityChange{ID: change.ID, Previous: change.Priority, Priority: change.Previous})
	}
	restored, err = ReorderProviderPriority(undo)
	require.NoError(t, err)
	assert.Equal(t, nullable.Items, restored.Items)
}
