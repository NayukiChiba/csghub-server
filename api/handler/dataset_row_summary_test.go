package handler

import (
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"opencsg.com/csghub-server/common/types"
	"testing"
)

func TestDatasetHandlerRowSummary(t *testing.T) {
	tester := NewDatasetTester(t).WithHandleFunc(func(h *DatasetHandler) gin.HandlerFunc { return h.RowSummary })
	tester.mocks.repo.EXPECT().DatasetRowSummary(tester.Ctx(), mock.MatchedBy(func(f *types.RepoFilter) bool {
		return f.Search == "chat & tools" && f.Source == "local" && len(f.Tags) == 1 && f.Tags[0].Name == "qa"
	})).Return(&types.DatasetRowSummary{TotalRows: 9007199254740993, TotalDatasets: 2, CountedDatasets: 1, UncountedDatasets: 1}, nil)
	tester.WithQuery("search", "chat & tools").WithQuery("source", "local").WithQuery("tag_category", "task").WithQuery("tag_name", "qa").WithQuery("page", "999").WithQuery("per", "1").Execute()
	tester.ResponseEqSimple(t, 200, gin.H{"msg": "OK", "data": gin.H{"total_rows": "9007199254740993", "total_datasets": 2, "counted_datasets": 1, "uncounted_datasets": 1, "complete": false}})
}

func TestDatasetHandlerRowSummaryValidation(t *testing.T) {
	for _, q := range []struct{ k, v string }{{"source", "invalid"}, {"sort", "invalid"}, {"repo_size_min", "-1"}, {"repo_size_max", "abc"}} {
		t.Run(q.k, func(t *testing.T) {
			tester := NewDatasetTester(t).WithHandleFunc(func(h *DatasetHandler) gin.HandlerFunc { return h.RowSummary })
			tester.WithQuery(q.k, q.v).Execute()
			require.Equal(t, 400, tester.Response().Code)
		})
	}
	tester := NewDatasetTester(t).WithHandleFunc(func(h *DatasetHandler) gin.HandlerFunc { return h.RowSummary })
	tester.mocks.repo.EXPECT().DatasetRowSummary(tester.Ctx(), mock.Anything).Return(nil, errors.New("database offline"))
	tester.Execute()
	require.Equal(t, 500, tester.Response().Code)
}
