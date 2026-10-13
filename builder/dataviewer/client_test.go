package dataviewer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"opencsg.com/csghub-server/api/httpbase"
	"opencsg.com/csghub-server/common/config"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) (DataviewerClient, func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	cfg := &config.Config{}
	cfg.DataViewer.Host = "http://" + u.Hostname()
	cfg.DataViewer.Port = port
	return NewDataviewerClient(cfg), srv.Close
}

func TestGetCatalog(t *testing.T) {
	client, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/datasets/opencompass/mmlu/dataviewer/catalog", r.URL.Path)
		var resp httpbase.R
		resp.Data = &CatalogResult{
			RowsNum: 14042,
			Configs: []ConfigEntry{{ConfigName: "all"}},
			DatasetInfos: []DatasetInfoEntry{{
				ConfigName: "all",
				Splits:     []SplitInfo{{Name: "test", NumExamples: 14042}},
			}},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	defer cleanup()

	catalog, err := client.GetCatalog(context.Background(), "opencompass", "mmlu")
	require.NoError(t, err)
	require.Equal(t, int64(14042), catalog.RowsNum)
	require.Equal(t, []DatasetInfoEntry{{
		ConfigName: "all",
		Splits:     []SplitInfo{{Name: "test", NumExamples: 14042}},
	}}, catalog.DatasetInfos)
}

func TestGetRows(t *testing.T) {
	client, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/datasets/opencompass/mmlu/dataviewer/rows", r.URL.Path)
		require.Equal(t, "all configs", r.URL.Query().Get("config"))
		require.Equal(t, "test", r.URL.Query().Get("split"))
		require.Equal(t, "3", r.URL.Query().Get("per"))
		require.Equal(t, "1", r.URL.Query().Get("page"))
		var resp httpbase.R
		resp.Data = &RowsResult{
			Columns: []string{"question", "answer"},
			Rows:    [][]any{{"1+1?", "2"}},
			Total:   14042,
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	defer cleanup()

	rows, err := client.GetRows(context.Background(), "opencompass", "mmlu", "all configs", "test", 3)
	require.NoError(t, err)
	require.Equal(t, []string{"question", "answer"}, rows.Columns)
	require.Equal(t, [][]any{{"1+1?", "2"}}, rows.Rows)
	require.Equal(t, 14042, rows.Total)
}

func TestGetCatalog_ServerError(t *testing.T) {
	client, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer cleanup()

	_, err := client.GetCatalog(context.Background(), "opencompass", "mmlu")
	require.Error(t, err)
}
