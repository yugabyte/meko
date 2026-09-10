insert into salesledger (ID, OrderID, OrderLineID, TypeID, CreatedByID, CreationTime, Description, OldValue, NewValue, RegionID)
select ID, OrderID, OrderLineID, TypeID, $1, CreationTime, Description, OldValue, NewValue, RegionID
from unnest($2)
returning 1;
