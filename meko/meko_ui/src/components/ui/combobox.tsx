// combobox.tsx — reusable searchable single-select dropdown (Popover + Command).
// Type to filter; the list is height-capped and scrolls (CommandList defaults to
// max-h-[300px] overflow-y-auto). Each option carries a stable `value` and a
// human `label`; the label is what's shown and searched.
import { useState } from "react";
import { Check, ChevronsUpDown } from "lucide-react";
import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";

export type ComboboxOption = { value: string; label: string };

type ComboboxProps = {
  options: ComboboxOption[];
  value: string;
  onChange: (value: string) => void;
  placeholder?: string; // trigger text when nothing is selected
  searchPlaceholder?: string;
  emptyText?: string;
  disabled?: boolean;
  className?: string; // trigger width / extra classes
  align?: "start" | "center" | "end";
  /** Put on the trigger, so a <Label htmlFor> can point at it. */
  id?: string;
  /** Set this when the combobox is inside a modal Dialog. The popover content
   *  is portalled to <body>, outside DialogContent, and a modal Dialog's
   *  scroll lock cancels wheel and touch scrolling everywhere outside its own
   *  subtree -- so the capped list cannot be scrolled by mouse at all. A modal
   *  popover installs its own lock covering its content, which restores it. */
  modal?: boolean;
  /** ARIA gives role=combobox its name from the author, never from its
   *  contents, so a trigger with neither this nor a Label is nameless to a
   *  screen reader however clear its placeholder reads. */
  "aria-label"?: string;
};

export function Combobox({
  options,
  value,
  onChange,
  placeholder = "Select…",
  searchPlaceholder = "Search…",
  emptyText = "No results found.",
  disabled,
  className,
  align = "start",
  id,
  "aria-label": ariaLabel,
  modal = false,
}: ComboboxProps) {
  const [open, setOpen] = useState(false);
  const selected = options.find((o) => o.value === value);

  return (
    <Popover open={open} onOpenChange={setOpen} modal={modal}>
      <PopoverTrigger asChild>
        <Button
          variant="outline"
          role="combobox"
          id={id}
          aria-label={ariaLabel}
          aria-expanded={open}
          disabled={disabled}
          className={cn("w-64 justify-between font-normal", className)}
        >
          <span className="truncate">{selected ? selected.label : placeholder}</span>
          <ChevronsUpDown className="ml-2 h-4 w-4 shrink-0 opacity-50" />
        </Button>
      </PopoverTrigger>
      <PopoverContent className="w-[var(--radix-popover-trigger-width)] p-0" align={align}>
        <Command>
          <CommandInput placeholder={searchPlaceholder} />
          <CommandList>
            <CommandEmpty>{emptyText}</CommandEmpty>
            <CommandGroup>
              {options.map((o) => (
                <CommandItem
                  // cmdk keys items by the string passed as `value`, so
                  // two options with the same label would collapse into
                  // one row (keyboard arrows can't move past the first,
                  // Enter always selects the first). Use the stable
                  // option.value as the cmdk key and keep the label as
                  // a search `keyword` so type-to-filter still matches.
                  key={o.value}
                  value={o.value}
                  keywords={[o.label]}
                  onSelect={() => {
                    onChange(o.value);
                    setOpen(false);
                  }}
                >
                  <Check
                    className={cn("mr-2 h-4 w-4", value === o.value ? "opacity-100" : "opacity-0")}
                  />
                  <span className="truncate">{o.label}</span>
                </CommandItem>
              ))}
            </CommandGroup>
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  );
}
