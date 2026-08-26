// Rules for a compute reservation, checked before the request goes out. The
// controller enforces the same ones and answers 400 (`memoryQuantityRE` in
// controller/internal/api/handlers.go); these exist so the user is told at the
// input rather than by a failed round trip.

/** Memory needs an explicit unit of Mi or larger, binary or decimal-SI.
 *  Kubernetes reads a unitless quantity as BYTES, so "4" reserves four bytes
 *  instead of 4Gi, and an endpoint sized that way can never be resized in place
 *  afterwards (meko#492). */
const MEMORY_QUANTITY = /^\d+(\.\d+)?(Mi|Gi|Ti|Pi|Ei|M|G|T|P|E)$/;

export function isValidMemory(memory: string): boolean {
  return MEMORY_QUANTITY.test(memory.trim());
}
