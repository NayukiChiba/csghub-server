package database_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"opencsg.com/csghub-server/builder/store/database"
	"opencsg.com/csghub-server/common/config"
	"opencsg.com/csghub-server/common/tests"
	"opencsg.com/csghub-server/common/types"
)

func TestDatasetRowSummaryIntegration(t *testing.T) {
	ctx := context.Background()
	db := tests.InitTestDB()
	store := database.NewRepoStoreWithDB(db)
	create := func(name string, private bool, card string) *database.Repository {
		repo, err := store.CreateRepo(ctx, database.Repository{Path: "row-summary/" + name, GitPath: "datasets_row-summary/" + name, Name: name, RepositoryType: types.DatasetRepo, Private: private, Source: "local"})
		require.NoError(t, err)
		_, err = db.Operator.Core.NewInsert().Model(&database.Dataset{RepositoryID: repo.ID}).Exec(ctx)
		require.NoError(t, err)
		if card != "" {
			workflow := fmt.Sprintf("summary-%d", repo.ID)
			_, err = db.Operator.Core.NewInsert().Model(&database.DataviewerJob{RepoID: repo.ID, WorkflowID: workflow, Status: types.WorkflowDone, CardData: card}).Exec(ctx)
			require.NoError(t, err)
			_, err = db.Operator.Core.NewInsert().Model(&database.Dataviewer{RepoID: repo.ID, RepoPath: repo.Path, WorkflowID: workflow}).Exec(ctx)
			require.NoError(t, err)
		}
		return repo
	}
	public := create("public", false, `{"rows_num":10}`)
	private := create("private", true, `{"rows_num":20}`)
	create("unknown", false, "")
	// Two matching tags must not multiply the public repository's rows.
	for _, name := range []string{"qa", "text"} {
		tag := &database.Tag{Name: name, Category: "row-summary-test", Scope: types.DatasetTagScope}
		_, err := db.Operator.Core.NewInsert().Model(tag).Exec(ctx)
		require.NoError(t, err)
		_, err = db.Operator.Core.NewInsert().Model(&database.RepositoryTag{RepositoryID: public.ID, TagID: tag.ID}).Exec(ctx)
		require.NoError(t, err)
	}
	filter := &types.RepoFilter{Owner: "row-summary", Sort: "recently_update"}
	got, err := store.DatasetRowSummary(ctx, database.NewRepositoryAccessScope(database.RepositoryAccessPublic, nil), filter)
	require.NoError(t, err)
	require.Equal(t, int64(10), got.TotalRows)
	require.Equal(t, int64(2), got.TotalDatasets)
	require.Equal(t, int64(1), got.UncountedDatasets)
	got, err = store.DatasetRowSummary(ctx, database.NewRepositoryAccessScope(database.RepositoryAccessReadable, []int64{private.ID}), filter)
	require.NoError(t, err)
	require.Equal(t, int64(30), got.TotalRows)
	require.Equal(t, int64(3), got.TotalDatasets)
	filter.Tags = []types.TagReq{{Category: "row-summary-test"}}
	got, err = store.DatasetRowSummary(ctx, database.NewRepositoryAccessScope(database.RepositoryAccessAdmin, nil), filter)
	require.NoError(t, err)
	require.Equal(t, int64(10), got.TotalRows)
	require.Equal(t, int64(1), got.TotalDatasets)
	require.True(t, got.Complete)
	list, _, err := store.PublicToUserWithAccess(ctx, types.DatasetRepo, database.NewRepositoryAccessScope(database.RepositoryAccessAdmin, nil), filter, 20, 1)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, public.ID, list[0].ID)
	require.Len(t, list[0].Tags, 2)
	filter.Tags = nil
	filter.Source = "huggingface"
	got, err = store.DatasetRowSummary(ctx, database.NewRepositoryAccessScope(database.RepositoryAccessAdmin, nil), filter)
	require.NoError(t, err)
	require.Zero(t, got.TotalRows)
	require.Zero(t, got.TotalDatasets)
	require.True(t, got.Complete)

	// Exercise PostgreSQL search using the same vector predicate as the list.
	cfg := &config.Config{}
	cfg.Database.Driver = "pg"
	cfg.Database.SearchConfiguration = "simple"
	searchStore := database.NewRepoStoreWithCache(cfg, db, nil)
	_, err = db.Operator.Core.NewUpdate().Model((*database.Repository)(nil)).
		Set("search_vector = to_tsvector('simple', name)").
		Where("id IN (?)", public.ID).Exec(ctx)
	require.NoError(t, err)
	filter.Source = ""
	filter.Search = "  PUBLIC  "
	got, err = searchStore.DatasetRowSummary(ctx, database.NewRepositoryAccessScope(database.RepositoryAccessPublic, nil), filter)
	require.NoError(t, err)
	require.Equal(t, int64(10), got.TotalRows)
	require.Equal(t, int64(1), got.TotalDatasets)
	require.True(t, got.Complete)
	require.Equal(t, "  PUBLIC  ", filter.Search, "summary must not mutate the caller's filters")
}
