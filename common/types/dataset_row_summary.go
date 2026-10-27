package types

// DatasetRowSummary describes all visible matches, independently of pagination.
type DatasetRowSummary struct {
	// Encode int64 as decimal text to preserve precision in browser clients.
	TotalRows         int64 `json:"total_rows,string"`
	TotalDatasets     int64 `json:"total_datasets"`
	CountedDatasets   int64 `json:"counted_datasets"`
	UncountedDatasets int64 `json:"uncounted_datasets"`
	Complete          bool  `json:"complete"`
}
