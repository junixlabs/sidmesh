package pgsink

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/junixlabs/sidmesh/go/engine"
)

// EnvURL names the environment variable holding the postgres connection URL.
const EnvURL = "SIDMESH_PG_URL"

// Flags select a worker's sink: "-sink jsonl -records <file>" (default, stdout without a
// file) or "-sink postgres -pg-url <url>".
type Flags struct {
	Kind    string
	Records string
	PGURL   string
}

// Register adds -sink, -records and -pg-url to fs. Defaults come from the environment:
// $SIDMESH_SINK, else postgres when $SIDMESH_PG_URL is set, else jsonl; $SIDMESH_RECORDS.
func Register(fs *flag.FlagSet) *Flags {
	f := &Flags{}
	pg := os.Getenv(EnvURL)
	kind := os.Getenv("SIDMESH_SINK")
	if kind == "" && pg != "" {
		kind = "postgres"
	}
	fs.StringVar(&f.Kind, "sink", cmp.Or(kind, "jsonl"), "record sink: jsonl or postgres (default $SIDMESH_SINK)")
	fs.StringVar(&f.Records, "records", os.Getenv("SIDMESH_RECORDS"), "jsonl sink: append records to this file (default $SIDMESH_RECORDS, else stdout)")
	fs.StringVar(&f.PGURL, "pg-url", "", "postgres sink: connection URL (default $SIDMESH_PG_URL)") // never a secret as default
	return f
}

// Open builds the selected sinks. Call close when the worker stops.
func (f *Flags) Open(ctx context.Context) (sinks engine.Sinks, close func(), err error) {
	switch f.Kind {
	case "jsonl", "":
		var out io.Writer = os.Stdout
		closeFn := func() {}
		if f.Records != "" {
			file, err := os.OpenFile(f.Records, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				return engine.Sinks{}, nil, err
			}
			out, closeFn = file, func() { file.Close() }
		}
		return engine.NewJSONL(out).Sinks(), closeFn, nil
	case "postgres":
		f.PGURL = cmp.Or(f.PGURL, os.Getenv(EnvURL))
		if f.PGURL == "" {
			return engine.Sinks{}, nil, fmt.Errorf("-sink postgres needs -pg-url or %s", EnvURL)
		}
		s, err := Open(ctx, f.PGURL)
		if err != nil {
			return engine.Sinks{}, nil, err
		}
		return s.Sinks(), s.Close, nil
	}
	return engine.Sinks{}, nil, fmt.Errorf("unknown sink %q (jsonl or postgres)", f.Kind)
}
