package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"

	sidmesh "github.com/junixlabs/sidmesh/go"
	"github.com/junixlabs/sidmesh/go/project"
)

// TestPayloadConformance runs spec/conformance/payloads.json through the Go call activity:
// the same vectors the TS runtime must pass.
func TestPayloadConformance(t *testing.T) {
	raw, err := os.ReadFile("../../spec/conformance/payloads.json")
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("spec/conformance/payloads.json not present")
	}
	require.NoError(t, err)
	var doc struct {
		Contracts map[string]project.Contract `json:"contracts"`
		Cases     []struct {
			Name     string         `json:"name"`
			Contract string         `json:"contract"`
			Input    map[string]any `json:"input"`
			Config   map[string]any `json:"config"`
			Result   sidmesh.Result `json:"result"`
			Expect   string         `json:"expect"`
		} `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.NotEmpty(t, doc.Cases)

	for _, tc := range doc.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			c, ok := doc.Contracts[tc.Contract]
			require.True(t, ok, "unknown contract %q", tc.Contract)
			c.Runtime = "go"
			key := sidmesh.Key(c.Capability, c.Version)
			p := &project.Project{Contracts: map[string]*project.Contract{key: &c}}
			reg := sidmesh.NewRegistry()
			ran := false
			reg.Add(c.Capability, c.Version, func(context.Context, map[string]any, map[string]any, sidmesh.Meta) (sidmesh.Result, error) {
				ran = true
				return tc.Result, nil
			})
			a, err := NewCallActivity(p, reg)
			require.NoError(t, err)

			_, err = a.Call(context.Background(), sidmesh.CallRequest{
				Capability: c.Capability, Version: c.Version, Input: tc.Input, Config: tc.Config,
			})
			if tc.Expect == "ok" {
				require.NoError(t, err)
				return
			}
			var appErr *temporal.ApplicationError
			require.True(t, errors.As(err, &appErr), "want %s, got %v", tc.Expect, err)
			require.Equal(t, tc.Expect, appErr.Type(), appErr.Error())
			if tc.Expect == sidmesh.ErrInputInvalid {
				require.False(t, ran, "handler must not run on invalid input")
			}
		})
	}
}
