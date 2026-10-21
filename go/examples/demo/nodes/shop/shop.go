// Package shop is the demo's order source. Order "1" has two lines; any other id is missing.
package shop

import (
	"context"

	sidmesh "gitlab.com/sidcorp-it/sidmesh/go"
)

// Input, config and output types come from contracts_gen.go (sidmesh codegen).

func init() {
	sidmesh.Register("shop.fetch_order", 1, FetchOrderV1)
}

func FetchOrderV1(_ context.Context, in FetchOrderV1Input, _ FetchOrderV1Config, _ sidmesh.Meta) (sidmesh.Result, error) {
	if in.OrderID != "1" {
		return sidmesh.Result{Outcome: "missing", Output: FetchOrderV1MissingOutput{}}, nil
	}
	return sidmesh.Result{Outcome: "found", Output: FetchOrderV1FoundOutput{
		Items: []FetchOrderV1FoundOutputItemsItem{{SKU: "A", Qty: 1}, {SKU: "B", Qty: 2}},
	}}, nil
}
