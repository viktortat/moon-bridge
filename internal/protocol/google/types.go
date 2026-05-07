// Package google implements the Google Generative AI (Gemini) ProviderAdapter for MoonBridge.
package google

import "encoding/json"

// ============================================================================
// Request DTOs
// ============================================================================

// GenerateContentRequest maps to Gemini's generateContent request body.
// https://ai.google.dev/api/generate-content
type GenerateContentRequest struct {
	Contents         []Content            `json:"contents"`
	SystemInstruction *Content            `json:"system_instruction,omitempty"`
	SafetySettings   []SafetySetting      `json:"safety_settings,omitempty"`
	GenerationConfig *GenerationConfig    `json:"generation_config,omitempty"`
	Tools            []Tool               `json:"tools,omitempty"`
	ToolConfig       json.RawMessage      `json:"tool_config,omitempty"`
	// CachedContent references a CachedContent resource for prompt caching.
	// When set, system_instruction, tools, and tool_config must not be set
	// (Gemini API constraint — they become part of the cached content).
	CachedContent string `json:"cached_content,omitempty"`
}

// Content represents a single message content in Gemini's format.
// Role is "user" or "model" (not "assistant").
type Content struct {
	Role  string `json:"role,omitempty"` // "user" | "model"
	Parts []Part `json:"parts"`
}

// Part represents a single part within Content.
type Part struct {
	Text             string             `json:"text,omitempty"`
	InlineData       *Blob              `json:"inline_data,omitempty"`
	FileData         *FileData          `json:"file_data,omitempty"`
	FunctionCall     *FunctionCall      `json:"function_call,omitempty"`
	FunctionResponse *FunctionResponse  `json:"function_response,omitempty"`
}

// Blob represents inline binary data.
type Blob struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"`
}

// FileData represents a reference to external file data.
type FileData struct {
	MimeType string `json:"mime_type"`
	FileURI  string `json:"file_uri"`
}

// FunctionCall represents a function call request from the model.
type FunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// FunctionResponse represents a function call response from the user.
type FunctionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

// SafetySetting configures content safety filtering.
type SafetySetting struct {
	Category  string `json:"category"`
	Threshold string `json:"threshold"`
}

// GenerationConfig controls text generation parameters.
type GenerationConfig struct {
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
	TopK             *float64 `json:"top_k,omitempty"`
	MaxOutputTokens  int      `json:"max_output_tokens,omitempty"`
	StopSequences    []string `json:"stop_sequences,omitempty"`
	ResponseMimeType string   `json:"response_mime_type,omitempty"`
	CandidateCount   int      `json:"candidate_count,omitempty"`
}

// Tool represents a tool available to the model.
type Tool struct {
	FunctionDeclarations []FunctionDeclaration `json:"function_declarations,omitempty"`
}

// FunctionDeclaration declares a function that the model may call.
type FunctionDeclaration struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ============================================================================
// Response DTOs
// ============================================================================

// GenerateContentResponse maps to a Gemini streaming or non-streaming response chunk.
// In streaming mode (streamGenerateContent), each SSE data: line contains one
// complete GenerateContentResponse snapshot.
type GenerateContentResponse struct {
	Candidates     []Candidate     `json:"candidates"`
	PromptFeedback *PromptFeedback `json:"prompt_feedback,omitempty"`
	UsageMetadata  *UsageMetadata  `json:"usage_metadata,omitempty"`
}

// Candidate represents a single response candidate.
type Candidate struct {
	Index         int             `json:"index"`
	Content       Content         `json:"content"`
	FinishReason  string          `json:"finish_reason"` // STOP, MAX_TOKENS, SAFETY, RECITATION, OTHER
	SafetyRatings []SafetyRating  `json:"safety_ratings,omitempty"`
}

// SafetyRating represents a safety rating for a category.
type SafetyRating struct {
	Category    string `json:"category"`
	Probability string `json:"probability"`
	Blocked     bool   `json:"blocked,omitempty"`
}

// PromptFeedback contains feedback on the prompt's safety.
type PromptFeedback struct {
	SafetyRatings []SafetyRating `json:"safety_ratings,omitempty"`
	BlockReason   string         `json:"block_reason,omitempty"`
}

// UsageMetadata contains token count information.
type UsageMetadata struct {
	PromptTokenCount     int `json:"prompt_token_count"`
	CandidatesTokenCount int `json:"candidates_token_count"`
	TotalTokenCount      int `json:"total_token_count"`
	// CachedContentTokenCount is the number of tokens served from context cache.
	// Maps to CoreUsage.CachedInputTokens.
	CachedContentTokenCount int `json:"cached_content_token_count,omitempty"`
}

// ============================================================================
// CachedContent DTOs
// ============================================================================

// CachedContentUsageMetadata contains token count info for a CachedContent resource.
type CachedContentUsageMetadata struct {
	TotalTokenCount int `json:"totalToken_count"`
}

// CachedContent represents a Google Gemini CachedContent resource.
type CachedContent struct {
	Name              string                      `json:"name,omitempty"`
	Model             string                      `json:"model"`
	DisplayName       string                      `json:"display_name,omitempty"`
	Contents          []Content                   `json:"contents"`
	SystemInstruction *Content                    `json:"system_instruction,omitempty"`
	Tools             []Tool                      `json:"tools,omitempty"`
	ToolConfig        json.RawMessage             `json:"tool_config,omitempty"`
	TTL               string                      `json:"ttl,omitempty"`
	ExpireTime        string                      `json:"expire_time,omitempty"`
	CreateTime        string                      `json:"create_time,omitempty"`
	UpdateTime        string                      `json:"update_time,omitempty"`
	UsageMetadata     *CachedContentUsageMetadata `json:"usage_metadata,omitempty"`
}

// CreateCachedContentRequest is the request body for POST /cachedContents.
type CreateCachedContentRequest CachedContent

// UpdateCachedContentRequest is the request body for PATCH /cachedContents/{name}.
type UpdateCachedContentRequest struct {
	TTL string `json:"ttl"`
	ExpireTime string `json:"expire_time,omitempty"`
}
