select ut.*
from accounttasks ut
inner join branches ou on ut.OrganizationUnitID = ou.ID and ou.TypeID & $1 = $1 and ou.IsDeleted = $3
where ut.IsActive = $4 and ut.IsCompleted = $5 and ut.TypeID = $2 and ut.UserID is null and ut.StartTime is null and ut.Data is not null;
