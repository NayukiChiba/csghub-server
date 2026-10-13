package database

import (
	"testing"

	"github.com/stretchr/testify/require"
	"opencsg.com/csghub-server/common/types"
)

// Size orderings are only backed by the repository_statistics join on
// dataset queries; other repo types must fall back to no explicit ordering
// instead of emitting SQL that references a missing table alias.
func TestRepoOrderBy_SizeSortDatasetOnly(t *testing.T) {
	require.Equal(t, "range_repository_statistics.total_size DESC NULLS LAST", repoOrderBy(types.DatasetRepo, "largest"))
	require.Equal(t, "range_repository_statistics.total_size ASC NULLS LAST", repoOrderBy(types.DatasetRepo, "smallest"))
	require.Empty(t, repoOrderBy(types.ModelRepo, "largest"))
	require.Empty(t, repoOrderBy(types.SpaceRepo, "smallest"))
}

func TestRepoOrderBy_NativeAndUnknownSorts(t *testing.T) {
	require.Equal(t, "download_count DESC NULLS LAST", repoOrderBy(types.DatasetRepo, "most_download"))
	require.Equal(t, "download_count DESC NULLS LAST", repoOrderBy(types.ModelRepo, "most_download"))
	require.Empty(t, repoOrderBy(types.DatasetRepo, "no-such-sort"))
	require.Empty(t, repoOrderBy(types.DatasetRepo, ""))
}
