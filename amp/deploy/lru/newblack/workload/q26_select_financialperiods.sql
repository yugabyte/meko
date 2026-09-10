select fp.*, pfp.*
from accountingperiods fp
left join accountingperiods pfp on fp.PreviousPeriodID = pfp.ID
where fp.StatusID = any($1);
