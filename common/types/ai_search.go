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
	// Size is the default-branch repository size in bytes; zero when unknown.
	Size int64 `json:"size,omitempty"`
	// CreatedAt is the repository creation date (YYYY-MM-DD).
	CreatedAt string `json:"created_at,omitempty"`
	// UpdatedAt is the repository update date (YYYY-MM-DD).
	UpdatedAt string `json:"updated_at,omitempty"`
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

// AISearchLLMConfig is the admin-editable LLM configuration for dataset AI
// search. It is persisted in agent_configs and overrides the static
// env/toml configuration field by field.
type AISearchLLMConfig struct {
	// LLMEndpoint is the full chat/completions URL of an OpenAI-compatible service.
	LLMEndpoint string `json:"llm_endpoint"`
	// LLMModel is the model name sent in chat requests.
	LLMModel string `json:"llm_model"`
	// LLMAPIKey is sent as "Authorization: Bearer <key>" when non-empty.
	LLMAPIKey string `json:"llm_api_key"`
}

// AISearchLLMTestRequest optionally overrides the saved configuration for a
// connectivity test; empty fields fall back to the saved/static values.
type AISearchLLMTestRequest struct {
	LLMEndpoint string `json:"llm_endpoint,omitempty"`
	LLMModel    string `json:"llm_model,omitempty"`
	LLMAPIKey   string `json:"llm_api_key,omitempty"`
}
