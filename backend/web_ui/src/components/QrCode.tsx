// A QR code of some text, drawn as SVG. The qrcode package loads only when one is shown, so the
// app's main bundle doesn't carry it.

import { useEffect, useState } from "react";
import { Spinner } from "./ui";

export function QrCode({ text, label }: { text: string; label: string }) {
  const [svg, setSvg] = useState<string>();
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let live = true;
    setSvg(undefined);
    setFailed(false);
    import("qrcode")
      .then((mod) => {
        // A CommonJS package: the bundler puts its exports on default.
        const qr = mod.default ?? mod;
        return qr.toString(text, { type: "svg", margin: 1, errorCorrectionLevel: "M" });
      })
      .then(
        (s) => live && setSvg(s),
        () => live && setFailed(true),
      );
    return () => {
      live = false;
    };
  }, [text]);

  if (failed) return <p className="text-sm text-muted-foreground">The QR code didn't load. Enter the key below instead.</p>;
  if (!svg) return <Spinner />;
  return (
    <img
      src={`data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}`}
      alt={label}
      className="size-44 rounded-lg border bg-white p-2"
    />
  );
}
