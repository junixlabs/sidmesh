import type { Handlers } from "./contracts.gen.js";

export const handlers: Handlers = {
  "greet@1": async (input, config, meta) => {
    if (input.name === "Mallory") {
      return { outcome: "refused", output: { reason: "on the refuse list" } };
    }
    return {
      outcome: "greeted",
      output: { message: `Hello, ${input.name}${config.punctuation ?? "."} (${meta.workflowId})`, runtime: "ts" },
    };
  },
};
