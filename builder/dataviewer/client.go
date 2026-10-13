package dataviewer

import (
	"context"
	"fmt"
	"net/url"

	"opencsg.com/csghub-server/api/httpbase"
	"opencsg.com/csghub-server/builder/rpc"
	"opencsg.com/csghub-server/common/config"
	"opencsg.com/csghub-server/common/types"
)

type DataviewerClient interface {
	TriggerWorkflow(ctx context.Context, req types.UpdateViewerReq) (*types.WorkFlowInfo, error)
	// GetCatalog returns the converted dataset catalog (configs, splits and
	// row counts) of a dataset's preview branch.
	GetCatalog(ctx context.Context, namespace, name string) (*CatalogResult, error)
	// GetRows returns up to per sample rows of one config/split.
	GetRows(ctx context.Context, namespace, name, configName, split string, per int) (*RowsResult, error)
}

// SplitInfo is one split of a converted dataset config.
type SplitInfo struct {
	Name        string `json:"name"`
	NumExamples int    `json:"num_examples"`
}

// DatasetInfoEntry groups splits by config name.
type DatasetInfoEntry struct {
	ConfigName string      `json:"config_name"`
	Splits     []SplitInfo `json:"splits"`
}

// ConfigEntry is one config of a converted dataset.
type ConfigEntry struct {
	ConfigName string `json:"config_name"`
}

// CatalogResult is the subset of the dataviewer catalog response needed by
// dataset introspection.
type CatalogResult struct {
	Configs      []ConfigEntry      `json:"configs"`
	DatasetInfos []DatasetInfoEntry `json:"dataset_info"`
	RowsNum      int64              `json:"rows_num"`
	Status       int                `json:"status"`
}

// RowsResult is the subset of the dataviewer rows response needed for sample
// previews.
type RowsResult struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
	Total   int      `json:"total"`
}

type dataviewerClientImpl struct {
	hc *rpc.HttpClient
}

func NewDataviewerClient(config *config.Config, opts ...rpc.RequestOption) DataviewerClient {
	remoteURL := fmt.Sprintf("%s:%d", config.DataViewer.Host, config.DataViewer.Port)
	return &dataviewerClientImpl{
		hc: rpc.NewHttpClient(remoteURL, opts...),
	}
}

func (c *dataviewerClientImpl) TriggerWorkflow(ctx context.Context, req types.UpdateViewerReq) (*types.WorkFlowInfo, error) {
	url := fmt.Sprintf("/api/v1/%ss/%s/%s/callback/%s", req.RepoType, req.Namespace, req.Name, req.Branch)
	var r httpbase.R
	r.Data = &types.WorkFlowInfo{}
	err := c.hc.Post(ctx, url, nil, &r)
	if err != nil {
		return nil, fmt.Errorf("fail trigger workflow repo %s/%s branch %s", req.Namespace, req.Name, req.Branch)
	}

	return r.Data.(*types.WorkFlowInfo), nil
}

func (c *dataviewerClientImpl) GetCatalog(ctx context.Context, namespace, name string) (*CatalogResult, error) {
	url := fmt.Sprintf("/api/v1/datasets/%s/%s/dataviewer/catalog", namespace, name)
	var r httpbase.R
	r.Data = &CatalogResult{}
	if err := c.hc.Get(ctx, url, &r); err != nil {
		return nil, fmt.Errorf("fail get dataviewer catalog of dataset %s/%s: %w", namespace, name, err)
	}
	return r.Data.(*CatalogResult), nil
}

func (c *dataviewerClientImpl) GetRows(ctx context.Context, namespace, name, configName, split string, per int) (*RowsResult, error) {
	path := fmt.Sprintf("/api/v1/datasets/%s/%s/dataviewer/rows?config=%s&split=%s&per=%d&page=1",
		namespace, name, url.QueryEscape(configName), url.QueryEscape(split), per)
	var r httpbase.R
	r.Data = &RowsResult{}
	if err := c.hc.Get(ctx, path, &r); err != nil {
		return nil, fmt.Errorf("fail get dataviewer rows of dataset %s/%s: %w", namespace, name, err)
	}
	return r.Data.(*RowsResult), nil
}
