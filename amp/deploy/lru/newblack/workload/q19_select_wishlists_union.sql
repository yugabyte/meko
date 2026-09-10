select w.*
from (
  select w.*, $3 as PrivilegesID
  from savedlists w
  inner join accounts u on u.ID = w.UserID and u.IsDeleted = $4
  where w.ID = $1 and w.IsDeleted = $5 and ($2 is null or w.UserID = $2)

  union

  select w.*, wsr.PrivilegesID
  from savedlists w
  inner join accounts u on u.ID = w.UserID and u.IsDeleted = $6
  inner join savedlistlinks wl on wl.WishlistID = w.ID
  inner join savedlistshares wsr on wsr.ID = wl.WishlistShareRegistrationID and wsr.IsDeleted = $7
  where w.ID = $1 and w.IsDeleted = $8 and ($2 is null or wl.UserID = $2)
) as w;
