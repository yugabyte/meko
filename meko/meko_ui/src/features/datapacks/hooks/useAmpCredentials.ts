// cloud_admin credential state for the AMP create-cluster wizard (and any
// add-read_write-compute flow): generate-by-default or user-entered, gated on a
// one-time acknowledgement because the control plane never returns the password.
import { useCallback, useState } from "react";
import { generateAmpPassword, validateAmpPassword } from "@/lib/ampPassword";

export type AmpCredentialMode = "generate" | "custom";

export type AmpCredentials = {
  mode: AmpCredentialMode;
  setMode: (m: AmpCredentialMode) => void;
  /** The generated candidate — stable per instance; regenerated only on reset(). */
  generated: string;
  custom: string;
  setCustom: (v: string) => void;
  confirm: string;
  setConfirm: (v: string) => void;
  /** Set once the user has copied or downloaded the generated password. */
  acknowledged: boolean;
  setAcknowledged: (v: boolean) => void;
  /** The resolved secret to send on create. */
  password: string;
  /** Length/charset problem with the typed password, or null. */
  customError: string | null;
  /** True when a typed password and its confirmation disagree. */
  confirmMismatch: boolean;
  /** Whether create may proceed. */
  valid: boolean;
  reset: () => void;
};

export function useAmpCredentials(): AmpCredentials {
  const [mode, setMode] = useState<AmpCredentialMode>("generate");
  const [generated, setGenerated] = useState(() => generateAmpPassword());
  const [custom, setCustom] = useState("");
  const [confirm, setConfirm] = useState("");
  const [acknowledged, setAcknowledged] = useState(false);

  const customError = custom.length > 0 ? validateAmpPassword(custom) : null;
  const confirmMismatch = custom.length > 0 && confirm.length > 0 && custom !== confirm;
  const password = mode === "generate" ? generated : custom;
  const valid =
    mode === "generate"
      ? acknowledged
      : custom.length > 0 && customError === null && custom === confirm;

  const reset = useCallback(() => {
    setMode("generate");
    setGenerated(generateAmpPassword());
    setCustom("");
    setConfirm("");
    setAcknowledged(false);
  }, []);

  return {
    mode,
    setMode,
    generated,
    custom,
    setCustom,
    confirm,
    setConfirm,
    acknowledged,
    setAcknowledged,
    password,
    customError,
    confirmMismatch,
    valid,
    reset,
  };
}
