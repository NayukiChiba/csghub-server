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
	require.Equal(t, "range_repository_statistics.total_size DESC NULLS LAST", repoOrderBy(types.DatasetRepo, "largest", false))
	require.Equal(t, "range_repository_statistics.total_size ASC NULLS LAST", repoOrderBy(types.DatasetRepo, "smallest", false))
	require.Empty(t, repoOrderBy(types.ModelRepo, "largest", false))
	require.Empty(t, repoOrderBy(types.SpaceRepo, "smallest", true))
}

// A tag-filtered query dedups with GROUP BY instead of DISTINCT, and SELECT
// DISTINCT/GROUP BY cannot ORDER BY a joined column outside the select list,
// so the size ordering must switch to the aggregate form. The default-branch
// join yields one stats row per repository, keeping MAX/MIN exact.
func TestRepoOrderBy_SizeSortUnderDedupUsesAggregate(t *testing.T) {
	require.Equal(t, "MAX(range_repository_statistics.total_size) DESC NULLS LAST", repoOrderBy(types.DatasetRepo, "largest", true))
	require.Equal(t, "MIN(range_repository_statistics.total_size) ASC NULLS LAST", repoOrderBy(types.DatasetRepo, "smallest", true))
	// non-size orderings never switch form
	require.Equal(t, "download_count DESC NULLS LAST", repoOrderBy(types.DatasetRepo, "most_download", true))
}

func TestRepoOrderBy_NativeAndUnknownSorts(t *testing.T) {
	require.Equal(t, "download_count DESC NULLS LAST", repoOrderBy(types.DatasetRepo, "most_download", false))
	require.Equal(t, "download_count DESC NULLS LAST", repoOrderBy(types.ModelRepo, "most_download", false))
	require.Empty(t, repoOrderBy(types.DatasetRepo, "no-such-sort", false))
	require.Empty(t, repoOrderBy(types.DatasetRepo, "", false))
}

// Creation-time orderings are plain created_at columns, valid for every repo
// type and unchanged under GROUP BY dedup (it lives in the select list).
func TestRepoOrderBy_CreationTimeSorts(t *testing.T) {
	require.Equal(t, "created_at ASC NULLS LAST", repoOrderBy(types.DatasetRepo, "oldest", false))
	require.Equal(t, "created_at DESC NULLS LAST", repoOrderBy(types.DatasetRepo, "newest", false))
	require.Equal(t, "created_at ASC NULLS LAST", repoOrderBy(types.DatasetRepo, "oldest", true))
	require.Equal(t, "created_at ASC NULLS LAST", repoOrderBy(types.ModelRepo, "oldest", false))
}
