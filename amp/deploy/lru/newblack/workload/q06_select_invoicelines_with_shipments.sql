select i.*, sl.*, s.*
from billinglines i
left join dispatchlines sl on (sl.ID = i.ShipmentLineID) and sl.RegionID = $1
left join dispatches s on (s.ID = sl.ShipmentID) and (s.RegionID = $1)
where i.InvoiceID = $2 and i.RegionID = $1;
