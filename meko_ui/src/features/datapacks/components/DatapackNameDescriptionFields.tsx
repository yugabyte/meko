import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";

export type DatapackNameDescriptionFieldsProps = {
  name: string;
  description: string;
  onNameChange: (value: string) => void;
  onDescriptionChange: (value: string) => void;
  disabled?: boolean;
  /** Lock only the name input while leaving description editable. */
  nameDisabled?: boolean;
  nameInputId: string;
  descriptionInputId: string;
  className?: string;
};

/**
 * Shared name + description inputs for datapack edit page and configure-wizard “Configure” step.
 */
export function DatapackNameDescriptionFields({
  name,
  description,
  onNameChange,
  onDescriptionChange,
  disabled,
  nameDisabled,
  nameInputId,
  descriptionInputId,
  className,
}: DatapackNameDescriptionFieldsProps) {
  return (
    <div className={cn("space-y-6 rounded-xl border border-border bg-card/40 p-6 shadow-sm", className)}>
      <div className="flex flex-col gap-3">
        <Label htmlFor={nameInputId}>Name</Label>
        <Input
          id={nameInputId}
          value={name}
          onChange={(e) => onNameChange(e.target.value)}
          placeholder="e.g. foo-project"
          autoComplete="off"
          disabled={disabled || nameDisabled}
          className="h-10 border-border bg-background shadow-sm"
        />
      </div>
      <div className="flex flex-col gap-3">
        <Label htmlFor={descriptionInputId}>Description</Label>
        <Textarea
          id={descriptionInputId}
          value={description}
          onChange={(e) => onDescriptionChange(e.target.value)}
          placeholder="Optional — shown where this datapack is summarized."
          rows={4}
          disabled={disabled}
          className="min-h-[100px] resize-y border-border bg-background shadow-sm"
        />
      </div>
    </div>
  );
}
