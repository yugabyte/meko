// The scope tabs decide which collections a user sees, including whether a
// shared one appears at all, so the narrowing is worth asserting directly.

import { describe, expect, it } from "vitest";
import type { Collection } from "@/lib/api/collectionsApi";
import {
  COLLECTION_SCOPE,
  emptyScopeMessage,
  scopedCollections,
} from "@/features/collections/lib/scopeCollections";

const collection = (id: string): Collection =>
  ({ collection_id: id, collection_name: id }) as Collection;

const owned = [collection("mine-a"), collection("mine-b")];
const shared = [collection("theirs")];

describe("scopedCollections", () => {
  it("shows both lists on All, owned first", () => {
    expect(scopedCollections(owned, shared, COLLECTION_SCOPE.all).map((c) => c.collection_id)).toEqual(
      ["mine-a", "mine-b", "theirs"],
    );
  });

  it("narrows to one list on the other two tabs", () => {
    expect(scopedCollections(owned, shared, COLLECTION_SCOPE.owned)).toEqual(owned);
    expect(scopedCollections(owned, shared, COLLECTION_SCOPE.shared)).toEqual(shared);
  });

  it("does not mutate the arrays it was given", () => {
    scopedCollections(owned, shared, COLLECTION_SCOPE.all);
    expect(owned).toHaveLength(2);
    expect(shared).toHaveLength(1);
  });
});

describe("emptyScopeMessage", () => {
  it("names the tab rather than giving one message for every case", () => {
    expect(emptyScopeMessage(COLLECTION_SCOPE.owned, false)).toContain("not created");
    expect(emptyScopeMessage(COLLECTION_SCOPE.shared, false)).toContain("shared with you");
  });

  it("blames the search first, because that is what the user just did", () => {
    expect(emptyScopeMessage(COLLECTION_SCOPE.owned, true)).toContain("match your search");
  });
});
