// Command worker runs the demo project's interpreter and Go activity workers.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/junixlabs/sidmesh/go/engine"
	"github.com/junixlabs/sidmesh/go/pgsink"
	"github.com/junixlabs/sidmesh/go/project"

	_ "github.com/junixlabs/sidmesh/go/examples/demo/nodes/mapper"
	_ "github.com/junixlabs/sidmesh/go/examples/demo/nodes/shop"
	_ "github.com/junixlabs/sidmesh/go/examples/demo/nodes/vendor"
)

func main() {
	dir := flag.String("project", "examples/demo", "project directory")
	address := flag.String("address", "", "Temporal frontend (default $TEMPORAL_ADDRESS, else 127.0.0.1:7233)")
	sinkFlags := pgsink.Register(flag.CommandLine)
	flag.Parse()

	p, err := project.Load(*dir)
	if err != nil {
		log.Fatal(err)
	}
	sinks, closeSinks, err := sinkFlags.Open(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	defer closeSinks()
	c, err := engine.Dial(*address) // SIDMESH_CODEC_KEYS turns the payload codec on
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	if err := engine.Run(c, engine.WorkerOptions{Project: p, Sinks: sinks}); err != nil {
		log.Fatal(err)
	}
}
