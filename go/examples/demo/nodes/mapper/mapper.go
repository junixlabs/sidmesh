// Package mapper is the demo's ai node: confident about every sku except "B", which goes to
// a person through the gate.
package mapper

import (
	"context"

	sidmesh "github.com/junixlabs/sidmesh/go"
)

// SuggestV1Input, SuggestV1Config and SuggestV1SuggestedOutput come from contracts_gen.go.

func init() {
	sidmesh.Register("mapper.suggest", 1, SuggestV1)
}

func SuggestV1(_ context.Context, in SuggestV1Input, _ SuggestV1Config, _ sidmesh.Meta) (sidmesh.Result, error) {
	confidence := 0.93
	if in.SKU == "B" {
		confidence = 0.41
	}
	return sidmesh.Result{Outcome: "suggested", Output: SuggestV1SuggestedOutput{
		OptionSet: "T-shirt 2D - " + in.SKU, Confidence: confidence,
	}}, nil
}
