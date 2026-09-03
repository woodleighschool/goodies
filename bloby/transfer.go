package bloby

// Upload strategies supported by the shared browser transfer contract.
const (
	StrategyDirectPut = "direct-put"
	StrategyMultipart = "multipart"
)

// UploadAction describes the upload selected by Service. Direct uploads have a
// target; multipart uploads use Service's part signing and completion endpoints.
type UploadAction struct {
	Strategy string        `json:"strategy"`
	Target   *UploadTarget `json:"target,omitempty"`
}
