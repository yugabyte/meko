// Right-hand agent side panel + the context that drives it.
//
// `AgentPanelProvider` holds which agent panel (if any) is open and
// renders a single <Sheet> for it, so any component under the provider
// can call `useAgentPanel().open(id)` to slide it in from any page.
// Panel content (name, icon, copy, CTA) comes from AGENT_PANELS in
// navConfig; styling comes from layoutStyles.

import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { Button } from "@/components/ui/button";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetTitle,
} from "@/components/ui/sheet";
import { cn } from "@/lib/utils";
import { AGENT_PANELS, type AgentPanelId } from "./navConfig";
import { layoutStyles as s } from "./layoutStyles";

type AgentPanelContextValue = { open: (id: AgentPanelId) => void };

const AgentPanelContext = createContext<AgentPanelContextValue | null>(null);

export function AgentPanelProvider({ children }: { children: ReactNode }) {
  const [activeId, setActiveId] = useState<AgentPanelId | null>(null);

  const open = useCallback((id: AgentPanelId) => setActiveId(id), []);
  const value = useMemo<AgentPanelContextValue>(() => ({ open }), [open]);

  const cfg = activeId ? AGENT_PANELS[activeId] : null;
  const Icon = cfg?.icon;

  return (
    <AgentPanelContext.Provider value={value}>
      {children}
      <Sheet
        open={activeId !== null}
        onOpenChange={(next) => {
          if (!next) setActiveId(null);
        }}
      >
        <SheetContent className={s.panelContent}>
          {cfg && Icon && (
            <>
              {/* Header */}
              <div className={s.panelHeader}>
                <span className={cn(s.panelTile, cfg.tileClass)}>
                  <Icon className={s.panelTileIcon} />
                </span>
                <div className="min-w-0">
                  <SheetTitle className={s.panelTitle}>{cfg.name}</SheetTitle>
                  <SheetDescription className={s.panelSubtitle}>
                    {cfg.subtitle}
                  </SheetDescription>
                </div>
              </div>

              {/* Body */}
              <div className={s.panelBodyRow}>
                <span className={cn(s.panelTileSm, cfg.tileClass)}>
                  <Icon className={s.panelTileSmIcon} />
                </span>
                <div className="min-w-0">
                  <div className={cn(s.panelLabel, cfg.labelClass)}>
                    {cfg.label}
                  </div>
                  <p className={s.panelText}>{cfg.description}</p>
                </div>
              </div>

              {/* CTA — disabled until a backend exists. */}
              <Button className={s.panelCta} disabled>
                {cfg.cta}
              </Button>
            </>
          )}
        </SheetContent>
      </Sheet>
    </AgentPanelContext.Provider>
  );
}

export function useAgentPanel(): AgentPanelContextValue {
  const ctx = useContext(AgentPanelContext);
  if (!ctx) {
    throw new Error("useAgentPanel must be used within an <AgentPanelProvider>");
  }
  return ctx;
}
