import { useEffect } from "react";
import { addAppEventListener, removeAppEventListener } from "@/lib/browserEvents";
import { setLocalItem } from "@/lib/browserStorage";
import {
  ONBOARDING_STATUS_CACHE_KEY,
  ONBOARDING_STATUS_UPDATED_EVENT,
} from "@/lib/api/mekoApi";

type Args = {
  setHasCompletedOnboarding: (value: boolean) => void;
};

export function useOnboardingStatusSync({ setHasCompletedOnboarding }: Args) {
  useEffect(() => {
    const onOnboardingUpdated = (event: Event) => {
      const custom = event as CustomEvent<{ has_completed_onboarding?: boolean }>;
      const next = Boolean(custom.detail?.has_completed_onboarding);
      setHasCompletedOnboarding(next);
      setLocalItem(ONBOARDING_STATUS_CACHE_KEY, String(next));
    };

    addAppEventListener(ONBOARDING_STATUS_UPDATED_EVENT, onOnboardingUpdated);
    return () => {
      removeAppEventListener(ONBOARDING_STATUS_UPDATED_EVENT, onOnboardingUpdated);
    };
  }, [setHasCompletedOnboarding]);
}
