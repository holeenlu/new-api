package model

import (
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestChannelSortOptionsDefaultUsesNameThenID(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Channel{}))

	channels := []*Channel{
		{Name: "zeta", Key: "key-z", Group: "default"},
		{Name: "alpha", Key: "key-a", Group: "default"},
		{Name: "alpha", Key: "key-a-2", Group: "default"},
	}
	for _, channel := range channels {
		require.NoError(t, db.Create(channel).Error)
	}

	var sorted []*Channel
	options := NewChannelSortOptions("", "", false)
	require.NoError(t, options.Apply(db).Find(&sorted).Error)
	require.Equal(t, []string{"alpha", "alpha", "zeta"}, []string{
		sorted[0].Name,
		sorted[1].Name,
		sorted[2].Name,
	})
	require.Less(t, sorted[0].Id, sorted[1].Id)
}

func TestChannelSortOptionsIDSortOverridesDefault(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Channel{}))

	for _, channel := range []*Channel{
		{Name: "alpha", Key: "key-a", Group: "default"},
		{Name: "zeta", Key: "key-z", Group: "default"},
	} {
		require.NoError(t, db.Create(channel).Error)
	}

	var sorted []*Channel
	options := NewChannelSortOptions("", "", true)
	require.NoError(t, options.Apply(db).Find(&sorted).Error)
	require.Greater(t, sorted[0].Id, sorted[1].Id)
}
