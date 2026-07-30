import { Info, Lock, Rocket, ShieldCheck } from "lucide-react";
import { Card } from "@/components/ui/card";
import { SALES_CONTACT_EMAIL } from "@/features/billing/constants/plans";

/**
 * Bottom row + footer copy from screenshot 4 — three info cards
 * (Compliance / Auth / Discounts) and a soft-cap explainer beneath.
 */
export function BillingInfoCards() {
  return (
    <section className="space-y-4">
      <div className="grid grid-cols-1 gap-3 md:grid-cols-3">
        <InfoCard
          icon={<ShieldCheck className="h-4 w-4 text-amber-500" aria-hidden />}
          label="Compliance"
          title="Need long-term decision traces?"
          body="EU AI Act Article 18 requires 10+ year retention for high-risk systems (credit, hiring, healthcare). Available on Enterprise."
          linkLabel="Talk to sales"
          href={`mailto:${SALES_CONTACT_EMAIL}`}
          linkClass="text-amber-600 dark:text-amber-300"
        />
        <InfoCard
          icon={<Lock className="h-4 w-4 text-emerald-500" aria-hidden />}
          label="Auth"
          title="Provisioning for your team?"
          body="Google & Microsoft Entra SSO on Scale. Full SAML/SCIM provisioning on Enterprise."
          linkLabel="See Scale features"
          href="#scale"
          linkClass="text-emerald-600 dark:text-emerald-300"
        />
        <InfoCard
          icon={<Rocket className="h-4 w-4 text-blue-500" aria-hidden />}
          label="Discounts"
          title="Startups & students"
          body="Pre-Series A startups (<$5M raised) get Growth free for 12 months. Students get Starter free with .edu."
          linkLabel="Apply"
          href={`mailto:${SALES_CONTACT_EMAIL}?subject=Startup%2FStudent%20discount`}
          linkClass="text-blue-600 dark:text-blue-300"
        />
      </div>

      <Card className="flex items-start gap-2 bg-card/40 p-4 text-xs text-muted-foreground shadow-none">
        <Info className="mt-0.5 h-3.5 w-3.5 shrink-0 text-muted-foreground" aria-hidden />
        <p>
          <span className="font-medium text-foreground">Soft caps with predictable overage.</span>{" "}
          Paid plans never hard-stop production traffic. You&apos;re notified at 80% and 100% of your
          limit; overage is billed at per-unit rates (e.g.{" "}
          <code className="rounded bg-foreground/5 px-1 py-0.5 font-mono text-[10px]">$0.50 / 1K memory ops</code>
          ) or you can set a hard cap to throttle requests once exceeded.
        </p>
      </Card>

      <p className="text-center text-[11px] text-muted-foreground">
        Prices in USD. Subject to applicable taxes.{" "}
        <a className="underline-offset-2 hover:underline" href="#terms">
          Terms
        </a>{" "}
        ·{" "}
        <a className="underline-offset-2 hover:underline" href="#fair-use">
          Fair-use policy
        </a>
      </p>
    </section>
  );
}

function InfoCard({
  icon,
  label,
  title,
  body,
  linkLabel,
  href,
  linkClass,
}: {
  icon: React.ReactNode;
  label: string;
  title: string;
  body: string;
  linkLabel: string;
  href: string;
  linkClass: string;
}) {
  return (
    <Card className="space-y-2 bg-card/60 p-5 shadow-none">
      <div className="flex items-center gap-1.5 text-[10px] font-semibold uppercase tracking-wider text-muted-foreground">
        {icon}
        {label}
      </div>
      <h3 className="text-sm font-semibold text-foreground">{title}</h3>
      <p className="text-xs leading-relaxed text-muted-foreground">{body}</p>
      <a
        href={href}
        className={`inline-flex items-center gap-1 text-xs font-medium ${linkClass}`}
      >
        {linkLabel} <span aria-hidden>→</span>
      </a>
    </Card>
  );
}
