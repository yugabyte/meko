select k.*, u.ID, u.SessionKey
from accesskeys k
join accounts u on k.UserID = u.ID
where k.IsActive = $4
  and (k.ExpirationDate is null or k.ExpirationDate > $1)
  and u.ID != get_system_id()
  and k.ID > $2
order by k.ID
limit $3;
