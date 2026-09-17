// What Exit and Restart discard, and what they must not. The "must not" half
// is the point: a forgotten deployment id is a running instance nobody can
// reach again.

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { resetLabState } from "./labSessionReset";

const LAB = "adlc-review-agent";

beforeEach(() => localStorage.clear());
afterEach(() => localStorage.clear());

describe("resetLabState", () => {
  it("clears progress, completion and the datapack choice", () => {
    localStorage.setItem(`meko:lab-progress:${LAB}`, '{"chapterIndex":2,"stageIndex":1}');
    localStorage.setItem(`meko:lab-completed:${LAB}`, '["see-the-pr"]');
    localStorage.setItem(`meko:lab-datapack:${LAB}`, "dp-1");

    resetLabState(LAB);

    expect(localStorage.getItem(`meko:lab-progress:${LAB}`)).toBeNull();
    expect(localStorage.getItem(`meko:lab-completed:${LAB}`)).toBeNull();
    expect(localStorage.getItem(`meko:lab-datapack:${LAB}`)).toBeNull();
  });

  // These hold ids of resources running in the user's own account, and
  // deploymentsApi has no list endpoint — dropping the id strands the
  // instance and bills for it invisibly.
  it("keeps the deployment and AMP handles, which nothing could re-derive", () => {
    localStorage.setItem(`meko:lab-deployment:${LAB}`, "dep-1");
    localStorage.setItem(`meko:lab-deployment-mode:${LAB}`, "dedicated");
    localStorage.setItem(`meko:lab-amp-cluster:${LAB}`, "cluster-1");
    localStorage.setItem(`meko:lab-amp-branch:${LAB}`, "branch-1");

    resetLabState(LAB);

    expect(localStorage.getItem(`meko:lab-deployment:${LAB}`)).toBe("dep-1");
    expect(localStorage.getItem(`meko:lab-deployment-mode:${LAB}`)).toBe("dedicated");
    expect(localStorage.getItem(`meko:lab-amp-cluster:${LAB}`)).toBe("cluster-1");
    expect(localStorage.getItem(`meko:lab-amp-branch:${LAB}`)).toBe("branch-1");
  });

  // One trace per datapack the user touched, so there is no single key —
  // including the "none" key useLabRunnerSession writes before a datapack is
  // chosen.
  it("clears every lesson trace, whichever datapack it was keyed by", () => {
    localStorage.setItem(`meko:lab-lesson-trace:${LAB}:dp-1`, "{}");
    localStorage.setItem(`meko:lab-lesson-trace:${LAB}:dp-2`, "{}");
    localStorage.setItem(`meko:lab-lesson-trace:${LAB}:none`, "{}");

    resetLabState(LAB);

    expect(localStorage.getItem(`meko:lab-lesson-trace:${LAB}:dp-1`)).toBeNull();
    expect(localStorage.getItem(`meko:lab-lesson-trace:${LAB}:dp-2`)).toBeNull();
    expect(localStorage.getItem(`meko:lab-lesson-trace:${LAB}:none`)).toBeNull();
  });

  it("leaves another lab's run alone", () => {
    localStorage.setItem("meko:lab-progress:other-lab", '{"chapterIndex":1,"stageIndex":0}');
    localStorage.setItem("meko:lab-lesson-trace:other-lab:dp-1", "{}");

    resetLabState(LAB);

    expect(localStorage.getItem("meko:lab-progress:other-lab")).not.toBeNull();
    expect(localStorage.getItem("meko:lab-lesson-trace:other-lab:dp-1")).not.toBeNull();
  });
});
