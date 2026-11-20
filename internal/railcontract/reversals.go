package railcontract

import (
	"context"
	"errors"
	"time"
)

// ErrEvidenceConflict identifies a response from the configured processor
// that cannot legitimately describe the original financial operation.
var ErrEvidenceConflict = errors.New("processor evidence conflicts with original operation")

type ReleaseInventory struct {
	Fact          Fact      `json:"payment"`
	Complete      bool      `json:"complete"`
	ObservedAt    time.Time `json:"observed_at"`
	Refunds       []Fact    `json:"refunds"`
	DisputeStatus string    `json:"dispute_status"`
}
type ReleaseInspector interface {
	InspectRelease(context.Context, Command) (ReleaseInventory, error)
}
