import { existsSync, mkdirSync, writeFileSync } from "node:fs";
import path from "node:path";

/**
 * Import path of the Go runtime package (module root, package name sidmesh) that provides
 * sidmesh.Register, Meta and Result.
 * Kept in step with ../go/go.mod.
 */
export const GO_RUNTIME_IMPORT = "gitlab.com/sidcorp-it/sidmesh/go";

const NODE_ID = /^[a-z][a-z0-9_]*$/;

function json(value: unknown): string {
  return `${JSON.stringify(value, null, 2)}\n`;
}

function exampleContract(id: string): unknown {
  return {
    capability: `${id}.example`,
    version: 1,
    kind: "action",
    effects: "none",
    summary: "Replace with what this capability does.",
    input: {
      type: "object",
      properties: { ref: { type: "string", description: "Id of the thing to act on." } },
      required: ["ref"],
      additionalProperties: false,
    },
    outcomes: {
      done: {
        description: "The work finished.",
        output: {
          type: "object",
          properties: { ref: { type: "string" } },
          required: ["ref"],
          additionalProperties: false,
        },
      },
    },
  };
}

function goHandler(id: string): string {
  return `package ${id}

import (
	"context"

	"${GO_RUNTIME_IMPORT}"
)

func init() {
	sidmesh.Register("${id}.example", 1, ExampleV1)
}

// ExampleV1 implements ${id}.example v1. Types come from contracts_gen.go.
func ExampleV1(ctx context.Context, in ExampleV1Input, cfg ExampleV1Config, meta sidmesh.Meta) (sidmesh.Result, error) {
	return sidmesh.Result{Outcome: "done", Output: ExampleV1DoneOutput{Ref: in.Ref}}, nil
}
`;
}

function tsHandler(): string {
  return `import type { Handlers } from "./contracts.gen.js";

export const handlers: Handlers = {
  "example@1": async (input) => ({ outcome: "done", output: { ref: input.ref } }),
};
`;
}

/** Create nodes/<id>/ with a manifest, one example contract and a handler stub. */
export function newNode(projectDir: string, id: string, runtime: "go" | "ts"): string[] {
  if (!NODE_ID.test(id)) {
    throw new Error(`node id "${id}" must match ^[a-z][a-z0-9_]*$`);
  }
  const dir = path.join(projectDir, "nodes", id);
  if (existsSync(dir)) {
    throw new Error(`nodes/${id} already exists`);
  }
  mkdirSync(path.join(dir, "contracts"), { recursive: true });
  const files: [string, string][] = [
    [
      "node.json",
      json({
        id,
        responsibility: "Replace with one sentence on what this node is responsible for.",
        notOwned: [],
        runtime,
        permissions: [],
      }),
    ],
    ["contracts/example.v1.json", json(exampleContract(id))],
    runtime === "go" ? ["handlers.go", goHandler(id)] : ["index.ts", tsHandler()],
  ];
  for (const [name, content] of files) {
    writeFileSync(path.join(dir, name), content);
  }
  return files.map(([name]) => `nodes/${id}/${name}`);
}
