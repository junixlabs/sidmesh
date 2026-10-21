// TS activity worker for the e2e project. Node 22+ runs this file directly (type stripping);
// it uses the built package, so run `pnpm build` first.
import path from "node:path";
import { runWorker } from "../../dist/runtime/worker.js";
import { handlers as greeter } from "./nodes/greeter/index.ts";

await runWorker({
  projectDir: path.dirname(new URL(import.meta.url).pathname),
  handlers: { greeter },
  address: process.env.TEMPORAL_ADDRESS ?? "127.0.0.1:7233",
});
