package bloby

// BlobCapabilityClaims is the signed payload for server-direct blob
// transfers.
type BlobCapabilityClaims struct {
	Op          string `json:"op"`
	Key         string `json:"key"`
	Exp         int64  `json:"exp"`
	ContentType string `json:"content_type,omitempty"`
}
