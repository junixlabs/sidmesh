// Command sidmesh-run starts and steers interpreter workflows from the command line.
//
//	sidmesh-run start   -project DIR -definition ID [-version N] -id WORKFLOW_ID [-tenant T] [-input JSON] [-params JSON] [-wait]
//	sidmesh-run signal  -id WORKFLOW_ID -name SIGNAL [-payload JSON]
//	sidmesh-run resolve -id WORKFLOW_ID -step STEP -outcome OUTCOME [-output JSON] [-actor NAME]
//	sidmesh-run result  -id WORKFLOW_ID
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"go.temporal.io/sdk/client"

	"gitlab.com/sidcorp-it/sidmesh/go/engine"
	"gitlab.com/sidcorp-it/sidmesh/go/pgsink"
	"gitlab.com/sidcorp-it/sidmesh/go/project"
)

func main() {
	if len(os.Args) < 2 {
		fail(fmt.Errorf("usage: sidmesh-run start|signal|resolve|result [flags]"))
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	address := fs.String("address", "", "Temporal frontend (default $TEMPORAL_ADDRESS, else 127.0.0.1:7233)")
	id := fs.String("id", "", "workflow id")
	var run func(context.Context, client.Client) error

	switch cmd {
	case "start":
		dir := fs.String("project", ".", "project directory")
		def := fs.String("definition", "", "definition id")
		version := fs.Int("version", 1, "definition version")
		tenant := fs.String("tenant", "default", "tenant")
		input := fs.String("input", "{}", "input JSON")
		params := fs.String("params", "{}", "param overrides JSON")
		wait := fs.Bool("wait", false, "wait for the result")
		run = func(ctx context.Context, c client.Client) error {
			p, err := project.Load(*dir)
			if err != nil {
				return err
			}
			req := engine.StartRequest{Definition: *def, Version: *version, WorkflowID: *id, Tenant: *tenant}
			if err := decode(*input, &req.Input); err != nil {
				return err
			}
			if err := decode(*params, &req.Params); err != nil {
				return err
			}
			in, err := engine.Prepare(p, req) // a request that does not validate publishes nothing
			if err != nil {
				return err
			}
			if url := os.Getenv(pgsink.EnvURL); url != "" { // publish the documents the run pins
				reg, err := pgsink.Open(ctx, url)
				if err != nil {
					return err
				}
				defer reg.Close()
				docs := make([]project.Document, 0, len(in.Pins))
				for key := range in.Pins {
					docs = append(docs, p.Documents[key])
				}
				if err := reg.Publish(ctx, docs); err != nil {
					return err
				}
			}
			wr, err := engine.Start(ctx, c, p, req)
			if err != nil {
				return err
			}
			fmt.Printf("started %s run %s\n", wr.GetID(), wr.GetRunID())
			if !*wait {
				return nil
			}
			return printResult(ctx, c, *id)
		}
	case "signal":
		name := fs.String("name", "", "signal name")
		payload := fs.String("payload", "{}", "payload JSON")
		run = func(ctx context.Context, c client.Client) error {
			var v any
			if err := decode(*payload, &v); err != nil {
				return err
			}
			return c.SignalWorkflow(ctx, *id, "", *name, v)
		}
	case "resolve":
		step := fs.String("step", "", "human step id")
		outcome := fs.String("outcome", "", "outcome")
		output := fs.String("output", "{}", "output JSON")
		actor := fs.String("actor", "", "who resolved it")
		run = func(ctx context.Context, c client.Client) error {
			r := engine.Resolution{StepID: *step, Outcome: *outcome, Actor: *actor}
			if err := decode(*output, &r.Output); err != nil {
				return err
			}
			return engine.ResolveTask(ctx, c, *id, r) // rejected resolutions come back as the error
		}
	case "result":
		run = func(ctx context.Context, c client.Client) error { return printResult(ctx, c, *id) }
	default:
		fail(fmt.Errorf("unknown command %q", cmd))
	}

	_ = fs.Parse(args)
	c, err := engine.Dial(*address) // SIDMESH_CODEC_KEYS turns the payload codec on
	if err != nil {
		fail(err)
	}
	defer c.Close()
	if err := run(context.Background(), c); err != nil {
		fail(err)
	}
}

func printResult(ctx context.Context, c client.Client, id string) error {
	var res engine.Result
	if err := c.GetWorkflow(ctx, id, "").Get(ctx, &res); err != nil {
		return err
	}
	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(out))
	return nil
}

func decode(s string, v any) error {
	if err := json.Unmarshal([]byte(s), v); err != nil {
		return fmt.Errorf("invalid JSON %q: %w", s, err)
	}
	return nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "sidmesh-run:", err)
	os.Exit(1)
}
