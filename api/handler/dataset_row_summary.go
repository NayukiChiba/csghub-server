package handler

import (
	"fmt"
	"slices"

	"github.com/gin-gonic/gin"
	"opencsg.com/csghub-server/api/httpbase"
	"opencsg.com/csghub-server/common/errorx"
	"opencsg.com/csghub-server/common/types"
)

// datasetFilterFromContext keeps list and summary validation identical.
func datasetFilterFromContext(ctx *gin.Context) (*types.RepoFilter, error) {
	filter := getFilterFromContext(ctx, &types.RepoFilter{
		Tags: parseTagReqs(ctx), Username: httpbase.GetCurrentUser(ctx),
	})
	var err error
	filter.RepoSizeMin, filter.RepoSizeMax, err = parseInt64RangeFromContext(ctx, "repo_size_min", "repo_size_max")
	if err != nil {
		return nil, errorx.ReqParamInvalid(err, errorx.Ctx().Set("query", "repo_size_range"))
	}
	if !slices.Contains(types.DatasetSorts, filter.Sort) {
		return nil, errorx.ReqParamInvalid(fmt.Errorf("sort parameter must be one of %v", types.DatasetSorts), errorx.Ctx().Set("query", "sort_filter"))
	}
	if filter.Source != "" && !slices.Contains(types.Sources, filter.Source) {
		return nil, errorx.ReqParamInvalid(fmt.Errorf("source parameter must be one of %v", types.Sources), errorx.Ctx().Set("query", "source_filter"))
	}
	return filter, nil
}

// RowSummary godoc
// @Summary Summarize sample rows for all visible filtered datasets
// @Description Uses completed DataViewer metadata; ignores pagination and search result caps. Missing counts are reported explicitly. total_rows is an int64 encoded as a decimal string.
// @Tags Dataset
// @Produce json
// @Param search query string false "Search text, same matching rules as dataset list"
// @Param source query string false "Source" Enums(opencsg,huggingface,local)
// @Param tag_category query []string false "Tag categories, paired with tag_name" collectionFormat(multi)
// @Param tag_name query []string false "Tag names" collectionFormat(multi)
// @Param tag_group query []string false "Tag groups" collectionFormat(multi)
// @Param compliance_status query string false "Compliance status"
// @Param dataset_type query string false "Dataset type"
// @Param user_purchased query bool false "Only purchased datasets"
// @Param xnet_migration_status query string false "Xnet migration status"
// @Param repo_size_min query int false "Minimum repository size in bytes"
// @Param repo_size_max query int false "Maximum repository size in bytes"
// @Success 200 {object} types.Response{data=types.DatasetRowSummary}
// @Failure 400 {object} types.APIBadRequest
// @Failure 500 {object} types.APIInternalServerError
// @Router /datasets/row-summary [get]
func (h *DatasetHandler) RowSummary(ctx *gin.Context) {
	filter, err := datasetFilterFromContext(ctx)
	if err != nil {
		httpbase.BadRequestWithExt(ctx, err)
		return
	}
	summary, err := h.repo.DatasetRowSummary(ctx.Request.Context(), filter)
	if err != nil {
		httpbase.ServerError(ctx, err)
		return
	}
	httpbase.OK(ctx, summary)
}
