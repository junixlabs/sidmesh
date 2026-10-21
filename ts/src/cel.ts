import { parse } from "@bufbuild/cel";

/** Variables available to every expression (SPEC §5, Bindings). */
export const CEL_ROOTS = ["input", "params", "vars", "steps", "signals", "item", "workflow"] as const;

export interface CelAnalysis {
  /** Parse error message, if the expression does not parse. */
  error?: string;
  /** Free identifiers (not bound by a comprehension). */
  identifiers: string[];
  /** Dotted field chains rooted at a free identifier, e.g. "steps.map.output.confidence". */
  paths: string[];
}

interface Node {
  exprKind?: { case?: string; value?: unknown };
}

type AnyRecord = Record<string, unknown>;

/**
 * Parse an expression and collect the variables it reads. @bufbuild/cel 0.6.1 does not
 * export its type checker, so "type-check" here means: it parses, and every free identifier
 * is one of the declared roots. Field paths are returned so callers can check them against
 * the schemas they read.
 */
export function analyzeCel(source: string): CelAnalysis {
  let root: Node;
  try {
    root = parse(source).expr as unknown as Node;
  } catch (error) {
    return { error: (error as Error).message, identifiers: [], paths: [] };
  }

  const identifiers = new Set<string>();
  const paths = new Set<string>();

  const chainOf = (node: Node): string[] | undefined => {
    const kind = node.exprKind?.case;
    const value = node.exprKind?.value as AnyRecord | undefined;
    if (kind === "identExpr") {
      return [String(value?.name)];
    }
    if (kind === "selectExpr" && value && !value.testOnly) {
      const base = chainOf(value.operand as Node);
      return base ? [...base, String(value.field)] : undefined;
    }
    return undefined;
  };

  const visit = (node: Node | undefined, bound: Set<string>): void => {
    if (!node || !node.exprKind) {
      return;
    }
    const kind = node.exprKind.case;
    const value = node.exprKind.value as AnyRecord;
    switch (kind) {
      case "identExpr":
      case "selectExpr": {
        const chain = chainOf(node);
        if (chain) {
          const head = chain[0] as string;
          if (!bound.has(head)) {
            identifiers.add(head);
            if (chain.length > 1) {
              paths.add(chain.join("."));
            }
          }
          return;
        }
        visit(value.operand as Node, bound);
        return;
      }
      case "callExpr":
        visit(value.target as Node | undefined, bound);
        for (const arg of (value.args as Node[]) ?? []) {
          visit(arg, bound);
        }
        return;
      case "listExpr":
        for (const element of (value.elements as Node[]) ?? []) {
          visit(element, bound);
        }
        return;
      case "structExpr":
        for (const entry of (value.entries as AnyRecord[]) ?? []) {
          const key = entry.keyKind as { case?: string; value?: unknown } | undefined;
          if (key?.case === "mapKey") {
            visit(key.value as Node, bound);
          }
          visit(entry.value as Node, bound);
        }
        return;
      case "comprehensionExpr": {
        visit(value.iterRange as Node, bound);
        visit(value.accuInit as Node, bound);
        const inner = new Set(bound);
        for (const name of [value.iterVar, value.iterVar2, value.accuVar]) {
          if (typeof name === "string" && name.length > 0) {
            inner.add(name);
          }
        }
        visit(value.loopCondition as Node, inner);
        visit(value.loopStep as Node, inner);
        visit(value.result as Node, inner);
        return;
      }
      default:
        return; // constants
    }
  };

  visit(root, new Set());
  return { identifiers: [...identifiers], paths: [...paths] };
}
