package component

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"opencsg.com/csghub-server/builder/store/database"
	"opencsg.com/csghub-server/common/types"
	"testing"
)

func TestDatasetRowSummaryPublicScope(t *testing.T) {
	ctx := context.Background()
	repo := initializeTestRepoComponent(ctx, t)
	filter := &types.RepoFilter{Search: "qa"}
	expected := &types.DatasetRowSummary{TotalRows: 12, Complete: true}
	repo.mocks.stores.RepoMock().EXPECT().DatasetRowSummary(ctx, database.NewRepositoryAccessScope(database.RepositoryAccessPublic, nil), filter).Return(expected, nil)
	got, err := repo.DatasetRowSummary(ctx, filter)
	require.NoError(t, err)
	require.Equal(t, expected, got)
}
func TestDatasetRowSummaryFailsClosed(t *testing.T) {
	ctx := context.Background()
	repo := initializeTestRepoComponent(ctx, t)
	repo.mocks.userSvcClient.EXPECT().GetUserInfo(ctx, "alice", "alice").Return(nil, errors.New("identity offline"))
	got, err := repo.DatasetRowSummary(ctx, &types.RepoFilter{Username: "alice"})
	require.Error(t, err)
	require.Nil(t, got)
}
