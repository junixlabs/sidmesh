package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"gitlab.com/sidcorp-it/sidmesh/go/codec"
	"gitlab.com/sidcorp-it/sidmesh/go/project"
)

// replayKeys is the SIDMESH_CODEC_KEYS of the e2e run that recorded the histories (key bytes
// 0x00..0x1f, a test key): its payloads are encrypted, so replay also exercises the codec.
const replayKeys = "e2e:AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

// recorded lists, per release, the histories recorded from the demo e2e run against a real
// Temporal server. Keep every release's histories while runs from it can still be open: an
// interpreter change must replay all of them (gate it with workflow.GetVersion otherwise).
var recorded = map[string]map[string]string{ // release dir -> file -> workflow id
	"2026-10-16-params": {
		"order-spawn.json":       "demo:shop:1-1792135060",
		"item-write-lookup.json": "demo:shop:1-1792135060/items/A",
		"item-human.json":        "demo:shop:1-1792135060/items/B",
		"inbox-drain.json":       "demo:inbox:1792135060", // ends in continue-as-new
	},
	"2026-10-16-started-flush": {
		"order-spawn.json":       "demo:shop:1-1792135803",
		"item-write-lookup.json": "demo:shop:1-1792135803/items/A",
		"item-human.json":        "demo:shop:1-1792135803/items/B",
		"inbox-drain.json":       "demo:inbox:1792135803",
	},
	"2026-10-16-pins": { // start input pins documents by hash
		"order-spawn.json":       "demo:shop:1-1792142303",
		"item-write-lookup.json": "demo:shop:1-1792142303/items/A",
		"item-human.json":        "demo:shop:1-1792142303/items/B",
		"inbox-drain.json":       "demo:inbox:1792142303",
	},
}

// TestReplayRecordedHistories fails if the interpreter is no longer deterministic for
// workflows already running.
func TestReplayRecordedHistories(t *testing.T) {
	keys, err := codec.ParseKeys(replayKeys)
	require.NoError(t, err)
	c, err := codec.New(keys)
	require.NoError(t, err)
	dc := converter.NewCodecDataConverter(converter.GetDefaultDataConverter(), c)
	demo, err := project.Load("../examples/demo")
	require.NoError(t, err)
	docs, err := NewDocs(demo, nil, nil) // pinned releases resolve from the demo project
	require.NoError(t, err)
	for release, files := range recorded {
		for name, id := range files {
			file := filepath.Join("testdata/histories", release, name)
			t.Run(release+"/"+name, func(t *testing.T) {
				f, err := os.Open(file)
				require.NoError(t, err)
				defer f.Close()
				history, err := client.HistoryFromJSON(f, client.HistoryJSONOptions{})
				require.NoError(t, err)
				r, err := worker.NewWorkflowReplayerWithOptions(worker.WorkflowReplayerOptions{DataConverter: dc})
				require.NoError(t, err)
				r.RegisterWorkflowWithOptions(docs.Interpret, workflow.RegisterOptions{Name: WorkflowName})
				require.NoError(t, r.ReplayWorkflowHistoryWithOptions(nil, history, worker.ReplayWorkflowHistoryOptions{
					OriginalExecution: workflow.Execution{ID: id},
				}))
			})
		}
	}
}
