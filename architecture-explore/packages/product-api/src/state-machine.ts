/**
 * Minimal deterministic transition table. Spec §11: every transition requires an authenticated
 * command, the expected version and a permitted transition. Auth is checked by the caller; this
 * module enforces permitted transitions and optimistic versioning.
 */
export type TransitionTable<S extends string, E extends string> = Readonly<
  Record<S, Readonly<Partial<Record<E, readonly S[]>>>>
>;

export class TransitionError extends Error {
  constructor(
    readonly code: "invalid_transition" | "version_conflict" | "terminal_state" | "invalid_target",
    message: string,
  ) {
    super(message);
    this.name = "TransitionError";
  }
}

export interface Versioned<S extends string> {
  readonly state: S;
  readonly version: number;
}

export interface StateMachine<S extends string, E extends string> {
  readonly name: string;
  readonly states: readonly S[];
  readonly terminal: readonly S[];
  /** Targets reachable from `from` via `event` (several when policy picks the outcome). */
  targets(from: S, event: E): readonly S[];
  can(from: S, event: E, to?: S): boolean;
  /** Apply with expected version (compare-and-set). Returns the new versioned state. */
  apply<T extends Versioned<S>>(current: T, event: E, expectedVersion: number, to?: S): T;
  isTerminal(s: S): boolean;
}

export function defineMachine<S extends string, E extends string>(
  name: string,
  table: TransitionTable<S, E>,
  terminal: readonly S[],
): StateMachine<S, E> {
  const states = Object.keys(table) as S[];
  const machine: StateMachine<S, E> = {
    name,
    states,
    terminal,
    targets: (from, event) => table[from]?.[event] ?? [],
    can(from, event, to) {
      const t = machine.targets(from, event);
      return to === undefined ? t.length === 1 : t.includes(to);
    },
    isTerminal: (s) => terminal.includes(s),
    apply(current, event, expectedVersion, to) {
      if (current.version !== expectedVersion) {
        throw new TransitionError(
          "version_conflict",
          `${name}: expected version ${expectedVersion}, current ${current.version}`,
        );
      }
      if (terminal.includes(current.state)) {
        throw new TransitionError("terminal_state", `${name}: ${current.state} is terminal`);
      }
      const t = machine.targets(current.state, event);
      if (t.length === 0) {
        throw new TransitionError("invalid_transition", `${name}: ${event} not permitted from ${current.state}`);
      }
      let next: S;
      if (to === undefined) {
        if (t.length !== 1) {
          throw new TransitionError("invalid_target", `${name}: ${event} from ${current.state} needs explicit target`);
        }
        next = t[0] as S;
      } else {
        if (!t.includes(to)) {
          throw new TransitionError("invalid_target", `${name}: ${event} cannot go ${current.state} -> ${to}`);
        }
        next = to;
      }
      return { ...current, state: next, version: current.version + 1 };
    },
  };
  return machine;
}
