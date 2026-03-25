package optimizer

// Proposal represents a proposed optimization from the LLM
type Proposal struct {
	Hypothesis string `json:"hypothesis"`
	Change     string `json:"change"` // human-readable description
	File       string `json:"file"`
	OldCode    string `json:"old_code"`
	NewCode    string `json:"new_code"`
}

// ProposalResponse is the expected JSON response from the LLM
type ProposalResponse struct {
	Proposal  *Proposal `json:"proposal,omitempty"`
	Done      bool      `json:"done,omitempty"`       // true if LLM thinks no more optimizations possible
	DoneReason string   `json:"done_reason,omitempty"` // why it's done
}
