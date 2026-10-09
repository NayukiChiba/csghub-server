package types

// DatasetSummary is the compact dataset view returned by the AI search tools
// and streamed to chat clients; dataset links may only be built from it.
type DatasetSummary struct {
	Namespace   string   `json:"namespace"`
	Name        string   `json:"name"`
	Nickname    string   `json:"nickname,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Downloads   int64    `json:"downloads"`
	Likes       int64    `json:"likes"`
}

// AIChatMessage is one message in the AI dataset chat history.
type AIChatMessage struct {
	Role    string `json:"role"` // "user" or "assistant"
	Content string `json:"content"`
}

// AIChatRequest is the request body of the dataset AI chat endpoint.
type AIChatRequest struct {
	Message string          `json:"message" binding:"required"`
	History []AIChatMessage `json:"history,omitempty"`
}

// AIChatEvent types emitted during a chat turn.
const (
	// AIChatEventText carries an assistant text chunk.
	AIChatEventText = "text"
	// AIChatEventDatasets carries the datasets actually returned by search
	// tools during this turn; clients must render dataset links only from it.
	AIChatEventDatasets = "datasets"
)

// AIChatEvent is one streamed event of the dataset AI chat.
type AIChatEvent struct {
	Type     string           `json:"type"`
	Text     string           `json:"text,omitempty"`
	Datasets []DatasetSummary `json:"datasets,omitempty"`
}
