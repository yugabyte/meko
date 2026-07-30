// UserScopeSelect — metrics scope picker: pick a single user by email. Thin
// wrapper mapping AdminUser[] onto the reusable searchable Combobox.
import { type AdminUser } from "@/lib/api/adminApi";
import { Combobox } from "@/components/ui/combobox";

type Props = {
  users: AdminUser[];
  value: string;
  onChange: (value: string) => void;
  disabled?: boolean;
};

export function UserScopeSelect({ users, value, onChange, disabled }: Props) {
  const options = users.map((u) => ({ value: u.id, label: u.email }));
  return (
    <Combobox
      options={options}
      value={value}
      onChange={onChange}
      disabled={disabled}
      placeholder="Select user…"
      searchPlaceholder="Search users…"
      emptyText="No users found."
    />
  );
}
