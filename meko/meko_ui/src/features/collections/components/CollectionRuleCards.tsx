// The two rules a reader of the pipeline above needs, one sentence each.
//
// No numbers here on purpose: they would be a second, competing source of
// truth for the same counts the tree and the panel already show.

import { Card } from "@/components/ui/card";
import {
  PROMOTION_RULE_BODY,
  PROMOTION_RULE_TITLE,
  READABLE_RULE_BODY,
  READABLE_RULE_TITLE,
} from "@/features/collections/collectionsCopy";

const RULES = [
  { title: PROMOTION_RULE_TITLE, body: PROMOTION_RULE_BODY },
  { title: READABLE_RULE_TITLE, body: READABLE_RULE_BODY },
];

export function CollectionRuleCards() {
  return (
    <div className="grid gap-4 md:grid-cols-2">
      {RULES.map((rule) => (
        <Card key={rule.title} className="border-border bg-card p-6">
          <h3 className="text-base font-semibold text-foreground">{rule.title}</h3>
          <p className="mt-2 text-sm leading-relaxed text-muted-foreground">{rule.body}</p>
        </Card>
      ))}
    </div>
  );
}
