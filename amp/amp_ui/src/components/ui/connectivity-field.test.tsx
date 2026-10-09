import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import {
  ConnectivityField,
  ConnectivityWarning,
  CONNECTIVITY_LABELS,
  takesAllowedCidrs,
} from "@/components/ui/connectivity-field";
import type { EndpointConnectivityMode } from "@/lib/api/types";

afterEach(cleanup);

describe("ConnectivityField", () => {
  // The controller refuses allowed_cidrs on the ingress_* modes with a 400,
  // because a shared load balancer cannot carry a per-endpoint allowlist. A
  // field offered there collects a value the API rejects, so this tracks the
  // API's refusal rather than "does the mode have a load balancer".
  it("offers an allowlist only for the modes the API accepts one on", () => {
    const takes: EndpointConnectivityMode[] = ["nlb_internal", "nlb_external"];
    const refuses: EndpointConnectivityMode[] = [
      "cluster_ip",
      "ingress_internal",
      "ingress_external",
    ];
    for (const m of takes) expect(takesAllowedCidrs(m)).toBe(true);
    for (const m of refuses) expect(takesAllowedCidrs(m)).toBe(false);
  });

  it("hides the CIDR input on an ingress mode and shows it on an nlb one", () => {
    const { rerender } = render(
      <ConnectivityField
        value={{ mode: "ingress_internal", allowedCidrs: "" }}
        onChange={vi.fn()}
      />,
    );
    expect(screen.queryByLabelText(/allowed cidrs/i)).toBeNull();

    rerender(
      <ConnectivityField value={{ mode: "nlb_internal", allowedCidrs: "" }} onChange={vi.fn()} />,
    );
    expect(screen.getByLabelText(/allowed cidrs/i)).toBeTruthy();
  });

  // The select's options ARE the label map's keys, so a mode missing from it is
  // not offered at all (tsc catches it first — the map is a Record over the mode
  // union). This pins that the two ingress modes are actually reachable.
  it("labels every mode", () => {
    const modes: EndpointConnectivityMode[] = [
      "cluster_ip",
      "ingress_internal",
      "ingress_external",
      "nlb_internal",
      "nlb_external",
    ];
    for (const m of modes) expect(CONNECTIVITY_LABELS[m]).toBeTruthy();
  });

  // Hiding the input is not enough: the value lives in the caller's state and
  // both submit paths post it unconditionally, so a CIDR typed on an nlb_* mode
  // would reach the API on an ingress_* create and earn a 400 naming a field
  // that is no longer on screen.
  it("clears a typed allowlist when switching to a mode that takes none", () => {
    const onChange = vi.fn();
    render(
      <ConnectivityField
        value={{ mode: "nlb_external", allowedCidrs: "10.0.0.0/8" }}
        onChange={onChange}
      />,
    );
    // Radix renders a native-select fallback in jsdom; drive the handler through it.
    const trigger = screen.getByRole("combobox");
    fireEvent.click(trigger);
    const opt = screen.getByText(CONNECTIVITY_LABELS.ingress_internal);
    fireEvent.click(opt);
    expect(onChange).toHaveBeenCalledWith(
      expect.objectContaining({ mode: "ingress_internal", allowedCidrs: "" }),
    );
  });
});

describe("ConnectivityWarning", () => {
  // Unconditional for ingress_external: it takes no allowlist, so unlike
  // nlb_external there is no entry that would narrow it.
  it("warns on ingress_external even with CIDRs typed", () => {
    render(
      <ConnectivityWarning value={{ mode: "ingress_external", allowedCidrs: "10.0.0.0/8" }} />,
    );
    expect(screen.getByText(/public internet/i)).toBeTruthy();
  });

  it("stays quiet on an internal ingress shard", () => {
    render(<ConnectivityWarning value={{ mode: "ingress_internal", allowedCidrs: "" }} />);
    expect(screen.queryByText(/public internet/i)).toBeNull();
  });
});
