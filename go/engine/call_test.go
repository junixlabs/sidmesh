package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"

	sidmesh "gitlab.com/sidcorp-it/sidmesh/go"
	"gitlab.com/sidcorp-it/sidmesh/go/project"
)

type fetchInput struct {
	OrderID string `json:"order_id"`
}

type item struct {
	SKU string `json:"sku"`
	Qty int    `json:"qty"`
}

type foundOutput struct {
	Items []item `json:"items"`
}

func callWith(t *testing.T, h func(context.Context, fetchInput, struct{}, sidmesh.Meta) (sidmesh.Result, error), input map[string]any) (sidmesh.Result, error) {
	t.Helper()
	p, err := project.Load("../testdata/project")
	require.NoError(t, err)
	reg := sidmesh.NewRegistry()
	reg.Add("shop.fetch_order", 1, sidmesh.Wrap(h))
	a, err := NewCallActivity(p, reg)
	require.NoError(t, err)
	return a.Call(context.Background(), sidmesh.CallRequest{Capability: "shop.fetch_order", Version: 1, Input: input})
}

func requireAppError(t *testing.T, err error, wantType string, wantNonRetryable bool) {
	t.Helper()
	var appErr *temporal.ApplicationError
	require.True(t, errors.As(err, &appErr), "want application error, got %v", err)
	require.Equal(t, wantType, appErr.Type())
	require.Equal(t, wantNonRetryable, appErr.NonRetryable())
}

func TestCallActivityContract(t *testing.T) {
	good := func(_ context.Context, in fetchInput, _ struct{}, _ sidmesh.Meta) (sidmesh.Result, error) {
		return sidmesh.Result{Outcome: "found", Output: foundOutput{Items: []item{{SKU: "A-" + in.OrderID, Qty: 2}}}}, nil
	}

	t.Run("valid call returns validated output", func(t *testing.T) {
		res, err := callWith(t, good, map[string]any{"order_id": "1"})
		require.NoError(t, err)
		require.Equal(t, "found", res.Outcome)
		require.Equal(t, map[string]any{"items": []any{map[string]any{"sku": "A-1", "qty": float64(2)}}}, res.Output)
	})
	t.Run("invalid input never reaches the handler", func(t *testing.T) {
		ran := false
		_, err := callWith(t, func(context.Context, fetchInput, struct{}, sidmesh.Meta) (sidmesh.Result, error) {
			ran = true
			return sidmesh.Result{}, nil
		}, map[string]any{"order": "1"})
		requireAppError(t, err, sidmesh.ErrInputInvalid, true)
		require.False(t, ran)
	})
	t.Run("undeclared outcome", func(t *testing.T) {
		_, err := callWith(t, func(context.Context, fetchInput, struct{}, sidmesh.Meta) (sidmesh.Result, error) {
			return sidmesh.Result{Outcome: "maybe"}, nil
		}, map[string]any{"order_id": "1"})
		requireAppError(t, err, sidmesh.ErrOutputInvalid, true)
	})
	t.Run("output breaks the outcome schema", func(t *testing.T) {
		_, err := callWith(t, func(context.Context, fetchInput, struct{}, sidmesh.Meta) (sidmesh.Result, error) {
			return sidmesh.Result{Outcome: "found", Output: map[string]any{"lines": 1}}, nil
		}, map[string]any{"order_id": "1"})
		requireAppError(t, err, sidmesh.ErrOutputInvalid, true)
	})
	t.Run("fatal error", func(t *testing.T) {
		_, err := callWith(t, func(context.Context, fetchInput, struct{}, sidmesh.Meta) (sidmesh.Result, error) {
			return sidmesh.Result{}, sidmesh.Fatal(errors.New("shop says no"))
		}, map[string]any{"order_id": "1"})
		requireAppError(t, err, sidmesh.ErrFatal, true)
	})
	t.Run("plain error is retryable", func(t *testing.T) {
		_, err := callWith(t, func(context.Context, fetchInput, struct{}, sidmesh.Meta) (sidmesh.Result, error) {
			return sidmesh.Result{}, errors.New("shop is down")
		}, map[string]any{"order_id": "1"})
		requireAppError(t, err, "HANDLER_ERROR", false)
	})
	t.Run("unknown capability", func(t *testing.T) {
		p, err := project.Load("../testdata/project")
		require.NoError(t, err)
		a, err := NewCallActivity(p, sidmesh.NewRegistry())
		require.NoError(t, err)
		_, err = a.Call(context.Background(), sidmesh.CallRequest{Capability: "shop.fetch_order", Version: 1})
		requireAppError(t, err, sidmesh.ErrUnknownCapability, true)
	})
}
