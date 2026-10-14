import { createHash } from "node:crypto";

/**
 * Canonical JSON (sorted keys, no whitespace) and SHA-256 action hash (spec §13, §33).
 * Binds operation + target + consequential inputs. Changed content => different hash => new approval.
 */
export function canonicalJson(value: unknown): string {
  if (value === null || typeof value !== "object") {
    if (typeof value === "number" && !Number.isFinite(value)) throw new Error("non-finite number");
    if (value === undefined) throw new Error("undefined is not canonicalizable");
    return JSON.stringify(value);
  }
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  const obj = value as Record<string, unknown>;
  const keys = Object.keys(obj).filter((k) => obj[k] !== undefined).sort();
  return `{${keys.map((k) => `${JSON.stringify(k)}:${canonicalJson(obj[k])}`).join(",")}}`;
}

export interface ActionIntent {
  operation: string;
  target: Record<string, unknown>;
  inputs: Record<string, unknown>;
}

export function actionHash(intent: ActionIntent): string {
  const c = canonicalJson({ operation: intent.operation, target: intent.target, inputs: intent.inputs });
  return "sha256:" + createHash("sha256").update(c).digest("hex");
}
