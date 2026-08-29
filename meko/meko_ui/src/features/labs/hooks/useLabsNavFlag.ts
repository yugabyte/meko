import { useFeatureFlag } from "@/lib/useFeatureFlag";

/** Pre-GA gate for the Labs nav entry + routes. */
export function useLabsNavFlag(): boolean {
  return useFeatureFlag("showLabs");
}
