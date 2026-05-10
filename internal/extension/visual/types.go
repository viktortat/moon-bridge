package visual

import "context"

// ImageInput carries image data in one of: URL, base64 data, or data URL.
type ImageInput struct {
	URL      string `json:"url,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// ConversationTurn is a single entry in visual clarification history.
type ConversationTurn struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// AnalysisRequest bundles the analysis prompt with images for the vision model.
type AnalysisRequest struct {
	Tool   string
	Prompt string
	Images []ImageInput
}

// VisionClient is the interface fulfilled by BridgeCoreClient.
type VisionClient interface {
	Analyze(context.Context, AnalysisRequest) (string, error)
}
