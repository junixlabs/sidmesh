/** A directed graph over string node ids. */
export type Graph = Map<string, string[]>;

/** Every node reachable from the given entries (entries included). */
export function reachable(graph: Graph, entries: string[]): Set<string> {
  const seen = new Set<string>();
  const stack = entries.filter((e) => graph.has(e));
  while (stack.length > 0) {
    const node = stack.pop() as string;
    if (seen.has(node)) {
      continue;
    }
    seen.add(node);
    for (const next of graph.get(node) ?? []) {
      if (graph.has(next) && !seen.has(next)) {
        stack.push(next);
      }
    }
  }
  return seen;
}

/** Every node that has a path to at least one of the targets (targets included). */
export function canReach(graph: Graph, targets: string[]): Set<string> {
  const reverse: Graph = new Map();
  for (const node of graph.keys()) {
    reverse.set(node, []);
  }
  for (const [node, nexts] of graph) {
    for (const next of nexts) {
      reverse.get(next)?.push(node);
    }
  }
  return reachable(reverse, targets);
}

const ROOT = "\u0000root";

/**
 * Dominator sets over the nodes reachable from the entries. A virtual root precedes every
 * entry, so with several entries a node only dominates what every entry path passes through.
 * Returned sets do not contain the virtual root. Unreachable nodes are absent from the map.
 */
export function dominators(graph: Graph, entries: string[]): Map<string, Set<string>> {
  const nodes = [...reachable(graph, entries)];
  const preds = new Map<string, string[]>();
  for (const node of nodes) {
    preds.set(node, []);
  }
  for (const node of nodes) {
    for (const next of graph.get(node) ?? []) {
      preds.get(next)?.push(node);
    }
  }
  for (const entry of entries) {
    preds.get(entry)?.push(ROOT);
  }

  const all = new Set([ROOT, ...nodes]);
  const dom = new Map<string, Set<string>>();
  dom.set(ROOT, new Set([ROOT]));
  for (const node of nodes) {
    dom.set(node, new Set(all));
  }

  let changed = true;
  while (changed) {
    changed = false;
    for (const node of nodes) {
      let next: Set<string> | undefined;
      for (const pred of preds.get(node) ?? []) {
        const predDom = dom.get(pred) as Set<string>;
        next = next ? new Set([...next].filter((d) => predDom.has(d))) : new Set(predDom);
      }
      next = next ?? new Set();
      next.add(node);
      const current = dom.get(node) as Set<string>;
      if (next.size !== current.size || [...next].some((d) => !current.has(d))) {
        dom.set(node, next);
        changed = true;
      }
    }
  }

  const result = new Map<string, Set<string>>();
  for (const node of nodes) {
    const set = new Set(dom.get(node));
    set.delete(ROOT);
    result.set(node, set);
  }
  return result;
}
