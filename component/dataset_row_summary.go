package component

import (
	"context"

	"opencsg.com/csghub-server/common/types"
)

func (c *repoComponentImpl) DatasetRowSummary(ctx context.Context, filter *types.RepoFilter) (*types.DatasetRowSummary, error) {
	scope, err := c.loadRepositoryReadScope(ctx, filter.Username)
	if err != nil {
		return nil, err
	}
	return c.repoStore.DatasetRowSummary(ctx, scope, filter)
}
