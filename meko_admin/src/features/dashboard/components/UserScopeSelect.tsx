import { type AdminUser } from "@/lib/adminApi";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";

const ALL = "all";

type Props = {
  users: AdminUser[];
  value: string;
  onChange: (value: string) => void;
  disabled?: boolean;
};

// Dashboard scope picker: "All users" (default) or a specific user by email.
export function UserScopeSelect({ users, value, onChange, disabled }: Props) {
  return (
    <Select value={value} onValueChange={onChange} disabled={disabled}>
      <SelectTrigger className="w-64 border-border">
        <SelectValue placeholder="All users" />
      </SelectTrigger>
      <SelectContent>
        <SelectItem value={ALL}>All users</SelectItem>
        {users.map((u) => (
          <SelectItem key={u.id} value={u.id}>
            {u.email}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}

export const ALL_USERS = ALL;
