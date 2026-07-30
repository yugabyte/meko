// Shared per-card context for the Decision Trace tree. These values are
// constant for the whole card but consumed only by deep leaves
// (KvRow for the *_id/email rendering, DecisionTraceStep for the footer
// "View Trace" action). Passing them through PhaseAccordion → step → KvBlock
// as props was pure drilling, so they live in context instead.

import { createContext, useContext } from "react";

export type DecisionTraceContextValue = {
  /** When set, a `datapack_id` PARAMS row renders as this name. */
  datapackName?: string;
  /** Guards datapack_name relabelling to the datapack we're in. */
  datapackId?: string;
  /** When set, a `user_id` PARAMS row renders as this email. */
  userEmail?: string;
  /** Enables the `conversation_id` PARAMS deep-link. */
  enableConversationLink?: boolean;
  /** When set, a step footer shows a "View Trace" button that opens the
   *  internal trace drawer focused on that step's span. */
  onViewTrace?: (spanId: string) => void;
};

const DecisionTraceContext = createContext<DecisionTraceContextValue>({});

export const DecisionTraceProvider = DecisionTraceContext.Provider;

export function useDecisionTraceContext(): DecisionTraceContextValue {
  return useContext(DecisionTraceContext);
}
