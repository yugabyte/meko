import { describe, it, expect } from "vitest";
import type { DatapackOutgoingMember } from "@/lib/api/mekoApi";
import {
  DATAPACK_SHARE_GRANT_CONTRIBUTOR,
  DATAPACK_SHARE_GRANT_MAINTAINER,
  DATAPACK_SHARE_GRANT_OWNER,
  DATAPACK_SHARE_GRANT_VIEWER,
} from "@/lib/datapackShareGrants";
import {
  EMPTY_SHARE_MEMBER_FILTER,
  SHARE_MEMBER_ROLE_FILTER_ALL,
  SHARE_MEMBER_ROLE_FILTER_OPTIONS,
  effectiveMemberGrant,
  filterShareMembers,
  isShareMemberFilterActive,
  memberEmailDomain,
} from "@/features/datapacks/components/datapack-share/shareMemberFilter";

function member(over: Partial<DatapackOutgoingMember>): DatapackOutgoingMember {
  return {
    user_id: over.user_email_id ?? "id",
    is_owner: false,
    user_name: "",
    user_email_id: "",
    grant: DATAPACK_SHARE_GRANT_VIEWER,
    ...over,
  };
}

const owner = member({
  user_email_id: "amiram@yugabyte.com",
  user_name: "Amiram Mizne",
  is_owner: true,
  // The API tags the owner's own row as maintainer — effectiveMemberGrant must
  // still resolve it to owner.
  grant: DATAPACK_SHARE_GRANT_MAINTAINER,
});
const maintainer = member({
  user_email_id: "Bo@Yugabyte.com",
  user_name: "Bo Yang",
  grant: DATAPACK_SHARE_GRANT_MAINTAINER,
});
const contributor = member({
  user_email_id: "cara@partner.io",
  user_name: "Cara Lopez",
  grant: DATAPACK_SHARE_GRANT_CONTRIBUTOR,
});
const viewer = member({
  user_email_id: "dev@partner.io",
  user_name: "",
  grant: DATAPACK_SHARE_GRANT_VIEWER,
});
const all = [owner, maintainer, contributor, viewer];

const emails = (rows: DatapackOutgoingMember[]) => rows.map((r) => r.user_email_id);

describe("effectiveMemberGrant", () => {
  it("resolves the owner row to owner regardless of its grant field", () => {
    expect(effectiveMemberGrant(owner)).toBe(DATAPACK_SHARE_GRANT_OWNER);
  });

  it("passes through the grant for everyone else", () => {
    expect(effectiveMemberGrant(contributor)).toBe(DATAPACK_SHARE_GRANT_CONTRIBUTOR);
  });
});

describe("memberEmailDomain", () => {
  it("lowercases the domain part", () => {
    expect(memberEmailDomain("Bo@Yugabyte.com")).toBe("yugabyte.com");
  });

  it("returns an empty string when there is no domain", () => {
    expect(memberEmailDomain("nope")).toBe("");
    expect(memberEmailDomain(null)).toBe("");
    expect(memberEmailDomain(undefined)).toBe("");
  });
});

describe("filterShareMembers", () => {
  it("returns everyone with no filter, preserving order", () => {
    expect(emails(filterShareMembers(all, EMPTY_SHARE_MEMBER_FILTER))).toEqual(emails(all));
  });

  it("matches on email substring, case-insensitively", () => {
    expect(
      emails(filterShareMembers(all, { query: "BO@", role: SHARE_MEMBER_ROLE_FILTER_ALL })),
    ).toEqual(["Bo@Yugabyte.com"]);
  });

  it("matches a whole domain, with or without the @", () => {
    expect(
      emails(
        filterShareMembers(all, { query: "@yugabyte.com", role: SHARE_MEMBER_ROLE_FILTER_ALL }),
      ),
    ).toEqual(["amiram@yugabyte.com", "Bo@Yugabyte.com"]);
    expect(
      emails(filterShareMembers(all, { query: "partner.io", role: SHARE_MEMBER_ROLE_FILTER_ALL })),
    ).toEqual(["cara@partner.io", "dev@partner.io"]);
  });

  it("matches on display name", () => {
    expect(
      emails(filterShareMembers(all, { query: "cara", role: SHARE_MEMBER_ROLE_FILTER_ALL })),
    ).toEqual(["cara@partner.io"]);
  });

  it("matches on role label", () => {
    expect(
      emails(filterShareMembers(all, { query: "viewer", role: SHARE_MEMBER_ROLE_FILTER_ALL })),
    ).toEqual(["dev@partner.io"]);
  });

  it("ANDs multiple whitespace-separated terms", () => {
    expect(
      emails(
        filterShareMembers(all, {
          query: "yugabyte maintainer",
          role: SHARE_MEMBER_ROLE_FILTER_ALL,
        }),
      ),
    ).toEqual(["Bo@Yugabyte.com"]);
    expect(
      emails(
        filterShareMembers(all, {
          query: "yugabyte contributor",
          role: SHARE_MEMBER_ROLE_FILTER_ALL,
        }),
      ),
    ).toEqual([]);
  });

  it("filters by role, treating the owner row as owner not maintainer", () => {
    expect(
      emails(filterShareMembers(all, { query: "", role: DATAPACK_SHARE_GRANT_OWNER })),
    ).toEqual(["amiram@yugabyte.com"]);
    expect(
      emails(filterShareMembers(all, { query: "", role: DATAPACK_SHARE_GRANT_MAINTAINER })),
    ).toEqual(["Bo@Yugabyte.com"]);
  });

  it("combines query and role", () => {
    expect(
      emails(filterShareMembers(all, { query: "partner.io", role: DATAPACK_SHARE_GRANT_VIEWER })),
    ).toEqual(["dev@partner.io"]);
  });

  it("ignores surrounding whitespace in the query", () => {
    expect(
      emails(filterShareMembers(all, { query: "   ", role: SHARE_MEMBER_ROLE_FILTER_ALL })),
    ).toEqual(emails(all));
  });
});

describe("isShareMemberFilterActive", () => {
  it("is false only for an untouched filter", () => {
    expect(isShareMemberFilterActive(EMPTY_SHARE_MEMBER_FILTER)).toBe(false);
    expect(isShareMemberFilterActive({ query: "  ", role: SHARE_MEMBER_ROLE_FILTER_ALL })).toBe(
      false,
    );
    expect(isShareMemberFilterActive({ query: "a", role: SHARE_MEMBER_ROLE_FILTER_ALL })).toBe(
      true,
    );
    expect(isShareMemberFilterActive({ query: "", role: DATAPACK_SHARE_GRANT_VIEWER })).toBe(true);
  });
});

describe("SHARE_MEMBER_ROLE_FILTER_OPTIONS", () => {
  it("offers All roles, Owner, and every invite role exactly once", () => {
    expect(SHARE_MEMBER_ROLE_FILTER_OPTIONS.map((o) => o.id)).toEqual([
      SHARE_MEMBER_ROLE_FILTER_ALL,
      DATAPACK_SHARE_GRANT_OWNER,
      DATAPACK_SHARE_GRANT_VIEWER,
      DATAPACK_SHARE_GRANT_CONTRIBUTOR,
      DATAPACK_SHARE_GRANT_MAINTAINER,
    ]);
  });
});
