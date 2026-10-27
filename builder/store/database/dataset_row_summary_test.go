package database

import (
	"context"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"math"
	"opencsg.com/csghub-server/common/types"
	"regexp"
	"testing"
)

func TestDatasetRowSummaryQueryAndCounts(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(func(expected, actual string) error {
		require.Contains(t, actual, `SELECT DISTINCT "filtered"."id"`)
		require.Contains(t, actual, "repository.private = FALSE")
		require.Contains(t, actual, "repository.id = ANY('{42}'::bigint[])")
		require.Contains(t, actual, "INNER JOIN datasets")
		require.Contains(t, actual, "repository_tags")
		require.Contains(t, actual, "job.workflow_id = viewer.workflow_id AND job.status = 2")
		require.NotContains(t, actual, "LIMIT")
		require.NotContains(t, actual, "OFFSET")
		return nil
	})))
	require.NoError(t, err)
	defer db.Close()
	b := bun.NewDB(db, pgdialect.New())
	b.RegisterModel((*RepositoryTag)(nil))
	store := NewRepoStoreWithDB(&DB{Operator: Operator{Core: b}, BunDB: b})
	mock.ExpectQuery("summary").WillReturnRows(sqlmock.NewRows([]string{"card_data"}).
		AddRow(`{"rows_num":9007199254740993}`).AddRow(`{"rows_num":7}`).AddRow(`{"rows_num":0}`).
		AddRow(nil).AddRow(`{}`).AddRow(`{"rows_num":-1}`).AddRow(`invalid`))
	result, err := store.DatasetRowSummary(context.Background(), NewRepositoryAccessScope(RepositoryAccessReadable, []int64{42}), &types.RepoFilter{Tags: []types.TagReq{{Category: "task", Name: "text"}}, Sort: "trending"})
	require.NoError(t, err)
	require.Equal(t, int64(9007199254741000), result.TotalRows)
	require.Equal(t, int64(7), result.TotalDatasets)
	require.Equal(t, int64(3), result.CountedDatasets)
	require.Equal(t, int64(4), result.UncountedDatasets)
	require.False(t, result.Complete)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDatasetRowSummaryErrorsAndEmpty(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail bool
	}{{"empty", false}, {"database failure", true}} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			b := bun.NewDB(db, pgdialect.New())
			b.RegisterModel((*RepositoryTag)(nil))
			store := NewRepoStoreWithDB(&DB{Operator: Operator{Core: b}, BunDB: b})
			q := mock.ExpectQuery(regexp.QuoteMeta("SELECT job.card_data"))
			if tc.fail {
				q.WillReturnError(errors.New("offline"))
			} else {
				q.WillReturnRows(sqlmock.NewRows([]string{"card_data"}))
			}
			result, err := store.DatasetRowSummary(context.Background(), NewRepositoryAccessScope(RepositoryAccessPublic, nil), &types.RepoFilter{})
			if tc.fail {
				require.Error(t, err)
				require.Nil(t, result)
			} else {
				require.NoError(t, err)
				require.True(t, result.Complete)
				require.Zero(t, result.TotalRows)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestDatasetRowCountOverflow(t *testing.T) {
	result := &types.DatasetRowSummary{TotalRows: math.MaxInt64}
	require.Error(t, addDatasetRowCount(result, `{"rows_num":1}`))
	require.Equal(t, int64(math.MaxInt64), result.TotalRows)
}
