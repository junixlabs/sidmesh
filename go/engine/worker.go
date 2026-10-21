package engine

import (
	"cmp"
	"context"
	"fmt"
	"os"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	sidmesh "gitlab.com/sidcorp-it/sidmesh/go"
	"gitlab.com/sidcorp-it/sidmesh/go/codec"
	"gitlab.com/sidcorp-it/sidmesh/go/project"
)

// WorkerOptions configure the workers of one project.
type WorkerOptions struct {
	Project  *project.Project
	Handlers *sidmesh.Registry // defaults to sidmesh.Default
	Sinks    Sinks
}

// NewWorkers returns the interpreter worker (workflow queue: the workflow, the sink and
// signal-with-start activities) and the Go activity worker (Go queue: sidmesh.call). Start both.
func NewWorkers(c client.Client, o WorkerOptions) ([]worker.Worker, error) {
	if o.Handlers == nil {
		o.Handlers = sidmesh.Default
	}
	q := o.Project.Manifest.Queues
	call, err := NewCallActivity(o.Project, o.Handlers)
	if err != nil {
		return nil, fmt.Errorf("call activity: %w", err)
	}

	docs, err := NewDocs(o.Project, o.Sinks.Registry, o.Sinks.Alerts)
	if err != nil {
		return nil, err
	}
	if err := docs.Publish(context.Background(), o.Project); err != nil {
		return nil, fmt.Errorf("publish documents: %w", err)
	}

	wf := worker.New(c, q.Workflow, worker.Options{})
	wf.RegisterWorkflowWithOptions(docs.Interpret, workflow.RegisterOptions{Name: WorkflowName})
	wf.RegisterActivityWithOptions(o.Sinks.Activity, activity.RegisterOptions{Name: SinkActivityName})
	wf.RegisterActivityWithOptions(o.Sinks.ParamsActivity, activity.RegisterOptions{Name: ParamsActivityName})
	wf.RegisterActivityWithOptions(Starter{Client: c}.SignalWithStart, activity.RegisterOptions{Name: SignalWithStartActivityName})

	act := worker.New(c, q.Go, worker.Options{})
	act.RegisterActivityWithOptions(call.Call, activity.RegisterOptions{Name: CallActivityName})
	return []worker.Worker{wf, act}, nil
}

// Dial connects to Temporal with the payload codec of SPEC §7 when SIDMESH_CODEC_KEYS is
// set. An empty hostPort means $TEMPORAL_ADDRESS, else 127.0.0.1:7233; the namespace is
// $TEMPORAL_NAMESPACE, else default. Every worker and client of a deployment dials this way.
func Dial(hostPort string) (client.Client, error) {
	dc, err := codec.DataConverter()
	if err != nil {
		return nil, err
	}
	if hostPort == "" {
		hostPort = cmp.Or(os.Getenv("TEMPORAL_ADDRESS"), client.DefaultHostPort)
	}
	return client.Dial(client.Options{HostPort: hostPort, Namespace: os.Getenv("TEMPORAL_NAMESPACE"), DataConverter: dc})
}

// Run starts the project's workers and blocks until the process is interrupted.
func Run(c client.Client, o WorkerOptions) error {
	workers, err := NewWorkers(c, o)
	if err != nil {
		return err
	}
	for i, w := range workers {
		if err := w.Start(); err != nil {
			for _, started := range workers[:i] {
				started.Stop()
			}
			return err
		}
	}
	<-worker.InterruptCh()
	for _, w := range workers {
		w.Stop()
	}
	return nil
}
