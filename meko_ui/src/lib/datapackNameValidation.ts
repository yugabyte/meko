const MIN_DATAPACK_NAME_LENGTH = 3;
const MAX_DATAPACK_NAME_LENGTH = 60;
const htmlRegex = /<[^>]*>/;

export const DATAPACK_NAME_REQUIREMENTS =
  "Use 3-60 characters and do not include HTML tags.";

export function validateDatapackName(name: string): string | null {
  const trimmed = name.trim();
  // Datapack names are mirrored to Langfuse project names during create and
  // rename, so this intentionally mirrors Langfuse's projectNameSchema.
  if (!trimmed) return "Enter a datapack name.";
  if (trimmed.length < MIN_DATAPACK_NAME_LENGTH) {
    return "Must have at least 3 characters";
  }
  if (trimmed.length > MAX_DATAPACK_NAME_LENGTH) {
    return "Must have at most 60 characters";
  }
  if (htmlRegex.test(trimmed)) {
    return "Text cannot contain HTML tags";
  }
  return null;
}
