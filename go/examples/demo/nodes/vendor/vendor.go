// Package vendor is the demo's write node, backed by an in-memory "vendor". Placing sku "A"
// records the order and then times out on the first attempt, so the engine has to ask
// find_order what happened instead of placing it twice.
package vendor

import (
	"context"
	"errors"
	"sync"

	sidmesh "github.com/junixlabs/sidmesh/go"
)

// Input, config and output types come from contracts_gen.go (sidmesh codegen).

var (
	mu        sync.Mutex
	orders    = map[string]string{} // idempotency key -> po id
	cancelled = map[string]bool{}   // po id
)

func init() {
	sidmesh.Register("vendor.place_order", 1, PlaceOrderV1)
	sidmesh.Register("vendor.find_order", 1, FindOrderV1)
	sidmesh.Register("vendor.cancel_order", 1, CancelOrderV1)
	sidmesh.Register("vendor.find_cancel", 1, FindCancelV1)
}

func PlaceOrderV1(_ context.Context, in PlaceOrderV1Input, _ PlaceOrderV1Config, meta sidmesh.Meta) (sidmesh.Result, error) {
	mu.Lock()
	defer mu.Unlock()
	po, exists := orders[meta.IdempotencyKey]
	if !exists {
		po = "PO-" + in.OrderID + "-" + in.SKU
		orders[meta.IdempotencyKey] = po
	}
	if in.SKU == "A" && meta.Attempt == 1 {
		return sidmesh.Result{}, errors.New("vendor did not answer in time")
	}
	return sidmesh.Result{Outcome: "accepted", Output: PlaceOrderV1AcceptedOutput{POID: po}}, nil
}

func FindOrderV1(_ context.Context, _ FindOrderV1Input, _ FindOrderV1Config, meta sidmesh.Meta) (sidmesh.Result, error) {
	mu.Lock()
	defer mu.Unlock()
	if po, ok := orders[meta.IdempotencyKey]; ok {
		return sidmesh.Result{Outcome: "accepted", Output: FindOrderV1AcceptedOutput{POID: po}}, nil
	}
	return sidmesh.Result{Outcome: "not_found", Output: FindOrderV1NotFoundOutput{}}, nil
}

func CancelOrderV1(_ context.Context, in CancelOrderV1Input, _ CancelOrderV1Config, _ sidmesh.Meta) (sidmesh.Result, error) {
	mu.Lock()
	defer mu.Unlock()
	cancelled[in.POID] = true
	return sidmesh.Result{Outcome: "cancelled", Output: CancelOrderV1CancelledOutput{}}, nil
}

func FindCancelV1(_ context.Context, in FindCancelV1Input, _ FindCancelV1Config, _ sidmesh.Meta) (sidmesh.Result, error) {
	mu.Lock()
	defer mu.Unlock()
	if cancelled[in.POID] {
		return sidmesh.Result{Outcome: "cancelled", Output: FindCancelV1CancelledOutput{}}, nil
	}
	return sidmesh.Result{Outcome: "not_found", Output: FindCancelV1NotFoundOutput{}}, nil
}
