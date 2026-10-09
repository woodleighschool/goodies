package bloby

// Upload strategies supported by the shared browser transfer contract.
const (
	StrategyDirectPut = "direct-put"
	StrategyMultipart = "multipart"
)

// UploadAction describes the upload selected by Service. Direct uploads have a
// target; multipart uploads sign each part through Service and are assembled
// when the object is finalized.
type UploadAction struct {
	Strategy string        `json:"strategy"`
	Target   *UploadTarget `json:"target,omitempty"`
}
