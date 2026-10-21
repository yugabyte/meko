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
  it("offers an allowlist on every mode with a load balancer", () => {
    const takes: EndpointConnectivityMode[] = [
      "nlb_internal",
      "nlb_external",
      "ingress_internal",
      "ingress_external",
    ];
    for (const m of takes) expect(takesAllowedCidrs(m)).toBe(true);
    expect(takesAllowedCidrs("cluster_ip")).toBe(false);
  });

  it("hides the CIDR input on cluster_ip and shows it on an ingress mode", () => {
    const { rerender } = render(
      <ConnectivityField value={{ mode: "cluster_ip", allowedCidrs: "" }} onChange={vi.fn()} />,
    );
    expect(screen.queryByLabelText(/allowed cidrs/i)).toBeNull();

    rerender(
      <ConnectivityField
        value={{ mode: "ingress_internal", allowedCidrs: "" }}
        onChange={vi.fn()}
      />,
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

  // Hiding the input is not enough: the wizard's Review row prints any typed
  // CIDRs whatever the mode.
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
    const opt = screen.getByText(CONNECTIVITY_LABELS.cluster_ip);
    fireEvent.click(opt);
    expect(onChange).toHaveBeenCalledWith(
      expect.objectContaining({ mode: "cluster_ip", allowedCidrs: "" }),
    );
  });
});

describe("ConnectivityWarning", () => {
  it("warns on ingress_external with no CIDRs, and not once one is typed", () => {
    const { rerender } = render(
      <ConnectivityWarning value={{ mode: "ingress_external", allowedCidrs: "" }} />,
    );
    expect(screen.getByText(/public internet/i)).toBeTruthy();
    rerender(
      <ConnectivityWarning value={{ mode: "ingress_external", allowedCidrs: "10.0.0.0/8" }} />,
    );
    expect(screen.queryByText(/public internet/i)).toBeNull();
  });

  it("stays quiet on an internal ingress shard", () => {
    render(<ConnectivityWarning value={{ mode: "ingress_internal", allowedCidrs: "" }} />);
    expect(screen.queryByText(/public internet/i)).toBeNull();
  });
});
