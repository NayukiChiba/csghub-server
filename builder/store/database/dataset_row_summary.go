package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"opencsg.com/csghub-server/common/types"
)

// DatasetRowSummary streams only persisted viewer metadata, never dataset samples.
// Each repository participates once, even when several tags match.
func (s *repoStoreImpl) DatasetRowSummary(ctx context.Context, scope RepositoryAccessScope, filter *types.RepoFilter) (*types.DatasetRowSummary, error) {
	normalized := *filter
	normalized.Search = strings.ToLower(strings.TrimSpace(filter.Search))
	q, _, err := s.repositoryListQuery(ctx, types.DatasetRepo, &normalized, &scope)
	if err != nil {
		return nil, err
	}
	result := &types.DatasetRowSummary{Complete: true}
	if q == nil {
		return result, nil
	}
	if normalized.Search != "" {
		if s.DbDriver == "pg" {
			q = buildVectorSearchQuery(q, &normalized, s.db.BunDB, s.SearchConfiguration, 0)
		} else {
			buildLikeQuery(q, &normalized, 0)
		}
	}
	ids := s.db.Operator.Core.NewSelect().TableExpr("(?) AS filtered", q).Column("filtered.id").Distinct()
	rows, err := s.db.Operator.Core.NewSelect().TableExpr("(?) AS matched", ids).
		ColumnExpr("job.card_data").
		Join("LEFT JOIN dataviewers AS viewer ON viewer.repo_id = matched.id").
		Join("LEFT JOIN dataviewer_jobs AS job ON job.workflow_id = viewer.workflow_id AND job.status = ?", types.WorkflowDone).
		Rows(ctx)
	if err != nil {
		return nil, fmt.Errorf("query dataset row summary: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var card sql.NullString
		if err := rows.Scan(&card); err != nil {
			return nil, err
		}
		if err := addDatasetRowCount(result, card.String); err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result.UncountedDatasets = result.TotalDatasets - result.CountedDatasets
	result.Complete = result.UncountedDatasets == 0
	return result, nil
}

func addDatasetRowCount(result *types.DatasetRowSummary, card string) error {
	result.TotalDatasets++
	var metadata struct {
		Rows *int64 `json:"rows_num"`
	}
	if err := json.Unmarshal([]byte(card), &metadata); err != nil || metadata.Rows == nil || *metadata.Rows < 0 {
		return nil
	}
	if *metadata.Rows > math.MaxInt64-result.TotalRows {
		return fmt.Errorf("dataset row total exceeds int64")
	}
	result.TotalRows += *metadata.Rows
	result.CountedDatasets++
	return nil
}
