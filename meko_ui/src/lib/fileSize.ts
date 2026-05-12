const KB = 1024;
const MB = KB * 1024;
const GB = MB * 1024;

// Reuse formatter instance instead of creating one each call.
const numberFormatter = new Intl.NumberFormat();

function withUnit(value: number, unit: "KB" | "MB" | "GB"): string {
  if (value < 10) {
    return `${value.toFixed(1)} ${unit}`;
  }
  return `${Math.round(value)} ${unit}`;
}

export function formatFileSize(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return "—";

  const roundedBytes = Math.round(bytes);
  if (roundedBytes < KB) return `${numberFormatter.format(roundedBytes)} B`;
  if (roundedBytes < MB) return withUnit(roundedBytes / KB, "KB");
  if (roundedBytes < GB) return withUnit(roundedBytes / MB, "MB");
  return withUnit(roundedBytes / GB, "GB");
}
